package tail

import (
	"io"
	"reflect"
	"testing"
	"time"
)

func TestLineReaderResume(t *testing.T) {

	type line struct {
		s string
		p int64
	}

	h := NewWatcherHarness(t, "line-reader-resume-test")

	c := Config{
		Path:      h.Path(),
		Interval:  time.Millisecond * 50,
		StopAtEOF: true,
	}

	onErr := func(e error) error {
		t.Fatal(e)
		return e
	}

	r, err := NewLineReader(c, onErr)
	if err != nil {
		t.Fatal(err)
	}

	writer := h.Create()
	writeString(t, writer, "hello\nworld\r\n!\n\n!\n")
	writer.Close()

	expected := []line{
		{
			s: "hello",
			p: 6,
		},
		{
			s: "world",
			p: 13,
		},
		{
			s: "!",
			p: 15,
		},
		{
			s: "",
			p: 16,
		},
		{
			s: "!",
			p: 18,
		},
	}

	actual := []line{}
	var cnt int
	for r.Next() {
		cnt++
		actual = append(actual, line{
			s: string(r.Bytes()),
			p: r.FileState().Position,
		})
		if cnt == 3 {
			break
		}
	}

	if r.Err() != nil {
		t.Fatalf("unexpected line reader error: %v", r.Err())
	}

	r.Close()
	info := r.FileState()

	c.StartState = &info
	r, err = NewLineReader(c, onErr)
	if err != nil {
		t.Fatal(err)
	}

	for r.Next() {
		actual = append(actual, line{
			s: string(r.Bytes()),
			p: r.FileState().Position,
		})
	}

	if r.Err() != io.EOF {
		t.Fatalf("unexpected line reader error: %v", r.Err())
	}

	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("expected %v doesn't match actual %v", expected, actual)
	}
	r.Close()
}

func readLine(t *testing.T, r *LineReader, expect string) {
	if !r.Next() {
		if r.Err() != nil {
			t.Fatalf("unexpected error: %v", r.Err())
		} else {
			t.Fatal("Next() returned false when expecting more data")
		}
	}

	if expect != string(r.Bytes()) {
		t.Fatalf("expected line '%v' doesn't match actual '%v'", expect, string(r.Bytes()))
	}
}

func TestLineReaderRotate(t *testing.T) {

	h := NewWatcherHarness(t, "line-reader-resume-test")

	c := Config{
		Path:     h.Path(),
		Interval: time.Millisecond * 50,
	}

	r, err := NewLineReader(c, func(e error) error {
		t.Fatal(e)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}

	writer := h.Create()
	writeString(t, writer, "file1\n")
	writer.Close()

	readLine(t, r, "file1")

	h.Rotate()
	writer = h.Create()
	writeString(t, writer, "file2\n")
	writer.Close()

	readLine(t, r, "file2")
}
