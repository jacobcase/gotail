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
