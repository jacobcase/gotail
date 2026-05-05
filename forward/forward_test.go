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
	"runtime"
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
	tr, err := tail.New(context.Background(), opts)
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
		Source:        tr,
		Decoder:       forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:          sink,
		MaxBatchBytes: 10, // 5+5 = 10 → flush on second record
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
		Source:          tr,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchAge:     50 * time.Millisecond,
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
	// Use multiple records so the test would fail if a regression caused a
	// retry to send a *different* batch (re-decode from stale buffer,
	// off-by-one in batch slicing, etc.).
	want := []string{"alpha", "beta", "gamma"}
	writeLines(t, path, want)

	tr := mustNewTailer(t, tail.Options{
		Source:    tail.SingleFile(path),
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
	})

	// Capture every batch that Send saw, including the failing attempts.
	// FailingSink/RecordingSink only record successful calls, so use a
	// purpose-built sink to assert byte-identity across retries.
	var (
		mu           sync.Mutex
		sendBatches  [][]string
		failsRemain  = 2
		simulatedErr = errors.New("forwardtest: simulated sink failure")
	)
	sink := forward.SinkFunc[[]byte](func(_ context.Context, batch [][]byte) error {
		cp := make([]string, len(batch))
		for i, b := range batch {
			cp[i] = string(b)
		}
		mu.Lock()
		sendBatches = append(sendBatches, cp)
		fails := failsRemain
		failsRemain--
		mu.Unlock()
		if fails > 0 {
			return simulatedErr
		}
		return nil
	})

	var sleepCalls int
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          tr,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 10,
		InitialBackoff:  time.Millisecond,
		OnBackoffSleep:  func(d time.Duration, attempt int) { sleepCalls++ },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fwd.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := len(sendBatches); got != 3 {
		t.Fatalf("want 3 Send attempts, got %d", got)
	}
	if sleepCalls != 2 {
		t.Fatalf("want 2 OnBackoffSleep calls, got %d", sleepCalls)
	}

	// Each retry must replay the *same* batch — same records, same order,
	// no duplication or loss across attempts.
	for i, got := range sendBatches {
		if len(got) != len(want) {
			t.Fatalf("attempt %d: want %d records, got %d (%v)", i, len(want), len(got), got)
		}
		for j, w := range want {
			if got[j] != w {
				t.Fatalf("attempt %d, record %d: want %q, got %q (full batch %v)", i, j, w, got[j], got)
			}
		}
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

	type Event struct {
		ID int `json:"id"`
	}

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

// ── TestForwarder_PermanentErrorReturnsImmediately ───────────────────────────

// blockingSource yields one record, then blocks on ctx until cancelled.
type blockingSource struct{}

func (blockingSource) Next(ctx context.Context) (tail.Record, error) {
	if !blockingSentOnce.swap(true) {
		return tail.Record{Line: []byte("trigger"), Pos: tail.Position{Offset: 1}}, nil
	}
	<-ctx.Done()
	return tail.Record{}, ctx.Err()
}
func (blockingSource) Commit(_ context.Context, _ forward.Position) error { return nil }
func (blockingSource) Done() <-chan struct{}                              { return make(chan struct{}) }

// blockingSentOnce ensures the source emits exactly one record across
// the (possibly multiple) Next calls in a single test run.
var blockingSentOnce atomicBool

type atomicBool struct{ v atomic.Bool }

func (b *atomicBool) swap(to bool) bool { return b.v.Swap(to) }

func TestForwarder_PermanentErrorReturnsImmediately(t *testing.T) {
	blockingSentOnce = atomicBool{} // reset across runs
	permErr := fmt.Errorf("auth failed: %w", forward.ErrPermanent)
	sink := forward.SinkFunc[[]byte](func(_ context.Context, _ [][]byte) error {
		return permErr
	})

	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          blockingSource{},
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	g0 := runtime.NumGoroutine()

	if err := fwd.Run(ctx); !errors.Is(err, forward.ErrPermanent) {
		t.Fatalf("Run: want ErrPermanent, got %v", err)
	}

	// Run owns no goroutines after H1; verify the count hasn't drifted.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= g0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutine count drifted after Run returned: baseline=%d, now=%d", g0, runtime.NumGoroutine())
}

// ── TestForwarder_ExhaustionFlushesFinalBatch ────────────────────────────────
//
// A custom RecordSource that drains by returning [tail.ErrSourceExhausted]
// after delivering its records. Ensures the final batch is flushed before
// Run returns nil.

type finiteSource struct {
	records []tail.Record
	idx     int
}

func (s *finiteSource) Next(_ context.Context) (tail.Record, error) {
	if s.idx >= len(s.records) {
		return tail.Record{}, tail.ErrSourceExhausted
	}
	r := s.records[s.idx]
	s.idx++
	return r, nil
}
func (s *finiteSource) Commit(_ context.Context, _ forward.Position) error { return nil }
func (s *finiteSource) Done() <-chan struct{}                              { return nil }

func TestForwarder_ExhaustionFlushesFinalBatch(t *testing.T) {
	src := &finiteSource{records: []tail.Record{
		{Line: []byte("a"), Pos: tail.Position{File: "x", Inode: 1, Offset: 1}},
		{Line: []byte("b"), Pos: tail.Position{File: "x", Inode: 1, Offset: 2}},
		{Line: []byte("c"), Pos: tail.Position{File: "x", Inode: 1, Offset: 3}},
	}}

	var got [][]byte
	sink := forward.SinkFunc[[]byte](func(_ context.Context, batch [][]byte) error {
		for _, b := range batch {
			got = append(got, append([]byte(nil), b...))
		}
		return nil
	})

	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          src,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 100, // larger than total → flushed only on exhaustion
	})

	if err := fwd.Run(context.Background()); err != nil {
		t.Fatalf("Run: want nil on natural drain, got %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 records delivered, got %d", len(got))
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

	tr, err := tail.New(context.Background(), tail.Options{
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

// ── Phase A: BackoffJitter tests ──────────────────────────────────────────────

// backoffSource yields n records then returns ErrSourceExhausted, always
// failing the sink so we can observe the backoff durations.
type backoffSource struct {
	n    int
	sent int
}

func (s *backoffSource) Next(_ context.Context) (tail.Record, error) {
	if s.sent >= s.n {
		return tail.Record{}, tail.ErrSourceExhausted
	}
	s.sent++
	return tail.Record{Line: []byte("x"), Pos: tail.Position{Offset: int64(s.sent)}}, nil
}
func (s *backoffSource) Commit(_ context.Context, _ forward.Position) error { return nil }
func (s *backoffSource) Done() <-chan struct{}                              { return nil }

func TestForwarder_BackoffJitter_Full(t *testing.T) {
	// BackoffJitter=1 preserves the current full-jitter behaviour:
	// all sleeps must be in [0, ceiling).
	const ceiling = 50 * time.Millisecond
	var sleeps []time.Duration
	sink := forward.SinkFunc[[]byte](func(_ context.Context, _ [][]byte) error {
		return errors.New("retryable")
	})
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          &backoffSource{n: 1},
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 1,
		InitialBackoff:  ceiling,
		MaxBackoff:      ceiling,
		BackoffJitter:   1.0,
		OnBackoffSleep:  func(d time.Duration, _ int) { sleeps = append(sleeps, d) },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- fwd.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	for i, d := range sleeps {
		if d < 0 || d >= ceiling {
			t.Fatalf("sleep[%d]=%v not in [0, %v)", i, d, ceiling)
		}
	}
}

func TestForwarder_BackoffJitter_OutOfRange(t *testing.T) {
	src := &backoffSource{n: 1}
	sink := forward.SinkFunc[[]byte](func(_ context.Context, _ [][]byte) error { return nil })
	for _, j := range []float64{-1, -0.1, 1.5, 2} {
		_, err := forward.New(forward.Options[[]byte]{
			Source:          src,
			Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
			Sink:            sink,
			MaxBatchRecords: 1,
			BackoffJitter:   j,
		})
		if err == nil {
			t.Fatalf("BackoffJitter=%g: want error, got nil", j)
		}
	}
}

// doneClosableSource is a RecordSource whose Done() channel can be closed
// externally. Next keeps returning records indefinitely until ctx cancels —
// exactly the case the §11.4 #7 fix targets.
type doneClosableSource struct {
	done chan struct{}
}

func (s *doneClosableSource) Next(ctx context.Context) (tail.Record, error) {
	select {
	case <-ctx.Done():
		return tail.Record{}, ctx.Err()
	default:
	}
	return tail.Record{Line: []byte("x"), Pos: tail.Position{File: "x", Inode: 1, Offset: 1}}, nil
}
func (s *doneClosableSource) Commit(_ context.Context, _ forward.Position) error { return nil }
func (s *doneClosableSource) Done() <-chan struct{}                              { return s.done }

// TestForwarder_HonoursSourceDone pins §11.4 #7 / §4 L3 Run docstring:
// when Source.Done() closes, Run must exit cleanly even if Next keeps
// returning records.
func TestForwarder_HonoursSourceDone(t *testing.T) {
	src := &doneClosableSource{done: make(chan struct{})}
	sink := forward.SinkFunc[[]byte](func(_ context.Context, _ [][]byte) error { return nil })
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          src,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 8,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Close Done() shortly after Run starts, simulating the Tailer
	// signalling exhaustion via channel.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(src.done)
	}()

	if err := fwd.Run(ctx); err != nil {
		t.Fatalf("Run: want nil after Source.Done() close, got %v", err)
	}
}

// TestForwarder_OnBatchSent_FullSignature pins the §11.1 #1 fix: the hook
// receives records, bytes, pos, and latency — the four pieces of info plan
// §4 L3 promised plus the position the implementation found useful.
// Latency reflects the successful Sink.Send call duration only (excludes
// batch fill time and failed retries).
func TestForwarder_OnBatchSent_FullSignature(t *testing.T) {
	src := &finiteSource{records: []tail.Record{
		{Line: []byte("alpha"), Pos: tail.Position{File: "x", Inode: 1, Offset: 5}},
		{Line: []byte("beta"), Pos: tail.Position{File: "x", Inode: 1, Offset: 10}},
		{Line: []byte("gammaaa"), Pos: tail.Position{File: "x", Inode: 1, Offset: 17}},
	}}

	const sleep = 60 * time.Millisecond
	sink := forward.SinkFunc[[]byte](func(_ context.Context, _ [][]byte) error {
		time.Sleep(sleep)
		return nil
	})

	var (
		gotRecords int
		gotBytes   int
		gotPos     forward.Position
		gotLatency time.Duration
		fired      int
	)
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          src,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 100, // single batch
		OnBatchSent: func(records int, bytes int, pos forward.Position, latency time.Duration) {
			gotRecords = records
			gotBytes = bytes
			gotPos = pos
			gotLatency = latency
			fired++
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fwd.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fired != 1 {
		t.Fatalf("OnBatchSent fired %d times, want 1", fired)
	}
	if gotRecords != 3 {
		t.Fatalf("records: got %d, want 3", gotRecords)
	}
	if want := len("alpha") + len("beta") + len("gammaaa"); gotBytes != want {
		t.Fatalf("bytes: got %d, want %d", gotBytes, want)
	}
	if gotPos.Offset != 17 {
		t.Fatalf("pos.Offset: got %d, want 17 (last record)", gotPos.Offset)
	}
	if gotLatency < sleep/2 {
		t.Fatalf("latency: got %v, want at least ~%v (Sink slept %v)", gotLatency, sleep/2, sleep)
	}
}

// TestForwarder_CtxCancelStillReturnsCtxErr pins that parent-ctx
// cancellation is distinguished from Source.Done() — the former returns
// ctx.Err(), the latter returns nil.
func TestForwarder_CtxCancelStillReturnsCtxErr(t *testing.T) {
	src := &doneClosableSource{done: make(chan struct{})} // never closes
	sink := forward.SinkFunc[[]byte](func(_ context.Context, _ [][]byte) error { return nil })
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          src,
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 8,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := fwd.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: want context.Canceled, got %v", err)
	}
}

// ── Security / hardening regression tests ────────────────────────────────────

// TestNew_BackoffJitter_ZeroIsDeterministic (SE-1): BackoffJitter=0 must mean
// "deterministic" per the doc (all sleeps == ceiling). Currently New rewrites
// 0 → 0.2, so sleeps are randomly in [0.8·ceiling, ceiling) and this test
// will fail until the silent rewrite is removed.
func TestNew_BackoffJitter_ZeroIsDeterministic(t *testing.T) {
	const ceiling = 50 * time.Millisecond
	var sleeps []time.Duration

	sink := forward.SinkFunc[[]byte](func(_ context.Context, _ [][]byte) error {
		return errors.New("retryable")
	})
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          &backoffSource{n: 1},
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 1,
		InitialBackoff:  ceiling,
		MaxBackoff:      ceiling,
		BackoffJitter:   0, // must mean deterministic (always ceiling), not default 0.2
		OnBackoffSleep:  func(d time.Duration, _ int) { sleeps = append(sleeps, d) },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- fwd.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if len(sleeps) == 0 {
		t.Skip("no backoff sleeps observed — test too fast")
	}
	for i, d := range sleeps {
		if d != ceiling {
			t.Fatalf("BackoffJitter=0 sleep[%d]=%v, want deterministic %v (got jitter)", i, d, ceiling)
		}
	}
}

// TestNew_RejectsNegativeBatchLimits (SE-4): negative batch-limit values must
// be rejected by New. Currently the validator only checks == 0, so any
// negative value passes and disables flushing silently.
func TestNew_RejectsNegativeBatchLimits(t *testing.T) {
	src := &backoffSource{n: 1}
	sink := forward.SinkFunc[[]byte](func(_ context.Context, _ [][]byte) error { return nil })
	decoder := forward.Decoder[[]byte](forward.IdentityDecoderCopy)

	cases := []struct {
		name string
		opts forward.Options[[]byte]
	}{
		{
			"negative MaxBatchRecords",
			forward.Options[[]byte]{
				Source: src, Decoder: decoder, Sink: sink,
				MaxBatchRecords: -1,
			},
		},
		{
			"negative MaxBatchBytes",
			forward.Options[[]byte]{
				Source: src, Decoder: decoder, Sink: sink,
				MaxBatchRecords: 1, MaxBatchBytes: -1,
			},
		},
		{
			"negative MaxBatchAge",
			forward.Options[[]byte]{
				Source: src, Decoder: decoder, Sink: sink,
				MaxBatchRecords: 1, MaxBatchAge: -time.Second,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := forward.New(tc.opts)
			if err == nil {
				t.Fatalf("%s: want error from New, got nil", tc.name)
			}
		})
	}
}

// TestForwarder_MaxAttemptsLimitsRetries (ID-4): when MaxAttempts is set, Run
// must stop retrying after that many Sink.Send calls and return an error.
// Currently MaxAttempts is not implemented; Run loops until ctx expires.
func TestForwarder_MaxAttemptsLimitsRetries(t *testing.T) {
	var sendCalls int
	sink := forward.SinkFunc[[]byte](func(_ context.Context, _ [][]byte) error {
		sendCalls++
		return errors.New("transient")
	})

	const maxAttempts = 3
	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          &backoffSource{n: 1},
		Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
		Sink:            sink,
		MaxBatchRecords: 1,
		MaxAttempts:     maxAttempts,
		InitialBackoff:  time.Millisecond,
		MaxBackoff:      5 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := fwd.Run(ctx)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("Run hit context deadline instead of MaxAttempts cap; sendCalls=%d", sendCalls)
	}
	if err == nil {
		t.Fatalf("Run: want non-nil error after MaxAttempts=%d, got nil", maxAttempts)
	}
	if sendCalls != maxAttempts {
		t.Fatalf("want %d sink calls (MaxAttempts), got %d", maxAttempts, sendCalls)
	}
}

// TestForwarder_DecoderPermanentError_AbortsRun (SE-9): when the Decoder
// returns a wrapped ErrPermanent, Run must return that error immediately.
// Currently the permanent flag is never checked; the line is silently skipped,
// the cursor advances, and Run returns nil. This test will pass once the
// ErrPermanent check is wired into the decode path.
func TestForwarder_DecoderPermanentError_AbortsRun(t *testing.T) {
	decoder := forward.Decoder[[]byte](func(_ []byte) ([]byte, error) {
		return nil, fmt.Errorf("schema rejected: %w", forward.ErrPermanent)
	})
	sink := forward.SinkFunc[[]byte](func(_ context.Context, _ [][]byte) error { return nil })

	fwd := mustNewForwarder(t, forward.Options[[]byte]{
		Source:          &backoffSource{n: 1},
		Decoder:         decoder,
		Sink:            sink,
		MaxBatchRecords: 1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := fwd.Run(ctx)
	if !errors.Is(err, forward.ErrPermanent) {
		t.Fatalf("Decoder returns ErrPermanent: want Run to return wrapping error, got %v", err)
	}
}
