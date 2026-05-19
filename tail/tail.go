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
	"sync/atomic"
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
	// ForcePolling disables the fsnotify backend even when it is compiled
	// in. Default is auto: the Tailer prefers fsnotify and falls back to
	// polling on platforms or builds where fsnotify is unavailable
	// (gotail_nofsnotify build tag, or unsupported OS).
	ForcePolling bool
	// Whence controls the initial seek position for the first file opened.
	// Must be [io.SeekStart] or [io.SeekEnd]; [io.SeekCurrent] is rejected
	// because there is no defined "current" position at construction time.
	// Zero (io.SeekStart) reads from the beginning; [io.SeekEnd] skips
	// existing content and tails only new data. Ignored when a Cursor
	// provides a resume point.
	Whence int
	// SkipExisting is a discoverable convenience for [io.SeekEnd]: when
	// true (and Whence is zero) the Tailer starts at the end of the first
	// file and only yields lines written after construction. Has no effect
	// when a Cursor provides a resume point. Setting both SkipExisting and
	// a non-zero Whence returns an error from [New].
	SkipExisting bool
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
	// AllowInodeMismatch controls behaviour when the file at the cursor's
	// path still exists but has a different inode than the cursor recorded.
	// Default (false) is fail-safe: [New] returns an error wrapping
	// [ErrInodeMismatch], which prevents silent ingest of substituted content
	// in shared-FS / multi-tenant deployments. Set to true to fire
	// [OnInodeMismatch] (if set) and fall through to [OnMissingCheckpoint]
	// policy.
	AllowInodeMismatch bool
	// RequireCursor causes [New] to return an error when Cursor is nil.
	// Use this to prevent accidental deployment without checkpointing
	// (e.g. a nil cursor from a missing YAML field).
	RequireCursor bool

	// Hooks — all optional and nil-safe. Hooks are invoked synchronously
	// from the read loop and must not block; offload slow work to a
	// goroutine or buffered channel if needed.

	// OnDropped fires when the cursor names a file that is no longer present
	// and [OnMissingCheckpoint] resolves to [FallbackOldest]. The argument
	// signals that a drop occurred; the precise count of aged-off backups is
	// not tracked by the library (it requires historical Source state) and
	// is reported as 1.
	OnDropped func(droppedFiles int)
	OnRotated func(from, to Position)
	OnError   func(err error)
	// OnTruncated may fire from two paths: the watcher's truncation event,
	// and a late-detection check inside LineReader.Next when the file's size
	// has dropped below the current offset (defensive against copytruncate
	// races the watcher missed). Hooks must be idempotent across both sites.
	OnTruncated     func(at Position)
	OnCheckpoint    func(c Checkpoint)
	OnInodeMismatch func(want, got uint64)
}

// Stats is a point-in-time snapshot of counters maintained by a [Tailer].
//
// The snapshot is not transactionally consistent — each field is loaded via
// an independent atomic read, so values may reflect different instants in time.
// This matches the semantics of Prometheus scrapes and similar pull-style
// metrics systems.
//
// Stats survives [Tailer.Close]: counters are preserved post-close so a final
// scrape can record totals.
type Stats struct {
	BytesRead    int64
	LinesYielded int64
	Rotations    int64
	Errors       int64
	Position     Position
}

// tailerStats holds the atomic counters for a Tailer.
type tailerStats struct {
	bytesRead    atomic.Int64
	linesYielded atomic.Int64
	rotations    atomic.Int64
	errors       atomic.Int64
}

