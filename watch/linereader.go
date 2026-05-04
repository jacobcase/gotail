package watch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
)

const (
	defaultBufferSize = 64 * 1024       // 64 KiB
	defaultMaxLine    = 1 * 1024 * 1024 // 1 MiB
)

// LineOptions configures a [LineReader].
type LineOptions struct {
	// BufferSize is the size of the initial read buffer in bytes.
	// Zero defaults to 64 KiB. The buffer grows as needed up to MaxLine bytes.
	BufferSize int
	// MaxLine is the maximum line length in bytes before [ErrLineTooLong] is
	// returned. Zero defaults to 1 MiB.
	MaxLine int
	// KeepNewline includes the trailing \n in returned lines. Default false.
	KeepNewline bool
	// OnTruncated is called when the watcher detects that the file was truncated
	// below the current read position. at is the position just before the reset.
	// The callback fires before the reader seeks back to offset 0.
	OnTruncated func(at Position)
}

// LineReader frames newline-delimited lines on top of a [Watcher]. It opens
// its own *os.File and owns its read buffer; the Watcher only signals state
// transitions.
//
// Buffer ownership: the []byte returned by Next is valid only until the next
// call to Next or Close. Copy if you need to retain it.
//
// LineReader is not safe for concurrent use, including Close. To stop a
// blocked Next from another goroutine, cancel the ctx passed to Next; once
// Next has returned, Close may run on the same goroutine. [Tailer] coordinates
// this internally via context.AfterFunc and a WaitGroup.
type LineReader struct {
	w    Watcher
	opts LineOptions

	f   *os.File // own fd to the active (or draining) file
	src io.Reader // == f normally; may temporarily differ only after switchToFile

	pos Position // logical position, updated after each line is emitted

	// buf is the owned backing buffer. It compacts in place and grows up to
	// MaxLine+1 bytes. The filled, unconsumed region is buf[head:tail].
	buf  []byte
	head int
	tail int

	// pendingNewFile/Pos are set when a rotation event is received and the
	// LineReader is still draining the old fd. Cleared by openNewFile().
	pendingNewFile string
	pendingNewPos  Position
}

// NewLineReader wraps w with line framing.
func NewLineReader(w Watcher, opts LineOptions) *LineReader {
	if opts.BufferSize <= 0 {
		opts.BufferSize = defaultBufferSize
	}
	if opts.MaxLine <= 0 {
		opts.MaxLine = defaultMaxLine
	}
	return &LineReader{
		w:    w,
		opts: opts,
		buf:  make([]byte, opts.BufferSize),
	}
}

// Next returns the next complete line. The returned slice aliases an internal
// buffer and is valid only until the next call to Next or Close.
//
// Next returns [ErrLineTooLong] when a line exceeds MaxLine; the reader skips
// to the next newline and may be called again.
func (l *LineReader) Next(ctx context.Context) (line []byte, pos Position, err error) {
	for {
		// ── Fast path: scan for newline in buffered data ─────────────────────
		if l.head < l.tail {
			if idx := bytes.IndexByte(l.buf[l.head:l.tail], '\n'); idx >= 0 {
				start := l.head
				l.head += idx + 1
				l.pos.Offset += int64(idx + 1)
				if idx >= l.opts.MaxLine {
					// Found a newline but line is too long.
					return nil, l.pos, ErrLineTooLong
				}
				return l.trimLine(l.buf[start : start+idx]), l.pos, nil
			}

			// No newline yet. Check accumulated size against MaxLine.
			buffered := l.tail - l.head
			if buffered >= l.opts.MaxLine {
				l.skipToNewline(ctx)
				return nil, l.pos, ErrLineTooLong
			}

			// Compact: slide unconsumed data to start of buf.
			if l.head > 0 {
				copy(l.buf, l.buf[l.head:l.tail])
				l.tail -= l.head
				l.head = 0
			}

			// Grow buf if full but within MaxLine limit.
			if l.tail == len(l.buf) {
				newSize := len(l.buf) * 2
				if newSize > l.opts.MaxLine+1 {
					newSize = l.opts.MaxLine + 1
				}
				grown := make([]byte, newSize)
				copy(grown, l.buf[:l.tail])
				l.buf = grown
			}
		}

		// ── Read from active source ──────────────────────────────────────────
		if l.src != nil {
			n, rerr := l.src.Read(l.buf[l.tail:])
			if n > 0 {
				l.tail += n
				continue
			}
			if rerr != nil && rerr != io.EOF {
				return nil, l.pos, fmt.Errorf("watch: read: %w", rerr)
			}
			// Source hit EOF.
			if l.pendingNewFile != "" {
				// Done draining the old file — open the new one.
				if err := l.openNewFile(); err != nil {
					return nil, l.pos, err
				}
				continue
			}
			// Detect truncation the polling watcher may have missed: if our fd
			// position is past the current file size, the file was truncated
			// (and possibly rewritten) while the watcher's p.pos watermark was
			// still below our position. This handles copytruncate scenarios
			// where the new content is smaller than the old content.
			if l.f != nil && l.pos.Offset > 0 {
				if fi, serr := l.f.Stat(); serr == nil && fi.Size() < l.pos.Offset {
					if l.opts.OnTruncated != nil {
						l.opts.OnTruncated(l.pos)
					}
					l.head = 0
					l.tail = 0
					l.pos.Offset = 0
					if _, err := l.f.Seek(0, io.SeekStart); err != nil {
						return nil, l.pos, fmt.Errorf("watch: seek after truncation: %w", err)
					}
					continue
				}
			}
			// Normal EOF on own fd: ask the Watcher for guidance.
		}

		// ── Ask the Watcher for the next event ───────────────────────────────
		ev, werr := l.w.Wait(ctx)
		if werr != nil {
			return nil, l.pos, werr
		}
		if err := l.handleEvent(ev); err != nil {
			return nil, l.pos, err
		}
	}
}

