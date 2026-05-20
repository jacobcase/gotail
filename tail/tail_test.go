package tail_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v3/tail"
	"github.com/jacobcase/gotail/v3/watch"
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
		AllowInodeMismatch:  true, // fall through to OnMissingCheckpoint policy
		OnMissingCheckpoint: tail.Fail,
	})
	if err != tail.ErrCheckpointMissing {
		t.Fatalf("want ErrCheckpointMissing, got %v", err)
	}
}

// TestTailer_NoInodeCheck_PrefersCursorPath pins the §11.4 #1 fix: under
// NoInodeCheck the tie-break prefers the file the cursor named over the
// first-existing fallback. Without the fix, a multi-file source would
// resume at the oldest file regardless of which file the cursor named —
// silent rotation drift on filesystems with unstable inodes.
func TestTailer_NoInodeCheck_PrefersCursorPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	oldest := filepath.Join(dir, "oldest.log")
	active := filepath.Join(dir, "active.log")
	if err := os.WriteFile(oldest, []byte("oldA\noldB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active, []byte("newA\nnewB\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cur := tail.NewMemoryCursor()
	// Cursor named the active file (offset 0). Inode is 0 — meaningless under
	// NoInodeCheck, but the fix must still pick `active` because it's the
	// named path.
	_ = cur.Save(context.Background(), tail.Checkpoint{
		Pos: tail.Position{File: active, Inode: 0, Offset: 0},
	})

	opts := tail.Options{
		Source:       tail.StaticSource([]string{oldest, active}),
		Cursor:       cur,
		Interval:     10 * time.Millisecond,
		StopAtEOF:    true,
		NoInodeCheck: true,
	}
	tr := mustNew(t, opts)

	var got []string
	for rec, err := range tr.Records(ctx) {
		if err != nil {
			if err == tail.ErrSourceExhausted {
				break
			}
			t.Fatalf("Records: %v", err)
		}
		got = append(got, string(rec.Line))
	}
	// The first record must come from `active`, not `oldest`. Without the
	// path-first tie-break this would yield "oldA" first.
	if len(got) == 0 || got[0] != "newA" {
		t.Fatalf("first record: got %q (full %v), want first record from active.log", got[0], got)
	}
}

// TestTailer_NoInodeCheck_FallbackWhenPathGone confirms that when the cursor's
// named file is no longer in the source enumeration, the fallback (first
// existing) still works.
func TestTailer_NoInodeCheck_FallbackWhenPathGone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	active := filepath.Join(dir, "active.log")
	if err := os.WriteFile(active, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cur := tail.NewMemoryCursor()
	gonePath := filepath.Join(dir, "gone.log")
	_ = cur.Save(context.Background(), tail.Checkpoint{
		Pos: tail.Position{File: gonePath, Inode: 0, Offset: 0},
	})

	opts := tail.Options{
		Source:              tail.StaticSource([]string{active}),
		Cursor:              cur,
		Interval:            10 * time.Millisecond,
		StopAtEOF:           true,
		NoInodeCheck:        true,
		OnMissingCheckpoint: tail.FallbackOldest,
	}
	tr := mustNew(t, opts)
	count := 0
	for _, err := range tr.Records(ctx) {
		if err != nil {
			if err == tail.ErrSourceExhausted {
				break
			}
			t.Fatalf("Records: %v", err)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("want 1 line from fallback, got %d", count)
	}
}

// TestTailer_InodeMismatch_FailsByDefault pins the §3 ext-row requirement
// that ErrInodeMismatch is reachable as a public sentinel. With the
// fail-safe default, an inode-mismatched cursor causes New to return the
// sentinel without any opt-in.
func TestTailer_InodeMismatch_FailsByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rot.log")
	if err := os.WriteFile(path, []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Save cursor pointing at the path with a deliberately wrong inode —
	// simulating a rotation that reused the path with a new inode.
	cur := tail.NewMemoryCursor()
	stalePos := tail.Position{File: path, Inode: 999999, Offset: 0}
	_ = cur.Save(context.Background(), tail.Checkpoint{Pos: stalePos})

	_, err := tail.New(context.Background(), tail.Options{
		Source:   tail.SingleFile(path),
		Cursor:   cur,
		Interval: 10 * time.Millisecond,
		// AllowInodeMismatch unset → default fail-safe.
	})
	if !errors.Is(err, tail.ErrInodeMismatch) {
		t.Fatalf("New: want tail.ErrInodeMismatch, got %v", err)
	}
}

// TestTailer_OnInodeMismatch_HookFires confirms the observation hook fires
// when the cursor's path exists with a different inode, in the
// AllowInodeMismatch=true (resume) path.
func TestTailer_OnInodeMismatch_HookFires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hook.log")
	if err := os.WriteFile(path, []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cur := tail.NewMemoryCursor()
	stalePos := tail.Position{File: path, Inode: 999999, Offset: 0}
	_ = cur.Save(context.Background(), tail.Checkpoint{Pos: stalePos})

	hookCalls := 0
	var hookWant uint64
	tr, err := tail.New(context.Background(), tail.Options{
		Source:              tail.SingleFile(path),
		Cursor:              cur,
		Interval:            10 * time.Millisecond,
		AllowInodeMismatch:  true,              // opt out of fail-safe default
		OnMissingCheckpoint: tail.SkipToActive, // force success past mismatch
		OnInodeMismatch: func(want, got uint64) {
			hookCalls++
			hookWant = want
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tr.Close()
	if hookCalls != 1 {
		t.Fatalf("OnInodeMismatch fired %d times, want 1", hookCalls)
	}
	if hookWant != 999999 {
		t.Fatalf("OnInodeMismatch want=%d, expected 999999", hookWant)
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
		AllowInodeMismatch:  true, // fall through to OnMissingCheckpoint policy
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

// TestTailer_Rotate_DrainsTrailingBytes pins the headline rotation
// correctness property at the Tailer level: bytes appended to the
// rotated-out file between the watcher's last poll and the rotation
// must still be yielded before content from the new file. The existing
// TestTailer_Rotate doesn't verify drain because it consumes the entire
// pre-rotation file before rotating; the LineReader's drain branch then
// fires with zero bytes to drain.
//
// Sequence: write "first\n", read it (LineReader buffer empties, blocks
// on the next poll), then in one go append "trailing\n" to the live
// inode and rotate (rename + write "second\n" at the original path).
// The Tailer must yield first → trailing → second with no loss.
func TestTailer_Rotate_DrainsTrailingBytes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "drain.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := mustNew(t, tailCfg(path))

	if got := string(nextLine(t, ctx, tr).Line); got != "first" {
		t.Fatalf("want first, got %q", got)
	}

	// Append trailing bytes to the inode the Tailer's LineReader has open,
	// then rotate the path away and plant new content. The trailing bytes
	// live on the rotated-out inode and must be drained via the still-open
	// fd before the watcher's ReOpened triggers the switch.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("trailing\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := string(nextLine(t, ctx, tr).Line); got != "trailing" {
		t.Fatalf("drain: want trailing, got %q (rotation lost trailing bytes)", got)
	}
	if got := string(nextLine(t, ctx, tr).Line); got != "second" {
		t.Fatalf("after switch: want second, got %q", got)
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
	tr.Close()
	// In-place rotation (new inode at the same path) must fire OnRotated;
	// previously the hook only fired on inter-file advance in the Source
	// enumeration.
	if rotations != 1 {
		t.Fatalf("want 1 OnRotated call after symlink swap, got %d", rotations)
	}
}

// TestRotation_RenameCreate_FiresOnRotated pins SD-1: when the active file
// is rotated in place (rename of the original + create at the same path),
// the LineReader detects the new inode and the Tailer's OnRotated hook
// must fire. Before the fix the hook never fired in this path.
func TestRotation_RenameCreate_FiresOnRotated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var rotations int
	var lastTo tail.Position
	opts := tail.Options{
		Source:   tail.SingleFile(path),
		Interval: 10 * time.Millisecond,
		OnRotated: func(_, to tail.Position) {
			rotations++
			lastTo = to
		},
	}
	tr := mustNew(t, opts)
	defer tr.Close()

	rec1 := nextLine(t, ctx, tr)
	if string(rec1.Line) != "first" {
		t.Fatalf("want first, got %q", rec1.Line)
	}

	// Rotate: rename original out, write new content at the original path.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec2 := nextLine(t, ctx, tr)
	if string(rec2.Line) != "second" {
		t.Fatalf("want second after rotation, got %q", rec2.Line)
	}
	if rotations != 1 {
		t.Fatalf("want 1 OnRotated call after in-place rotation, got %d", rotations)
	}
	if lastTo.Offset != 0 {
		t.Fatalf("rotation to-position must reset offset to 0, got %d", lastTo.Offset)
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

// ── Phase A: SkipExisting ─────────────────────────────────────────────────────

func TestTailer_SkipExisting_NoCheckpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "skip.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f.WriteString("before\n")
	}
	f.Close()

	opts := tail.Options{
		Source:       tail.SingleFile(path),
		Interval:     10 * time.Millisecond,
		SkipExisting: true,
	}
	tr := mustNew(t, opts)

	// Start consumer goroutines first — they will block at the file's current
	// end waiting for new data. The watcher's openFirst seeks to EOF before
	// the first poll; appending after ensures we only yield post-creation lines.
	type lineResult struct {
		line string
		err  error
	}
	results := make(chan lineResult, 3)
	go func() {
		for i := 0; i < 3; i++ {
			rec, err := tr.Next(ctx)
			results <- lineResult{line: string(rec.Line), err: err}
			if err != nil {
				return
			}
		}
	}()

	// Give the goroutine time to enter its blocking Wait in the watcher so
	// openFirst has already consumed the existing bytes.
	time.Sleep(50 * time.Millisecond)

	// Append 3 more lines after the tailer is positioned at EOF.
	af, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		af.WriteString("after\n")
	}
	af.Close()

	for i := 0; i < 3; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("Next #%d: %v", i, r.err)
		}
		if r.line != "after" {
			t.Fatalf("want 'after', got %q", r.line)
		}
	}
}

func TestTailer_SkipExisting_WithCheckpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "skip_cp.log")
	if err := os.WriteFile(path, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build a cursor pointing at offset 0 (start of file).
	inode, err := watchStatInode(path)
	if err != nil {
		t.Fatal(err)
	}
	cur := tail.NewMemoryCursor()
	cur.Save(ctx, tail.Checkpoint{Pos: tail.Position{File: path, Inode: inode, Offset: 0}})

	opts := tail.Options{
		Source:       tail.SingleFile(path),
		Cursor:       cur,
		Interval:     10 * time.Millisecond,
		StopAtEOF:    true,
		SkipExisting: true, // should be ignored because checkpoint exists
	}
	tr := mustNew(t, opts)

	var lines []string
	for rec, err := range tr.Records(ctx) {
		if err != nil {
			break
		}
		lines = append(lines, string(rec.Line))
	}
	// Should yield both lines (checkpoint overrides SkipExisting).
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (checkpoint overrides SkipExisting), got %d: %v", len(lines), lines)
	}
}