// Tailer delivers lines from a file series with optional durable checkpointing.
//
// Tailer is not safe for concurrent use by multiple goroutines; however,
// Close and Position may be called from any goroutine.
type Tailer struct {
	opts  Options
	files []string // enumerated at construction; oldest-first snapshot

	// lr is single-writer: written by New and by advance (which only runs
	// inside Next). Close reads it only after activeNext.Wait() has parked
	// the Next goroutine, so no lock is required around access.
	lr *watch.LineReader

	fileIdx    int  // index of the currently-watched file in t.files
	atActive   bool // true when fileIdx == len(files)-1
	whenceUsed bool // true after the initial seek (opts.Whence) has been applied

	// mu guards cur and lastMeta — accessed by Position/Commit from any
	// goroutine concurrent with Next.
	mu       sync.Mutex
	cur      Position
	lastMeta json.RawMessage // preserved across plain Commit calls

	stats tailerStats

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
// New calls Source.Enumerate and, if a Cursor is provided, Cursor.Load using
// the supplied ctx. The ctx governs only startup I/O; the Tailer's runtime
// loop uses the per-call ctx passed to [Tailer.Next] and the internal close
// signal from [Tailer.Close].
func New(ctx context.Context, opts Options) (_ *Tailer, returnErr error) {
	if opts.Source == nil {
		return nil, errors.New("tail: Options.Source must not be nil")
	}
	if opts.RequireCursor && opts.Cursor == nil {
		return nil, errors.New("tail: RequireCursor is set but Cursor is nil")
	}
	// Once New is entered with a non-nil Cursor, it owns the cursor's
	// lifecycle for the duration of this call. On any failure return, close
	// the cursor so callers don't leak the flock (acquired inside
	// NewFileCursor) when New fails after the cursor was already constructed.
	// The canonical caller pattern is `defer t.Close()` only on success; this
	// defer plugs the failure path.
	if opts.Cursor != nil {
		defer func() {
			if returnErr != nil {
				_ = opts.Cursor.Close()
			}
		}()
	}
	if opts.SkipExisting && opts.Whence != 0 {
		return nil, errors.New("tail: SkipExisting and Whence are mutually exclusive")
	}
	// SE-3: io.SeekCurrent has no defined semantics here (no resume point to
	// seek relative to) and silently falls through to SeekStart in the
	// watcher. Reject it explicitly so callers see the gap immediately.
	if opts.Whence != 0 && opts.Whence != io.SeekStart && opts.Whence != io.SeekEnd {
		return nil, fmt.Errorf("tail: Options.Whence %d is invalid (must be io.SeekStart or io.SeekEnd)", opts.Whence)
	}
	if opts.SkipExisting {
		opts.Whence = io.SeekEnd
	}
	// SE-11: negative Interval was silently coerced to 1s; reject it so
	// YAML-mapper bugs (e.g. unset duration → -1) don't get a default.
	if opts.Interval < 0 {
		return nil, fmt.Errorf("tail: Options.Interval must not be negative, got %v", opts.Interval)
	}
	if opts.Interval == 0 {
		opts.Interval = time.Second
	}
	lg := opts.Logger
	if lg == nil {
		lg = slog.Default()
	}

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
			matchIdx := findFileByInode(files, cp.Pos.Inode, cp.Pos.File, opts.NoInodeCheck)

			if matchIdx >= 0 {
				startIdx = matchIdx
				resumePos = &cp.Pos
			} else {
				// No file matches the checkpoint by inode. Distinguish two cases:
				//   (a) the cursor's named path still exists but has a different
				//       inode (rotation reused the path; inode-mismatch event), or
				//   (b) the path is gone entirely (drop event).
				// Both fire the appropriate hook; resolution depends on policy.
				if cp.Pos.File != "" {
					if curInode, serr := watch.StatInode(cp.Pos.File); serr == nil && curInode != cp.Pos.Inode {
						if opts.OnInodeMismatch != nil {
							opts.OnInodeMismatch(cp.Pos.Inode, curInode)
						}
						if !opts.AllowInodeMismatch {
							return nil, fmt.Errorf(
								"tail: cursor %s inode mismatch: want=%d got=%d: %w",
								cp.Pos.File, cp.Pos.Inode, curInode, ErrInodeMismatch)
						}
					}
				}
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
		Path:               path,
		Interval:           t.opts.Interval,
		Whence:             whence,
		Resume:             resume,
		StopAtEOF:          !isActive || t.opts.StopAtEOF,
		NoInodeCheck:       t.opts.NoInodeCheck,
		AllowInodeMismatch: t.opts.AllowInodeMismatch,
		OnInodeMismatch:    t.opts.OnInodeMismatch,
		Logger:             lg,
	}
	var w watch.Watcher
	var err error
	if t.opts.ForcePolling {
		w, err = watch.NewPolling(wc)
	} else {
		w, err = watch.New(wc)
	}
	if err != nil {
		return fmt.Errorf("tail: open %s: %w", path, err)
	}
	t.lr = watch.NewLineReader(w, watch.LineOptions{
		OnTruncated: t.opts.OnTruncated,
		OnRotated: func(from, to Position) {
			t.stats.rotations.Add(1)
			if t.opts.OnRotated != nil {
				t.opts.OnRotated(from, to)
			}
		},
	})
	return nil
}

// findFileByInode returns the index in files whose inode matches want, or -1.
//
// When noInodeCheck is true (filesystems without stable inodes — ReFS, some
// FUSE mounts), the inode check is skipped. In that case the tie-break is:
//  1. Prefer the file whose path equals wantPath (the cursor's named file)
//     if it still exists in the current enumeration.
//  2. Otherwise return the first existing file (oldest-existing fallback).
//
// The path-first tie-break prevents resume from landing at the wrong file
// when the cursor named a still-present file but the source enumeration
// also contains older files.
func findFileByInode(files []string, want uint64, wantPath string, noInodeCheck bool) int {
	if noInodeCheck {
		// Path-first tie-break: prefer the cursor's named file when present.
		if wantPath != "" {
			for i, path := range files {
				if path != wantPath {
					continue
				}
				if _, err := watch.StatInode(path); err == nil {
					return i
				}
			}
		}
		// Fallback: first existing file.
		for i, path := range files {
			if _, err := watch.StatInode(path); err == nil {
				return i
			}
		}
		return -1
	}
	for i, path := range files {
		cur, err := watch.StatInode(path)
		if err != nil {
			continue // file may not exist yet
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

	nextIdx := t.fileIdx + 1
	if nextIdx >= len(t.files) {
		// Defensive: advance() is only entered when !atActive, and we set
		// atActive on the transition to the last file, so this branch is
		// not reached under normal flow. A custom Source with finite
		// enumeration could still reach it. Only close done in StopAtEOF
		// mode; live-tail callers don't expect Done() to fire spontaneously.
		// Leave t.lr alone: closing it before knowing whether we can advance
		// would leave Next operating on a closed reader if the caller invokes
		// it again. Tailer.Close handles the eventual cleanup.
		if t.opts.StopAtEOF {
			t.doneOnce.Do(func() { close(t.done) })
		}
		return ErrSourceExhausted
	}

	// Close the previous reader only now that we know we are about to open a
	// new one. The order matters: see the defensive branch above.
	_ = t.lr.Close()

	nextPath := t.files[nextIdx]
	t.fileIdx = nextIdx
	t.atActive = nextIdx == len(t.files)-1

	if err := t.openFile(nextPath, nil, t.opts.Logger); err != nil {
		return err
	}

	t.stats.rotations.Add(1)
	if t.opts.OnRotated != nil {
		// Best-effort inode lookup: on failure the LineReader will surface
		// the real error during the first read against the new file; firing
		// the hook with inode=0 is no worse than leaving it unpopulated.
		inode, _ := watch.StatInode(nextPath)
		t.opts.OnRotated(prev, Position{File: nextPath, Inode: inode})
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
		line, pos, err := t.lr.Next(callCtx)
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
			t.stats.errors.Add(1)
			if t.opts.OnError != nil {
				t.opts.OnError(err)
			}
			return Record{}, err
		}

		t.mu.Lock()
		t.cur = pos
		t.mu.Unlock()

		t.stats.linesYielded.Add(1)
		t.stats.bytesRead.Add(int64(len(line)) + 1) // +1 for the newline

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

// Position returns the position of the most recently yielded record — the
// post-yield invariant the cursor commits against. Between calls to Next
// this matches the live read position; inside a hook firing mid-Next (e.g.
// OnTruncated) it may not yet reflect the in-flight event, since t.cur is
// assigned after a record is yielded, not after each watcher event.
// Safe to call from any goroutine.
func (t *Tailer) Position() Position {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur
}

// Stats returns a snapshot of runtime counters. The snapshot is not
// transactionally consistent across fields; each is loaded independently.
// Safe to call from any goroutine, including after [Tailer.Close].
func (t *Tailer) Stats() Stats {
	return Stats{
		BytesRead:    t.stats.bytesRead.Load(),
		LinesYielded: t.stats.linesYielded.Load(),
		Rotations:    t.stats.rotations.Load(),
		Errors:       t.stats.errors.Load(),
		Position:     t.Position(),
	}
}

// CloseWithFlush saves the most-recently-yielded position as a Checkpoint
// and then performs the normal [Tailer.Close] teardown. It is the explicit
// opt-in to "commit the current position before closing"; [Tailer.Close]
// alone discards uncommitted lines (Decision #19).
//
// If no Cursor was configured, CloseWithFlush degrades to Close.
//
// The ctx controls the Cursor.Save call. If ctx is cancelled, the save
// is aborted (and the error returned), but the underlying Close still runs
// to release file descriptors and locks.
//
// CloseWithFlush is idempotent: calling it (or Close) more than once is safe.
//
// The committed meta is the most-recently-committed meta; CloseWithFlush does
// not invent new metadata.
func (t *Tailer) CloseWithFlush(ctx context.Context) error {
	var saveErr error
	t.closeOnce.Do(func() {
		t.closeCancel()
		t.activeNext.Wait()

		// Persist current position if a cursor is configured and we have a
		// non-zero position to commit.
		if t.opts.Cursor != nil {
			t.mu.Lock()
			pos := t.cur
			meta := t.lastMeta
			t.mu.Unlock()

			if pos != (Position{}) {
				cp := Checkpoint{Pos: pos, Meta: meta}
				saveErr = t.opts.Cursor.Save(ctx, cp)
				// In SyncOnCommit / SyncBackground modes, Save buffers and
				// returns without writing to disk. CloseWithFlush promises
				// durability before teardown, so drive Sync explicitly when
				// the cursor implements Syncer. SyncAlways is a no-op here
				// because Save already flushed.
				if saveErr == nil {
					if s, ok := t.opts.Cursor.(Syncer); ok {
						saveErr = s.Sync(ctx)
					}
				}
				if saveErr == nil && t.opts.OnCheckpoint != nil {
					t.opts.OnCheckpoint(cp)
				}
			}
		}

		// Always close the LineReader and Cursor regardless of Save error.
		if t.lr != nil {
			if err := t.lr.Close(); err != nil && saveErr == nil {
				saveErr = err
			}
		}
		if t.opts.Cursor != nil {
			if err := t.opts.Cursor.Close(); err != nil && saveErr == nil {
				saveErr = err
			}
		}
	})
	return saveErr
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

		if t.lr != nil {
			rerr = t.lr.Close()
		}
		if t.opts.Cursor != nil {
			if err := t.opts.Cursor.Close(); err != nil && rerr == nil {
				rerr = err
			}
		}
	})
	return rerr
}
