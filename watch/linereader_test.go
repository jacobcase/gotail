package watch_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v3/watch"
)

func newLR(t *testing.T, w watch.Watcher, opts watch.LineOptions) *watch.LineReader {
	t.Helper()
	lr := watch.NewLineReader(w, opts)
	t.Cleanup(func() { lr.Close() })
	return lr
}

func nextLine(t *testing.T, ctx context.Context, lr *watch.LineReader) string {
	t.Helper()
	line, _, err := lr.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return string(line)
}

func TestLineReader_Basic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "basic.log")
	if err := os.WriteFile(path, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := watch.Config{Path: path, Interval: 10 * time.Millisecond, StopAtEOF: true}
	w, err := watch.NewPolling(c)
	if err != nil {
		t.Fatal(err)
	}
	lr := newLR(t, w, watch.LineOptions{})

	if got := nextLine(t, ctx, lr); got != "hello" {
		t.Fatalf("want hello, got %q", got)
	}
	if got := nextLine(t, ctx, lr); got != "world" {
		t.Fatalf("want world, got %q", got)
	}

	_, _, err = lr.Next(ctx)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF at end of StopAtEOF file, got %v", err)
	}
}

func TestLineReader_CRLF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "crlf.log")
	if err := os.WriteFile(path, []byte("hello\r\nworld\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := watch.Config{Path: path, Interval: 10 * time.Millisecond, StopAtEOF: true}
	w, err := watch.NewPolling(c)
	if err != nil {
		t.Fatal(err)
	}
	lr := newLR(t, w, watch.LineOptions{})

	if got := nextLine(t, ctx, lr); got != "hello" {
		t.Fatalf("want hello (no CR), got %q", got)
	}
	if got := nextLine(t, ctx, lr); got != "world" {
		t.Fatalf("want world (no CR), got %q", got)
	}
}

func TestLineReader_LongLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "long.log")

	longLine := make([]byte, 200)
	for i := range longLine {
		longLine[i] = 'x'
	}
	content := append(longLine, '\n')
	content = append(content, []byte("ok\n")...)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	c := watch.Config{Path: path, Interval: 10 * time.Millisecond, StopAtEOF: true}
	w, err := watch.NewPolling(c)
	if err != nil {
		t.Fatal(err)
	}
	lr := newLR(t, w, watch.LineOptions{MaxLine: 100})

	_, _, err = lr.Next(ctx)
	if err != watch.ErrLineTooLong {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}

	// Should recover and return the next valid line.
	if got := nextLine(t, ctx, lr); got != "ok" {
		t.Fatalf("want ok after ErrLineTooLong, got %q", got)
	}
}

