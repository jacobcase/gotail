package forward

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jacobcase/gotail/v2/tail"
)

// Position is an alias for [tail.Position] so callers do not need to import
// the tail package just to handle positions.
type Position = tail.Position

// RecordSource is the read side of a [tail.Tailer]. [*tail.Tailer] satisfies
// this interface, enabling Forwarder to be tested with lightweight fakes.
type RecordSource interface {
	Records(ctx context.Context) iter.Seq2[tail.Record, error]
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

	Logger *slog.Logger

	// Hooks — all optional and nil-safe.
	OnBatchSent    func(n int, pos Position)
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
	if opts.MaxBatchRecords == 0 && opts.MaxBatchBytes == 0 && opts.MaxBatchAge == 0 {
		return nil, errors.New("forward: at least one of MaxBatchRecords, MaxBatchBytes, MaxBatchAge must be set")
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
// It returns nil when Source signals exhaustion (StopAtEOF), ctx.Err() on
// cancellation, or a wrapped [ErrPermanent] on a non-retryable Sink failure.
func (f *Forwarder[T]) Run(ctx context.Context) error {
	type recItem struct {
		rec tail.Record
		err error
	}

	// Derive a child context so that any return from Run cancels the feeder
	// goroutine; wg ensures Run does not return until the feeder has fully
	// exited. Defers run LIFO: cancel() (registered last) fires first,
	// freeing the feeder, then wg.Wait() blocks for its exit.
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	// Feed records into a buffered channel so the batch-age timer can interrupt
	// the wait for the next record.
	recCh := make(chan recItem, 16)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(recCh)
		for rec, err := range f.opts.Source.Records(ctx) {
			select {
			case recCh <- recItem{rec, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var (
		batch       []T
		batchBytes  int
		batchLastPos Position
		ageTimer    *time.Timer
		ageTimerC   <-chan time.Time
	)

	startAge := func() {
		if f.opts.MaxBatchAge > 0 && ageTimer == nil {
			ageTimer = time.NewTimer(f.opts.MaxBatchAge)
			ageTimerC = ageTimer.C
		}
	}
	stopAge := func() {
		if ageTimer != nil {
			ageTimer.Stop()
			ageTimer = nil
			ageTimerC = nil
		}
	}
	defer stopAge()

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := f.sendWithRetry(ctx, batch, batchLastPos)
		batch = batch[:0]
		batchBytes = 0
		stopAge()
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ageTimerC:
			if err := flush(); err != nil {
				return err
			}

		case item, ok := <-recCh:
			if !ok {
				// Goroutine exited: either ctx was cancelled or source was drained.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return flush()
			}
			if item.err != nil {
				if errors.Is(item.err, tail.ErrSourceExhausted) {
					if err := flush(); err != nil {
						return err
					}
					return nil
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return item.err
			}

			val, err := f.opts.Decoder(item.rec.Line)
			if err != nil {
				if f.opts.OnDecodeError != nil {
					f.opts.OnDecodeError(item.rec.Line, item.rec.Pos, err)
				}
				// Advance position past this line even though we skip it.
				batchLastPos = item.rec.Pos
				continue
			}

			batch = append(batch, val)
			batchBytes += len(item.rec.Line)
			batchLastPos = item.rec.Pos
			startAge()

			shouldFlush := (f.opts.MaxBatchRecords > 0 && len(batch) >= f.opts.MaxBatchRecords) ||
				(f.opts.MaxBatchBytes > 0 && batchBytes >= f.opts.MaxBatchBytes)
			if shouldFlush {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
}

// sendWithRetry calls Sink.Send with exponential backoff. On success it commits
// the position and fires OnCommitted / OnBatchSent. It returns ctx.Err() if the
// context is cancelled during a backoff sleep.
func (f *Forwarder[T]) sendWithRetry(ctx context.Context, batch []T, pos Position) error {
	for attempt := 0; ; attempt++ {
		err := f.opts.Sink.Send(ctx, batch)
		if err == nil {
			if cerr := f.opts.Source.Commit(ctx, pos); cerr != nil {
				f.opts.Logger.Warn("forward: commit failed", "err", cerr, "offset", pos.Offset)
			}
			if f.opts.OnCommitted != nil {
				f.opts.OnCommitted(pos)
			}
			if f.opts.OnBatchSent != nil {
				f.opts.OnBatchSent(len(batch), pos)
			}
			return nil
		}
		if errors.Is(err, ErrPermanent) {
			if f.opts.OnSendError != nil {
				f.opts.OnSendError(err, attempt, false)
			}
			return fmt.Errorf("forward: permanent sink error: %w", err)
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
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// jitteredBackoff returns a full-jitter duration for the given attempt:
// rand(0, min(MaxBackoff, InitialBackoff * 2^attempt)).
func (f *Forwarder[T]) jitteredBackoff(attempt int) time.Duration {
	cap := f.opts.InitialBackoff
	for i := 0; i < attempt; i++ {
		cap *= 2
		if cap <= 0 || cap > f.opts.MaxBackoff {
			cap = f.opts.MaxBackoff
			break
		}
	}
	if cap <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(cap)))
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
