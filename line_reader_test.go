package tail

import (
	"reflect"
	"testing"
	"time"
)

func TestLineReader(t *testing.T) {

	type line struct {
		s string
		p int64
	}

	h := NewWatcherHarness(t, "line-reader-test")

	c := Config{
		Path:     h.Path(),
		Interval: time.Millisecond * 50,
	}

	onErr := func(e error) error {
		t.Fatal(e)
		return e
	}

	r, err := NewLineReader(c, onErr)
	if err != nil {
		t.Fatal(err)
	}

	defer r.Close()

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
	for r.Next() {
		actual = append(actual, line{
			s: string(r.Bytes()),
			p: r.FileState().Position,
		})

		if len(actual) == 5 {
			break
		}
	}

	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("expected %v doesn't match actual %v", expected, actual)
	}

}
