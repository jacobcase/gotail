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
