package tail

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type TestHarness struct {
	t    *testing.T
	f    *os.File
	path string
}

func NewTestHarness(t *testing.T, name string) *TestHarness {
	path := filepath.Join(t.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	t.TempDir()
	return &TestHarness{
		t:    t,
		f:    f,
		path: path,
	}
}

func (h *TestHarness) Rotate() {
	err := h.f.Close()
	if err != nil {
		h.t.Fatal(err)
	}

	err = os.Rename(h.path, h.path+".old")
	if err != nil {
		h.t.Fatal(err)
	}

	h.f, err = os.Create(h.path)
	if err != nil {
		h.t.Fatal(err)
	}

	return
}

func (h *TestHarness) Write(b []byte) {
	n, err := h.f.Write(b)
	if err != nil {
		h.t.Fatal(err)
	}

	if n < len(b) {
		h.t.Fatal("couldn't write full byte buffer")
	}
}

func (h *TestHarness) Path() string {
	return h.path
}

func (h *TestHarness) Close() {
	err := h.f.Close()
	if err != nil {
		h.t.Fatal(err)
	}

	os.RemoveAll(h.t.TempDir())
}

func (h *TestHarness) ErrorFunc(err error) {
	h.t.Log(err)
}

func TestPoller(t *testing.T) {

	h := NewTestHarness(t, "test")
	defer h.Close()

	p := NewPoller(PollerConfig{
		FilePath: h.Path(),
		OnError:  h.ErrorFunc,
	})

	in := []byte("hello")

	h.Write(in)

	out := make([]byte, len(in))

	_, err := p.Read(out)
	if err != nil {
		h.t.Fatal(err)
	}

	if !reflect.DeepEqual(in, out) {
		h.t.Fatal("file input doesn't match output")
	}

	h.Rotate()

	in = []byte("world")

	h.Write(in)

	out = make([]byte, len(in))

	_, err = p.Read(out)
	if err != nil {
		h.t.Fatal(err)
	}

	if !reflect.DeepEqual(in, out) {
		h.t.Fatal("file input doesn't match output")
	}
}
