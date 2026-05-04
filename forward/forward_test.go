package forward_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v2/forward"
	"github.com/jacobcase/gotail/v2/forwardtest"
	"github.com/jacobcase/gotail/v2/tail"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func mustNewForwarder[T any](t *testing.T, opts forward.Options[T]) *forward.Forwarder[T] {
	t.Helper()
	f, err := forward.New(opts)
	if err != nil {
		t.Fatalf("forward.New: %v", err)
	}
	return f
}

func mustNewTailer(t *testing.T, opts tail.Options) *tail.Tailer {
	t.Helper()
	tr, err := tail.New(opts)
	if err != nil {
		t.Fatalf("tail.New: %v", err)
	}
	t.Cleanup(func() { tr.Close() })
	return tr
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
}

func appendLines(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
}

// ── TestForwarder_BatchByCount ────────────────────────────────────────────────

func TestForwarder_BatchByCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")

	var lines []string
	for i := 0; i < 25; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	writeLines(t, path, lines)

	tr := mustNewTailer(t, tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})

	sink := &forwardtest.RecordingSink[[]byte]{}
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          tr,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fwd.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	batches := sink.Batches()
	if len(batches) != 3 {
		t.Fatalf("want 3 batches, got %d", len(batches))
	}
	if len(batches[0]) != 10 || len(batches[1]) != 10 || len(batches[2]) != 5 {
		t.Fatalf("unexpected batch sizes: %d %d %d",
			len(batches[0]), len(batches[1]), len(batches[2]))
	}
}

// ── TestForwarder_BatchByBytes ────────────────────────────────────────────────

func TestForwarder_BatchByBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")

	// Each line is exactly 5 bytes ("lineN" where N is single digit + \n trimmed = 5 chars).
	// Use 10-byte threshold → flush every 2 records.
	writeLines(t, path, []string{"aaaaa", "bbbbb", "ccccc", "ddddd"})

	tr := mustNewTailer(t, tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})

	sink := &forwardtest.RecordingSink[[]byte]{}
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:         tr,
		Decoder:        forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:           sink,
		MaxBatchBytes:  10, // 5+5 = 10 → flush on second record
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fwd.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	batches := sink.Batches()
	if len(batches) != 2 {
		t.Fatalf("want 2 batches, got %d", len(batches))
	}
	for i, b := range batches {
		if len(b) != 2 {
			t.Fatalf("batch %d: want 2 records, got %d", i, len(b))
		}
	}
}

// ── TestForwarder_BatchByAge ──────────────────────────────────────────────────

func TestForwarder_BatchByAge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")

	writeLines(t, path, []string{"only-line"})

	tr := mustNewTailer(t, tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})

	sink := &forwardtest.RecordingSink[[]byte]{}
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:       tr,
		Decoder:      forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:         sink,
		MaxBatchAge:  50 * time.Millisecond,
		MaxBatchRecords: 1000, // won't be reached
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fwd.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	all := sink.All()
	if len(all) != 1 {
		t.Fatalf("want 1 record, got %d", len(all))
	}
	if string(all[0]) != "only-line" {
		t.Fatalf("want %q, got %q", "only-line", all[0])
	}
}

// ── TestForwarder_RetryOnError ────────────────────────────────────────────────

func TestForwarder_RetryOnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	writeLines(t, path, []string{"hello"})

	tr := mustNewTailer(t, tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})

	recording := &forwardtest.RecordingSink[[]byte]{}
	failing := forwardtest.NewFailingSink[[]byte](2, nil, recording)

	var sleepCalls int
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          tr,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            failing,
		MaxBatchRecords: 10,
		InitialBackoff:  time.Millisecond,
		OnBackoffSleep:  func(d time.Duration, attempt int) { sleepCalls++ },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fwd.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if failing.Attempts() != 3 {
		t.Fatalf("want 3 Send attempts, got %d", failing.Attempts())
	}
	if sleepCalls != 2 {
		t.Fatalf("want 2 OnBackoffSleep calls, got %d", sleepCalls)
	}
	// Cursor not advanced until success: check we got the record.
	all := recording.All()
	if len(all) != 1 || string(all[0]) != "hello" {
		t.Fatalf("unexpected records: %v", all)
	}
}

// ── TestForwarder_PermanentErrorExits ────────────────────────────────────────

func TestForwarder_PermanentErrorExits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	writeLines(t, path, []string{"line1", "line2"})

	tr := mustNewTailer(t, tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})

	permErr := fmt.Errorf("auth failed: %w", forward.ErrPermanent)
	sink := forward.SinkFunc[[]byte](func(_ context.Context, _ [][]byte) error {
		return permErr
	})

	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          tr,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := fwd.Run(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, forward.ErrPermanent) {
		t.Fatalf("want ErrPermanent in chain, got %v", err)
	}
}

// ── TestForwarder_ContextCancellation ────────────────────────────────────────

func TestForwarder_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	writeLines(t, path, []string{"line1"})

	tr := mustNewTailer(t, tail.Options{
		Source:   tail.SingleFile(path),
		Interval: 10 * time.Millisecond,
		// NOT StopAtEOF: live tail blocks forever
	})

	var cancelledDuring int32
	sink := forward.SinkFunc[[]byte](func(ctx context.Context, _ [][]byte) error {
		// Simulate a slow sink that blocks until ctx is cancelled.
		<-ctx.Done()
		atomic.StoreInt32(&cancelledDuring, 1)
		return ctx.Err()
	})

	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          tr,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 1,
		InitialBackoff:  time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := fwd.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