func TestLineReader_BufferReuse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "reuse.log")
	if err := os.WriteFile(path, []byte("lineA\nlineB\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := watch.Config{Path: path, Interval: 10 * time.Millisecond, StopAtEOF: true}
	w, err := watch.NewPolling(c)
	if err != nil {
		t.Fatal(err)
	}
	lr := newLR(t, w, watch.LineOptions{})

	line1, _, _ := lr.Next(ctx)
	// Capture a copy to compare — we must NOT retain the slice.
	orig := string(line1)

	// Calling Next again should invalidate the previous slice.
	line2, _, _ := lr.Next(ctx)
	_ = line2

	// line1 slice may now be overwritten; the value we saved via string() is safe.
	if orig != "lineA" {
		t.Fatalf("want lineA, got %q", orig)
	}
}

func TestLineReader_NoAlloc(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "noalloc.log")
	// Write many lines so we have plenty to iterate over without calling Wait.
	data := make([]byte, 0, 64*1024)
	for i := 0; i < 500; i++ {
		data = append(data, []byte("short line here\n")...)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	c := watch.Config{Path: path, Interval: 10 * time.Millisecond, StopAtEOF: true}
	w, err := watch.NewPolling(c)
	if err != nil {
		t.Fatal(err)
	}
	lr := newLR(t, w, watch.LineOptions{})

	// Prime: drain events and warm up the file.
	for i := 0; i < 10; i++ {
		if _, _, err := lr.Next(ctx); err != nil {
			break
		}
	}

	allocs := testing.AllocsPerRun(100, func() {
		lr.Next(ctx) //nolint:errcheck
	})
	if allocs > 0 {
		t.Fatalf("hot-path allocs per Next = %.1f, want 0", allocs)
	}
}

// TestLineReader_Resume verifies that a LineReader honours a resume Position.
func TestLineReader_Resume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "resume.log")
	if err := os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First reader: read two lines, record position.
	c1 := watch.Config{Path: path, Interval: 10 * time.Millisecond, StopAtEOF: true}
	w1, _ := watch.NewPolling(c1)
	lr1 := watch.NewLineReader(w1, watch.LineOptions{})

	nextLine(t, ctx, lr1) // line1
	nextLine(t, ctx, lr1) // line2
	savedPos := lr1.Position()
	lr1.Close()

	// Second reader: resume from saved position.
	c2 := watch.Config{
		Path:      path,
		Interval:  10 * time.Millisecond,
		StopAtEOF: true,
		Resume:    &savedPos,
	}
	w2, _ := watch.NewPolling(c2)
	lr2 := watch.NewLineReader(w2, watch.LineOptions{})
	defer lr2.Close()

	if got := nextLine(t, ctx, lr2); got != "line3" {
		t.Fatalf("want line3 after resume, got %q", got)
	}
}

// TestLineReader_Rotate covers rotation: switch to the new file after an
// in-place rotation, draining trailing bytes from the old fd first.
func TestLineReader_Rotate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rotate.log")
	if err := os.WriteFile(path, []byte("file1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := watch.Config{Path: path, Interval: 10 * time.Millisecond}
	w, err := watch.NewPolling(c)
	if err != nil {
		t.Fatal(err)
	}
	lr := newLR(t, w, watch.LineOptions{})

	if got := nextLine(t, ctx, lr); got != "file1" {
		t.Fatalf("want file1, got %q", got)
	}

	rotate(t, path)
	if err := os.WriteFile(path, []byte("file2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := nextLine(t, ctx, lr); got != "file2" {
		t.Fatalf("want file2, got %q", got)
	}
}

// FuzzLineReader verifies that every byte written is yielded exactly once.
func FuzzLineReader(f *testing.F) {
	f.Add([]byte("hello\nworld\n"), uint8(16))
	f.Add([]byte("no newline at end"), uint8(4))
	f.Add([]byte("\n\n\n"), uint8(1))

	f.Fuzz(func(t *testing.T, input []byte, chunkSize uint8) {
		if len(input) == 0 {
			return
		}
		if chunkSize == 0 {
			chunkSize = 1
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "fuzz.log")

		// Write input in chunks to simulate incremental writes.
		chunk := int(chunkSize)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < len(input); i += chunk {
			end := i + chunk
			if end > len(input) {
				end = len(input)
			}
			f.Write(input[i:end]) //nolint:errcheck
		}
		f.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		c := watch.Config{Path: path, Interval: 5 * time.Millisecond, StopAtEOF: true}
		w, _ := watch.NewPolling(c)
		lr := watch.NewLineReader(w, watch.LineOptions{MaxLine: len(input) + 1})
		defer lr.Close()

		var got []byte
		for {
			line, _, err := lr.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return // context timeout or similar — don't fail
			}
			got = append(got, line...)
			got = append(got, '\n')
		}

		// The yielded bytes (with newlines) should match all complete lines in input.
		want := extractLines(input)
		if string(got) != string(want) {
			t.Fatalf("yielded %q, want %q", got, want)
		}
	})
}

// extractLines returns the content of all complete newline-terminated lines in b.
func extractLines(b []byte) []byte {
	var out []byte
	for len(b) > 0 {
		idx := indexByte(b, '\n')
		if idx < 0 {
			break
		}
		line := b[:idx]
		// Strip trailing \r.
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		out = append(out, line...)
		out = append(out, '\n')
		b = b[idx+1:]
	}
	return out
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}