// handleEvent updates LineReader state in response to a Watcher event.
func (l *LineReader) handleEvent(ev Event) error {
	switch {
	case ev.Truncated:
		// File was truncated; fire hook with pre-truncation position, then reset.
		if l.opts.OnTruncated != nil {
			l.opts.OnTruncated(l.pos)
		}
		l.head = 0
		l.tail = 0
		l.pos.Offset = 0
		if l.f != nil {
			if _, err := l.f.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("watch: seek after truncation: %w", err)
			}
		}

	case ev.ReOpened:
		if l.f == nil {
			// First open — switch immediately to the new file.
			if err := l.switchToFile(ev.Path, ev.Pos); err != nil {
				return err
			}
		} else {
			// Rotation — continue reading from own fd (old file) until EOF,
			// then switch to the new file. The Watcher's PreRotation.Reader
			// is not used; the LineReader manages its own fd.
			l.pendingNewFile = ev.Path
			l.pendingNewPos = ev.Pos
		}

	default:
		// New data available on the current file; just loop and read.
	}
	return nil
}

// openNewFile opens the new file after the pre-rotation drain finishes.
func (l *LineReader) openNewFile() error {
	path := l.pendingNewFile
	pos := l.pendingNewPos
	l.pendingNewFile = ""
	l.pendingNewPos = Position{}
	return l.switchToFile(path, pos)
}

// switchToFile closes any existing fd and opens the file at path, seeking to
// pos.Offset. Preserves buffered data from the pre-rotation drain.
func (l *LineReader) switchToFile(path string, pos Position) error {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("watch: open %s: %w", path, err)
	}
	if pos.Offset > 0 {
		if _, err := f.Seek(pos.Offset, io.SeekStart); err != nil {
			f.Close()
			return fmt.Errorf("watch: seek to %d: %w", pos.Offset, err)
		}
	}
	l.f = f
	l.src = f
	l.pos = pos
	// Reset buffer only if empty — preserves a partial line that might span
	// the rotation boundary (rare but correct).
	if l.head == l.tail {
		l.head = 0
		l.tail = 0
	}
	return nil
}

// trimLine strips a trailing \r and optionally appends \n.
func (l *LineReader) trimLine(raw []byte) []byte {
	if len(raw) > 0 && raw[len(raw)-1] == '\r' {
		raw = raw[:len(raw)-1]
	}
	if l.opts.KeepNewline {
		raw = append(raw, '\n')
	}
	return raw
}

// skipToNewline discards bytes until the next \n, recovering from ErrLineTooLong.
func (l *LineReader) skipToNewline(ctx context.Context) {
	for l.head < l.tail {
		ch := l.buf[l.head]
		l.head++
		l.pos.Offset++
		if ch == '\n' {
			return
		}
	}
	l.head = 0
	l.tail = 0
	for {
		if ctx.Err() != nil {
			return
		}
		if l.src == nil {
			return
		}
		n, err := l.src.Read(l.buf)
		for i := range n {
			l.pos.Offset++
			if l.buf[i] == '\n' {
				l.head = i + 1
				l.tail = n
				return
			}
		}
		if err != nil {
			l.head = 0
			l.tail = 0
			return
		}
	}
}

// Position returns the current logical position without consuming a record.
func (l *LineReader) Position() Position {
	return l.pos
}

// Close releases the Watcher and any open file descriptor.
func (l *LineReader) Close() error {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	l.src = nil
	return l.w.Close()
}
