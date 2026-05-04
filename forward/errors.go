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
