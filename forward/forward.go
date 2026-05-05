package forward

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jacobcase/gotail/v2/tail"
)

// Position is an alias for [tail.Position] so external implementations of
// [Sink], [RecordSource], and [Cursor] do not need to import the tail
// package just to handle positions.
type Position = tail.Position

// Record is an alias for [tail.Record] for the same reason as [Position]:
// it lets third-party RecordSource/Sink implementations refer to the record
// shape without importing the tail package directly.
type Record = tail.Record

// RecordSource is the read side of a [tail.Tailer]. [*tail.Tailer] satisfies
// this interface, enabling Forwarder to be tested with lightweight fakes.
//
// Next blocks until the next record is available, ctx is cancelled, or the
// source is exhausted. It must honour ctx cancellation (including a deadline
// derived from ctx by Forwarder for batch-age enforcement). To signal natural
// exhaustion, return [tail.ErrSourceExhausted].
type RecordSource interface {
	Next(ctx context.Context) (Record, error)
	Commit(ctx context.Context, pos Position) error
	Done() <-chan struct{}
}

// Sink accepts a decoded batch and delivers it to an external system.
// Return contracts:
//   - nil → commit the batch
//   - errors.Is(err, [ErrPermanent]) → non-retryable; Run returns the error
//   - any other error → retryable; Forwarder backs off and retries the same batch
type Sink[T any] interface {
	Send(ctx context.Context, batch []T) error
}

// SinkFunc adapts a plain function to the [Sink] interface.
type SinkFunc[T any] func(ctx context.Context, batch []T) error

// Send implements [Sink].
func (f SinkFunc[T]) Send(ctx context.Context, batch []T) error {
	return f(ctx, batch)
}

// Options configures a [Forwarder].
type Options[T any] struct {
	// Source, Decoder, and Sink are required.
	Source  RecordSource
	Decoder Decoder[T]
	Sink    Sink[T]

	// Batching — at least one must be set; flush triggers when ANY bound fires.
	MaxBatchRecords int           // flush when batch reaches this record count (0 = no limit)
	MaxBatchBytes   int           // flush when batch reaches this byte size (0 = no limit)
	MaxBatchAge     time.Duration // flush when oldest record in batch is this old (0 = no limit)

	// Retry configuration.
	InitialBackoff time.Duration // first retry sleep; default 100ms
	MaxBackoff     time.Duration // backoff ceiling; default 30s
	// BackoffJitter controls the fraction of the ceiling used for jitter.
	// Must be in [0, 1]. 0 = deterministic (always ceiling, the zero-value
	// default). 1 = full jitter (rand in [0, ceiling)). 0.2 = ±20% around
	// 0.8×ceiling. Negative or >1 is rejected by [New]. There is no implicit
	// default — set 0.2 explicitly for the conventional ±20% jitter.
	BackoffJitter float64
	// MaxAttempts is the maximum number of Sink.Send calls per batch before
	// Run gives up and returns an error wrapping [ErrMaxAttempts]. 0 (the
	// zero-value default) means no limit; retries continue until ctx
	// cancellation or a permanent sink error.
	MaxAttempts int

	Logger *slog.Logger

	// Hooks — all optional and nil-safe. Hooks are invoked synchronously
	// from the batching loop and must not block; offload slow work to a
	// goroutine or buffered channel if needed.
	// OnBatchSent fires after Sink.Send returns nil and Source.Commit completes.
	// records is the count of decoded items in the batch; bytes is the sum of
	// raw line lengths; pos is the last record's Position; latency is the
	// duration of the successful Sink.Send call (excludes batch fill time and
	// excludes any failed retry attempts — only the final, successful call).
	OnBatchSent    func(records int, bytes int, pos Position, latency time.Duration)
	OnSendError    func(err error, attempt int, willRetry bool)
	OnCommitted    func(pos Position)
	OnDecodeError  func(line []byte, pos Position, err error)
	OnBackoffSleep func(d time.Duration, attempt int)
}

// Forwarder reads lines from a [RecordSource], decodes them, batches them, and
// delivers them to a [Sink] with at-least-once semantics.
//
// Run is one-shot; create a new Forwarder to run again.
type Forwarder[T any] struct {
	opts Options[T]
}

