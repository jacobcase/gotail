// Package forward provides a batched, retried, at-least-once log shipper
// (Layer 3 of the gotail stack).
//
// # Overview
//
// A [Forwarder] reads lines from a [RecordSource] (typically a [tail.Tailer]),
// decodes them with a [Decoder], accumulates them into batches, and delivers
// each batch to a [Sink]. If Sink.Send fails with a retryable error, the
// Forwarder backs off and retries the same batch without advancing the cursor;
// this guarantees at-least-once delivery.
//
// Batches are flushed when any of the configured bounds is reached:
// [Options.MaxBatchRecords], [Options.MaxBatchBytes], or [Options.MaxBatchAge].
//
// # Error handling
//
// Wrap a non-retryable error with [ErrPermanent] to terminate Run immediately.
// All other Sink errors are retried with exponential backoff.
//
// # Hooks
//
// [Options] exposes several nil-safe hooks for observability:
// OnBatchSent, OnSendError, OnCommitted, OnDecodeError, OnBackoffSleep.
//
// # slog keys
//
// Library log lines use the following attribute keys: err, offset, attempt, latency_ms.
package forward
