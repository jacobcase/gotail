package watch_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v2/watch"
)

func pollCfg(path string) watch.Config {
	return watch.Config{
		Path:     path,
		Interval: 10 * time.Millisecond,
	}
}

func mustNewPolling(t *testing.T, c watch.Config) watch.Watcher {
	t.Helper()
	w, err := watch.NewPolling(c)
	if err != nil {
		t.Fatalf("NewPolling: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func waitEvent(t *testing.T, ctx context.Context, w watch.Watcher) watch.Event {
	t.Helper()
	ev, err := w.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return ev
}

// writeFile creates (or truncates) path and writes data.
func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// appendFile appends data to an existing file.
func appendFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

// rotate renames path → path.1 (lumberjack-style).
func rotate(t *testing.T, path string) {
	t.Helper()
	if err := os.Rename(path, path+".1"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// TestReadAfterWatcher verifies the race-aware rotation drain: bytes written to
// the old file between the size check and rotation detection must be surfaced
// before switching to the new file.
//
// In v2, the Watcher emits a non-ReOpened "new data" event for any unread
// bytes in the old file (size > p.pos watermark) before emitting the rotation
// ReOpened event. The Watcher's p.pos tracks what it has told the consumer is
// available, not the consumer's actual read position.
func TestReadAfterWatcher(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Write 9 bytes to file.
	writeFile(t, path, "foobarbaz")

	w := mustNewPolling(t, pollCfg(path))

	// Wait 1: file is opened. p.pos = 0.
	ev := waitEvent(t, ctx, w)
	if !ev.ReOpened {
		t.Fatal("expected ReOpened on first open")
	}
	if ev.Pos.Offset != 0 {
		t.Fatalf("expected Pos.Offset=0, got %d", ev.Pos.Offset)
	}

	// Rotate the file now, before calling Wait again.
	// p.pos is still 0; file size is 9.
	rotate(t, path)
	writeFile(t, path, "newfile")

	// Wait 2: Watcher sees size=9 > p.pos=0 → "new data" event (non-ReOpened).
	// This is the trailing-bytes signal for the old file (all 9 bytes).
	ev = waitEvent(t, ctx, w)
	if ev.ReOpened {
		t.Fatalf("expected non-ReOpened trailing-bytes event, got ReOpened")
	}
	if ev.Pos.Offset != 0 {
		t.Fatalf("expected Pos.Offset=0 for trailing bytes, got %d", ev.Pos.Offset)
	}

	// Wait 3: p.pos=9, size=9; rotation is now detected.
	// Race-aware check: fi2.size==p.pos → no further trailing bytes → ReOpened.
	ev = waitEvent(t, ctx, w)
	if !ev.ReOpened {
		t.Fatal("expected ReOpened for new file after drain")
	}
	if ev.Pos.Offset != 0 {
		t.Fatalf("expected new file to start at offset 0, got %d", ev.Pos.Offset)
	}
	if ev.PreRotation == nil {
		t.Fatal("expected PreRotation to carry old file reference")
	}
	if ev.PreRotation.FinalSize != 9 {
		t.Fatalf("expected PreRotation.FinalSize=9, got %d", ev.PreRotation.FinalSize)
	}
}

// TestReadAfterWatcher_RaceDrain verifies the specific race: bytes appended to
// the old file AFTER the initial size check but BEFORE rotation detection are
// caught by the re-stat in the Watcher.
func TestReadAfterWatcher_RaceDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "race.log")
	writeFile(t, path, "initial\n")

	c := pollCfg(path)
	w := mustNewPolling(t, c)

	// Open event.
	ev := waitEvent(t, ctx, w)
	if !ev.ReOpened {
		t.Fatal("expected ReOpened")
	}

	// Wait 2: new data (size=8 > p.pos=0).
	ev = waitEvent(t, ctx, w)
	if ev.ReOpened {
		t.Fatal("expected non-ReOpened")
	}

	// Simulate race: append bytes to old file, then rotate, in one poll tick.
	appendFile(t, path, "appended\n")
	rotate(t, path)
	writeFile(t, path, "new\n")

	// Wait 3: Watcher sees p.pos=8, file-at-path is new. Race re-stat sees
	// old fd size=17 (8+9) > p.pos=8 → emit "new data" for appended bytes.
	ev = waitEvent(t, ctx, w)
	if ev.ReOpened {
		t.Fatalf("race drain: expected non-ReOpened for appended bytes, got ReOpened")
	}

	// Wait 4: rotation finally detected and emitted.
	ev = waitEvent(t, ctx, w)
	if !ev.ReOpened {
		t.Fatal("race drain: expected ReOpened after drain")
	}
}

// TestPollingWatcher_FileNotExistInitially verifies the watcher blocks until
// the file is created.
func TestPollingWatcher_FileNotExistInitially(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "late.log")

	w := mustNewPolling(t, pollCfg(path))

	evCh := make(chan watch.Event, 1)
	go func() {
		ev, _ := w.Wait(ctx)
		evCh <- ev
	}()

	time.Sleep(30 * time.Millisecond) // let watcher start polling
	writeFile(t, path, "hello\n")

	ev := <-evCh
	if !ev.ReOpened {
		t.Fatal("expected ReOpened when file is created")
	}
}

// TestPollingWatcher_Truncation verifies Truncated event when file size drops.
func TestPollingWatcher_Truncation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "trunc.log")
	writeFile(t, path, "initial content here")

	w := mustNewPolling(t, pollCfg(path))

	// First event: open.
	ev := waitEvent(t, ctx, w)
	if !ev.ReOpened {
		t.Fatal("expected ReOpened")
	}

	// Advance watcher position by marking as read.
	ev = waitEvent(t, ctx, w) // picks up the data

	// Truncate the file.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}

	ev = waitEvent(t, ctx, w)
	if !ev.Truncated {
		t.Fatalf("expected Truncated event, got %+v", ev)
	}
	if ev.Pos.Offset != 0 {
		t.Fatalf("expected offset 0 after truncation, got %d", ev.Pos.Offset)
	}
}

