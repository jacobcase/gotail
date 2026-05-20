package forward

import "errors"

// ErrPermanent wraps a Sink error to signal that the failure is non-retryable.
// Forwarder.Run returns the wrapped error immediately when Sink.Send returns a
// permanent error.
//
// Usage:
//
//	return fmt.Errorf("invalid credentials: %w", forward.ErrPermanent)
var ErrPermanent = errors.New("permanent error")

// ErrMaxAttempts is returned (wrapped) by [Forwarder.Run] when [Options.MaxAttempts]
// is set and the Sink has failed that many times in a row. The wrapped error
// chain also contains the last sink error, so callers can match either:
//
//	if errors.Is(err, forward.ErrMaxAttempts) { /* gave up */ }
//	var perm *MyPermErr
//	if errors.As(err, &perm) { /* original sink error */ }
var ErrMaxAttempts = errors.New("forward: max attempts reached")
