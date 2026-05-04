package tail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/jacobcase/gotail/v2/watch"
)

// Position is an alias for [watch.Position] so L2 callers do not need to
// import the watch package just to handle positions and checkpoints.
type Position = watch.Position

// Record carries one complete line and its position in the file series.
// The Line slice is valid only until the next call to [Tailer.Next] or
// [Tailer.Close]; copy it if you need to retain it beyond the iteration step.
type Record struct {
	Line []byte
	Pos  Position
}

// MissingPolicy controls what [New] does when the loaded checkpoint names a
// file no longer present in the source.
type MissingPolicy int

const (
	// FallbackOldest resumes at the oldest still-present file, offset 0, and
	// fires [Options.OnDropped] with the count of dropped files.
	FallbackOldest MissingPolicy = iota
	// Fail returns [ErrCheckpointMissing] from [New].
	Fail
	// SkipToActive ignores the stale checkpoint and resumes at the active
	// file (last in Source.Enumerate), offset 0.
	SkipToActive
)

// Options configures a [Tailer].
type Options struct {
	// Source enumerates the files in the log stream (required).
	Source Source
	// Cursor persists checkpoints across restarts. Nil means no persistence.
	Cursor Cursor
	// Logger is optional; defaults to slog.Default().
	Logger *slog.Logger
	// Interval is the poll interval. Zero defaults to 1 second.
	Interval time.Duration
	// UseFsnotify requests the fsnotify backend when available. Falls back to
	// polling.
	UseFsnotify bool
	// Whence controls the initial seek position for the first file opened.
	// Must be [io.SeekStart], [io.SeekCurrent], or [io.SeekEnd].
	// Zero (io.SeekStart) reads from the beginning; [io.SeekEnd] skips
	// existing content and tails only new data. Ignored when a Cursor
	// provides a resume point.
	Whence int
	// StopAtEOF causes Next to return [ErrSourceExhausted] once the active
	// file reaches EOF instead of blocking for new data.
	StopAtEOF bool
	// OnMissingCheckpoint controls behaviour when the loaded checkpoint names
	// a file that is no longer present. Default is [FallbackOldest].
	OnMissingCheckpoint MissingPolicy
	// NoInodeCheck disables inode comparison during checkpoint resume and
	// rotation detection. Use on filesystems with unstable inodes (Windows
	// ReFS, certain FUSE mounts).
	NoInodeCheck bool

	// Hooks — all optional and nil-safe. Hooks are invoked synchronously
	// from the read loop and must not block; offload slow work to a
	// goroutine or buffered channel if needed.
	OnDropped    func(droppedFiles int)
	OnRotated    func(from, to Position)
	OnError      func(err error)
	OnTruncated  func(at Position)
	OnCheckpoint func(c Checkpoint)
}

// Tailer delivers lines from a file series with optional durable checkpointing.
//
// Tailer is not safe for concurrent use by multiple goroutines; however,
// Close and Position may be called from any goroutine.
type Tailer struct {
	opts  Options
	files []string // enumerated at construction; oldest-first snapshot
	lr    *watch.LineReader

	fileIdx    int  // index of the currently-watched file in t.files
	atActive   bool // true when fileIdx == len(files)-1
	whenceUsed bool // true after the initial seek (opts.Whence) has been applied

	mu        sync.Mutex
	cur       Position
	lastMeta  json.RawMessage // preserved across plain Commit calls

	done     chan struct{}
	doneOnce sync.Once

	// closeCtx is cancelled by Close. It is wired into the per-call ctx of
	// Next via context.AfterFunc so that Close interrupts any blocking
	// LineReader.Next, allowing Close to await in-flight Next calls before
	// touching lr (which is not safe for concurrent use).
	closeCtx    context.Context
	closeCancel context.CancelFunc
	activeNext  sync.WaitGroup
	closeOnce   sync.Once
}

