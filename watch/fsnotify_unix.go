//go:build !gotail_nofsnotify && (linux || darwin || freebsd || netbsd || openbsd)

package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// fsnotifyWatcher implements [Watcher] using OS-level file notifications.
// It watches the parent directory (so it can detect creation of a not-yet-
// existing file) and reacts to write/create/rename/remove events on the
// watched path. Its internal state machine mirrors pollWatcher exactly; the
// only difference is the wait mechanism.
//
// Like pollWatcher, it owns no fd to the active file: the [LineReader]
// holds the only fd; this watcher observes via os.Stat.
type fsnotifyWatcher struct {
	c      Config
	target   string // filepath.Clean(c.Path) — cached for per-event matching
	logger   *slog.Logger
	fw       *fsnotify.Watcher
	shutdown <-chan struct{} // closed by the owner to interrupt a blocking Wait

	resume *Position
	whence int

	started bool
	pos     int64
	inode   uint64
}

// NewFsnotify returns a Watcher backed by OS file notifications (inotify on
// Linux, kqueue on macOS/BSD). It is compiled in by default; opt out with
// the gotail_nofsnotify build tag.
//
// It watches the parent directory so it detects creation of files that do not
// yet exist at construction time.
func NewFsnotify(c Config) (Watcher, error) {
	if c.Path == "" {
		return nil, errors.New("watch: Config.Path must not be empty")
	}
	if c.Whence != io.SeekStart && c.Whence != io.SeekEnd {
		return nil, fmt.Errorf("watch: Config.Whence %v is invalid (must be io.SeekStart or io.SeekEnd)", c.Whence)
	}
	lg := c.Logger
	if lg == nil {
		lg = slog.Default()
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watch: create fsnotify watcher: %w", err)
	}
	dir := filepath.Dir(c.Path)
	if err := fw.Add(dir); err != nil {
		fw.Close()
		return nil, fmt.Errorf("watch: watch dir %s: %w", dir, err)
	}
	return &fsnotifyWatcher{
		c:        c,
		target:   filepath.Clean(c.Path),
		logger:   lg,
		fw:       fw,
		shutdown: c.Shutdown,
		resume:   c.Resume,
		whence:   c.Whence,
	}, nil
}

// Wait mirrors pollWatcher.Wait but replaces the fixed-interval sleep with an
// OS event wait, giving sub-millisecond latency on supported platforms.
func (w *fsnotifyWatcher) Wait(ctx context.Context) (Event, error) {
	for {
		if err := w.stopErr(ctx); err != nil {
			return Event{}, err
		}

		if !w.started {
			ev, ok, err := w.openFirst()
			if err != nil {
				return Event{}, err
			}
			if ok {
				return ev, nil
			}
			if err := w.fsnWait(ctx); err != nil {
				return Event{}, err
			}
			continue
		}

		size, inode, err := statSizeInode(w.c.Path)
		if errors.Is(err, fs.ErrNotExist) {
			if w.c.StopAtEOF {
				return Event{}, io.EOF
			}
			if err := w.fsnWait(ctx); err != nil {
				return Event{}, err
			}
			continue
		}
		if err != nil {
			return Event{}, fmt.Errorf("watch: stat %s: %w", w.c.Path, err)
		}

		if !w.c.NoInodeCheck && inode != w.inode {
			oldInode := w.inode
			w.pos = 0
			w.inode = inode
			w.logger.Debug(fmt.Sprintf("watch: rotated (prev inode %d)", oldInode),
				"path", w.c.Path, "inode", inode, "offset", int64(0))
			return Event{
				Path:     w.c.Path,
				Pos:      Position{File: w.c.Path, Inode: inode, Offset: 0},
				ReOpened: true,
			}, nil
		}

		if size < w.pos {
			w.logger.Debug(fmt.Sprintf("watch: truncation (size %d -> %d)", w.pos, size),
				"path", w.c.Path, "inode", w.inode, "offset", int64(0))
			w.pos = 0
			return Event{
				Path:      w.c.Path,
				Pos:       Position{File: w.c.Path, Inode: w.inode, Offset: 0},
				Truncated: true,
			}, nil
		}

		if size > w.pos {
			ev := Event{
				Path: w.c.Path,
				Pos:  Position{File: w.c.Path, Inode: w.inode, Offset: w.pos},
			}
			w.pos = size
			return ev, nil
		}

		if w.c.StopAtEOF {
			return Event{}, io.EOF
		}
		if err := w.fsnWait(ctx); err != nil {
			return Event{}, err
		}
	}
}

func (w *fsnotifyWatcher) openFirst() (Event, bool, error) {
	size, inode, err := statSizeInode(w.c.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("watch: stat %s: %w", w.c.Path, err)
	}

	var offset int64
	if w.resume != nil && !w.resume.IsZero() {
		r := w.resume
		w.resume = nil
		if w.c.NoInodeCheck || r.Inode == inode {
			if r.Offset <= size {
				offset = r.Offset
			}
		} else {
			if w.c.OnInodeMismatch != nil {
				w.c.OnInodeMismatch(r.Inode, inode)
			}
			if !w.c.AllowInodeMismatch {
				return Event{}, false, fmt.Errorf(
					"watch: resume point inode mismatch on %s: want=%d got=%d: %w",
					w.c.Path, r.Inode, inode, ErrInodeMismatch)
			}
			w.logger.Warn(fmt.Sprintf("watch: resume point inode mismatch (want %d) — restarting at offset 0", r.Inode),
				"path", w.c.Path, "inode", inode, "offset", int64(0))
		}
	} else if w.whence == io.SeekEnd {
		offset = size
	}
	w.whence = io.SeekStart

	w.started = true
	w.pos = offset
	w.inode = inode

	w.logger.Debug("watch: opened",
		"path", w.c.Path, "inode", inode, "offset", offset)

	return Event{
		Path:     w.c.Path,
		Pos:      Position{File: w.c.Path, Inode: inode, Offset: offset},
		ReOpened: true,
	}, true, nil
}

// fsnWait blocks until a relevant fsnotify event arrives for the watched path,
// the watcher's event channel closes, or ctx is done. Returns nil when an
// event arrived, ctx.Err() when ctx is done, or ErrWatcherClosed when the
// watcher was closed concurrently (Events or Errors channel closed).
func (w *fsnotifyWatcher) fsnWait(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.shutdown:
			return ErrWatcherClosed
		case ev, ok := <-w.fw.Events:
			if !ok {
				return ErrWatcherClosed
			}
			if filepath.Clean(ev.Name) == w.target {
				return nil
			}
		case err, ok := <-w.fw.Errors:
			if !ok {
				return ErrWatcherClosed
			}
			w.logger.Warn("watch: fsnotify error", "err", err, "path", w.c.Path)
		}
	}
}

// stopErr reports a non-nil error if ctx is cancelled or the owner has closed
// Shutdown, letting Wait bail out between stat phases rather than only inside
// fsnWait.
func (w *fsnotifyWatcher) stopErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-w.shutdown:
		return ErrWatcherClosed
	default:
		return nil
	}
}

func (w *fsnotifyWatcher) Close() error {
	return w.fw.Close()
}
