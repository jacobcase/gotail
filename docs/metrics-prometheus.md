# Metrics: Prometheus

Wire gotail hooks to Prometheus counters and histograms. No gotail dependency
on the Prometheus client — all wiring is in your application code.

```go
import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
    "github.com/jacobcase/gotail/v2/tail"
    "github.com/jacobcase/gotail/v2/forward"
)

var (
    linesProcessed = promauto.NewCounter(prometheus.CounterOpts{
        Name: "gotail_lines_processed_total",
        Help: "Total lines read from the log source.",
    })
    rotations = promauto.NewCounter(prometheus.CounterOpts{
        Name: "gotail_rotations_total",
        Help: "Number of log rotation events.",
    })
    truncations = promauto.NewCounter(prometheus.CounterOpts{
        Name: "gotail_truncations_total",
        Help: "Number of truncation events.",
    })
    checkpoints = promauto.NewCounter(prometheus.CounterOpts{
        Name: "gotail_checkpoints_total",
        Help: "Number of checkpoint writes.",
    })
    batchesSent = promauto.NewCounter(prometheus.CounterOpts{
        Name: "gotail_batches_sent_total",
        Help: "Total batches delivered to sink.",
    })
    sendErrors = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "gotail_send_errors_total",
        Help: "Sink send errors by retry outcome.",
    }, []string{"will_retry"})
    decodeErrors = promauto.NewCounter(prometheus.CounterOpts{
        Name: "gotail_decode_errors_total",
        Help: "Lines skipped due to decode errors.",
    })
)

// TailerOpts returns tail.Options with Prometheus hooks wired in.
func TailerOpts(base tail.Options) tail.Options {
    base.OnRotated = func(_, _ tail.Position) { rotations.Inc() }
    base.OnTruncated = func(_ tail.Position) { truncations.Inc() }
    base.OnCheckpoint = func(_ tail.Checkpoint) { checkpoints.Inc() }
    return base
}

// ForwarderOpts returns forward.Options with Prometheus hooks wired in.
func ForwarderOpts[T any](base forward.Options[T]) forward.Options[T] {
    innerBatchSent := base.OnBatchSent
    base.OnBatchSent = func(n int, pos forward.Position) {
        batchesSent.Inc()
        linesProcessed.Add(float64(n))
        if innerBatchSent != nil {
            innerBatchSent(n, pos)
        }
    }
    base.OnSendError = func(_ error, _ int, willRetry bool) {
        label := "false"
        if willRetry {
            label = "true"
        }
        sendErrors.WithLabelValues(label).Inc()
    }
    base.OnDecodeError = func(_ []byte, _ forward.Position, _ error) {
        decodeErrors.Inc()
    }
    return base
}
```
