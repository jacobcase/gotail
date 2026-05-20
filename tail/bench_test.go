package tail_test

// High-level benchmarks targeting the L2 Tailer surface. The Tailer wraps
// the L1 watch.LineReader+Watcher, so profiles taken with `-cpuprofile` and
// `-memprofile` against these benchmarks cover the entire L1+L2 stack:
// poll loop → file read → line framing → position tracking → cursor commit.
//
// Each benchmark recycles a finite pre-filled file rather than pre-sizing
// to b.N. The recycle loop bounds disk usage and amortises Tailer
// construction over thousands of Next() calls per round.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v2/tail"
)

const (
	benchPollInterval = time.Millisecond
	benchLinesPerFile = 50_000 // recycle horizon
)

// makeLine returns a payload of size bytes ending in '\n'. The Tailer
// reports per-line latency; SetBytes(len(line)) gives a throughput number.
//
// Callers should pick a size that does NOT evenly divide the LineReader's
// 64 KiB buffer. With an aligned size (e.g. 64 or 1024 bytes) the buffer
// fills with exactly N full lines and consuming all of them leaves
// head==tail==len(buf); the next read targets an empty slice, returns
// (0, nil), and the LineReader falls through to Watcher.Wait — which
// returns EOF in StopAtEOF mode but BLOCKS in live mode. Use 65/1025/etc.
func makeLine(size int) []byte {
	if size < 1 {
		size = 1
	}
	b := make([]byte, size)
	for i := range b[:size-1] {
		b[i] = 'a' + byte(i%26)
	}
	b[size-1] = '\n'
	return b
}

