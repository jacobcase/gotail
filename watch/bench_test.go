package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v3/watch"
)

// BenchmarkLineReader_NoAlloc asserts the steady-state read+frame hot path is
// allocation-free. It drains a finite pre-filled file in StopAtEOF mode and
// rebuilds the Watcher+LineReader when a round exhausts, so the per-op figures
// stay valid at any -benchtime: the line-yield path never blocks on the poll
// loop. (An earlier version tailed in live mode, which silently stopped
// measuring the hot path once b.N exceeded the file's line count — every extra
// Next blocked on the watcher until a ctx deadline, polluting the alloc count.)
func BenchmarkLineReader_NoAlloc(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.log")

	// A large recycle horizon so the per-round Watcher+LineReader construction
	// (chiefly the 64 KiB read buffer) amortises to ~0 B/op over the round.
	const linesPerFile = 50_000
	data := make([]byte, 0, linesPerFile*len("benchmark line content here\n"))
	for i := 0; i < linesPerFile; i++ {
		data = append(data, []byte("benchmark line content here\n")...)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()
	c := watch.Config{Path: path, Interval: time.Millisecond, StopAtEOF: true}

	newReader := func() *watch.LineReader {
		w, err := watch.NewPolling(c)
		if err != nil {
			b.Fatal(err)
		}
		return watch.NewLineReader(w, watch.LineOptions{})
	}

	b.ResetTimer()
	b.ReportAllocs()

	remaining := b.N
	for remaining > 0 {
		lr := newReader()
		for remaining > 0 {
			if _, _, err := lr.Next(ctx); err != nil {
				break // io.EOF: round exhausted, rebuild below
			}
			remaining--
		}
		lr.Close()
	}
}

func BenchmarkLineReader_LongLines(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench_long.log")

	line := make([]byte, 1024) // 1 KiB lines
	for i := range line {
		line[i] = 'a'
	}
	line = append(line, '\n')

	data := make([]byte, 0, len(line)*1000)
	for i := 0; i < 1000; i++ {
		data = append(data, line...)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		b.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := watch.Config{Path: path, Interval: time.Millisecond}
	w, _ := watch.NewPolling(c)
	lr := watch.NewLineReader(w, watch.LineOptions{})
	defer lr.Close()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lr.Next(ctx) //nolint:errcheck
	}
}

func BenchmarkPolling_Overhead(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench_poll.log")
	if err := os.WriteFile(path, []byte("data\n"), 0o644); err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()
	c := watch.Config{Path: path, Interval: time.Nanosecond}
	w, _ := watch.NewPolling(c)
	defer w.Close()

	// Prime.
	w.Wait(ctx) //nolint:errcheck

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Append a byte so Wait returns immediately with new data each iteration.
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("x")
		f.Close()
		w.Wait(ctx) //nolint:errcheck
	}
}
