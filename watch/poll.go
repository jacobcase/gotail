package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"time"
)

// pollWatcher implements [Watcher] by polling stat at a fixed interval.
//
// It owns no file descriptor: the [LineReader] holds the only fd to the
// active file. The watcher observes path-side state (inode + size) via
// os.Stat and emits state-transition events. Trailing bytes on the
// rotated-out file are surfaced naturally by the LineReader's reads
// against its own fd.
//
// pos tracks the file size at the last "new data" event the watcher
// emitted — i.e. what we have told the consumer is available, not the
// consumer's actual read position.
type pollWatcher struct {
	c      Config
	logger *slog.Logger

	// resume and whence hold the one-shot initial-open state copied from
	// Config at construction. They are zeroed after the first event has
	// been emitted, leaving Config untouched (callers may share a Config).
	resume *Position
	whence int

	started bool   // true once the first event has been emitted
	pos     int64  // last-emitted size watermark
	inode   uint64 // inode the LineReader currently has open
}

// NewPolling returns a [Watcher] that detects file changes by polling stat.
// It is available on all platforms and requires no external dependencies.
func NewPolling(c Config) (Watcher, error) {
	if c.Path == "" {
		return nil, errors.New("watch: Config.Path must not be empty")
	}
	if c.Interval < 0 {
		return nil, fmt.Errorf("watch: Config.Interval must not be negative, got %v", c.Interval)
	}
	if c.Interval == 0 {
		c.Interval = time.Second
	}
	if c.Whence != io.SeekStart && c.Whence != io.SeekCurrent && c.Whence != io.SeekEnd {
		return nil, fmt.Errorf("watch: Config.Whence %v is invalid", c.Whence)
	}
	lg := c.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return &pollWatcher{c: c, logger: lg, resume: c.Resume, whence: c.Whence}, nil
}

// Wait blocks until there is a state change on the watched file, then returns
// a description of that change. It is not safe for concurrent use.
func (p *pollWatcher) Wait(ctx context.Context) (Event, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}

		// ── Phase 1: open the file if we haven't yet ───────────────────────
		if !p.started {
			ev, ok, err := p.openFirst()
			if err != nil {
				return Event{}, err
			}
			if ok {
				return ev, nil
			}
			// File not present yet; sleep and retry.
			if !p.sleep(ctx) {
				return Event{}, ctx.Err()
			}
			continue
		}

		// ── Phase 2: stat the path ─────────────────────────────────────────
		size, inode, err := statSizeInode(p.c.Path)
		if errors.Is(err, fs.ErrNotExist) {
			// Path momentarily gone (mid-rotation); retry.
			if p.c.StopAtEOF {
				return Event{}, io.EOF
			}
			if !p.sleep(ctx) {
				return Event{}, ctx.Err()
			}
			continue
		}
		if err != nil {
			return Event{}, fmt.Errorf("watch: stat %s: %w", p.c.Path, err)
		}

		// ── Phase 3: rotation (path now points to a different inode) ───────
		// Detected before truncation: a path swap that happens to land on a
		// smaller file would otherwise be misclassified as truncation.
		if !p.c.NoInodeCheck && inode != p.inode {
			oldInode := p.inode
			p.pos = 0
			p.inode = inode
			p.logger.Debug("watch: rotated",
				"path", p.c.Path, "old_inode", oldInode, "new_inode", inode)
			return Event{
				Path:     p.c.Path,
				Pos:      Position{File: p.c.Path, Inode: inode, Offset: 0},
				ReOpened: true,
			}, nil
		}

		// ── Phase 4: truncation (size dropped below watermark) ─────────────
		if size < p.pos {
			p.logger.Debug("watch: truncation",
				"path", p.c.Path, "inode", p.inode, "was", p.pos, "now", size)
			p.pos = 0
			return Event{
				Path:      p.c.Path,
				Pos:       Position{File: p.c.Path, Inode: p.inode, Offset: 0},
				Truncated: true,
			}, nil
		}

		// ── Phase 5: new data ──────────────────────────────────────────────
		if size > p.pos {
			ev := Event{
				Path: p.c.Path,
				Pos:  Position{File: p.c.Path, Inode: p.inode, Offset: p.pos},
			}
			p.pos = size
			return ev, nil
		}

		// ── Phase 6: no change ─────────────────────────────────────────────
		if p.c.StopAtEOF {
			return Event{}, io.EOF
		}
		if !p.sleep(ctx) {
			return Event{}, ctx.Err()
		}
	}
}

// openFirst stats the path and (if present) computes the initial offset from
// Whence/Resume. Returns ok=false when the path does not yet exist.
func (p *pollWatcher) openFirst() (Event, bool, error) {
	size, inode, err := statSizeInode(p.c.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("watch: stat %s: %w", p.c.Path, err)
	}

	var offset int64
	if p.resume != nil && !p.resume.IsZero() {
		r := p.resume
		p.resume = nil // one-shot
		if p.c.NoInodeCheck || r.Inode == inode {
			if r.Offset <= size {
				offset = r.Offset
			}
		} else {
			// Resume cursor's inode does not match the file currently at Path.
			// Fire the observation hook before any decision so observers see
			// the mismatch regardless of the resolution path.
			if p.c.OnInodeMismatch != nil {
				p.c.OnInodeMismatch(r.Inode, inode)
			}
			if p.c.FailOnInodeMismatch {
				return Event{}, false, fmt.Errorf(
					"watch: resume point inode mismatch on %s: want=%d got=%d: %w",
					p.c.Path, r.Inode, inode, ErrInodeMismatch)
			}
			// Default: continue from offset 0 of the new file, but log so
			// the dropped resume is not silent.
			p.logger.Warn("watch: resume point inode mismatch — restarting at offset 0",
				"path", p.c.Path, "want_inode", r.Inode, "got_inode", inode)
		}
	} else if p.whence == io.SeekEnd {
		offset = size
	}
	p.whence = io.SeekStart // consumed; subsequent opens always start at 0

	p.started = true
	p.pos = offset
	p.inode = inode

	p.logger.Debug("watch: opened",
		"path", p.c.Path, "inode", inode, "offset", offset)

	return Event{
		Path:     p.c.Path,
		Pos:      Position{File: p.c.Path, Inode: inode, Offset: offset},
		ReOpened: true,
	}, true, nil
}

func (p *pollWatcher) sleep(ctx context.Context) bool {
	t := time.NewTimer(p.c.Interval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (p *pollWatcher) Close() error { return nil }