// New constructs a Tailer. opts.Source must be non-nil.
//
// New calls Source.Enumerate and, if a Cursor is provided, Cursor.Load with
// context.Background(). Long-running I/O during startup should be avoided
// in custom Source and Cursor implementations.
func New(opts Options) (*Tailer, error) {
	if opts.Source == nil {
		return nil, errors.New("tail: Options.Source must not be nil")
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	lg := opts.Logger
	if lg == nil {
		lg = slog.Default()
	}

	ctx := context.Background()

	files, err := opts.Source.Enumerate(ctx)
	if err != nil {
		return nil, fmt.Errorf("tail: enumerate source: %w", err)
	}
	if len(files) == 0 {
		return nil, ErrSourceExhausted
	}

	startIdx := len(files) - 1 // default: start at active file
	var resumePos *watch.Position
	var lastMeta json.RawMessage

	if opts.Cursor != nil {
		cp, found, err := opts.Cursor.Load(ctx)
		if err != nil {
			return nil, fmt.Errorf("tail: load cursor: %w", err)
		}
		if found {
			lastMeta = cp.Meta
			// Find which file in the series matches the checkpoint inode.
			matchIdx := findFileByInode(files, cp.Pos.Inode, opts.NoInodeCheck)

			if matchIdx >= 0 {
				startIdx = matchIdx
				resumePos = &cp.Pos
			} else {
				// No file matches the checkpoint → apply missing policy.
				switch opts.OnMissingCheckpoint {
				case Fail:
					return nil, ErrCheckpointMissing
				case SkipToActive:
					startIdx = len(files) - 1
				case FallbackOldest:
					startIdx = 0
					if opts.OnDropped != nil {
						opts.OnDropped(1)
					}
				}
			}
		}
	}

	closeCtx, closeCancel := context.WithCancel(context.Background())
	t := &Tailer{
		opts:        opts,
		files:       files,
		fileIdx:     startIdx,
		atActive:    startIdx == len(files)-1,
		lastMeta:    lastMeta,
		done:        make(chan struct{}),
		closeCtx:    closeCtx,
		closeCancel: closeCancel,
	}

	if err := t.openFile(files[startIdx], resumePos, lg); err != nil {
		return nil, err
	}
	return t, nil
}

// openFile creates a new watcher+linereader for the file at path.
// isBackup files use StopAtEOF=true so the watcher signals exhaustion.
func (t *Tailer) openFile(path string, resume *watch.Position, lg *slog.Logger) error {
	isActive := t.fileIdx == len(t.files)-1
	// Whence is a one-shot initial-seek setting: it applies only on the first
	// open (no resume cursor and the initial seek has not yet been consumed).
	// Subsequent opens triggered by advance/rotate always start at offset 0.
	whence := io.SeekStart
	if resume == nil && !t.whenceUsed && t.opts.Whence != 0 {
		whence = t.opts.Whence
		t.whenceUsed = true
	}
	wc := watch.Config{
		Path:         path,
		Interval:     t.opts.Interval,
		Whence:       whence,
		Resume:       resume,
		StopAtEOF:    !isActive || t.opts.StopAtEOF,
		NoInodeCheck: t.opts.NoInodeCheck,
		Logger:       lg,
	}
	var w watch.Watcher
	var err error
	if t.opts.UseFsnotify {
		w, err = watch.New(wc)
	} else {
		w, err = watch.NewPolling(wc)
	}
	if err != nil {
		return fmt.Errorf("tail: open %s: %w", path, err)
	}
	lrOpts := watch.LineOptions{}
	if t.opts.OnTruncated != nil {
		fn := t.opts.OnTruncated
		lrOpts.OnTruncated = func(at watch.Position) { fn(at) }
	}
	t.lr = watch.NewLineReader(w, lrOpts)
	return nil
}

// findFileByInode returns the index in files whose inode matches want, or -1.
// When noInodeCheck is true, the first existing file is treated as a match
// (used for filesystems without stable inodes — ReFS, some FUSE mounts).
func findFileByInode(files []string, want uint64, noInodeCheck bool) int {
	for i, path := range files {
		cur, err := watch.StatInode(path)
		if err != nil {
			continue // file may not exist yet
		}
		if noInodeCheck {
			return i
		}
		if cur == want {
			return i
		}
	}
	return -1
}

// advance closes the current LineReader and opens the next file in the series.
func (t *Tailer) advance(ctx context.Context) error {
	prev := t.cur

	t.mu.Lock()
	lr := t.lr
	t.mu.Unlock()
	_ = lr.Close()

	nextIdx := t.fileIdx + 1
	if nextIdx >= len(t.files) {
		// Defensive: advance() is only entered when !atActive, and we set
		// atActive on the transition to the last file, so this branch is
		// not reached under normal flow. A custom Source with finite
		// enumeration could still reach it. Only close done in StopAtEOF
		// mode; live-tail callers don't expect Done() to fire spontaneously.
		if t.opts.StopAtEOF {
			t.doneOnce.Do(func() { close(t.done) })
		}
		return ErrSourceExhausted
	}

	nextPath := t.files[nextIdx]
	t.fileIdx = nextIdx
	t.atActive = nextIdx == len(t.files)-1

	if err := t.openFile(nextPath, nil, t.opts.Logger); err != nil {
		return err
	}

	if t.opts.OnRotated != nil {
		t.opts.OnRotated(prev, Position{File: nextPath})
	}
	return nil
}

// Records returns an iterator over lines in the file series.
//
// The cursor is never auto-advanced; call [Tailer.Commit] explicitly.
// Breaking out of a range-over-Records loop stops iteration cleanly.
func (t *Tailer) Records(ctx context.Context) iter.Seq2[Record, error] {
	return func(yield func(Record, error) bool) {
		for {
			rec, err := t.Next(ctx)
			if !yield(rec, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

// Next returns the next line. It blocks until a line is available, the context
// is cancelled, or (in StopAtEOF mode) the file series is exhausted.
//
// When [Tailer.Close] is called concurrently, the internal closeCtx is wired
// into Next's ctx via context.AfterFunc so a blocking LineReader.Next returns
// promptly. Close waits for all in-flight Next calls before tearing down lr.
func (t *Tailer) Next(ctx context.Context) (Record, error) {
	t.activeNext.Add(1)
	defer t.activeNext.Done()

	if err := t.closeCtx.Err(); err != nil {
		return Record{}, ErrSourceExhausted
	}

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(t.closeCtx, cancel)
	defer stop()

	for {
		t.mu.Lock()
		lr := t.lr
		t.mu.Unlock()

		line, pos, err := lr.Next(callCtx)
		if err != nil {
			if t.closeCtx.Err() != nil {
				// Close is in progress; surface as exhaustion rather than ctx.Err.
				return Record{}, ErrSourceExhausted
			}
			if errors.Is(err, io.EOF) {
				if t.atActive {
					// Active file exhausted (StopAtEOF was set in watcher).
					t.doneOnce.Do(func() { close(t.done) })
					return Record{}, ErrSourceExhausted
				}
				// Backup file exhausted — advance to the next file.
				if aerr := t.advance(callCtx); aerr != nil {
					return Record{}, aerr
				}
				continue
			}
			if t.opts.OnError != nil {
				t.opts.OnError(err)
			}
			return Record{}, err
		}

		t.mu.Lock()
		t.cur = pos
		t.mu.Unlock()

		return Record{Line: line, Pos: pos}, nil
	}
}

// Commit persists pos as a new Checkpoint, preserving any metadata from the
// previous commit. If no Cursor was configured it is a no-op.
func (t *Tailer) Commit(ctx context.Context, pos Position) error {
	if t.opts.Cursor == nil {
		return nil
	}
	t.mu.Lock()
	meta := t.lastMeta
	t.mu.Unlock()

	cp := Checkpoint{Pos: pos, Meta: meta}
	if err := t.opts.Cursor.Save(ctx, cp); err != nil {
		return fmt.Errorf("tail: commit: %w", err)
	}
	if t.opts.OnCheckpoint != nil {
		t.opts.OnCheckpoint(cp)
	}
	return nil
}

// CommitWithMeta persists pos together with user-defined metadata. meta must
// be JSON-serializable. The encoded metadata is retained in memory so that
// subsequent plain [Tailer.Commit] calls preserve it.
func (t *Tailer) CommitWithMeta(ctx context.Context, pos Position, meta any) error {
	if t.opts.Cursor == nil {
		return nil
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("tail: marshal meta: %w", err)
	}

	t.mu.Lock()
	t.lastMeta = json.RawMessage(b)
	t.mu.Unlock()

	cp := Checkpoint{Pos: pos, Meta: json.RawMessage(b)}
	if err := t.opts.Cursor.Save(ctx, cp); err != nil {
		return fmt.Errorf("tail: commit: %w", err)
	}
	if t.opts.OnCheckpoint != nil {
		t.opts.OnCheckpoint(cp)
	}
	return nil
}

// Position returns the position of the most recently yielded record.
// Safe to call from any goroutine.
func (t *Tailer) Position() Position {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur
}

// Done is closed when the file series is fully exhausted in StopAtEOF mode —
// either the active file reached EOF, or a finite Source's enumeration was
// walked off the end. In live-tail mode (StopAtEOF=false) the Tailer never
// closes Done itself; callers signal shutdown via [Tailer.Close] and observe
// it through the regular error path.
func (t *Tailer) Done() <-chan struct{} {
	return t.done
}

// Close releases all resources held by the Tailer. It is idempotent and safe
// to call from any goroutine. Any pending uncommitted line is discarded;
// commit before closing if needed.
//
// Close cancels the internal closeCtx and blocks until all in-flight Next
// calls have returned, so that LineReader.Close — which is not safe for
// concurrent use with Next — runs without a race.
func (t *Tailer) Close() error {
	var rerr error
	t.closeOnce.Do(func() {
		t.closeCancel()
		t.activeNext.Wait()

		t.mu.Lock()
		lr := t.lr
		t.mu.Unlock()
		if lr != nil {
			rerr = lr.Close()
		}
		if t.opts.Cursor != nil {
			if err := t.opts.Cursor.Close(); err != nil && rerr == nil {
				rerr = err
			}
		}
	})
	return rerr
}

