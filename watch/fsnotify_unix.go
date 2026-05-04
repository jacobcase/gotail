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
	target string // filepath.Clean(c.Path) — cached for per-event matching
	logger *slog.Logger
	fw     *fsnotify.Watcher

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
	if c.Whence != io.SeekStart && c.Whence != io.SeekCurrent && c.Whence != io.SeekEnd {
		return nil, fmt.Errorf("watch: Config.Whence %v is invalid", c.Whence)
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
		c:      c,
		target: filepath.Clean(c.Path),
		logger: lg,
		fw:     fw,
		resume: c.Resume,
		whence: c.Whence,
	}, nil
}

// Wait mirrors pollWatcher.Wait but replaces the fixed-interval sleep with an
// OS event wait, giving sub-millisecond latency on supported platforms.
func (w *fsnotifyWatcher) Wait(ctx context.Context) (Event, error) {
	for {
		if err := ctx.Err(); err != nil {
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
			if !w.fsnWait(ctx) {
				return Event{}, ctx.Err()
			}
			continue
		}

		size, inode, err := statSizeInode(w.c.Path)
		if errors.Is(err, fs.ErrNotExist) {
			if w.c.StopAtEOF {
				return Event{}, io.EOF
			}
			if !w.fsnWait(ctx) {
				return Event{}, ctx.Err()
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
			w.logger.Debug("watch: rotated",
				"path", w.c.Path, "old_inode", oldInode, "new_inode", inode)
			return Event{
				Path:     w.c.Path,
				Pos:      Position{File: w.c.Path, Inode: inode, Offset: 0},
				ReOpened: true,
			}, nil
		}

		if size < w.pos {
			w.logger.Debug("watch: truncation",
				"path", w.c.Path, "inode", w.inode, "was", w.pos, "now", size)
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
		if !w.fsnWait(ctx) {
			return Event{}, ctx.Err()
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
			w.logger.Warn("watch: resume point inode mismatch — restarting at offset 0",
				"path", w.c.Path, "want_inode", r.Inode, "got_inode", inode)
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
// the watcher's event channel closes, or ctx is done.
func (w *fsnotifyWatcher) fsnWait(ctx context.Context) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case ev, ok := <-w.fw.Events:
			if !ok {
				return false
			}
			if filepath.Clean(ev.Name) == w.target {
				return true
			}
		case err, ok := <-w.fw.Errors:
			if !ok {
				return false
			}
			w.logger.Warn("watch: fsnotify error", "err", err, "path", w.c.Path)
		}
	}
}

func (w *fsnotifyWatcher) Close() error {
	return w.fw.Close()
}
