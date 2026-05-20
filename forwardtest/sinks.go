// Package forwardtest provides test helpers for the forward package.
//
// Construction idiom: helpers with required parameters expose a New*
// constructor (e.g. [NewFailingSink] needs a failure count, error, and
// inner sink). Helpers with no required parameters are zero-value-usable
// — declare them with `var s RecordingSink[T]` and call methods directly.
// The same rule applies in the sibling [tailtest] and [watchtest] packages.
package forwardtest

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/jacobcase/gotail/v2/forward"
)

// RecordingSink captures every batch delivered to it.
// Safe for concurrent use.
type RecordingSink[T any] struct {
	mu      sync.Mutex
	batches [][]T
}

// Send appends a copy of batch to the recorded batches.
func (s *RecordingSink[T]) Send(_ context.Context, batch []T) error {
	cp := make([]T, len(batch))
	copy(cp, batch)
	s.mu.Lock()
	s.batches = append(s.batches, cp)
	s.mu.Unlock()
	return nil
}

// Batches returns a snapshot of all received batches.
func (s *RecordingSink[T]) Batches() [][]T {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]T, len(s.batches))
	copy(out, s.batches)
	return out
}

// All returns all received items in order, flattened across batches.
func (s *RecordingSink[T]) All() []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Concat(s.batches...)
}

// FailingSink fails the first n Send calls with failWith (or a generic error
// if failWith is nil), then succeeds for all subsequent calls.
// Safe for concurrent use.
type FailingSink[T any] struct {
	mu       sync.Mutex
	failWith error
	failN    int
	attempts int
	inner    forward.Sink[T]
}

// NewFailingSink returns a FailingSink that fails the first n calls with
// failWith, then delegates to inner for successes.
// Pass nil for inner to use a no-op success sink.
func NewFailingSink[T any](n int, failWith error, inner forward.Sink[T]) *FailingSink[T] {
	if failWith == nil {
		failWith = errors.New("forwardtest: simulated sink failure")
	}
	if inner == nil {
		inner = forward.SinkFunc[T](func(_ context.Context, _ []T) error { return nil })
	}
	return &FailingSink[T]{failN: n, failWith: failWith, inner: inner}
}

// Send fails the first n times, then delegates to the inner sink.
func (s *FailingSink[T]) Send(ctx context.Context, batch []T) error {
	s.mu.Lock()
	attempt := s.attempts
	s.attempts++
	s.mu.Unlock()
	if attempt < s.failN {
		return s.failWith
	}
	return s.inner.Send(ctx, batch)
}

// Attempts returns the total number of Send calls made.
func (s *FailingSink[T]) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}