// New validates opts and returns a new Forwarder.
func New[T any](opts Options[T]) (*Forwarder[T], error) {
	if opts.Source == nil {
		return nil, errors.New("forward: Options.Source must not be nil")
	}
	if opts.Decoder == nil {
		return nil, errors.New("forward: Options.Decoder must not be nil")
	}
	if opts.Sink == nil {
		return nil, errors.New("forward: Options.Sink must not be nil")
	}
	// SE-4: reject negative batch limits explicitly (the prior `== 0` check
	// silently accepted negatives, which then disabled flushing in the
	// consumer because every guard is `> 0`).
	if opts.MaxBatchRecords < 0 {
		return nil, fmt.Errorf("forward: MaxBatchRecords must not be negative, got %d", opts.MaxBatchRecords)
	}
	if opts.MaxBatchBytes < 0 {
		return nil, fmt.Errorf("forward: MaxBatchBytes must not be negative, got %d", opts.MaxBatchBytes)
	}
	if opts.MaxBatchAge < 0 {
		return nil, fmt.Errorf("forward: MaxBatchAge must not be negative, got %v", opts.MaxBatchAge)
	}
	if opts.MaxBatchRecords == 0 && opts.MaxBatchBytes == 0 && opts.MaxBatchAge == 0 {
		return nil, errors.New("forward: at least one of MaxBatchRecords, MaxBatchBytes, MaxBatchAge must be set")
	}
	if opts.BackoffJitter < 0 || opts.BackoffJitter > 1 {
		return nil, fmt.Errorf("forward: BackoffJitter must be in [0, 1], got %g", opts.BackoffJitter)
	}
	// SE-1: no implicit default for BackoffJitter — 0 means deterministic.
	if opts.MaxAttempts < 0 {
		return nil, fmt.Errorf("forward: MaxAttempts must not be negative, got %d", opts.MaxAttempts)
	}
	if opts.InitialBackoff <= 0 {
		opts.InitialBackoff = 100 * time.Millisecond
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Forwarder[T]{opts: opts}, nil
}

// Run reads from Source until it is exhausted or ctx is cancelled.
// It returns nil when Source signals exhaustion ([tail.ErrSourceExhausted]
// from Next, or [RecordSource.Done] closing), ctx.Err() on cancellation,
// or a wrapped [ErrPermanent] on a non-retryable Sink failure.
//
// MaxBatchAge is enforced by giving each Source.Next call a derived
// deadline of (batchStart + MaxBatchAge) when a non-empty batch is in
// flight; on context.DeadlineExceeded the batch is flushed.
func (f *Forwarder[T]) Run(ctx context.Context) error {
	// Inner ctx that cancels on parent ctx OR Source.Done() closure. This
	// catches the case where a 3rd-party RecordSource signals exhaustion via
	// Done() but its Next keeps returning records (or blocks). Defers run
	// LIFO; the wait must run AFTER runCancel so the watcher always exits.
	runCtx, runCancel := context.WithCancel(ctx)
	doneWatcherDone := make(chan struct{})
	go func() {
		defer close(doneWatcherDone)
		select {
		case <-runCtx.Done():
		case <-f.opts.Source.Done():
			runCancel()
		}
	}()
	defer func() {
		runCancel()
		<-doneWatcherDone
	}()

	var (
		batch        []T
		batchBytes   int
		batchLastPos Position
		batchStart   time.Time
	)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		// sendWithRetry uses the parent ctx, not runCtx: Source.Done()
		// signals "no more new records" — the in-flight batch should still
		// be delivered. Only parent-ctx cancellation aborts retries.
		err := f.sendWithRetry(ctx, batch, batchBytes, batchLastPos)
		batch = batch[:0]
		batchBytes = 0
		batchStart = time.Time{}
		return err
	}

	for {
		// Per-Next deadline: only when a batch is in flight and an age cap
		// is configured. The deadline is runCtx if neither.
		nextCtx := runCtx
		var cancel context.CancelFunc
		if len(batch) > 0 && f.opts.MaxBatchAge > 0 {
			nextCtx, cancel = context.WithDeadline(runCtx, batchStart.Add(f.opts.MaxBatchAge))
		}
		rec, err := f.opts.Source.Next(nextCtx)
		if cancel != nil {
			cancel()
		}

		if err != nil {
			// Parent ctx canceled: return its err.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// runCtx canceled but parent fine → Source.Done() fired. Treat
			// as normal exhaustion.
			if runCtx.Err() != nil {
				if ferr := flush(); ferr != nil {
					return ferr
				}
				return nil
			}
			if errors.Is(err, context.DeadlineExceeded) {
				if ferr := flush(); ferr != nil {
					return ferr
				}
				continue
			}
			if errors.Is(err, tail.ErrSourceExhausted) {
				if ferr := flush(); ferr != nil {
					return ferr
				}
				return nil
			}
			return err
		}

		val, derr := f.opts.Decoder(rec.Line)
		if derr != nil {
			if f.opts.OnDecodeError != nil {
				f.opts.OnDecodeError(rec.Line, rec.Pos, derr)
			}
			// SE-9: a Decoder may wrap ErrPermanent to abort Run on
			// schema breakage rather than skip-and-advance. The cursor
			// must NOT advance past the rejected record (no
			// batchLastPos update before the early return).
			if errors.Is(derr, ErrPermanent) {
				return fmt.Errorf("forward: permanent decoder error: %w", derr)
			}
			batchLastPos = rec.Pos
			continue
		}

		if len(batch) == 0 {
			batchStart = time.Now()
		}
		batch = append(batch, val)
		batchBytes += len(rec.Line)
		batchLastPos = rec.Pos

		shouldFlush := (f.opts.MaxBatchRecords > 0 && len(batch) >= f.opts.MaxBatchRecords) ||
			(f.opts.MaxBatchBytes > 0 && batchBytes >= f.opts.MaxBatchBytes)
		if shouldFlush {
			if ferr := flush(); ferr != nil {
				return ferr
			}
		}
	}
}

