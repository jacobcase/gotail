package tail_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v2/tail"
	"github.com/jacobcase/gotail/v2/watch"
)

func tailCfg(path string) tail.Options {
	return tail.Options{
		Source:   tail.SingleFile(path),
		Interval: 10 * time.Millisecond,
	}
}

func mustNew(t *testing.T, opts tail.Options) *tail.Tailer {
	t.Helper()
	tr, err := tail.New(context.Background(), opts)
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

	_, err := tail.New(context.Background(), tail.Options{
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

	tr, err := tail.New(context.Background(), tailCfg(path))
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

// TestTailer_CloseInterruptsBlockedNext pins the new contract: Close from a
// different goroutine cancels the internal closeCtx, which (via
// context.AfterFunc) interrupts a Tailer.Next blocked inside LineReader.Next.
// Close then awaits the in-flight Next via WaitGroup before tearing down
// the LineReader, removing the previous data race between lr.Close and
// lr.Next on l.f / l.src.
func TestTailer_CloseInterruptsBlockedNext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := mustNew(t, tail.Options{
		Source:   tail.SingleFile(path),
		Interval: 10 * time.Millisecond,
		// Live tail (no StopAtEOF) — Next blocks waiting for new data.
	})

	// Long-lived ctx that we deliberately do NOT cancel; the test asserts
	// that Close alone is enough to unblock Next.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Drain the pre-existing line so the next Next blocks waiting for new data.
	if rec := nextLine(t, ctx, tr); string(rec.Line) != "first" {
		t.Fatalf("first line = %q, want %q", rec.Line, "first")
	}

	// Start a consumer that will block in Next.
	done := make(chan struct{})
	var consumerErr error
	go func() {
		defer close(done)
		_, consumerErr = tr.Next(ctx)
	}()

	// Give the consumer a moment to enter its blocking call.
	time.Sleep(50 * time.Millisecond)

	closed := make(chan error, 1)
	go func() { closed <- tr.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2s — closeCtx is not interrupting blocked Next")
	}

	select {
	case <-done:
		// Consumer should report exhaustion (closeCtx fired), not ctx.Err
		// (the user's ctx is still alive).
		if consumerErr == nil {
			t.Fatal("consumer Next returned nil error after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer goroutine did not return after Close")
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

func TestTailer_MissingCheckpoint_FallbackOldest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	active := filepath.Join(dir, "app.log")
	backup := filepath.Join(dir, "app-2024-01-01T00-00-00.log")

	os.WriteFile(backup, []byte("backup-data\n"), 0o644)
	os.WriteFile(active, []byte("active-data\n"), 0o644)

	// Capture backup's inode for the cursor checkpoint.
	inode, err := watchStatInode(backup)
	if err != nil {
		t.Fatalf("StatInode: %v", err)
	}

	cur := tail.NewMemoryCursor()
	cur.Save(ctx, tail.Checkpoint{Pos: tail.Position{File: backup, Inode: inode, Offset: 0}})

	// Delete the backup to simulate aging off.
	os.Remove(backup)

	var dropped int
	opts := tail.Options{
		Source:              tail.Lumberjack(active),
		Cursor:              cur,
		Interval:            10 * time.Millisecond,
		StopAtEOF:           true,
		OnMissingCheckpoint: tail.FallbackOldest,
		OnDropped:           func(n int) { dropped = n },
	}
	tr := mustNew(t, opts)

	var lines []string
	for rec, err := range tr.Records(ctx) {
		if err != nil {
			break
		}
		lines = append(lines, string(rec.Line))
	}

	if len(lines) != 1 || lines[0] != "active-data" {
		t.Fatalf("want [active-data], got %v", lines)
	}
	if dropped != 1 {
		t.Fatalf("want OnDropped(1), got OnDropped(%d)", dropped)
	}
}

func TestTailer_RotatesAcrossLumberjackBackups(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	active := filepath.Join(dir, "app.log")
	backup1 := filepath.Join(dir, "app-2024-01-01T00-00-00.log")
	backup2 := filepath.Join(dir, "app-2024-01-02T00-00-00.log")

	os.WriteFile(backup1, []byte("b1\n"), 0o644)
	os.WriteFile(backup2, []byte("b2\n"), 0o644)
	os.WriteFile(active, []byte("active\n"), 0o644)

	inode1, err := watchStatInode(backup1)
	if err != nil {
		t.Fatalf("StatInode backup1: %v", err)
	}

	cur := tail.NewMemoryCursor()
	cur.Save(ctx, tail.Checkpoint{Pos: tail.Position{File: backup1, Inode: inode1, Offset: 0}})

	var rotations []string
	opts := tail.Options{
		Source:    tail.Lumberjack(active),
		Cursor:    cur,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
		OnRotated: func(_, to tail.Position) {
			rotations = append(rotations, filepath.Base(to.File))
		},
	}
	tr := mustNew(t, opts)

	var lines []string
	for rec, err := range tr.Records(ctx) {
		if err != nil {
			break
		}
		lines = append(lines, string(rec.Line))
	}

	wantLines := []string{"b1", "b2", "active"}
	if len(lines) != len(wantLines) {
		t.Fatalf("want lines %v, got %v", wantLines, lines)
	}
	for i, w := range wantLines {
		if lines[i] != w {
			t.Fatalf("line[%d]: want %q, got %q", i, w, lines[i])
		}
	}
	if len(rotations) != 2 {
		t.Fatalf("want 2 OnRotated calls, got %d: %v", len(rotations), rotations)
	}
}

func watchStatInode(path string) (uint64, error) {
	return watch.StatInode(path)
}

// TestTailer_ResumeAcrossLumberjackBackupsToActive pins the headline use case:
// commit on a backup file partway through the series, restart, resume from
// the cursor, and continue rotating through remaining backups into the
// active file with zero gaps and zero duplicates.
//
// Existing coverage is split: TestTailer_ResumeAcrossRestart exercises a
// single file, and TestTailer_RotatesAcrossLumberjackBackups exercises
// multi-file rotation but without a restart. This test combines them.
func TestTailer_ResumeAcrossLumberjackBackupsToActive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	active := filepath.Join(dir, "app.log")
	backup1 := filepath.Join(dir, "app-2024-01-01T00-00-00.log")
	backup2 := filepath.Join(dir, "app-2024-01-02T00-00-00.log")

	// 7 records spread across three files. Resume must land mid-backup2
	// (after the 4th record), yielding the remaining 3 records on restart.
	if err := os.WriteFile(backup1, []byte("b1a\nb1b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup2, []byte("b2a\nb2b\nb2c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active, []byte("a1\na2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantAll := []string{"b1a", "b1b", "b2a", "b2b", "b2c", "a1", "a2"}

	// Pre-seed the cursor pointing at backup1 offset 0 so the first Tailer
	// starts at the oldest file. (Without a cursor, New defaults to the
	// active file, which is the right behavior for live tail but wrong for
	// "drain backlog" first runs.)
	inode1, err := watchStatInode(backup1)
	if err != nil {
		t.Fatalf("StatInode backup1: %v", err)
	}
	cur := tail.NewMemoryCursor()
	if err := cur.Save(ctx, tail.Checkpoint{
		Pos: tail.Position{File: backup1, Inode: inode1, Offset: 0},
	}); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	// First Tailer: read the first 4 records, commit the 4th's position,
	// close. The 4th record is "b2b" inside backup2.
	tr1 := mustNew(t, tail.Options{
		Source:    tail.Lumberjack(active),
		Cursor:    cur,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})

	var firstRun []string
	var lastPos tail.Position
	for i := 0; i < 4; i++ {
		rec, err := tr1.Next(ctx)
		if err != nil {
			t.Fatalf("first run Next #%d: %v", i, err)
		}
		firstRun = append(firstRun, string(rec.Line))
		lastPos = rec.Pos
	}
	if err := tr1.Commit(ctx, lastPos); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	tr1.Close()

	if got, want := firstRun, wantAll[:4]; !equalStrings(got, want) {
		t.Fatalf("first run: want %v, got %v", want, got)
	}
	// Cursor must point inside backup2, not active or backup1.
	if filepath.Base(lastPos.File) != filepath.Base(backup2) {
		t.Fatalf("commit position file: want %s, got %s",
			filepath.Base(backup2), filepath.Base(lastPos.File))
	}

	// Second Tailer: resume. It must yield the remaining 3 records,
	// crossing backup2 → active without re-emitting committed lines.
	tr2 := mustNew(t, tail.Options{
		Source:    tail.Lumberjack(active),
		Cursor:    cur,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})
	defer tr2.Close()

	var secondRun []string
	for rec, err := range tr2.Records(ctx) {
		if err != nil {
			break
		}
		secondRun = append(secondRun, string(rec.Line))
	}

	if got, want := secondRun, wantAll[4:]; !equalStrings(got, want) {
		t.Fatalf("second run: want %v, got %v", want, got)
	}

	// Combined runs must equal the original series exactly: no loss, no
	// duplication across the restart boundary.
	combined := append(append([]string(nil), firstRun...), secondRun...)
	if !equalStrings(combined, wantAll) {
		t.Fatalf("combined: want %v, got %v", wantAll, combined)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// ── Phase 4: Rotation hardening tests ──────────────────────────────────────

func TestRotation_Copytruncate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ct.log")
	// Use content longer than replacement so size(new) < p.pos(old) guarantees
	// the polling watcher detects the truncation regardless of timing.
	if err := os.WriteFile(path, []byte("old content here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var truncatedAt tail.Position
	opts := tail.Options{
		Source:      tail.SingleFile(path),
		Interval:    10 * time.Millisecond,
		OnTruncated: func(at tail.Position) { truncatedAt = at },
	}
	tr := mustNew(t, opts)

	rec1 := nextLine(t, ctx, tr)
	if string(rec1.Line) != "old content here" {
		t.Fatalf("want %q, got %q", "old content here", rec1.Line)
	}

	// Simulate copytruncate: truncate the file, write new content.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("new\n")
	f.Close()

	rec2 := nextLine(t, ctx, tr)
	if string(rec2.Line) != "new" {
		t.Fatalf("want new after truncation, got %q", rec2.Line)
	}

	if truncatedAt.Offset == 0 {
		t.Fatal("expected OnTruncated to fire with non-zero offset")
	}
}

func TestRotation_MidWriteTruncate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "trunc.log")
	// Two lines only: after reading both the LR buffer is empty, so the
	// truncation event cannot be masked by buffered-but-unread data.
	if err := os.WriteFile(path, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var truncations int
	opts := tail.Options{
		Source:      tail.SingleFile(path),
		Interval:    10 * time.Millisecond,
		OnTruncated: func(_ tail.Position) { truncations++ },
	}
	tr := mustNew(t, opts)

	// Read both lines, draining the buffer.
	nextLine(t, ctx, tr)
	nextLine(t, ctx, tr)

	// Mid-stream truncate.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := nextLine(t, ctx, tr)
	if string(rec.Line) != "fresh" {
		t.Fatalf("want fresh after mid-write truncate, got %q", rec.Line)
	}
	if truncations != 1 {
		t.Fatalf("want 1 OnTruncated call, got %d", truncations)
	}
}

func TestRotation_SymlinkSwap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.log")
	fileB := filepath.Join(dir, "b.log")
	link := filepath.Join(dir, "current.log")

	os.WriteFile(fileA, []byte("from-a\n"), 0o644)
	os.WriteFile(fileB, []byte("from-b\n"), 0o644)
	os.Symlink(fileA, link)

	var rotations int
	opts := tail.Options{
		Source:    tail.SingleFile(link),
		Interval:  10 * time.Millisecond,
		OnRotated: func(_, _ tail.Position) { rotations++ },
	}
	tr := mustNew(t, opts)

	rec1 := nextLine(t, ctx, tr)
	if string(rec1.Line) != "from-a" {
		t.Fatalf("want from-a, got %q", rec1.Line)
	}

	// Atomically swap the symlink.
	tmpLink := link + ".tmp"
	os.Symlink(fileB, tmpLink)
	os.Rename(tmpLink, link)

	rec2 := nextLine(t, ctx, tr)
	if string(rec2.Line) != "from-b" {
		t.Fatalf("want from-b after symlink swap, got %q", rec2.Line)
	}
}

// TestProperty_AllBytesYieldedExactlyOnce is a quick-check-style property test:
// for any sequence of lines written, the Tailer yields them all exactly once.
func TestProperty_AllBytesYieldedExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dir := t.TempDir()

	cases := [][]string{
		{"hello", "world"},
		{"single"},
		{"a", "bb", "ccc", "dddd"},
		{"", "nonempty", ""},
		{"long line: " + string(make([]byte, 500))},
	}

	for _, lines := range cases {
		path := filepath.Join(dir, "prop.log")
		var content []byte
		for _, l := range lines {
			if l == "" {
				continue // skip empty strings (no newline-terminated line)
			}
			content = append(content, []byte(l+"\n")...)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}

		opts := tail.Options{
			Source:    tail.SingleFile(path),
			Interval:  5 * time.Millisecond,
			StopAtEOF: true,
		}
		tr, err := tail.New(ctx, opts)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		var got []string
		for rec, err := range tr.Records(ctx) {
			if err != nil {
				break
			}
			got = append(got, string(rec.Line))
		}
		tr.Close()

		var want []string
		for _, l := range lines {
			if l != "" {
				want = append(want, l)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("input %v: want %d lines, got %d: %v", lines, len(want), len(got), got)
		}
		for i, w := range want {
			if got[i] != w {
				t.Fatalf("line[%d]: want %q, got %q", i, w, got[i])
			}
		}
	}
}

// TestProperty_NoByteYieldedTwice verifies crash-restart durability:
// read N lines, commit K, close, restart with same cursor, assert exactly
// the remaining N-K lines are yielded.
func TestProperty_NoByteYieldedTwice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "crash.log")
	const total = 20
	var content []byte
	for i := 0; i < total; i++ {
		content = append(content, []byte("line\n")...)
	}
	os.WriteFile(path, content, 0o644)

	for commit := 0; commit <= total; commit += 5 {
		cur := tail.NewMemoryCursor()

		opts1 := tail.Options{
			Source:    tail.SingleFile(path),
			Cursor:    cur,
			Interval:  5 * time.Millisecond,
			StopAtEOF: true,
		}
		tr1, _ := tail.New(ctx, opts1)
		var lastPos tail.Position
		for i := 0; i < commit; i++ {
			rec, err := tr1.Next(ctx)
			if err != nil {
				break
			}
			lastPos = rec.Pos
		}
		if commit > 0 {
			tr1.Commit(ctx, lastPos)
		}
		tr1.Close()

		// Restart.
		opts2 := tail.Options{
			Source:    tail.SingleFile(path),
			Cursor:    cur,
			Interval:  5 * time.Millisecond,
			StopAtEOF: true,
		}
		tr2, _ := tail.New(ctx, opts2)
		var got int
		for _, err := range tr2.Records(ctx) {
			if err != nil {
				break
			}
			got++
		}
		tr2.Close()

		want := total - commit
		if got != want {
			t.Fatalf("commit=%d: want %d remaining lines, got %d", commit, want, got)
		}
	}
}
