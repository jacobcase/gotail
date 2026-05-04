package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v2/watch"
)

func BenchmarkLineReader_NoAlloc(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.log")

	data := make([]byte, 0, 64*1024)
	for i := 0; i < 1000; i++ {
		data = append(data, []byte("benchmark line content here\n")...)
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

	// Warm up: drain the first batch.
	for i := 0; i < 100; i++ {
		if _, _, err := lr.Next(ctx); err != nil {
			break
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lr.Next(ctx) //nolint:errcheck
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