func TestTailer_SkipExisting_ConflictsWithWhence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conflict.log")
	if err := os.WriteFile(path, []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tail.New(context.Background(), tail.Options{
		Source:       tail.SingleFile(path),
		Interval:     10 * time.Millisecond,
		SkipExisting: true,
		Whence:       1, // io.SeekCurrent
	})
	if err == nil {
		t.Fatal("want error for SkipExisting+Whence, got nil")
	}
}

// ── Phase B: Stats ────────────────────────────────────────────────────────────

func TestTailer_Stats_LineAndByteCounters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "stats.log")
	// Three lines of known lengths: 3+1, 5+1, 4+1 = 15 bytes total.
	if err := os.WriteFile(path, []byte("abc\nhello\ntest\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	tr := mustNew(t, opts)

	var got []tail.Record
	for rec, err := range tr.Records(ctx) {
		if err != nil {
			break
		}
		got = append(got, rec)
	}

	if len(got) != 3 {
		t.Fatalf("want 3 records, got %d", len(got))
	}

	s := tr.Stats()
	if s.LinesYielded != 3 {
		t.Fatalf("LinesYielded: want 3, got %d", s.LinesYielded)
	}
	// BytesRead = sum of (len(line)+1) for each line = 4+6+5 = 15.
	if s.BytesRead != 15 {
		t.Fatalf("BytesRead: want 15, got %d", s.BytesRead)
	}
}

func TestTailer_Stats_Rotations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	active := filepath.Join(dir, "app.log")
	backup := filepath.Join(dir, "app-2024-01-01T00-00-00.log")

	os.WriteFile(backup, []byte("b1\n"), 0o644)
	os.WriteFile(active, []byte("active\n"), 0o644)

	inode1, _ := watchStatInode(backup)
	cur := tail.NewMemoryCursor()
	cur.Save(ctx, tail.Checkpoint{Pos: tail.Position{File: backup, Inode: inode1, Offset: 0}})

	opts := tail.Options{
		Source:    tail.Lumberjack(active),
		Cursor:    cur,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	tr := mustNew(t, opts)

	for _, err := range tr.Records(ctx) {
		if err != nil {
			break
		}
	}

	s := tr.Stats()
	if s.Rotations != 1 {
		t.Fatalf("want 1 rotation, got %d", s.Rotations)
	}
}

func TestTailer_Stats_ConcurrentReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "conc.log")
	var content []byte
	for i := 0; i < 100; i++ {
		content = append(content, []byte("line\n")...)
	}
	os.WriteFile(path, content, 0o644)

	opts := tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  5 * time.Millisecond,
		StopAtEOF: true,
	}
	tr := mustNew(t, opts)

	// Scrape stats from a side goroutine while Next runs.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			s := tr.Stats()
			_ = s
			select {
			case <-ctx.Done():
				return
			default:
			}
			if s.LinesYielded >= 100 {
				return
			}
		}
	}()

	for _, err := range tr.Records(ctx) {
		if err != nil {
			break
		}
	}
	<-done
}

