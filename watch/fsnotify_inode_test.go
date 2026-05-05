//go:build !gotail_nofsnotify && (linux || darwin || freebsd || netbsd || openbsd)

package watch_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v2/watch"
)

// These three tests mirror the polling-watcher inode-mismatch contracts
// (see TestPollWatcher_ResumeInodeMismatch_*) on the fsnotify backend so
// the shared resume semantics are pinned for both implementations.

func TestFsnotify_ResumeInodeMismatch_Warns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mismatch.log")
	writeFile(t, path, "hello\n")

	var logBuf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	resume := watch.Position{File: path, Inode: 1, Offset: 3}
	w, err := watch.NewFsnotify(watch.Config{
		Path:               path,
		Resume:             &resume,
		AllowInodeMismatch: true,
		Logger:             lg,
	})
	if err != nil {
		t.Fatalf("NewFsnotify: %v", err)
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

func TestFsnotify_ResumeInodeMismatch_FailsByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fail.log")
	writeFile(t, path, "hello\n")

	resume := watch.Position{File: path, Inode: 1, Offset: 3}
	w, err := watch.NewFsnotify(watch.Config{
		Path:   path,
		Resume: &resume,
	})
	if err != nil {
		t.Fatalf("NewFsnotify: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := w.Wait(ctx); !errors.Is(err, watch.ErrInodeMismatch) {
		t.Fatalf("Wait: want ErrInodeMismatch, got %v", err)
	}
}

func TestFsnotify_OnInodeMismatch_HookFires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hook.log")
	writeFile(t, path, "hello\n")

	resume := watch.Position{File: path, Inode: 1, Offset: 3}
	var hookWant, hookGot uint64
	hookCalls := 0
	w, err := watch.NewFsnotify(watch.Config{
		Path:               path,
		Resume:             &resume,
		AllowInodeMismatch: true,
		OnInodeMismatch: func(want, got uint64) {
			hookWant, hookGot = want, got
			hookCalls++
		},
	})
	if err != nil {
		t.Fatalf("NewFsnotify: %v", err)
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
