package watch_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v2/watch"
)

// scriptedWatcher returns a fixed sequence of events from Wait, then io.EOF.
// It exists for tests that need to drive LineReader through specific
// state transitions (Truncated, ReOpened-rotation) that a real polling
// watcher only emits in racy or hard-to-stage conditions.
type scriptedWatcher struct {
	events []watch.Event
	idx    int
}

func (s *scriptedWatcher) Wait(ctx context.Context) (watch.Event, error) {
	if err := ctx.Err(); err != nil {
		return watch.Event{}, err
	}
	if s.idx >= len(s.events) {
		return watch.Event{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}

func (s *scriptedWatcher) Close() error { return nil }

// TestLineReader_HandleTruncatedEvent exercises handleEvent's Truncated
// branch: OnTruncated must fire with the pre-truncation position, the file
// fd must be re-seeked to 0, and subsequent reads must yield content from
// offset 0 again.
func TestLineReader_HandleTruncatedEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trunc.log")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := &scriptedWatcher{events: []watch.Event{
		{Path: path, Pos: watch.Position{File: path}, ReOpened: true},
		{Path: path, Pos: watch.Position{File: path, Offset: 0}, Truncated: true},
	}}

	var truncatedAt watch.Position
	truncCalls := 0
	lr := newLR(t, w, watch.LineOptions{
		OnTruncated: func(p watch.Position) {
			truncatedAt = p
			truncCalls++
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if got := nextLine(t, ctx, lr); got != "a" {
		t.Fatalf("want a, got %q", got)
	}
	if got := nextLine(t, ctx, lr); got != "b" {
		t.Fatalf("want b, got %q", got)
	}

	// After both lines: l.pos.Offset = 4. EOF on own fd; self-truncation
	// check sees fi.Size()=4 == l.pos.Offset, no self-detect. The next
	// Wait returns Truncated → handleEvent fires OnTruncated(offset=4),
	// resets pos to 0, seeks fd to 0. Loop re-reads from start.
	if got := nextLine(t, ctx, lr); got != "a" {
		t.Fatalf("after Truncated event, want re-read 'a', got %q", got)
	}
	if got := nextLine(t, ctx, lr); got != "b" {
		t.Fatalf("after Truncated event, want re-read 'b', got %q", got)
	}

	if truncCalls != 1 {
		t.Fatalf("OnTruncated calls = %d, want 1", truncCalls)
	}
	if truncatedAt.Offset != 4 {
		t.Fatalf("OnTruncated pos.Offset = %d, want 4 (pre-truncation)", truncatedAt.Offset)
	}
}

// TestLineReader_Rotate_DrainsTrailingBytes pins the load-bearing rotation
// invariant: bytes appended to the rotated-out inode after the LineReader
// last read from it must still be yielded, because the LineReader keeps the
// fd open and Unix keeps the inode alive while any fd references it. This
// is what the comment at linereader.go:201-204 ("trailing bytes get drained")
// promises; without this test, a regression that closes the old fd
// immediately on ReOpened would still pass TestLineReader_Rotate (where
// the old file is fully consumed before rotation).
//
// The scripted watcher lets us deterministically stage the sequence:
//   - Event 1: ReOpened on the original file. LineReader opens it, reads
//     "first", drains the buffer, then blocks on the next Wait.
//   - The test then appends "trailing\n" to the still-open inode and
//     renames it out from under the path, planting "second\n" at the
//     original path.
//   - Event 2: ReOpened on the (now new) file. LineReader sets
//     pendingNewFile, drains "trailing" from the old fd, then switches to
//     the new file and reads "second".
func TestLineReader_Rotate_DrainsTrailingBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drain.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pos := watch.Position{File: path}
	w := &scriptedWatcher{events: []watch.Event{
		{Path: path, Pos: pos, ReOpened: true},
		{Path: path, Pos: pos, ReOpened: true},
	}}
	lr := newLR(t, w, watch.LineOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if got := nextLine(t, ctx, lr); got != "first" {
		t.Fatalf("want first, got %q", got)
	}

	// Append trailing bytes to the inode the LineReader still has open via
	// l.f, then rotate the path away and plant new content. The LineReader's
	// fd continues to reference the original inode (kernel-alive while any
	// fd holds it), so the next read from l.src must yield "trailing\n"
	// before the second ReOpened triggers the switch.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("trailing\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	rotated := path + ".1"
	if err := os.Rename(path, rotated); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := nextLine(t, ctx, lr); got != "trailing" {
		t.Fatalf("drain: want trailing, got %q (rotation lost trailing bytes)", got)
	}
	if got := nextLine(t, ctx, lr); got != "second" {
		t.Fatalf("after switch: want second, got %q", got)
	}
}

// TestLineReader_LongLineNoNewline exercises skipToNewline: a line that
// fills the buffer to MaxLine without a newline forces the
// `buffered >= MaxLine` branch in Next, which calls skipToNewline. The
// existing TestLineReader_LongLine fits the entire long line + newline in
// the default 4 KiB buffer, so the fast-path returns ErrLineTooLong without
// invoking skipToNewline.
func TestLineReader_LongLineNoNewline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "long_no_nl.log")

	long := bytes.Repeat([]byte{'x'}, 100)
	content := append(long, '\n')
	content = append(content, []byte("ok\n")...)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	c := watch.Config{Path: path, Interval: 10 * time.Millisecond, StopAtEOF: true}
	w, err := watch.NewPolling(c)
	if err != nil {
		t.Fatal(err)
	}
	// BufferSize == MaxLine forces skipToNewline: after one Read fills the
	// buffer with 64 'x' bytes (no newline visible), buffered >= MaxLine
	// triggers skipToNewline, which scans through the rest of the long
	// segment to its terminating newline.
	lr := newLR(t, w, watch.LineOptions{BufferSize: 64, MaxLine: 64})

	if _, _, err := lr.Next(ctx); !errors.Is(err, watch.ErrLineTooLong) {
		t.Fatalf("Next: want ErrLineTooLong, got %v", err)
	}
	if got := nextLine(t, ctx, lr); got != "ok" {
		t.Fatalf("after skipToNewline, want 'ok', got %q", got)
	}
}
