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
