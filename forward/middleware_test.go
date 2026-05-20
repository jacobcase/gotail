package forward_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jacobcase/gotail/v3/forward"
)

func TestWithSinkTimeout_CancelsSlowSend(t *testing.T) {
	t.Parallel()

	// Inner sink blocks until ctx is cancelled. With a per-call timeout of
	// 5ms, Send must return DeadlineExceeded promptly.
	inner := forward.SinkFunc[string](func(ctx context.Context, _ []string) error {
		<-ctx.Done()
		return ctx.Err()
	})

	wrapped := forward.WithSinkTimeout[string](5 * time.Millisecond)(inner)

	start := time.Now()
	err := wrapped.Send(context.Background(), []string{"x"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send err = %v, want DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Send took %v; expected ~5ms", elapsed)
	}
}

func TestWithSinkTimeout_FastSendUnaffected(t *testing.T) {
	t.Parallel()

	called := false
	inner := forward.SinkFunc[int](func(_ context.Context, batch []int) error {
		called = true
		if len(batch) != 2 {
			t.Errorf("batch len = %d, want 2", len(batch))
		}
		return nil
	})

	wrapped := forward.WithSinkTimeout[int](time.Second)(inner)

	if err := wrapped.Send(context.Background(), []int{1, 2}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !called {
		t.Fatal("inner sink not called")
	}
}