func prefillFile(b *testing.B, dir, name string, line []byte, n int) string {
	b.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	for range n {
		if _, err := bw.Write(line); err != nil {
			b.Fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	return path
}

// runBackfillBench drains the pre-filled file in StopAtEOF mode for b.N
// records, reconstructing the Tailer when a round is exhausted. makeOpts
// is invoked per round so cursor benchmarks can install a fresh cursor.
//
// commitEvery=0 disables Commit; >0 calls tr.Commit every Nth record.
func runBackfillBench(b *testing.B, path string, lineBytes int, commitEvery int, makeOpts func() tail.Options) {
	b.Helper()
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(lineBytes))

	remaining := b.N
	for remaining > 0 {
		opts := makeOpts()
		opts.StopAtEOF = true
		opts.ForcePolling = true
		opts.Interval = benchPollInterval

		tr, err := tail.New(ctx, opts)
		if err != nil {
			b.Fatal(err)
		}

		count := 0
		for remaining > 0 {
			rec, err := tr.Next(ctx)
			if err != nil {
				break // ErrSourceExhausted
			}
			count++
			remaining--
			if commitEvery > 0 && count%commitEvery == 0 {
				if cerr := tr.Commit(ctx, rec.Pos); cerr != nil {
					b.Fatal(cerr)
				}
			}
		}
		if err := tr.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// ── Backfill throughput: pure L1+L2 read path ─────────────────────────────────

// BenchmarkTailer_Backfill_ShortLines is the baseline: ~28-byte lines, no
// cursor, no hooks. Profiles the read+frame+yield hot path through the
// Watcher → LineReader → Tailer.Next chain.
func BenchmarkTailer_Backfill_ShortLines(b *testing.B) {
	line := makeLine(28)
	dir := b.TempDir()
	path := prefillFile(b, dir, "short.log", line, benchLinesPerFile)
	runBackfillBench(b, path, len(line), 0, func() tail.Options {
		return tail.Options{Source: tail.SingleFile(path)}
	})
}

// BenchmarkTailer_Backfill_LongLines uses 1 KiB lines to surface per-byte
// costs (memcpy in LineReader.compact, buffer growth thresholds).
func BenchmarkTailer_Backfill_LongLines(b *testing.B) {
	line := makeLine(1025)
	dir := b.TempDir()
	path := prefillFile(b, dir, "long.log", line, benchLinesPerFile)
	runBackfillBench(b, path, len(line), 0, func() tail.Options {
		return tail.Options{Source: tail.SingleFile(path)}
	})
}

// ── Backfill with commit: cursor hot path ─────────────────────────────────────

// BenchmarkTailer_Backfill_MemoryCursor_CommitPerLine measures the
// worst-case commit rate: every record drives a Commit. The cursor is
// in-memory, so this isolates the Tailer.Commit lock + meta-preserving
// path from disk I/O.
func BenchmarkTailer_Backfill_MemoryCursor_CommitPerLine(b *testing.B) {
	line := makeLine(65)
	dir := b.TempDir()
	path := prefillFile(b, dir, "memcur1.log", line, benchLinesPerFile)
	runBackfillBench(b, path, len(line), 1, func() tail.Options {
		return tail.Options{
			Source: tail.SingleFile(path),
			Cursor: tail.NewMemoryCursor(),
		}
	})
}

// BenchmarkTailer_Backfill_MemoryCursor_CommitEvery100 is the realistic
// shape: commit alongside a batched shipper that flushes every ~100
// records. Compare against CommitPerLine to size the commit cost.
func BenchmarkTailer_Backfill_MemoryCursor_CommitEvery100(b *testing.B) {
	line := makeLine(65)
	dir := b.TempDir()
	path := prefillFile(b, dir, "memcur100.log", line, benchLinesPerFile)
	runBackfillBench(b, path, len(line), 100, func() tail.Options {
		return tail.Options{
			Source: tail.SingleFile(path),
			Cursor: tail.NewMemoryCursor(),
		}
	})
}

// BenchmarkTailer_Backfill_FileCursor_SyncAlways_CommitEvery100 includes
// the full atomic write + fsync + dir-fsync per commit — the durable
// shipper default. Throughput is bounded by fsync latency, so b.N/op
// largely reports the file-cursor cost, not the read path.
func BenchmarkTailer_Backfill_FileCursor_SyncAlways_CommitEvery100(b *testing.B) {
	line := makeLine(65)
	dir := b.TempDir()
	path := prefillFile(b, dir, "fcur.log", line, benchLinesPerFile)
	runBackfillBench(b, path, len(line), 100, func() tail.Options {
		cur, err := tail.NewFileCursor(filepath.Join(dir, fmt.Sprintf("cursor-%d.json", time.Now().UnixNano())))
		if err != nil {
			b.Fatal(err)
		}
		return tail.Options{Source: tail.SingleFile(path), Cursor: cur}
	})
}

// BenchmarkTailer_Backfill_FileCursor_SyncOnCommit_CommitEvery100 buffers
// commits in memory; Sync would normally be driven on an external cadence.
// Surfaces the cost of the file-cursor wrapper without the fsync penalty.
func BenchmarkTailer_Backfill_FileCursor_SyncOnCommit_CommitEvery100(b *testing.B) {
	line := makeLine(65)
	dir := b.TempDir()
	path := prefillFile(b, dir, "fcursoc.log", line, benchLinesPerFile)
	runBackfillBench(b, path, len(line), 100, func() tail.Options {
		cur, err := tail.NewFileCursor(
			filepath.Join(dir, fmt.Sprintf("cursoc-%d.json", time.Now().UnixNano())),
			tail.WithSyncMode(tail.SyncOnCommit),
		)
		if err != nil {
			b.Fatal(err)
		}
		return tail.Options{Source: tail.SingleFile(path), Cursor: cur}
	})
}

// ── Rotation: multi-file Lumberjack source ────────────────────────────────────

// BenchmarkTailer_Backfill_Lumberjack_AcrossBackups exercises the
// advance() path: two backup files plus an active file, all drained
// in one StopAtEOF run. Crossing rotation requires closing the
// LineReader, opening a new one, and bumping rotation stats.
func BenchmarkTailer_Backfill_Lumberjack_AcrossBackups(b *testing.B) {
	line := makeLine(65)
	dir := b.TempDir()
	active := filepath.Join(dir, "app.log")
	backup1 := filepath.Join(dir, "app-2024-01-01T00-00-00.log")
	backup2 := filepath.Join(dir, "app-2024-01-02T00-00-00.log")

	perFile := benchLinesPerFile / 3
	for _, p := range []string{backup1, backup2, active} {
		prefillFile(b, filepath.Dir(p), filepath.Base(p), line, perFile)
	}

	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))

	remaining := b.N
	for remaining > 0 {
		tr, err := tail.New(ctx, tail.Options{
			Source:       tail.Lumberjack(active),
			Interval:     benchPollInterval,
			ForcePolling: true,
			StopAtEOF:    true,
		})
		if err != nil {
			b.Fatal(err)
		}
		for remaining > 0 {
			if _, err := tr.Next(ctx); err != nil {
				break
			}
			remaining--
		}
		tr.Close()
	}
}

// ── Live-mode read path (StopAtEOF=false) ─────────────────────────────────────

// BenchmarkTailer_LiveMode_PreFilled exercises the live-tail code path
// over a pre-filled file: StopAtEOF=false, so the watcher uses its
// production "block on EOF and re-poll" branch. The recycle limit is
// benchLinesPerFile per round, which keeps every Next call satisfied
// from buffered content — we never trip the poll wait, isolating the
// live-mode per-line overhead from the poll-interval latency.
//
// For poll-wait latency in isolation, see the watch package's
// BenchmarkPolling_Overhead.
func BenchmarkTailer_LiveMode_PreFilled(b *testing.B) {
	line := makeLine(65)
	dir := b.TempDir()
	path := prefillFile(b, dir, "live.log", line, benchLinesPerFile)

	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))

	remaining := b.N
	for remaining > 0 {
		tr, err := tail.New(ctx, tail.Options{
			Source:       tail.SingleFile(path),
			Interval:     benchPollInterval,
			ForcePolling: true,
			// StopAtEOF deliberately false: live-mode path.
		})
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < benchLinesPerFile && remaining > 0; i++ {
			if _, err := tr.Next(ctx); err != nil {
				b.Fatalf("Next: %v", err)
			}
			remaining--
		}
		tr.Close()
	}
}

// ── Hook overhead ────────────────────────────────────────────────────────────

// BenchmarkTailer_Backfill_WithHooks installs every non-error hook. The
// happy-path hooks (OnRotated, OnTruncated, OnCheckpoint, OnDropped,
// OnInodeMismatch) never fire here, so this measures the nil-vs-set
// branch cost in the read loop. OnError fires only on real errors.
func BenchmarkTailer_Backfill_WithHooks(b *testing.B) {
	line := makeLine(65)
	dir := b.TempDir()
	path := prefillFile(b, dir, "hooks.log", line, benchLinesPerFile)

	noopRotated := func(_, _ tail.Position) {}
	noopErr := func(error) {}
	noopTrunc := func(tail.Position) {}
	noopCP := func(tail.Checkpoint) {}
	noopDropped := func(int) {}
	noopInode := func(uint64, uint64) {}

	runBackfillBench(b, path, len(line), 0, func() tail.Options {
		return tail.Options{
			Source:          tail.SingleFile(path),
			OnRotated:       noopRotated,
			OnError:         noopErr,
			OnTruncated:     noopTrunc,
			OnCheckpoint:    noopCP,
			OnDropped:       noopDropped,
			OnInodeMismatch: noopInode,
		}
	})
}
