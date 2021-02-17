package tail

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fileList struct {
	name  string
	files []string
}

func newFileList(name string) fileList {
	return fileList{name: name}
}

func (l fileList) push() error {

	if len(l.files) == 0 {
		l.files = []string{l.name}
	}

	l.files = append(l.files, fmt.Sprintf("%s.%v", l.name, len(l.files)))

	for i := len(l.files) - 1; i > 0; i-- {
		if err := os.Rename(l.files[i-1], l.files[i]); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	return nil
}

func (l *fileList) removeAll() error {
	for _, name := range l.files {
		if err := os.Remove(name); err != nil {
			return err
		}
	}
	return nil
}

func (l fileList) Name() string {
	return l.name
}

type RotaterHarness struct {
	files fileList
	t     *testing.T
	path  string
}

func NewRotaterHarness(t *testing.T, name string) *RotaterHarness {
	p := filepath.Join(t.TempDir(), name)

	r := &RotaterHarness{
		t:     t,
		path:  p,
		files: newFileList(p),
	}
	t.Cleanup(r.cleanup)
	return r
}

func (r *RotaterHarness) Path() string {
	return r.path
}

func (r *RotaterHarness) cleanup() {
	r.t.Helper()
	if err := r.files.removeAll(); err != nil {
		r.t.Fatal(err)
	}
}

func (h *RotaterHarness) Wait(r Rotater, reOpened bool, closed bool, err error) *os.File {
	h.t.Helper()
	s, c, e := r.Wait()
	if e != err {
		h.t.Fatalf("rotater returned %v for error, expected %v", e, err)
	}

	if c != closed {
		h.t.Fatalf("rotater returned %v for closed, expected %v", c, closed)
	}

	if s.ReOpened != reOpened {
		h.t.Fatalf("rotater returned %v for new file, expected %v", s.ReOpened, reOpened)
	}

	return s.File
}

func (h *RotaterHarness) Rotate() {
	if err := h.files.push(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *RotaterHarness) Create() *os.File {
	f, err := os.OpenFile(h.files.Name(), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0644)
	if err != nil {
		h.t.Fatal(err)
	}
	return f
}

func readString(t *testing.T, r io.Reader, count int) string {
	t.Helper()
	buf := make([]byte, count)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatal(err)
	}

	if n != count {
		t.Fatalf("read %v bytes, expected %v", n, count)
	}
	return string(buf)
}

func expectString(t *testing.T, r io.Reader, expect string) {
	t.Helper()
	if actual := readString(t, r, len(expect)); actual != expect {
		t.Fatalf("read %s from reader, expected %s", actual, expect)
	}
}

func writeString(t *testing.T, w io.Writer, s string) {
	sb := []byte(s)
	_, err := w.Write(sb)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadAfterRotate(t *testing.T) {

	h := NewRotaterHarness(t, "write-after-rotate")

	c := PollerConfig{
		Path:     h.Path(),
		Interval: time.Millisecond * 50,
	}

	r, err := NewPollingRotater(c)
	if err != nil {
		t.Fatal(err)
	}

	// Write a string to the file.
	writer := h.Create()
	writeString(t, writer, "foobarbaz")
	writer.Close()

	// Read part of data, ensures the poller picks up this file
	// and opens it before rotating it.
	reader := h.Wait(r, true, false, nil)
	expectString(t, reader, "foo")

	// Rotate file, but don't create the new one yet.
	h.Rotate()

	// Read more data. Optimally you'd read until the first EOF,
	// but Wait should behave all the same.
	reader = h.Wait(r, false, false, nil)
	expectString(t, reader, "bar")

	// Create new file. The poller shouldn't pick this up
	// because it should still see 3 unread bytes in the old file
	reader2 := h.Create()
	defer reader2.Close()

	// Read more data
	reader = h.Wait(r, false, false, nil)
	expectString(t, reader, "baz")
}
