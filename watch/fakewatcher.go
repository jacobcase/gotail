package watch

import (
	"context"
	"io"
)

type fakeWatcher struct {
	path    string
	pos     Position
	opened  bool
	closed  bool
}

// FakeWatcher returns a [Watcher] that emits a single ReOpened event for path
// at pos, then returns [io.EOF] on subsequent Wait calls. Combine with a
// tmpfile populated by the test for full LineReader unit-testability without
// a real polling loop.
func FakeWatcher(path string, pos Position) Watcher {
	return &fakeWatcher{path: path, pos: pos}
}

func (f *fakeWatcher) Wait(ctx context.Context) (Event, error) {
	if f.closed {
		return Event{}, io.EOF
	}
	if !f.opened {
		f.opened = true
		return Event{
			Path:     f.path,
			Pos:      f.pos,
			ReOpened: true,
		}, nil
	}
	// Subsequent calls: block on ctx or return EOF if StopAtEOF semantics.
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	}
}

func (f *fakeWatcher) Close() error {
	f.closed = true
	return nil
}
