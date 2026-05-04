//go:build !gotail_nofsnotify && (linux || darwin || freebsd || netbsd || openbsd)

package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// fsnotifyWatcher implements [Watcher] using OS-level file notifications.
// It watches the parent directory (so it can detect creation of a not-yet-
// existing file) and reacts to write/create/rename/remove events on the
// watched path. Its internal state machine mirrors pollWatcher exactly; the
// only difference is the wait mechanism.
type fsnotifyWatcher struct {
	c      Config
	logger *slog.Logger
	fw     *fsnotify.Watcher

	// resume and whence hold the one-shot initial-open state copied from
	// Config at construction. They are zeroed after the first open consumes
	// them, leaving Config untouched (callers may share a Config).
	resume *Position
	whence int

	f       *os.File
	pos     int64
	inode   uint64
	oldFile *os.File
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
	return &fsnotifyWatcher{c: c, logger: lg, fw: fw, resume: c.Resume, whence: c.Whence}, nil
}

// Wait mirrors pollWatcher.Wait but replaces the fixed-interval sleep with an
// OS event wait, giving sub-millisecond latency on supported platforms.
func (w *fsnotifyWatcher) Wait(ctx context.Context) (Event, error) {
	if w.oldFile != nil {
		w.oldFile.Close()
		w.oldFile = nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}

		if w.f == nil {
			ev, err := w.fsnOpenFirst()
			if err != nil {
				return Event{}, err
			}
			if ev != nil {
				return *ev, nil
			}
			if !w.fsnWait(ctx) {
				return Event{}, ctx.Err()
			}
			continue
		}

		fi, err := w.f.Stat()
		if err != nil {
			return Event{}, fmt.Errorf("watch: stat open file: %w", err)
		}
		size := fi.Size()

		if size < w.pos {
			w.logger.Debug("watch: truncation",
				"path", w.c.Path, "inode", w.inode, "was", w.pos, "now", size)
			w.pos = 0
			if _, err := w.f.Seek(0, io.SeekStart); err != nil {
				return Event{}, fmt.Errorf("watch: seek after truncation: %w", err)
			}
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

		rotated, newInode, err := w.fsnIsRotated()
		if err != nil {
			return Event{}, err
		}

		if !rotated {
			if w.c.StopAtEOF {
				return Event{}, io.EOF
			}
			if !w.fsnWait(ctx) {
				return Event{}, ctx.Err()
			}
			continue
		}

		fi2, err := w.f.Stat()
		if err != nil {
			return Event{}, fmt.Errorf("watch: re-stat old file: %w", err)
		}
		if fi2.Size() > w.pos {
			ev := Event{
				Path: w.c.Path,
				Pos:  Position{File: w.c.Path, Inode: w.inode, Offset: w.pos},
			}
			w.pos = fi2.Size()
			return ev, nil
		}

		return w.fsnRotate(newInode)
	}
}

func (w *fsnotifyWatcher) fsnOpenFirst() (*Event, error) {
	f, err := os.Open(w.c.Path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("watch: open %s: %w", w.c.Path, err)
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("watch: stat %s: %w", w.c.Path, err)
	}

	inode := fileID(f)
	var seekPos int64

	if w.resume != nil && !w.resume.IsZero() {
		r := w.resume
		w.resume = nil
		switch {
		case w.c.NoInodeCheck || r.Inode == inode:
			if r.Offset <= fi.Size() {
				seekPos, err = f.Seek(r.Offset, io.SeekStart)
				if err != nil {
					f.Close()
					return nil, fmt.Errorf("watch: seek to resume point: %w", err)
				}
			}
		default:
			w.logger.Warn("watch: resume point inode mismatch — restarting at offset 0",
				"path", w.c.Path, "want_inode", r.Inode, "got_inode", inode)
		}
	} else if w.whence != io.SeekStart {
		seekPos, err = f.Seek(0, w.whence)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("watch: initial seek: %w", err)
		}
		w.whence = io.SeekStart
	}

	w.f = f
	w.pos = seekPos
	w.inode = inode

	w.logger.Debug("watch: opened", "path", w.c.Path, "inode", inode, "offset", seekPos)

	return &Event{
		Path:     w.c.Path,
		Pos:      Position{File: w.c.Path, Inode: inode, Offset: seekPos},
		ReOpened: true,
	}, nil
}

func (w *fsnotifyWatcher) fsnIsRotated() (bool, uint64, error) {
	f, err := os.Open(w.c.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("watch: open path for rotation check: %w", err)
	}
	defer f.Close()
	newInode := fileID(f)
	if w.c.NoInodeCheck {
		fi, err := f.Stat()
		if err != nil {
			return false, 0, fmt.Errorf("watch: stat path for rotation check: %w", err)
		}
		return fi.Size() < w.pos, newInode, nil
	}
	return newInode != w.inode, newInode, nil
}

func (w *fsnotifyWatcher) fsnRotate(newInode uint64) (Event, error) {
	oldFile := w.f
	oldInode := w.inode
	oldPos := w.pos

	oldFi, err := oldFile.Stat()
	if err != nil {
		return Event{}, fmt.Errorf("watch: stat old file during rotation: %w", err)
	}

	w.logger.Debug("watch: rotating",
		"path", w.c.Path, "old_inode", oldInode, "new_inode", newInode)

	newFile, err := os.Open(w.c.Path)
	if err != nil {
		return Event{}, fmt.Errorf("watch: open new file after rotation: %w", err)
	}

	w.f = newFile
	w.pos = 0
	w.inode = fileID(newFile)
	w.oldFile = oldFile

	return Event{
		Path:     w.c.Path,
		Pos:      Position{File: w.c.Path, Inode: w.inode, Offset: 0},
		ReOpened: true,
		PreRotation: &PreRotation{
			Reader:    oldFile,
			FinalSize: oldFi.Size(),
			StartPos:  oldPos,
		},
	}, nil
}

// fsnWait blocks until a relevant fsnotify event arrives for the watched path,
// the watcher's event channel closes, or ctx is done.
func (w *fsnotifyWatcher) fsnWait(ctx context.Context) bool {
	target := filepath.Clean(w.c.Path)
	for {
		select {
		case <-ctx.Done():
			return false
		case ev, ok := <-w.fw.Events:
			if !ok {
				return false
			}
			if filepath.Clean(ev.Name) == target {
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
	if w.oldFile != nil {
		w.oldFile.Close()
		w.oldFile = nil
	}
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	return w.fw.Close()
}
