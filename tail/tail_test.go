package tail_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v2/tail"
)

func tailCfg(path string) tail.Options {
	return tail.Options{
		Source:   tail.SingleFile(path),
		Interval: 10 * time.Millisecond,
	}
}

func mustNew(t *testing.T, opts tail.Options) *tail.Tailer {
	t.Helper()
	tr, err := tail.New(opts)
	if err != nil {
		t.Fatalf("tail.New: %v", err)
	}
	t.Cleanup(func() { tr.Close() })
	return tr
}

func nextLine(t *testing.T, ctx context.Context, tr *tail.Tailer) tail.Record {
	t.Helper()
	rec, err := tr.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return rec
}

// rotate renames path → path.1 (lumberjack-style).
func rotateTail(t *testing.T, path string) {
	t.Helper()
	if err := os.Rename(path, path+".1"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestTailer_ResumeAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "resume.log")

	// Write 20 lines.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		f.WriteString("line\n")
	}
	f.Close()

	cur := tail.NewMemoryCursor()

	// First Tailer: read 10 lines, commit, close.
	opts1 := tail.Options{
		Source:    tail.SingleFile(path),
		Cursor:    cur,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	tr1 := mustNew(t, opts1)

	var lastPos tail.Position
	for i := 0; i < 10; i++ {
		rec := nextLine(t, ctx, tr1)
		lastPos = rec.Pos
	}
	if err := tr1.Commit(ctx, lastPos); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	tr1.Close()

	// Second Tailer: resume from cursor, should start at line 11.
	opts2 := tail.Options{
		Source:    tail.SingleFile(path),
		Cursor:    cur,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	tr2 := mustNew(t, opts2)

	count := 0
	for rec, err := range tr2.Records(ctx) {
		if err != nil {
			if err == tail.ErrSourceExhausted {
				break
			}
			t.Fatalf("Records: %v", err)
		}
		_ = rec
		count++
	}
	if count != 10 {
		t.Fatalf("want 10 remaining lines, got %d", count)
	}
}

func TestTailer_MissingCheckpoint_Fail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.log")
	if err := os.WriteFile(path, []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build a cursor with a checkpoint referencing a non-existent inode.
	cur := tail.NewMemoryCursor()
	stalePos := tail.Position{File: path, Inode: 999999, Offset: 0}
	_ = cur.Save(context.Background(), tail.Checkpoint{Pos: stalePos})

	_, err := tail.New(tail.Options{
		Source:              tail.SingleFile(path),
		Cursor:              cur,
		Interval:            10 * time.Millisecond,
		OnMissingCheckpoint: tail.Fail,
	})
	if err != tail.ErrCheckpointMissing {
		t.Fatalf("want ErrCheckpointMissing, got %v", err)
	}
}

func TestTailer_MissingCheckpoint_SkipToActive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "skip.log")
	if err := os.WriteFile(path, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cur := tail.NewMemoryCursor()
	stalePos := tail.Position{File: path, Inode: 999999, Offset: 0}
	_ = cur.Save(context.Background(), tail.Checkpoint{Pos: stalePos})

	opts := tail.Options{
		Source:              tail.SingleFile(path),
		Cursor:              cur,
		Interval:            10 * time.Millisecond,
		StopAtEOF:           true,
		OnMissingCheckpoint: tail.SkipToActive,
	}
	tr := mustNew(t, opts)

	count := 0
	for rec, err := range tr.Records(ctx) {
		if err != nil {
			if err == tail.ErrSourceExhausted {
				break
			}
			t.Fatalf("Records: %v", err)
		}
		_ = rec
		count++
	}
	if count != 2 {
		t.Fatalf("want 2 lines, got %d", count)
	}
}

func TestTailer_StopAtEOF_ClosesDone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "eof.log")
	if err := os.WriteFile(path, []byte("only line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	tr := mustNew(t, opts)

	// Drain all lines.
	for rec, err := range tr.Records(ctx) {
		if err != nil {
			break
		}
		_ = rec
	}

	select {
	case <-tr.Done():
	case <-time.After(time.Second):
		t.Fatal("Done() was not closed after source exhaustion")
	}
}

func TestTailer_Records_Iterator(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "iter.log")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	tr := mustNew(t, opts)

	var lines []string
	for rec, err := range tr.Records(ctx) {
		if err != nil {
			break
		}
		lines = append(lines, string(rec.Line))
	}

	want := []string{"a", "b", "c"}
	if len(lines) != len(want) {
		t.Fatalf("want %v, got %v", want, lines)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("line[%d]: want %q, got %q", i, w, lines[i])
		}
	}
}