// ── Phase B: CloseWithFlush ───────────────────────────────────────────────────

func TestTailer_CloseWithFlush_PersistsLastPos(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "flush.log")
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
	tr, err := tail.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}

	// Read one record without committing.
	rec := nextLine(t, ctx, tr)

	// CloseWithFlush should persist the last-read position.
	if err := tr.CloseWithFlush(ctx); err != nil {
		t.Fatalf("CloseWithFlush: %v", err)
	}

	// Verify the cursor has the position.
	cp, found, err := cur.Load(ctx)
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	if cp.Pos != rec.Pos {
		t.Fatalf("pos mismatch: want %+v, got %+v", rec.Pos, cp.Pos)
	}
}

func TestTailer_Close_DoesNotPersist(t *testing.T) {
	// Decision #19: Close alone leaves cursor empty for a yielded-but-uncommitted line.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "no_persist.log")
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

	nextLine(t, ctx, tr)
	tr.Close()

	_, found, err := cur.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Fatal("Close() must not persist; want cursor empty after non-committed read")
	}
}

func TestTailer_CloseWithFlush_NilCursor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nocursor.log")
	os.WriteFile(path, []byte("data\n"), 0o644)

	opts := tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	tr, err := tail.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}

	nextLine(t, ctx, tr)
	if err := tr.CloseWithFlush(ctx); err != nil {
		t.Fatalf("CloseWithFlush with nil cursor: %v", err)
	}
}

