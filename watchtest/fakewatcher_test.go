package watchtest_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jacobcase/gotail/v3/watch"
	"github.com/jacobcase/gotail/v3/watchtest"
)

func TestFakeWatcher_FirstWaitReturnsReOpened(t *testing.T) {
	pos := watch.Position{File: "/var/log/x.log", Inode: 7, Offset: 42}
	w := watchtest.FakeWatcher(pos.File, pos)
	t.Cleanup(func() { w.Close() })

	ev, err := w.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait #1: %v", err)
	}
	if !ev.ReOpened {
		t.Fatalf("first Wait: want ReOpened, got %+v", ev)
	}
	if ev.Path != pos.File {
		t.Fatalf("first Wait: path = %q, want %q", ev.Path, pos.File)
	}
	if ev.Pos != pos {
		t.Fatalf("first Wait: pos = %+v, want %+v", ev.Pos, pos)
	}
}

func TestFakeWatcher_SubsequentWaitReturnsEOF(t *testing.T) {
	w := watchtest.FakeWatcher("/x", watch.Position{})
	t.Cleanup(func() { w.Close() })

	if _, err := w.Wait(context.Background()); err != nil {
		t.Fatalf("Wait #1: %v", err)
	}
	_, err := w.Wait(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Wait #2: want io.EOF, got %v", err)
	}
}

func TestFakeWatcher_CancelledContext(t *testing.T) {
	w := watchtest.FakeWatcher("/x", watch.Position{})
	t.Cleanup(func() { w.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := w.Wait(ctx); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("Wait on cancelled ctx: want ctx error, got %v", err)
	}
}