// TestPollingWatcher_SymlinkSwap verifies rotation detection via symlink swap.
func TestPollingWatcher_SymlinkSwap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.log")
	fileB := filepath.Join(dir, "b.log")
	link := filepath.Join(dir, "current.log")

	writeFile(t, fileA, "from-a\n")
	writeFile(t, fileB, "from-b\n")

	if err := os.Symlink(fileA, link); err != nil {
		t.Fatal(err)
	}

	w := mustNewPolling(t, pollCfg(link))

	ev := waitEvent(t, ctx, w)
	if !ev.ReOpened {
		t.Fatal("expected ReOpened on first event")
	}

	// Atomically swap symlink to fileB.
	tmpLink := link + ".tmp"
	if err := os.Symlink(fileB, tmpLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpLink, link); err != nil {
		t.Fatal(err)
	}

	// Consume data from fileA first.
	ev = waitEvent(t, ctx, w)

	// Eventually we should see a ReOpened event (new inode).
	deadline := time.Now().Add(3 * time.Second)
	for !ev.ReOpened {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for symlink-swap rotation")
		}
		ev = waitEvent(t, ctx, w)
	}
}

// TestContextCancellation verifies Wait returns promptly on ctx cancellation.
func TestContextCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cancel.log")
	writeFile(t, path, "data\n")

	w := mustNewPolling(t, pollCfg(path))

	ctx, cancel := context.WithCancel(context.Background())
	// Drain the initial open event.
	w.Wait(ctx) //nolint:errcheck
	// Drain the data event.
	w.Wait(ctx) //nolint:errcheck

	// Now the file is at EOF. Cancel and verify Wait returns.
	cancel()
	done := make(chan struct{})
	go func() {
		w.Wait(ctx) //nolint:errcheck
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait did not return after ctx cancel")
	}
}

// TestStopAtEOF verifies the watcher returns io.EOF when StopAtEOF is set.
func TestStopAtEOF(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "stop.log")
	writeFile(t, path, "line\n")

	c := pollCfg(path)
	c.StopAtEOF = true
	w := mustNewPolling(t, c)

	// Open event.
	w.Wait(ctx) //nolint:errcheck
	// Data event.
	w.Wait(ctx) //nolint:errcheck
	// Should return EOF.
	_, err := w.Wait(ctx)
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