func TestTailer_Next_PullStyle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pull.log")
	if err := os.WriteFile(path, []byte("x\ny\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	tr := mustNew(t, opts)

	rec1 := nextLine(t, ctx, tr)
	if string(rec1.Line) != "x" {
		t.Fatalf("want x, got %q", rec1.Line)
	}
	rec2 := nextLine(t, ctx, tr)
	if string(rec2.Line) != "y" {
		t.Fatalf("want y, got %q", rec2.Line)
	}

	_, err := tr.Next(ctx)
	if err != tail.ErrSourceExhausted {
		t.Fatalf("want ErrSourceExhausted, got %v", err)
	}
}

func TestTailer_Close_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "close.log")
	if err := os.WriteFile(path, []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tr, err := tail.New(tailCfg(path))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := tr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestTailer_CommitWithMeta(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "meta.log")
	if err := os.WriteFile(path, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cur := tail.NewMemoryCursor()
	opts := tail.Options{
		Source:    tail.SingleFile(path),
		Cursor:    cur,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	tr := mustNew(t, opts)

	type meta struct{ ID string }
	rec := nextLine(t, ctx, tr)
	if err := tr.CommitWithMeta(ctx, rec.Pos, meta{ID: "batch-1"}); err != nil {
		t.Fatalf("CommitWithMeta: %v", err)
	}

	// Reload from cursor and verify meta survived.
	cp, found, err := cur.Load(ctx)
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	var got meta
	if err := json.Unmarshal(cp.Meta, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "batch-1" {
		t.Fatalf("want ID batch-1, got %q", got.ID)
	}
}

func TestTailer_PendingLineDiscardedOnClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "discard.log")
	if err := os.WriteFile(path, []byte("important\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cur := tail.NewMemoryCursor()
	opts := tail.Options{
		Source:    tail.SingleFile(path),
		Cursor:    cur,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	tr := mustNew(t, opts)

	// Read one line but do NOT commit.
	nextLine(t, ctx, tr)

	// Close without committing — cursor must still be empty.
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, found, err := cur.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Fatal("expected no checkpoint (line was not committed), but cursor has data")
	}
}

func TestTailer_Rotate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rotate.log")
	if err := os.WriteFile(path, []byte("file1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := tail.Options{
		Source:   tail.SingleFile(path),
		Interval: 10 * time.Millisecond,
	}
	tr := mustNew(t, opts)

	rec1 := nextLine(t, ctx, tr)
	if string(rec1.Line) != "file1" {
		t.Fatalf("want file1, got %q", rec1.Line)
	}

	rotateTail(t, path)
	if err := os.WriteFile(path, []byte("file2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec2 := nextLine(t, ctx, tr)
	if string(rec2.Line) != "file2" {
		t.Fatalf("want file2 after rotation, got %q", rec2.Line)
	}
}

func TestTailer_NoCursor_NoError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nocursor.log")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	tr := mustNew(t, opts)

	// Commit with no cursor is a no-op, not an error.
	rec := nextLine(t, ctx, tr)
	if err := tr.Commit(ctx, rec.Pos); err != nil {
		t.Fatalf("Commit without cursor: %v", err)
	}
}

func TestTailer_OnCheckpoint_Hook(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "hook.log")
	if err := os.WriteFile(path, []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var fired int
	cur := tail.NewMemoryCursor()
	opts := tail.Options{
		Source:       tail.SingleFile(path),
		Cursor:       cur,
		Interval:     10 * time.Millisecond,
		StopAtEOF:    true,
		OnCheckpoint: func(_ tail.Checkpoint) { fired++ },
	}
	tr := mustNew(t, opts)

	rec := nextLine(t, ctx, tr)
	if err := tr.Commit(ctx, rec.Pos); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if fired != 1 {
		t.Fatalf("want OnCheckpoint fired 1 time, got %d", fired)
	}
}

// BenchmarkCursor_Save measures the per-Save cost.
func BenchmarkCursor_Save(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.cursor")

	c, err := tail.NewFileCursor(path)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()

	cp := tail.Checkpoint{Pos: tail.Position{File: path, Inode: 1, Offset: 1024}}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cp.Pos.Offset = int64(i)
		if err := c.Save(ctx, cp); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCursor_Save_NoDirSync(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	path := filepath.Join(dir, "bench_nd.cursor")

	c, err := tail.NewFileCursor(path, tail.WithDirSync(false))
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()

	cp := tail.Checkpoint{Pos: tail.Position{File: path, Inode: 1, Offset: 0}}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cp.Pos.Offset = int64(i)
		_ = c.Save(ctx, cp)
	}
}

