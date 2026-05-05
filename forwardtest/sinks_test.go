package forwardtest_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/jacobcase/gotail/v2/forward"
	"github.com/jacobcase/gotail/v2/forwardtest"
)

func TestRecordingSink(t *testing.T) {
	t.Parallel()

	s := &forwardtest.RecordingSink[int]{}
	ctx := context.Background()

	if err := s.Send(ctx, []int{1, 2}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(ctx, []int{3}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := s.Batches()
	want := [][]int{{1, 2}, {3}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("Batches() = %v, want %v", got, want)
	}

	if all := s.All(); !slices.Equal(all, []int{1, 2, 3}) {
		t.Fatalf("All() = %v, want [1 2 3]", all)
	}
}

func TestRecordingSink_CopiesInput(t *testing.T) {
	t.Parallel()

	// RecordingSink must defensively copy the batch slice so the caller can
	// reuse its buffer between sends.
	s := &forwardtest.RecordingSink[int]{}
	buf := []int{1, 2, 3}
	if err := s.Send(context.Background(), buf); err != nil {
		t.Fatalf("Send: %v", err)
	}
	buf[0] = 999

	if got := s.All(); got[0] != 1 {
		t.Fatalf("RecordingSink retained alias to caller buffer: got[0] = %d, want 1", got[0])
	}
}

func TestRecordingSink_ConcurrentSends(t *testing.T) {
	t.Parallel()

	s := &forwardtest.RecordingSink[int]{}
	const senders = 8
	const each = 100

	var wg sync.WaitGroup
	wg.Add(senders)
	for range senders {
		go func() {
			defer wg.Done()
			for j := range each {
				_ = s.Send(context.Background(), []int{j})
			}
		}()
	}
	wg.Wait()

	if n := len(s.All()); n != senders*each {
		t.Fatalf("recorded %d items, want %d", n, senders*each)
	}
}

func TestFailingSink_FailsThenSucceeds(t *testing.T) {
	t.Parallel()

	inner := &forwardtest.RecordingSink[string]{}
	myErr := errors.New("boom")
	s := forwardtest.NewFailingSink[string](2, myErr, inner)

	ctx := context.Background()
	if err := s.Send(ctx, []string{"a"}); !errors.Is(err, myErr) {
		t.Fatalf("call 1: err = %v, want %v", err, myErr)
	}
	if err := s.Send(ctx, []string{"b"}); !errors.Is(err, myErr) {
		t.Fatalf("call 2: err = %v, want %v", err, myErr)
	}
	if err := s.Send(ctx, []string{"c"}); err != nil {
		t.Fatalf("call 3: err = %v, want nil", err)
	}
	if err := s.Send(ctx, []string{"d"}); err != nil {
		t.Fatalf("call 4: err = %v, want nil", err)
	}

	if got := s.Attempts(); got != 4 {
		t.Fatalf("Attempts = %d, want 4", got)
	}

	if got := inner.All(); !slices.Equal(got, []string{"c", "d"}) {
		t.Fatalf("inner received %v, want [c d]", got)
	}
}

func TestFailingSink_DefaultError(t *testing.T) {
	t.Parallel()

	// nil failWith → generic synthesized error.
	s := forwardtest.NewFailingSink[int](1, nil, nil)

	err := s.Send(context.Background(), []int{1})
	if err == nil {
		t.Fatal("expected synthesized error, got nil")
	}
	// Subsequent call hits the default no-op success sink.
	if err := s.Send(context.Background(), []int{2}); err != nil {
		t.Fatalf("call 2: %v", err)
	}
}

// Compile-time check: FailingSink and RecordingSink must satisfy forward.Sink.
var (
	_ forward.Sink[int] = (*forwardtest.RecordingSink[int])(nil)
	_ forward.Sink[int] = (*forwardtest.FailingSink[int])(nil)
)
