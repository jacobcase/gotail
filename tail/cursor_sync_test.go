package tail_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/jacobcase/gotail/v2/tail"
	"github.com/jacobcase/gotail/v2/watch"
)

// TestFileCursor_SyncAlways_FlushesEveryCall verifies that each Save produces
// a new on-disk file (stat mtime bump or content change).
func TestFileCursor_SyncAlways_FlushesEveryCall(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "syncalways.cursor")

	c, err := tail.NewFileCursor(path) // default: SyncAlways
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	pos := watch.Position{File: path, Inode: 1, Offset: 0}
	for i := 0; i < 3; i++ {
		pos.Offset = int64(i * 100)
		if err := c.Save(ctx, tail.Checkpoint{Pos: pos}); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
		// File must exist immediately after Save.
		got, found, err := c.Load(ctx)
		if err != nil || !found {
			t.Fatalf("Load after Save #%d: found=%v err=%v", i, found, err)
		}
		if got.Pos.Offset != pos.Offset {
			t.Fatalf("Save #%d: want offset %d, got %d", i, pos.Offset, got.Pos.Offset)
		}
	}
}

// TestFileCursor_SyncOnCommit_DefersFsync verifies that multiple Saves without
// an explicit Sync leave only the latest checkpoint on disk after the Sync.
func TestFileCursor_SyncOnCommit_DefersFsync(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "synconcommit.cursor")

	c, err := tail.NewFileCursor(path, tail.WithSyncMode(tail.SyncOnCommit))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Multiple Saves without Sync should not write to disk.
	for i := 0; i < 5; i++ {
		cp := tail.Checkpoint{Pos: watch.Position{File: path, Inode: 1, Offset: int64(i * 10)}}
		if err := c.Save(ctx, cp); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
	}

	// File should not exist yet (no Sync called).
	if _, err := os.Stat(path); err == nil {
		t.Fatal("file exists before Sync — SyncOnCommit must buffer")
	}

	// Type-assert to Syncer and call Sync.
	syncer, ok := c.(tail.Syncer)
	if !ok {
		t.Fatal("FileCursor with SyncOnCommit must implement Syncer")
	}
	if err := syncer.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Now the file must exist and contain the LAST checkpoint (offset 40).
	got, found, err := c.Load(ctx)
	if err != nil || !found {
		t.Fatalf("Load after Sync: found=%v err=%v", found, err)
	}
	if got.Pos.Offset != 40 {
		t.Fatalf("want offset 40 after Sync, got %d", got.Pos.Offset)
	}

	// Second Sync with no new saves must be a no-op (not overwrite with zero).
	if err := syncer.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	got2, _, _ := c.Load(ctx)
	if got2.Pos.Offset != 40 {
		t.Fatalf("second Sync changed value: want 40, got %d", got2.Pos.Offset)
	}
}

// TestFileCursor_SyncBackground_FlushesPeriodically verifies that a saved
// checkpoint eventually reaches disk without an explicit Sync call.
func TestFileCursor_SyncBackground_FlushesPeriodically(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "syncbg.cursor")

	const interval = 50 * time.Millisecond
	c, err := tail.NewFileCursor(path,
		tail.WithSyncMode(tail.SyncBackground),
		tail.WithSyncBackgroundInterval(interval),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	cp := tail.Checkpoint{Pos: watch.Position{File: path, Inode: 1, Offset: 999}}
	if err := c.Save(ctx, cp); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Wait up to 5× interval for the background flusher to fire.
	deadline := time.Now().Add(5 * interval)
	for time.Now().Before(deadline) {
		got, found, err := c.Load(ctx)
		if err == nil && found && got.Pos.Offset == 999 {
			return // success
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background flusher did not write checkpoint within 5×interval")
}

// TestTailer_CloseWithFlush_SyncOnCommit verifies that CloseWithFlush forces
// the buffered checkpoint to disk under SyncOnCommit, matching its documented
// "commit the current position before tearing down" contract. Without the
// explicit Sync call inside CloseWithFlush the buffered position would be
// silently discarded by Cursor.Close.
func TestTailer_CloseWithFlush_SyncOnCommit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")
	cursorPath := filepath.Join(dir, "events.cursor")
	if err := os.WriteFile(logPath, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cur, err := tail.NewFileCursor(cursorPath, tail.WithSyncMode(tail.SyncOnCommit))
	if err != nil {
		t.Fatal(err)
	}

	tr, err := tail.New(ctx, tail.Options{
		Source:    tail.SingleFile(logPath),
		Cursor:    cur,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Consume both lines so t.cur is non-zero.
	rec, err := tr.Next(ctx)
	if err != nil {
		t.Fatalf("Next #1: %v", err)
	}
	want := rec.Pos
	if _, err := tr.Next(ctx); err != nil {
		t.Fatalf("Next #2: %v", err)
	}

	if err := tr.CloseWithFlush(ctx); err != nil {
		t.Fatalf("CloseWithFlush: %v", err)
	}

	// Cursor file must exist on disk after CloseWithFlush.
	if _, err := os.Stat(cursorPath); err != nil {
		t.Fatalf("cursor file not written after CloseWithFlush: %v", err)
	}

	// Re-open and verify the persisted offset is past the first line.
	verify, err := tail.NewFileCursor(cursorPath, tail.WithSyncMode(tail.SyncOnCommit))
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	cp, found, err := verify.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("CloseWithFlush did not persist checkpoint under SyncOnCommit")
	}
	if cp.Pos.Offset <= want.Offset {
		t.Fatalf("persisted offset %d should be past first line offset %d", cp.Pos.Offset, want.Offset)
	}
}

// TestTailer_CloseWithFlush_SyncBackground mirrors the SyncOnCommit case
// for SyncBackground mode: CloseWithFlush must flush the in-memory pending
// write before the background flusher's next tick.
func TestTailer_CloseWithFlush_SyncBackground(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")
	cursorPath := filepath.Join(dir, "events.cursor")
	if err := os.WriteFile(logPath, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Use a long flush interval so the background flusher will not fire
	// between Save and CloseWithFlush — only the in-flush call should land
	// the checkpoint on disk.
	cur, err := tail.NewFileCursor(cursorPath,
		tail.WithSyncMode(tail.SyncBackground),
		tail.WithSyncBackgroundInterval(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	tr, err := tail.New(ctx, tail.Options{
		Source:    tail.SingleFile(logPath),
		Cursor:    cur,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tr.Next(ctx); err != nil {
		t.Fatalf("Next: %v", err)
	}

	if err := tr.CloseWithFlush(ctx); err != nil {
		t.Fatalf("CloseWithFlush: %v", err)
	}

	if _, err := os.Stat(cursorPath); err != nil {
		t.Fatalf("cursor file not written after CloseWithFlush: %v", err)
	}
}

// TestFileCursor_SyncBackground_ClosePropagatesGoroutineExit verifies that
// Close terminates the background goroutine. goleak detects any leak.
func TestFileCursor_SyncBackground_ClosePropagatesGoroutineExit(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "syncbg_close.cursor")

	c, err := tail.NewFileCursor(path,
		tail.WithSyncMode(tail.SyncBackground),
		tail.WithSyncBackgroundInterval(50*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Save something so there's a dirty checkpoint to flush on close.
	_ = c.Save(ctx, tail.Checkpoint{Pos: watch.Position{File: path, Inode: 1, Offset: 1}})

	// Close must stop the goroutine.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// goleak.VerifyNone fires after this function returns and checks that
	// no new goroutines are running relative to the start of the test.
}
