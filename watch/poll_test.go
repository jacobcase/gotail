package watch_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v3/watch"
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

// TestPollingWatcher_RotationEmitsReopened verifies that when the path
// inode changes, the watcher emits a single ReOpened event with the new
// inode at offset 0. After H3 the watcher holds no fd to the active file,
// so trailing-bytes drain on the rotated-out inode is the LineReader's
// concern (it owns the fd) — see TestLineReader_Rotate.
func TestPollingWatcher_RotationEmitsReopened(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	writeFile(t, path, "foobarbaz") // 9 bytes

	w := mustNewPolling(t, pollCfg(path))

	// Wait 1: open at offset 0.
	ev := waitEvent(t, ctx, w)
	if !ev.ReOpened {
		t.Fatal("expected ReOpened on first open")
	}
	if ev.Pos.Offset != 0 {
		t.Fatalf("expected Pos.Offset=0, got %d", ev.Pos.Offset)
	}
	initialInode := ev.Pos.Inode

	// Wait 2: 9 bytes of new data.
	ev = waitEvent(t, ctx, w)
	if ev.ReOpened {
		t.Fatalf("expected non-ReOpened new-data event, got ReOpened")
	}
	if ev.Pos.Offset != 0 {
		t.Fatalf("expected Pos.Offset=0 for new data, got %d", ev.Pos.Offset)
	}

	// Rotate.
	rotate(t, path)
	writeFile(t, path, "newfile")

	// Wait 3: rotation detected via inode change → ReOpened, offset 0, new inode.
	ev = waitEvent(t, ctx, w)
	if !ev.ReOpened {
		t.Fatalf("expected ReOpened after rotation, got %+v", ev)
	}
	if ev.Pos.Offset != 0 {
		t.Fatalf("rotation: expected offset 0, got %d", ev.Pos.Offset)
	}
	if ev.Pos.Inode != 0 && ev.Pos.Inode == initialInode {
		t.Fatalf("expected new inode after rotation; both events have inode %d", initialInode)
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
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

// TestPollingWatcher_Rotation verifies the full rename+create rotation path:
// the watcher emits ReOpened with the new inode after rotation. Drain of
// trailing bytes on the old inode is the LineReader's responsibility.
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
	initialInode := ev.Pos.Inode

	// Rotate: rename old file, create new.
	rotate(t, path)
	writeFile(t, path, "line3\n")

	// Drain any pre-rotation new-data events, then expect ReOpened.
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
	if rotationEv.Pos.Offset != 0 {
		t.Fatalf("rotation: expected offset 0, got %d", rotationEv.Pos.Offset)
	}
	if rotationEv.Pos.Inode != 0 && rotationEv.Pos.Inode == initialInode {
		t.Fatalf("expected new inode after rotation; got %d (same as initial)", initialInode)
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
// AllowInodeMismatch is the explicit opt-in to this resume path; the
// fail-safe default would error here (see _Fails test below).
func TestPollWatcher_ResumeInodeMismatch_Warns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mismatch.log")
	writeFile(t, path, "hello\n")

	var logBuf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	resume := watch.Position{File: path, Inode: 1, Offset: 3} // inode deliberately wrong
	w, err := watch.NewPolling(watch.Config{
		Path:               path,
		Interval:           10 * time.Millisecond,
		Resume:             &resume,
		AllowInodeMismatch: true,
		Logger:             lg,
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

// TestPollWatcher_ResumeInodeMismatch_FailsByDefault pins the fail-safe
// default contract: Wait returns an error wrapping ErrInodeMismatch instead
// of falling through to offset 0. Opt out by setting AllowInodeMismatch.
func TestPollWatcher_ResumeInodeMismatch_FailsByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fail.log")
	writeFile(t, path, "hello\n")

	resume := watch.Position{File: path, Inode: 1, Offset: 3}
	w, err := watch.NewPolling(watch.Config{
		Path:     path,
		Interval: 10 * time.Millisecond,
		Resume:   &resume,
	})
	if err != nil {
		t.Fatalf("NewPolling: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = w.Wait(ctx)
	if !errors.Is(err, watch.ErrInodeMismatch) {
		t.Fatalf("Wait: want ErrInodeMismatch, got %v", err)
	}
}

// TestPollWatcher_OnInodeMismatch_HookFires confirms the observation hook
// fires regardless of the resolution path (default fallback or fail).
// AllowInodeMismatch=true selects the resume path so Wait succeeds.
func TestPollWatcher_OnInodeMismatch_HookFires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hook.log")
	writeFile(t, path, "hello\n")

	resume := watch.Position{File: path, Inode: 1, Offset: 3}
	var hookWant, hookGot uint64
	hookCalls := 0
	w, err := watch.NewPolling(watch.Config{
		Path:               path,
		Interval:           10 * time.Millisecond,
		Resume:             &resume,
		AllowInodeMismatch: true,
		OnInodeMismatch: func(want, got uint64) {
			hookWant, hookGot = want, got
			hookCalls++
		},
	})
	if err != nil {
		t.Fatalf("NewPolling: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := w.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if hookCalls != 1 {
		t.Fatalf("OnInodeMismatch fired %d times, want 1", hookCalls)
	}
	if hookWant != 1 || hookGot == 0 {
		t.Fatalf("OnInodeMismatch args: want=1 got=%d, hookGot must be nonzero (was %d)", hookWant, hookGot)
	}
}
