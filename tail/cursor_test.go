package tail_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcase/gotail/v2/tail"
	"github.com/jacobcase/gotail/v2/watch"
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
