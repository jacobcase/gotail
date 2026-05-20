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
// They fire synchronously from the batching loop and must not block:
// a slow hook stalls send/retry/commit. If a hook needs to do I/O,
// hand the event off to a goroutine or buffered channel.
//
// # slog keys
//
// Library log lines use a subset of the gotail-wide canonical keys
// ([github.com/jacobcase/gotail/v2/watch] documents the full set):
// err, offset, attempt, latency_ms. Future events that need path or inode
// will use those canonical names too.
//
// # Layering
//
// forward depends on tail (the canonical [RecordSource] is [*tail.Tailer]).
// The [Position] and [Record] types are re-exported as aliases so that
// third-party implementations of [RecordSource], [Sink], and similar
// interfaces do not need to import the tail package directly. The aliases
// are package-level types: forward.Record is the same type as tail.Record,
// so values flow freely between the two packages.
//
// # Usage
//
//	tr, _ := tail.New(ctx, tail.Options{
//	    Source:   tail.SingleFile("/var/log/app.log"),
//	    Cursor:   cur,
//	    Interval: time.Second,
//	})
//	defer tr.Close()
//
//	sink := forward.SinkFunc[[]byte](func(ctx context.Context, batch [][]byte) error {
//	    return httpPost(ctx, batch)
//	})
//
//	fwd, err := forward.New(forward.Options[[]byte]{
//	    Source:          tr,
//	    Decoder:         forward.IdentityDecoderCopy,
//	    Sink:            sink,
//	    MaxBatchRecords: 500,
//	    MaxBatchAge:     5 * time.Second,
//	    BackoffJitter:   0.2,
//	})
//	if err != nil { return err }
//
//	if err := fwd.Run(ctx); err != nil { /* permanent or ctx error */ }
package forward