// ── TestForwarder_DecodeErrorSkips ───────────────────────────────────────────

func TestForwarder_DecodeErrorSkips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	// line 5 is invalid JSON
	writeLines(t, path, []string{
		`{"id":1}`, `{"id":2}`, `{"id":3}`, `{"id":4}`,
		`not-json`,
		`{"id":6}`,
	})

	type Event struct{ ID int `json:"id"` }

	tr := mustNewTailer(t, tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})

	var decodeErrors int
	var decodeErrLine string
	recording := &forwardtest.RecordingSink[Event]{}
	fwd := mustNewForwarder(t, forward.Options[Event]{
		Source:          tr,
		Decoder:         forward.JSONDecoder[Event](),
		Sink:            recording,
		MaxBatchRecords: 100,
		OnDecodeError: func(line []byte, _ forward.Position, _ error) {
			decodeErrors++
			decodeErrLine = string(line)
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fwd.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	all := recording.All()
	if len(all) != 5 {
		t.Fatalf("want 5 records (line 5 skipped), got %d", len(all))
	}
	if decodeErrors != 1 {
		t.Fatalf("want 1 OnDecodeError, got %d", decodeErrors)
	}
	if decodeErrLine != "not-json" {
		t.Fatalf("wrong error line: %q", decodeErrLine)
	}
}

// ── TestForwarder_GenericTypes ────────────────────────────────────────────────

type MyEvent struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

func TestForwarder_GenericTypes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	events := []MyEvent{{"alpha", 1}, {"beta", 2}, {"gamma", 3}}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, e := range events {
		enc.Encode(e)
	}
	f.Close()

	tr := mustNewTailer(t, tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})

	recording := &forwardtest.RecordingSink[MyEvent]{}
	fwd := mustNewForwarder(t, forward.Options[MyEvent]{
		Source:          tr,
		Decoder:         forward.JSONDecoder[MyEvent](),
		Sink:            recording,
		MaxBatchRecords: 10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fwd.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	all := recording.All()
	if len(all) != 3 {
		t.Fatalf("want 3, got %d", len(all))
	}
	for i, got := range all {
		if got != events[i] {
			t.Fatalf("record %d: want %+v, got %+v", i, events[i], got)
		}
	}
}

// ── TestForwarder_StopAtEOF ───────────────────────────────────────────────────

func TestForwarder_StopAtEOF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")
	writeLines(t, path, []string{"a", "b", "c"})

	tr := mustNewTailer(t, tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})

	recording := &forwardtest.RecordingSink[[]byte]{}
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          tr,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            recording,
		MaxBatchRecords: 10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fwd.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if n := len(recording.All()); n != 3 {
		t.Fatalf("want 3, got %d", n)
	}
}

// ── TestForwarder_RecordingSink ───────────────────────────────────────────────

func TestForwarder_RecordingSink(t *testing.T) {
	sink := &forwardtest.RecordingSink[string]{}
	ctx := context.Background()
	_ = sink.Send(ctx, []string{"a", "b"})
	_ = sink.Send(ctx, []string{"c"})

	batches := sink.Batches()
	if len(batches) != 2 {
		t.Fatalf("want 2 batches, got %d", len(batches))
	}
	all := sink.All()
	if len(all) != 3 || all[0] != "a" || all[1] != "b" || all[2] != "c" {
		t.Fatalf("unexpected All: %v", all)
	}
}

// ── End-to-end test ──────────────────────────────────────────────────────────

func TestForwarder_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// Write initial content.
	const total = 50
	var lines []string
	for i := 0; i < total; i++ {
		lines = append(lines, fmt.Sprintf("event-%03d", i))
	}
	writeLines(t, path, lines)

	tr := mustNewTailer(t, tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})

	// httptest server that collects posted lines.
	var mu sync.Mutex
	var received []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []string
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		mu.Lock()
		received = append(received, batch...)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	httpSink := forward.SinkFunc[[]byte](func(ctx context.Context, batch [][]byte) error {
		var strs []string
		for _, b := range batch {
			strs = append(strs, string(b))
		}
		body, _ := json.Marshal(strs)
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("server returned %d", resp.StatusCode)
		}
		return nil
	})

	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          tr,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            httpSink,
		MaxBatchRecords: 10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := fwd.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != total {
		t.Fatalf("want %d records at server, got %d", total, len(received))
	}
	for i, got := range received {
		want := fmt.Sprintf("event-%03d", i)
		if got != want {
			t.Fatalf("record %d: want %q, got %q", i, want, got)
		}
	}
}

// ── BenchmarkForwarder_Throughput ─────────────────────────────────────────────

func BenchmarkForwarder_Throughput(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.log")

	const batchSize = 1000
	lines := make([]string, b.N)
	for i := range lines {
		lines[i] = fmt.Sprintf("benchline-%08d", i)
	}
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
	f.Close()

	tr, err := tail.New(tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  time.Millisecond,
		StopAtEOF: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer tr.Close()

	var sent int64
	sink := forward.SinkFunc[[]byte](func(_ context.Context, batch [][]byte) error {
		atomic.AddInt64(&sent, int64(len(batch)))
		return nil
	})

	fwd, err := forward.New(forward.Options[[]byte]{
		Source:          tr,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoder),
		Sink:            sink,
		MaxBatchRecords: batchSize,
	})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	ctx := context.Background()
	if err := fwd.Run(ctx); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(sent)/b.Elapsed().Seconds(), "records/s")
}