func TestTailer_CloseWithFlush_CtxCanceled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ctxcancel.log")
	os.WriteFile(path, []byte("data\n"), 0o644)

	// Use a cursor that fails on Save.
	failCursor := &failingSaveCursor{}

	opts := tail.Options{
		Source:    tail.SingleFile(path),
		Cursor:    failCursor,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	}
	ctx := context.Background()
	tr, err := tail.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}

	// Read one line (so t.cur is non-zero).
	nextLine(t, context.Background(), tr)

	// Cancel the ctx passed to CloseWithFlush — Save errors, but Close must
	// still run and release the fd.
	ctxCanceled, cancel := context.WithCancel(context.Background())
	cancel()

	saveErr := tr.CloseWithFlush(ctxCanceled)
	if saveErr == nil {
		t.Fatal("want error from failing save, got nil")
	}

	// Double-close must be safe (no panic, no extra error).
	_ = tr.Close()
}

// failingSaveCursor always returns an error from Save.
type failingSaveCursor struct{}

func (f *failingSaveCursor) Load(_ context.Context) (tail.Checkpoint, bool, error) {
	return tail.Checkpoint{}, false, nil
}
func (f *failingSaveCursor) Save(_ context.Context, _ tail.Checkpoint) error {
	return errors.New("save failed: test error")
}
func (f *failingSaveCursor) Close() error { return nil }

