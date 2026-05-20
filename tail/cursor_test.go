package tail_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcase/gotail/v3/tail"
	"github.com/jacobcase/gotail/v3/watch"
)

func newFileCursor(t *testing.T, opts ...tail.FileCursorOption) (tail.Cursor, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.cursor")
	c, err := tail.NewFileCursor(path, opts...)
	if err != nil {
		t.Fatalf("NewFileCursor: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c, path
}

func TestFileCursor_RoundTrip(t *testing.T) {
	ctx := context.Background()
	c, _ := newFileCursor(t)

	pos := watch.Position{File: "/var/log/app.log", Inode: 42, Offset: 1024}
	cp := tail.Checkpoint{Pos: pos}

	if err := c.Save(ctx, cp); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, found, err := c.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("Load: expected found=true after Save")
	}
	if got.Pos != pos {
		t.Fatalf("pos mismatch: want %+v, got %+v", pos, got.Pos)
	}
}

func TestFileCursor_NotFound(t *testing.T) {
	ctx := context.Background()
	c, _ := newFileCursor(t)

	_, found, err := c.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Fatal("expected found=false on empty cursor")
	}
}

// TestFileCursor_StaleTmpDoesNotCorrupt exercises the atomicwrite contract:
// the canonical cursor path is either fully old or fully new on disk, never
// partial. We can't directly induce a crash mid-rename in-process, but the
// observable post-condition — a leftover .tmp from an interrupted write —
// is reproducible. Load must ignore the .tmp, and a subsequent Save must
// replace it cleanly.
func TestFileCursor_StaleTmpDoesNotCorrupt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "atomic.cursor")

	// Pre-existing stale tmp from a hypothetical crashed Save.
	if err := os.WriteFile(path+".tmp", []byte(`{"pos":"garbage`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := tail.NewFileCursor(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Canonical path is missing → Load reports not-found regardless of .tmp.
	if _, found, err := c.Load(ctx); err != nil {
		t.Fatalf("Load before Save: %v", err)
	} else if found {
		t.Fatal("Load returned found=true when only a stale .tmp existed")
	}

	cp1 := tail.Checkpoint{Pos: watch.Position{File: "a", Inode: 1, Offset: 100}}
	if err := c.Save(ctx, cp1); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, found, err := c.Load(ctx)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if !found {
		t.Fatal("Load: expected found=true after Save")
	}
	if got.Pos != cp1.Pos {
		t.Fatalf("got %+v, want %+v", got.Pos, cp1.Pos)
	}
}

func TestFileCursor_DirSync(t *testing.T) {
	ctx := context.Background()

	// With dirSync off.
	c, _ := newFileCursor(t, tail.WithDirSync(false))
	pos := watch.Position{File: "x", Inode: 5, Offset: 0}
	if err := c.Save(ctx, tail.Checkpoint{Pos: pos}); err != nil {
		t.Fatalf("Save (no dirsync): %v", err)
	}
	got, found, err := c.Load(ctx)
	if err != nil || !found || got.Pos != pos {
		t.Fatalf("round-trip failed with WithDirSync(false): found=%v err=%v pos=%+v", found, err, got.Pos)
	}
}

func TestFileCursor_Meta_RoundTrip(t *testing.T) {
	ctx := context.Background()
	c, _ := newFileCursor(t)

	type myMeta struct {
		BatchID string `json:"batch_id"`
		Count   int    `json:"count"`
	}
	m := myMeta{BatchID: "abc-123", Count: 42}
	raw, _ := json.Marshal(m)

	cp := tail.Checkpoint{
		Pos:  watch.Position{File: "f", Inode: 1, Offset: 500},
		Meta: json.RawMessage(raw),
	}
	if err := c.Save(ctx, cp); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, _, err := c.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var gotMeta myMeta
	if err := json.Unmarshal(got.Meta, &gotMeta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if gotMeta != m {
		t.Fatalf("meta mismatch: want %+v, got %+v", m, gotMeta)
	}
}

func TestFileCursor_OversizeMeta(t *testing.T) {
	ctx := context.Background()
	c, _ := newFileCursor(t)

	bigMeta := json.RawMessage(`"` + strings.Repeat("x", 65*1024) + `"`)
	cp := tail.Checkpoint{
		Pos:  watch.Position{File: "f", Inode: 1, Offset: 0},
		Meta: bigMeta,
	}
	if err := c.Save(ctx, cp); err == nil {
		t.Fatal("expected error for oversize meta, got nil")
	}
}

// TestFileCursor_Load_RejectsFutureVersion pins the schema-version check.
// A cursor file written with a version higher (or lower) than what this
// build supports must be rejected with ErrUnsupportedCursorVersion so the
// user sees the upgrade/migration condition instead of a silent stale read.
func TestFileCursor_Load_RejectsFutureVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "future.cursor")

	// Hand-write a JSON file with version=99.
	body := `{"pos":{"file":"/x","inode":"7","offset":"100"},"version":99}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := tail.NewFileCursor(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, _, err = c.Load(ctx)
	if err == nil {
		t.Fatal("Load: expected error for unsupported version, got nil")
	}
	if !errors.Is(err, tail.ErrUnsupportedCursorVersion) {
		t.Fatalf("Load: want ErrUnsupportedCursorVersion in chain, got %v", err)
	}
}

// TestFileCursor_Load_HonoursCanceledCtx and the matching Save test pin the
// §3 ext "ctx on every blocking call" requirement: the in-tree cursor
// implementations must check ctx at entry, so plugin authors copying these
// as references see the correct pattern.
func TestFileCursor_Load_HonoursCanceledCtx(t *testing.T) {
	c, _ := newFileCursor(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.Load(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load: want context.Canceled, got %v", err)
	}
}

func TestFileCursor_Save_HonoursCanceledCtx(t *testing.T) {
	c, _ := newFileCursor(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cp := tail.Checkpoint{Pos: watch.Position{File: "/x", Inode: 1, Offset: 0}}
	if err := c.Save(cancelled, cp); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save: want context.Canceled, got %v", err)
	}
}

func TestMemoryCursor_HonoursCanceledCtx(t *testing.T) {
	c := tail.NewMemoryCursor()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.Load(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load: want context.Canceled, got %v", err)
	}
	cp := tail.Checkpoint{Pos: watch.Position{File: "/x", Inode: 1, Offset: 0}}
	if err := c.Save(cancelled, cp); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save: want context.Canceled, got %v", err)
	}
}

// TestFileCursor_Load_RejectsOversizedMeta pins the symmetric application
// of Decision #6 (64 KiB raw-meta cap). Save rejects oversized meta; Load
// must too, so an externally-edited or future-build cursor file with a
// larger meta blob does not silently load.
func TestFileCursor_Load_RejectsOversizedMeta(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.cursor")

	// Build a meta blob > 64 KiB (the cap is 64*1024). 70 KiB leaves headroom.
	huge := strings.Repeat("a", 70*1024)
	body := `{"pos":{"file":"/x","inode":"7","offset":"100"},"meta":"` + huge + `","version":1}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := tail.NewFileCursor(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, _, err = c.Load(ctx)
	if err == nil {
		t.Fatal("Load: expected error for oversized meta, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Load: want size-exceeds error, got %v", err)
	}
}

func TestFileCursor_Load_RejectsMissingVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v0.cursor")

	// Version field absent → unmarshal leaves it at 0, which is not the
	// supported version. Catches corrupted/hand-edited files.
	body := `{"pos":{"file":"/x","inode":"7","offset":"100"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := tail.NewFileCursor(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, _, err = c.Load(ctx)
	if !errors.Is(err, tail.ErrUnsupportedCursorVersion) {
		t.Fatalf("Load: want ErrUnsupportedCursorVersion, got %v", err)
	}
}

// Flock tests are gated on unix so they don't run on unsupported platforms.

func TestFileCursor_Flock_Conflict(t *testing.T) {
	testFlockConflict(t)
}

func TestFileCursor_Flock_ReleasedOnClose(t *testing.T) {
	testFlockReleasedOnClose(t)
}

func TestFileCursor_Flock_PIDInFile(t *testing.T) {
	testFlockPIDInFile(t)
}

func TestFileCursor_Flock_CrossProcess(t *testing.T) {
	testFlockCrossProcess(t)
}

// TestFileCursor_Flock_SymlinkFollow: flock open must not follow a
// pre-positioned symlink at lockPath.
func TestFileCursor_Flock_SymlinkFollow(t *testing.T) {
	testFlockSymlinkFollow(t)
}

// TestFileCursor_Flock_SameAsCursorPath: using the cursor path as the
// flock path silently breaks mutual exclusion after the first Save (the atomic
// rename orphans the held fd). NewFileCursor must reject this configuration.
func TestFileCursor_Flock_SameAsCursorPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor.json")

	_, err := tail.NewFileCursor(path, tail.WithFlock(path))
	if err == nil {
		t.Fatal("WithFlock with same path as cursor: want error, got nil")
	}
}

// TestWithFileMode_RejectsUnsafeModes: WithFileMode must reject modes
// that have group-write, world-write, or special (setuid/setgid/sticky) bits.
func TestWithFileMode_RejectsUnsafeModes(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name string
		mode os.FileMode
	}{
		{"world-writable", 0o666},
		{"group-writable", 0o660},
		{"world-and-group-writable", 0o622},
		{"setuid", 0o4600},
		{"setgid", 0o2600},
		{"sticky", 0o1600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".cursor")
			_, err := tail.NewFileCursor(path, tail.WithFileMode(tc.mode))
			if err == nil {
				t.Fatalf("WithFileMode(%04o): want error, got nil", tc.mode)
			}
		})
	}
}

// TestFileCursor_SyncBackgroundIntervalWithoutMode:
// WithSyncBackgroundInterval is silently ignored when the sync mode is not
// SyncBackground. NewFileCursor must return an error in that case so the
// misconfiguration is caught at construction time.
func TestFileCursor_SyncBackgroundIntervalWithoutMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor.json")

	_, err := tail.NewFileCursor(path,
		tail.WithSyncBackgroundInterval(10*tail.DefaultSyncBackgroundInterval),
		// WithSyncMode(tail.SyncBackground) intentionally omitted.
	)
	if err == nil {
		t.Fatal("WithSyncBackgroundInterval without SyncBackground mode: want error, got nil")
	}
}

// ── Phase C: WithCursorMigration ─────────────────────────────────────────────

func TestFileCursor_WithCursorMigration_FromV0(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v0.cursor")

	// Write a v0 cursor by hand (no version field → unmarshal sets Version=0).
	// Use the same pos JSON shape but omit the version field.
	v0body := `{"pos":{"file":"/var/log/app.log","inode":"42","offset":"512"}}`
	if err := os.WriteFile(path, []byte(v0body), 0o600); err != nil {
		t.Fatal(err)
	}

	wantPos := watch.Position{File: "/var/log/app.log", Inode: 42, Offset: 512}

	migrator := func(version int, raw []byte) (tail.Checkpoint, error) {
		if version != 0 {
			t.Errorf("migrator: want version 0, got %d", version)
		}
		// The v0 format already has a pos field with the right shape.
		// Parse it manually.
		type v0cursor struct {
			Pos watch.Position `json:"pos"`
		}
		var v0 v0cursor
		if err := json.Unmarshal(raw, &v0); err != nil {
			return tail.Checkpoint{}, err
		}
		return tail.Checkpoint{Pos: v0.Pos}, nil
	}

	c, err := tail.NewFileCursor(path, tail.WithCursorMigration(migrator))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	got, found, err := c.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("Load: expected found=true after migration")
	}
	if got.Pos != wantPos {
		t.Fatalf("pos mismatch: want %+v, got %+v", wantPos, got.Pos)
	}

	// Verify the on-disk file was upgraded to version 1.
	got2, found2, err := c.Load(ctx)
	if err != nil || !found2 {
		t.Fatalf("second Load: found=%v err=%v", found2, err)
	}
	if got2.Pos != wantPos {
		t.Fatalf("second Load pos mismatch: want %+v, got %+v", wantPos, got2.Pos)
	}
}

func TestFileCursor_WithCursorMigration_NotConfigured(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v0_nomig.cursor")

	v0body := `{"pos":{"file":"/x","inode":"1","offset":"0"}}`
	if err := os.WriteFile(path, []byte(v0body), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := tail.NewFileCursor(path) // no WithCursorMigration
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, _, err = c.Load(ctx)
	if err == nil {
		t.Fatal("Load: expected error for unsupported version, got nil")
	}
	if !errors.Is(err, tail.ErrUnsupportedCursorVersion) {
		t.Fatalf("Load: want ErrUnsupportedCursorVersion, got %v", err)
	}
}

func TestFileCursor_WithCursorMigration_ErrorPropagates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v0_migerr.cursor")

	v0body := `{"pos":{"file":"/x","inode":"1","offset":"0"}}`
	originalContent := []byte(v0body)
	if err := os.WriteFile(path, originalContent, 0o600); err != nil {
		t.Fatal(err)
	}

	migErr := errors.New("migrator: cannot handle this version")
	migrator := func(_ int, _ []byte) (tail.Checkpoint, error) {
		return tail.Checkpoint{}, migErr
	}

	c, err := tail.NewFileCursor(path, tail.WithCursorMigration(migrator))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, _, err = c.Load(ctx)
	if err == nil {
		t.Fatal("Load: expected error, got nil")
	}
	if !errors.Is(err, tail.ErrUnsupportedCursorVersion) {
		t.Fatalf("Load: want ErrUnsupportedCursorVersion in chain, got %v", err)
	}

	// File must be unchanged after a failed migration.
	diskContent, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(diskContent) != string(originalContent) {
		t.Fatalf("file was modified despite migration failure: got %q", diskContent)
	}
}

func FuzzCursorParse(f *testing.F) {
	f.Add([]byte(`{"pos":{"file":"a","inode":"1","offset":"0"},"version":1}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, data []byte) {
		var cp tail.Checkpoint
		// Must not panic regardless of input.
		_ = json.Unmarshal(data, &cp)
	})
}
