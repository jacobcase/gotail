package tail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
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
	// FallbackOldest resumes at the oldest still-present file, offset 0.
	// For single-file sources this is equivalent to SkipToActive.
	// Full multi-file semantics are implemented in Phase 3.
	FallbackOldest MissingPolicy = iota
	// Fail returns [ErrCheckpointMissing] from [New].
	Fail
	// SkipToActive ignores the stale checkpoint and resumes at the active
	// file, offset 0.
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
	// polling. (Phase 6 feature; currently always uses polling.)
	UseFsnotify bool
	// StopAtEOF causes Next to return [ErrSourceExhausted] once the active
	// file reaches EOF instead of blocking for new data.
	StopAtEOF bool
	// OnMissingCheckpoint controls behaviour when the loaded checkpoint names
	// a file that is no longer present. Default is [FallbackOldest].
	OnMissingCheckpoint MissingPolicy

	// Hooks — all optional and nil-safe.
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
	opts   Options
	lr     *watch.LineReader

	mu       sync.Mutex
	cur      Position
	lastMeta json.RawMessage // last committed meta; preserved across plain Commit calls

	done     chan struct{}
	doneOnce sync.Once
	closed   chan struct{}
	closeOnce sync.Once
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
	activePath := files[len(files)-1]

	var resumePos *watch.Position
	var lastMeta json.RawMessage

	if opts.Cursor != nil {
		cp, found, err := opts.Cursor.Load(ctx)
		if err != nil {
			return nil, fmt.Errorf("tail: load cursor: %w", err)
		}
		if found {
			lastMeta = cp.Meta
			inodeMismatch, err := checkInode(activePath, cp.Pos.Inode)
			if err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("tail: stat %s: %w", activePath, err)
			}
			if !inodeMismatch {
				resumePos = &cp.Pos
			} else {
				switch opts.OnMissingCheckpoint {
				case Fail:
					return nil, ErrCheckpointMissing
				case SkipToActive, FallbackOldest:
					// Start from offset 0 of the active file.
					// FallbackOldest's full multi-file semantics are Phase 3.
				}
			}
		}
	}

	wc := watch.Config{
		Path:      activePath,
		Interval:  opts.Interval,
		Resume:    resumePos,
		StopAtEOF: opts.StopAtEOF,
		Logger:    lg,
	}
	w, err := watch.NewPolling(wc)
	if err != nil {
		return nil, fmt.Errorf("tail: create watcher: %w", err)
	}
	lr := watch.NewLineReader(w, watch.LineOptions{})

	return &Tailer{
		opts:     opts,
		lr:       lr,
		lastMeta: lastMeta,
		done:     make(chan struct{}),
		closed:   make(chan struct{}),
	}, nil
}

// checkInode stats path and reports whether its inode differs from want.
// Returns (false, nil) if the inodes match. Returns (true, nil) on mismatch.
// Returns (false, err) if stat fails.
func checkInode(path string, want uint64) (mismatch bool, err error) {
	current, err := watch.StatInode(path)
	if err != nil {
		return false, err
	}
	// If both are 0 (Windows or inode-less FS), treat as match so callers
	// can resume by offset without inode validation.
	if want == 0 && current == 0 {
		return false, nil
	}
	return current != want, nil
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
// is cancelled, or (in StopAtEOF mode) the file is exhausted.
func (t *Tailer) Next(ctx context.Context) (Record, error) {
	select {
	case <-t.closed:
		return Record{}, ErrSourceExhausted
	default:
	}

	line, pos, err := t.lr.Next(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			t.doneOnce.Do(func() { close(t.done) })
			return Record{}, ErrSourceExhausted
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
// be JSON-serializable. The encoded metadata is also retained in memory so
// that subsequent plain [Tailer.Commit] calls preserve it.
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

// Done is closed when the source is exhausted in StopAtEOF mode.
// In live-tail mode it is never closed by the Tailer.
func (t *Tailer) Done() <-chan struct{} {
	return t.done
}

// Close releases all resources held by the Tailer. It is idempotent. Any
// pending uncommitted line is discarded; commit before closing if needed.
func (t *Tailer) Close() error {
	var rerr error
	t.closeOnce.Do(func() {
		close(t.closed)
		rerr = t.lr.Close()
		if t.opts.Cursor != nil {
			if err := t.opts.Cursor.Close(); err != nil && rerr == nil {
				rerr = err
			}
		}
	})
	return rerr
}
