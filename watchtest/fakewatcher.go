// Package watchtest provides test helpers for the watch package.
//
// Construction idiom: helpers with required parameters expose a New*
// constructor (e.g. [FakeWatcher] takes a path and resume position).
// Helpers with no required parameters are zero-value-usable. Mirrors
// the convention in the sibling [tailtest] and [forwardtest] packages.
package watchtest

import (
	"context"
	"io"

	"github.com/jacobcase/gotail/v2/watch"
)

type fakeWatcher struct {
	path   string
	pos    watch.Position
	opened bool
}

// FakeWatcher returns a [watch.Watcher] that emits a single ReOpened event for
// path at pos, then returns [io.EOF] on subsequent Wait calls. Combine with a
// tmpfile populated by the test for full LineReader unit-testability without a
// real polling loop.
func FakeWatcher(path string, pos watch.Position) watch.Watcher {
	return &fakeWatcher{path: path, pos: pos}
}

func (f *fakeWatcher) Wait(ctx context.Context) (watch.Event, error) {
	if err := ctx.Err(); err != nil {
		return watch.Event{}, err
	}
	if !f.opened {
		f.opened = true
		return watch.Event{
			Path:     f.path,
			Pos:      f.pos,
			ReOpened: true,
		}, nil
	}
	return watch.Event{}, io.EOF
}

func (f *fakeWatcher) Close() error { return nil }