// TestPollingWatcher_Rotation verifies the full rename+create rotation path
// including the race-aware drain and PreRotation handoff.
func TestPollingWatcher_Rotation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	writeFile(t, path, "line1\nline2\n")

	w := mustNewPolling(t, pollCfg(path))

	ev := waitEvent(t, ctx, w)
	if !ev.ReOpened {
		t.Fatal("expected initial ReOpened")
	}

	// Rotate: rename old file, create new.
	rotate(t, path)
	writeFile(t, path, "line3\n")

	// Drain old file events, then expect ReOpened with PreRotation.
	var rotationEv watch.Event
	for i := 0; i < 20; i++ {
		ev = waitEvent(t, ctx, w)
		if ev.ReOpened {
			rotationEv = ev
			break
		}
	}

	if !rotationEv.ReOpened {
		t.Fatal("never got ReOpened after rotation")
	}
	if rotationEv.PreRotation == nil {
		t.Fatal("expected non-nil PreRotation on rotation event")
	}
}

// TestRotation_NewFileStartsAtZero is a regression test for the invariant
// that rotation always starts the new file at offset 0, regardless of any
// Resume position that was set on the initial open.
func TestRotation_NewFileStartsAtZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "zero.log")
	writeFile(t, path, "initial\n")

	// Resume from a non-zero position so that the watcher opens with seek.
	// Use the real inode so Resume is honored (a mismatched inode would be
	// dropped with a Warn log, which is a different code path).
	inode, err := watch.StatInode(path)
	if err != nil {
		t.Fatalf("StatInode: %v", err)
	}
	resume := watch.Position{File: path, Inode: inode, Offset: 4} // mid-line
	c := watch.Config{Path: path, Interval: 10 * time.Millisecond, Resume: &resume}
	w := mustNewPolling(t, c)

	// Open event — Resume honored, offset is 4.
	ev := waitEvent(t, ctx, w)
	if !ev.ReOpened {
		t.Fatal("expected ReOpened")
	}
	if ev.Pos.Offset != 4 {
		t.Fatalf("Resume not honored: want offset 4, got %d", ev.Pos.Offset)
	}

	// Rotate to a new file.
	rotate(t, path)
	writeFile(t, path, "newfile\n")

	// Drain any trailing-bytes event for the old file.
	deadline := time.Now().Add(3 * time.Second)
	for !ev.ReOpened || ev.Pos.Offset != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for rotation with offset=0; last ev=%+v", ev)
		}
		ev = waitEvent(t, ctx, w)
	}
	// The ReOpened event for the new file must have offset 0.
	if ev.Pos.Offset != 0 {
		t.Fatalf("new file after rotation must start at offset 0, got %d", ev.Pos.Offset)
	}
}

// TestPollWatcher_ResumeInodeMismatch_Warns pins the contract that a Resume
// whose inode does not match the on-disk file is dropped (offset reset to 0)
// AND surfaced as a Warn log so the data-loss-adjacent fallback is visible.
func TestPollWatcher_ResumeInodeMismatch_Warns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mismatch.log")
	writeFile(t, path, "hello\n")

	var logBuf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	resume := watch.Position{File: path, Inode: 1, Offset: 3} // inode deliberately wrong
	w, err := watch.NewPolling(watch.Config{
		Path:     path,
		Interval: 10 * time.Millisecond,
		Resume:   &resume,
		Logger:   lg,
	})
	if err != nil {
		t.Fatalf("NewPolling: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ev, err := w.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !ev.ReOpened {
		t.Fatalf("expected ReOpened, got %+v", ev)
	}
	if ev.Pos.Offset != 0 {
		t.Fatalf("inode mismatch should reset offset to 0, got %d", ev.Pos.Offset)
	}
	if !strings.Contains(logBuf.String(), "resume point inode mismatch") {
		t.Fatalf("expected inode-mismatch Warn log, got: %s", logBuf.String())
	}
}
