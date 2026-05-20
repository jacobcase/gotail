package forward_test

// High-level benchmarks targeting the L3 Forwarder. The Forwarder wraps
// a tail.Tailer (L2) which wraps a watch.LineReader+Watcher (L1), so
// profiles taken against these benchmarks cover the entire L1+L2+L3
// stack: poll → file read → line framing → position tracking →
// decoder → batcher → sink → cursor commit.
//
// Each benchmark pre-fills a file with b.N lines and runs the Forwarder
// once to completion (StopAtEOF on the Tailer). The sink is a discarding
// SinkFunc by default — the goal is to isolate pipeline overhead from
// any work a real sink would do (HTTP, gRPC, batching to a write buffer).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v3/forward"
	"github.com/jacobcase/gotail/v3/tail"
)

const benchPollInterval = time.Millisecond

// makeBenchLine returns a payload of size bytes ending in '\n'. Sizes
// that evenly divide the LineReader's 64 KiB buffer (e.g. 64, 1024)
// can leave head==tail==len(buf) at chunk boundaries; pick 65/1025/etc.
func makeBenchLine(size int) []byte {
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

// makeBenchJSONLine returns a single NDJSON record ending in '\n'.
// padTo pads the JSON body up to that total length (including the
// trailing newline), so byte-based batching has a predictable size.
func makeBenchJSONLine(seq int, padTo int) []byte {
	body, _ := json.Marshal(map[string]any{
		"msg": "benchmark record content for json decode path",
		"seq": seq,
	})
	if padTo > len(body)+1 {
		pad := make([]byte, padTo-len(body)-1)
		for i := range pad {
			pad[i] = ' '
		}
		// Inject padding before the closing '}' so JSON stays valid:
		// {"msg":"...","seq":N,"_":"   "}
		body = body[:len(body)-1]
		body = append(body, []byte(`,"_":"`)...)
		body = append(body, pad[:padTo-len(body)-3]...)
		body = append(body, []byte(`"}`)...)
	}
	body = append(body, '\n')
	return body
}

func prefillBenchFile(b *testing.B, path string, line []byte, n int) {
	b.Helper()
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
}

func prefillBenchFileFn(b *testing.B, path string, n int, lineFn func(i int) []byte) {
	b.Helper()
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	for i := range n {
		if _, err := bw.Write(lineFn(i)); err != nil {
			b.Fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
}

// discardingSink is a zero-cost Sink that drops every batch. Used to
// isolate pipeline overhead from sink work.
func discardingSink[T any]() forward.Sink[T] {
	return forward.SinkFunc[T](func(_ context.Context, _ []T) error { return nil })
}

// runForwarderBench is the shared driver: pre-fill, build Tailer +
// Forwarder, run once, return. Caller supplies the Forwarder options
// (already populated with Source, Decoder, Sink). bytesPerLine seeds
// b.SetBytes for an MB/s number.
func runForwarderBench[T any](b *testing.B, path string, bytesPerLine int, opts forward.Options[T], cursor tail.Cursor) {
	b.Helper()
	ctx := context.Background()

	tr, err := tail.New(ctx, tail.Options{
		Source:       tail.SingleFile(path),
		Cursor:       cursor,
		Interval:     benchPollInterval,
		ForcePolling: true,
		StopAtEOF:    true,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer tr.Close()

	opts.Source = tr
	fwd, err := forward.New(opts)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(bytesPerLine))

	if err := fwd.Run(ctx); err != nil {
		b.Fatalf("Run: %v", err)
	}
	b.StopTimer()
}

// ── Identity decoder: pure pipeline overhead ─────────────────────────────────

// BenchmarkForwarder_Identity_BatchByCount measures the full L1+L2+L3
// pipeline with the zero-cost decoder (IdentityDecoder), count-based
// batching of 100 records per batch, an in-memory cursor, and a
// discarding sink. This isolates the steady-state forwarder cost:
// Source.Next → decode → append → flush trigger → Sink.Send → Commit.
func BenchmarkForwarder_Identity_BatchByCount(b *testing.B) {
	line := makeBenchLine(65)
	dir := b.TempDir()
	path := filepath.Join(dir, "fwd.log")
	prefillBenchFile(b, path, line, b.N)

	runForwarderBench(b, path, len(line), forward.Options[[]byte]{
		Decoder:         forward.IdentityDecoder,
		Sink:            discardingSink[[]byte](),
		MaxBatchRecords: 100,
	}, tail.NewMemoryCursor())
}

// BenchmarkForwarder_Identity_BatchByBytes flushes by accumulated byte
// size rather than record count. Same pipeline, different flush trigger.
// Useful for spotting overhead differences between the two checks in the
// Run loop's `shouldFlush` expression.
func BenchmarkForwarder_Identity_BatchByBytes(b *testing.B) {
	line := makeBenchLine(65)
	dir := b.TempDir()
	path := filepath.Join(dir, "fwd_bytes.log")
	prefillBenchFile(b, path, line, b.N)

	runForwarderBench(b, path, len(line), forward.Options[[]byte]{
		Decoder:       forward.IdentityDecoder,
		Sink:          discardingSink[[]byte](),
		MaxBatchBytes: 64 * 1024, // ~1000 lines of 65 bytes per batch
	}, tail.NewMemoryCursor())
}

// BenchmarkForwarder_Identity_NoCursor removes the commit path entirely
// (nil Cursor → Commit is a no-op). The delta against
// Identity_BatchByCount sizes the commit overhead.
func BenchmarkForwarder_Identity_NoCursor(b *testing.B) {
	line := makeBenchLine(65)
	dir := b.TempDir()
	path := filepath.Join(dir, "fwd_nocur.log")
	prefillBenchFile(b, path, line, b.N)

	runForwarderBench(b, path, len(line), forward.Options[[]byte]{
		Decoder:         forward.IdentityDecoder,
		Sink:            discardingSink[[]byte](),
		MaxBatchRecords: 100,
	}, nil)
}

// ── JSON decoder: realistic shape ────────────────────────────────────────────

// jsonPayload is the decode target for the JSON benchmarks. Keeping the
// struct small and using json.RawMessage for the padding field avoids
// reflection-heavy paths and isolates the json.Unmarshal cost.
type jsonPayload struct {
	Msg string          `json:"msg"`
	Seq int             `json:"seq"`
	Pad json.RawMessage `json:"_,omitempty"`
}

// BenchmarkForwarder_JSON_BatchByCount is the headline shape from the
// design doc: NDJSON records on disk, JSON decoder, batched delivery,
// memory cursor. Profiles the same pipeline as Identity_BatchByCount
// plus encoding/json.Unmarshal. The delta isolates decode cost.
func BenchmarkForwarder_JSON_BatchByCount(b *testing.B) {
	const padTo = 129 // non-aligned w.r.t. the 64 KiB LineReader buffer (see makeBenchLine)
	dir := b.TempDir()
	path := filepath.Join(dir, "fwd_json.log")
	prefillBenchFileFn(b, path, b.N, func(i int) []byte {
		return makeBenchJSONLine(i, padTo)
	})

	runForwarderBench(b, path, padTo, forward.Options[jsonPayload]{
		Decoder:         forward.JSONDecoder[jsonPayload](),
		Sink:            discardingSink[jsonPayload](),
		MaxBatchRecords: 100,
	}, tail.NewMemoryCursor())
}

// ── Durable cursor: full shipper shape ───────────────────────────────────────

// BenchmarkForwarder_Identity_FileCursor_SyncOnCommit pairs the
// pipeline with a buffered file cursor. SyncAlways would tie every
// flush to an fsync — covered by the equivalent Tailer benchmark. Here
// the goal is to confirm SyncOnCommit stays in the same per-record
// cost envelope as MemoryCursor when no explicit Sync is invoked.
func BenchmarkForwarder_Identity_FileCursor_SyncOnCommit(b *testing.B) {
	line := makeBenchLine(65)
	dir := b.TempDir()
	path := filepath.Join(dir, "fwd_fc.log")
	prefillBenchFile(b, path, line, b.N)

	cur, err := tail.NewFileCursor(
		filepath.Join(dir, fmt.Sprintf("cursor-%d.json", time.Now().UnixNano())),
		tail.WithSyncMode(tail.SyncOnCommit),
	)
	if err != nil {
		b.Fatal(err)
	}

	runForwarderBench(b, path, len(line), forward.Options[[]byte]{
		Decoder:         forward.IdentityDecoder,
		Sink:            discardingSink[[]byte](),
		MaxBatchRecords: 100,
	}, cur)
}
