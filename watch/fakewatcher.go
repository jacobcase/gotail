package watch

import (
	"context"
	"io"
)

type fakeWatcher struct {
	path   string
	pos    Position
	opened bool
}

// FakeWatcher returns a [Watcher] that emits a single ReOpened event for path
// at pos, then returns [io.EOF] on subsequent Wait calls. Combine with a
// tmpfile populated by the test for full LineReader unit-testability without
// a real polling loop.
func FakeWatcher(path string, pos Position) Watcher {
	return &fakeWatcher{path: path, pos: pos}
}

func (f *fakeWatcher) Wait(ctx context.Context) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	if !f.opened {
		f.opened = true
		return Event{
			Path:     f.path,
			Pos:      f.pos,
			ReOpened: true,
		}, nil
	}
	return Event{}, io.EOF
}

func (f *fakeWatcher) Close() error { return nil }
