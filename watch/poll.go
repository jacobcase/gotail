package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

// pollWatcher implements [Watcher] by polling stat at a fixed interval.
// p.pos is the file size at the time we last emitted a "new data" event —
// it tracks what we have told the consumer is available, not the consumer's
// actual read position. By the time the consumer calls Wait again it will
// have drained everything up to p.pos, so the race-aware rotation check
// (step 5.4 in the loop) uses p.pos as the "fully-read" watermark.
type pollWatcher struct {
	c      Config
	logger *slog.Logger

	f       *os.File // currently watched file (nil until first open)
	pos     int64    // last-emitted watermark; see doc above
	inode   uint64
	oldFile *os.File // previous file kept open for PreRotation; closed at start of next Wait
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
	return &pollWatcher{c: c, logger: lg}, nil
}

// Wait blocks until there is a state change on the watched file, then returns
// a description of that change. It is not safe for concurrent use.
func (p *pollWatcher) Wait(ctx context.Context) (Event, error) {
	// Release the old file left open from the previous rotation so the caller
	// has had a chance to drain PreRotation.Reader between Wait calls.
	if p.oldFile != nil {
		p.oldFile.Close()
		p.oldFile = nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}

		// ── Phase 1: open the file if we don't have it yet ──────────────────
		if p.f == nil {
			ev, err := p.openFirst()
			if err != nil {
				return Event{}, err
			}
			if ev != nil {
				// p.pos already set inside openFirst.
				return *ev, nil
			}
			// File not present yet; sleep and retry.
			if !p.sleep(ctx) {
				return Event{}, ctx.Err()
			}
			continue
		}

		// ── Phase 2: stat the open fd ────────────────────────────────────────
		fi, err := p.f.Stat()
		if err != nil {
			return Event{}, fmt.Errorf("watch: stat open file: %w", err)
		}
		size := fi.Size()

		// ── Phase 3: truncation ──────────────────────────────────────────────
		if size < p.pos {
			p.logger.Debug("watch: truncation",
				"path", p.c.Path, "inode", p.inode,
				"was", p.pos, "now", size)
			p.pos = 0
			if _, err := p.f.Seek(0, io.SeekStart); err != nil {
				return Event{}, fmt.Errorf("watch: seek after truncation: %w", err)
			}
			return Event{
				Path:      p.c.Path,
				Pos:       Position{File: p.c.Path, Inode: p.inode, Offset: 0},
				Truncated: true,
			}, nil
		}

		// ── Phase 4: new data in the current file ────────────────────────────
		if size > p.pos {
			ev := Event{
				Path: p.c.Path,
				Pos:  Position{File: p.c.Path, Inode: p.inode, Offset: p.pos},
			}
			p.pos = size // advance watermark before returning
			return ev, nil
		}

		// ── Phase 5: at EOF — check for rotation ─────────────────────────────
		rotated, newInode, err := p.isRotated()
		if err != nil {
			return Event{}, err
		}

		if !rotated {
			if p.c.StopAtEOF {
				return Event{}, io.EOF
			}
			if !p.sleep(ctx) {
				return Event{}, ctx.Err()
			}
			continue
		}

		// Phase 5.4 — race-aware drain: re-stat the old fd to catch bytes
		// written between our size check (Phase 2) and rotation detection.
		// Preserves the correctness property from v1 poll_watcher.go:131-143.
		fi2, err := p.f.Stat()
		if err != nil {
			return Event{}, fmt.Errorf("watch: re-stat old file: %w", err)
		}
		if fi2.Size() > p.pos {
			// Trailing bytes exist — surface them before switching.
			// Consumer reads these from its own fd, calls Wait again.
			ev := Event{
				Path: p.c.Path,
				Pos:  Position{File: p.c.Path, Inode: p.inode, Offset: p.pos},
			}
			p.pos = fi2.Size()
			return ev, nil
		}

		// Old file fully drained — switch to the new file.
		return p.rotate(newInode)
	}
}

// openFirst opens the file for the first time (or after it disappeared).
// Returns nil, nil when the file does not exist yet.
func (p *pollWatcher) openFirst() (*Event, error) {
	f, err := os.Open(p.c.Path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("watch: open %s: %w", p.c.Path, err)
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("watch: stat %s: %w", p.c.Path, err)
	}

	inode := fileInode(fi)
	var seekPos int64

	if p.c.Resume != nil && !p.c.Resume.IsZero() {
		r := p.c.Resume
		p.c.Resume = nil // one-shot
		if p.c.NoInodeCheck || r.Inode == inode {
			if r.Offset <= fi.Size() {
				seekPos, err = f.Seek(r.Offset, io.SeekStart)
				if err != nil {
					f.Close()
					return nil, fmt.Errorf("watch: seek to resume point: %w", err)
				}
			}
		}
	} else if p.c.Whence != io.SeekStart {
		seekPos, err = f.Seek(0, p.c.Whence)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("watch: initial seek: %w", err)
		}
		p.c.Whence = io.SeekStart
	}

	p.f = f
	p.pos = seekPos
	p.inode = inode

	p.logger.Debug("watch: opened",
		"path", p.c.Path, "inode", inode, "offset", seekPos)

	return &Event{
		Path:     p.c.Path,
		Pos:      Position{File: p.c.Path, Inode: inode, Offset: seekPos},
		ReOpened: true,
	}, nil
}

// isRotated checks whether the named path now holds a different file.
func (p *pollWatcher) isRotated() (bool, uint64, error) {
	fi, err := os.Stat(p.c.Path)
	if os.IsNotExist(err) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("watch: stat path for rotation check: %w", err)
	}
	newInode := fileInode(fi)
	if p.c.NoInodeCheck {
		return fi.Size() < p.pos, newInode, nil
	}
	return newInode != p.inode, newInode, nil
}

// rotate closes the active file after draining, opens the new file, and
// returns a ReOpened Event carrying PreRotation access to the old fd.
// The old fd stays alive until the start of the next Wait call.
func (p *pollWatcher) rotate(newInode uint64) (Event, error) {
	oldFile := p.f
	oldInode := p.inode
	oldPos := p.pos

	// Final size of the old file for PreRotation metadata.
	oldFi, err := oldFile.Stat()
	if err != nil {
		return Event{}, fmt.Errorf("watch: stat old file during rotation: %w", err)
	}

	p.logger.Debug("watch: rotating",
		"path", p.c.Path, "old_inode", oldInode, "new_inode", newInode)

	newFile, err := os.Open(p.c.Path)
	if err != nil {
		return Event{}, fmt.Errorf("watch: open new file after rotation: %w", err)
	}
	newFi, err := newFile.Stat()
	if err != nil {
		newFile.Close()
		return Event{}, fmt.Errorf("watch: stat new file: %w", err)
	}

	p.f = newFile
	p.pos = 0
	p.inode = fileInode(newFi)
	p.oldFile = oldFile // released at start of next Wait

	return Event{
		Path:     p.c.Path,
		Pos:      Position{File: p.c.Path, Inode: p.inode, Offset: 0},
		ReOpened: true,
		PreRotation: &PreRotation{
			Reader:    oldFile,
			FinalSize: oldFi.Size(),
			StartPos:  oldPos,
		},
	}, nil
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

func (p *pollWatcher) Close() error {
	if p.oldFile != nil {
		p.oldFile.Close()
		p.oldFile = nil
	}
	if p.f != nil {
		err := p.f.Close()
		p.f = nil
		return err
	}
	return nil
}
