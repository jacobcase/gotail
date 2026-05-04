//go:build !gotail_nofsnotify && (linux || darwin || freebsd || netbsd || openbsd)

package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v2/watch"
)

// TestInotifyBackend / TestKqueueBackend: both exercise the same code via
// different OS paths. One test covers both since the build tag filters by OS.
func TestFsnotify_BasicWriteDetect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	w, err := watch.NewFsnotify(watch.Config{
		Path:      path,
		StopAtEOF: false,
	})
	if err != nil {
		t.Fatalf("NewFsnotify: %v", err)
	}
	defer w.Close()

	// File doesn't exist yet — create it while the watcher is running.
	go func() {
		time.Sleep(20 * time.Millisecond)
		os.WriteFile(path, []byte("hello\n"), 0o644)
	}()

	ev, err := w.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !ev.ReOpened {
		t.Fatalf("expected ReOpened on first event, got %+v", ev)
	}
}

func TestFsnotify_WriteEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "write.log")
	if err := os.WriteFile(path, []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := watch.NewFsnotify(watch.Config{Path: path})
	if err != nil {
		t.Fatalf("NewFsnotify: %v", err)
	}
	defer w.Close()

	// First event: open.
	ev, err := w.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait #1: %v", err)
	}
	if !ev.ReOpened {
		t.Fatalf("want ReOpened, got %+v", ev)
	}

	// Append data; watcher should return a new-data event.
	go func() {
		time.Sleep(20 * time.Millisecond)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("appended\n")
		f.Close()
	}()

	ev, err = w.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait #2: %v", err)
	}
	if ev.ReOpened || ev.Truncated {
		t.Fatalf("want plain new-data event, got %+v", ev)
	}
}

// TestFsnotify_ReadAfterWatcher_RaceDrain mirrors the polling backend's
// race-drain test (TestReadAfterWatcher_RaceDrain in poll_test.go) on the
// fsnotify path. The two backends share the same rotation-drain state
// machine; this test pins that the fsnotify branch's re-stat after rotation
// surfaces bytes appended to the old fd before switching to the new file.
func TestFsnotify_ReadAfterWatcher_RaceDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "race.log")
	writeFile(t, path, "initial\n") // 8 bytes

	w, err := watch.NewFsnotify(watch.Config{Path: path})
	if err != nil {
		t.Fatalf("NewFsnotify: %v", err)
	}
	defer w.Close()

	// Wait 1: open event (ReOpened).
	ev, err := w.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait #1: %v", err)
	}
	if !ev.ReOpened {
		t.Fatalf("expected ReOpened, got %+v", ev)
	}

	// Wait 2: initial 8 bytes surfaced as a new-data event.
	ev, err = w.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait #2: %v", err)
	}
	if ev.ReOpened || ev.Truncated {
		t.Fatalf("expected non-ReOpened new-data event, got %+v", ev)
	}

	// Race: append 9 bytes to old file, then rotate it away, then create new
	// file at the path. Old fd remains open and visible at the original inode.
	appendFile(t, path, "appended\n")
	rotate(t, path)
	writeFile(t, path, "new\n")

	// Wait 3: race-drain. Old fd's size is now 17 > pos=8. The fsnotify Wait
	// loop must emit a new-data event for the 9 appended bytes before
	// switching to the rotated-in file.
	ev, err = w.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait #3: %v", err)
	}
	if ev.ReOpened {
		t.Fatalf("race drain: expected non-ReOpened for appended bytes, got %+v", ev)
	}

	// Wait 4: rotation surfaces.
	ev, err = w.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait #4: %v", err)
	}
	if !ev.ReOpened {
		t.Fatalf("expected ReOpened after race drain, got %+v", ev)
	}
}

func TestFsnotify_FallbackToPolling(t *testing.T) {
	// When ForcePolling is true, tail.New uses NewPolling directly.
	// This test verifies watch.New falls back correctly when we request
	// the fsnotify backend for a valid path (i.e., no error expected).
	dir := t.TempDir()
	path := filepath.Join(dir, "fallback.log")
	os.WriteFile(path, []byte("x\n"), 0o644)

	w, err := watch.New(watch.Config{Path: path})
	if err != nil {
		t.Fatalf("watch.New: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ev, err := w.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !ev.ReOpened {
		t.Fatalf("expected ReOpened, got %+v", ev)
	}
}
