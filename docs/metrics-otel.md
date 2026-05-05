# Metrics: OpenTelemetry

Wire gotail hooks to OpenTelemetry counters. No gotail dependency on
OTel — all wiring is in your application code. The hook signatures
(`OnRotated`, `OnTruncated`, `OnCheckpoint`, `OnBatchSent`,
`OnSendError`, `OnDecodeError`) are part of the public API in
`tail.Options` and `forward.Options[T]`.

```go
import (
    "context"
    "time"

    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/metric"
    "github.com/jacobcase/gotail/v2/tail"
    "github.com/jacobcase/gotail/v2/forward"
)

type Metrics struct {
    linesProcessed metric.Int64Counter
    bytesShipped   metric.Int64Counter
    rotations      metric.Int64Counter
    truncations    metric.Int64Counter
    checkpoints    metric.Int64Counter
    batchesSent    metric.Int64Counter
    sendErrors     metric.Int64Counter
    decodeErrors   metric.Int64Counter
    sendLatency    metric.Float64Histogram
}

func NewMetrics() (*Metrics, error) {
    meter := otel.Meter("gotail")
    m := &Metrics{}
    var err error

    if m.linesProcessed, err = meter.Int64Counter("gotail.lines_processed",
        metric.WithDescription("Total lines read")); err != nil {
        return nil, err
    }
    if m.bytesShipped, err = meter.Int64Counter("gotail.bytes_shipped",
        metric.WithDescription("Bytes (sum of raw line lengths) delivered to sink")); err != nil {
        return nil, err
    }
    if m.rotations, err = meter.Int64Counter("gotail.rotations",
        metric.WithDescription("Log rotation events")); err != nil {
        return nil, err
    }
    if m.truncations, err = meter.Int64Counter("gotail.truncations",
        metric.WithDescription("Truncation events")); err != nil {
        return nil, err
    }
    if m.checkpoints, err = meter.Int64Counter("gotail.checkpoints",
        metric.WithDescription("Number of checkpoint writes")); err != nil {
        return nil, err
    }
    if m.batchesSent, err = meter.Int64Counter("gotail.batches_sent",
        metric.WithDescription("Batches delivered to sink")); err != nil {
        return nil, err
    }
    if m.sendErrors, err = meter.Int64Counter("gotail.send_errors",
        metric.WithDescription("Sink send errors")); err != nil {
        return nil, err
    }
    if m.decodeErrors, err = meter.Int64Counter("gotail.decode_errors",
        metric.WithDescription("Lines skipped due to decode errors")); err != nil {
        return nil, err
    }
    if m.sendLatency, err = meter.Float64Histogram("gotail.send_latency_seconds",
        metric.WithDescription("Latency of successful Sink.Send calls")); err != nil {
        return nil, err
    }
    return m, nil
}

func (m *Metrics) TailerOpts(base tail.Options) tail.Options {
    ctx := context.Background()
    base.OnRotated = func(_, _ tail.Position) { m.rotations.Add(ctx, 1) }
    base.OnTruncated = func(_ tail.Position) { m.truncations.Add(ctx, 1) }
    base.OnCheckpoint = func(_ tail.Checkpoint) { m.checkpoints.Add(ctx, 1) }
    return base
}

func (m *Metrics) ForwarderOpts[T any](base forward.Options[T]) forward.Options[T] {
    ctx := context.Background()
    base.OnBatchSent = func(records, bytes int, _ forward.Position, latency time.Duration) {
        m.batchesSent.Add(ctx, 1)
        m.linesProcessed.Add(ctx, int64(records))
        m.bytesShipped.Add(ctx, int64(bytes))
        m.sendLatency.Record(ctx, latency.Seconds())
    }
    base.OnSendError = func(_ error, _ int, _ bool) { m.sendErrors.Add(ctx, 1) }
    base.OnDecodeError = func(_ []byte, _ forward.Position, _ error) {
        m.decodeErrors.Add(ctx, 1)
    }
    return base
}
```