// ── Security / hardening regression tests ────────────────────────────────────

// TestNew_RequireCursor_NilErrors: when RequireCursor is set, New must
// return an error instead of silently disabling checkpointing.
func TestNew_RequireCursor_NilErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tail.New(context.Background(), tail.Options{
		Source:        tail.SingleFile(path),
		Interval:      10 * time.Millisecond,
		RequireCursor: true,
		// Cursor: nil (intentionally omitted)
	})
	if err == nil {
		t.Fatal("RequireCursor=true with nil Cursor: want error, got nil")
	}
}

// TestNew_InodeMismatch_FailsByDefault: the default behaviour
// (AllowInodeMismatch=false) must fail-safe. With default Options an inode
// swap must cause New to return an error wrapping ErrInodeMismatch.
func TestNew_InodeMismatch_FailsByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cur := tail.NewMemoryCursor()
	ctx := context.Background()

	// First tailer: read one line and checkpoint (captures the real inode).
	tr, err := tail.New(ctx, tail.Options{
		Source:    tail.SingleFile(path),
		Cursor:    cur,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	rec, err := tr.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if err := tr.Commit(ctx, rec.Pos); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	tr.Close()

	// Rotate: write the replacement to a sibling path, then atomically
	// rename it over the original. The sibling is allocated while `path`
	// still holds the original inode, so it is guaranteed to land on a
	// different inode. os.Remove + os.WriteFile is unsafe here because
	// Linux ext4/tmpfs reuses freed inodes immediately, occasionally
	// handing the new file the same inode the cursor recorded.
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte("new-content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	// Second tailer with default Options (AllowInodeMismatch unset / false).
	// The fail-safe default must error with ErrInodeMismatch.
	_, err = tail.New(ctx, tail.Options{
		Source:   tail.SingleFile(path),
		Cursor:   cur,
		Interval: 10 * time.Millisecond,
	})
	if !errors.Is(err, tail.ErrInodeMismatch) {
		t.Fatalf("default options after inode swap: want ErrInodeMismatch, got %v", err)
	}
}

// TestNew_RejectsNegativeInterval: a negative Interval must be
// rejected by New, not silently coerced to 1s.
func TestNew_RejectsNegativeInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tail.New(context.Background(), tail.Options{
		Source:   tail.SingleFile(path),
		Interval: -time.Second,
	})
	if err == nil {
		t.Fatal("Interval=-1s: want error, got nil")
	}
}

// TestNew_RejectsSeekCurrent: io.SeekCurrent is accepted by the
// validator but falls through to SeekStart semantics (full-file replay).
// New must reject it explicitly so callers get a clear error.
func TestNew_RejectsSeekCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tail.New(context.Background(), tail.Options{
		Source:   tail.SingleFile(path),
		Interval: 10 * time.Millisecond,
		Whence:   io.SeekCurrent,
	})
	if err == nil {
		t.Fatal("Whence=io.SeekCurrent: want error, got nil")
	}
}