// sendWithRetry calls Sink.Send with exponential backoff. On success it commits
// the position and fires OnCommitted / OnBatchSent. It returns ctx.Err() if the
// context is cancelled during a backoff sleep.
func (f *Forwarder[T]) sendWithRetry(ctx context.Context, batch []T, bytes int, pos Position) error {
	for attempt := 0; ; attempt++ {
		sendStart := time.Now()
		err := f.opts.Sink.Send(ctx, batch)
		if err == nil {
			latency := time.Since(sendStart)
			if cerr := f.opts.Source.Commit(ctx, pos); cerr != nil {
				f.opts.Logger.Warn("forward: commit failed", "err", cerr, "offset", pos.Offset)
			}
			if f.opts.OnCommitted != nil {
				f.opts.OnCommitted(pos)
			}
			if f.opts.OnBatchSent != nil {
				f.opts.OnBatchSent(len(batch), bytes, pos, latency)
			}
			return nil
		}
		if errors.Is(err, ErrPermanent) {
			if f.opts.OnSendError != nil {
				f.opts.OnSendError(err, attempt, false)
			}
			return fmt.Errorf("forward: permanent sink error: %w", err)
		}
		// ID-4: bound the retry loop when MaxAttempts is set. attempt is
		// 0-based, so attempt+1 is the count of Send calls completed; once
		// it reaches MaxAttempts there are no more attempts to make.
		if f.opts.MaxAttempts > 0 && attempt+1 >= f.opts.MaxAttempts {
			if f.opts.OnSendError != nil {
				f.opts.OnSendError(err, attempt, false)
			}
			return fmt.Errorf("forward: gave up after %d attempts: %w", attempt+1, err)
		}
		if f.opts.OnSendError != nil {
			f.opts.OnSendError(err, attempt, true)
		}
		d := f.jitteredBackoff(attempt)
		f.opts.Logger.Debug("forward: sink error, retrying",
			"attempt", attempt, "latency_ms", d.Milliseconds(), "err", err)
		if f.opts.OnBackoffSleep != nil {
			f.opts.OnBackoffSleep(d, attempt)
		}
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}

// jitteredBackoff returns a jitter-scaled duration for the given attempt.
// The jitter factor is BackoffJitter (0..1):
//   - 0 → deterministic, always returns ceiling.
//   - 1 → full jitter, rand in [0, ceiling).
//   - 0.2 (default) → rand in [0.8·ceiling, ceiling).
func (f *Forwarder[T]) jitteredBackoff(attempt int) time.Duration {
	shift := attempt
	if shift > 62 {
		shift = 62
	}
	ceiling := f.opts.InitialBackoff << shift
	if ceiling <= 0 || ceiling > f.opts.MaxBackoff {
		ceiling = f.opts.MaxBackoff
	}
	if ceiling <= 0 {
		return 0
	}
	jitter := f.opts.BackoffJitter
	base := time.Duration(float64(ceiling) * (1 - jitter))
	jitterRange := ceiling - base
	if jitterRange <= 0 {
		return base
	}
	return base + time.Duration(rand.Int64N(int64(jitterRange)))
}

// WithSinkTimeout returns a middleware that wraps a [Sink] so each Send call
// has an independent per-call timeout derived from the parent context.
func WithSinkTimeout[T any](d time.Duration) func(Sink[T]) Sink[T] {
	return func(s Sink[T]) Sink[T] {
		return SinkFunc[T](func(ctx context.Context, batch []T) error {
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return s.Send(ctx, batch)
		})
	}
}
