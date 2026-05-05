# Cookbook: HTTPS forwarder with mTLS

A complete example: lumberjack-rotated log file → `tail.Lumberjack`
source → `forward.Forwarder` → mTLS HTTPS sink. The shape generalises to
any batched, retried log shipper.

```go
package main

import (
    "bytes"
    "context"
    "crypto/tls"
    "crypto/x509"
    "encoding/json"
    "fmt"
    "log/slog"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/jacobcase/gotail/v2/forward"
    "github.com/jacobcase/gotail/v2/tail"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    // ── TLS config ──────────────────────────────────────────────────────
    cert, err := tls.LoadX509KeyPair("client.crt", "client.key")
    if err != nil {
        slog.Error("load cert", "err", err)
        os.Exit(1)
    }
    caPEM, err := os.ReadFile("ca.crt")
    if err != nil {
        slog.Error("load ca", "err", err)
        os.Exit(1)
    }
    pool := x509.NewCertPool()
    if !pool.AppendCertsFromPEM(caPEM) {
        slog.Error("ca.crt contained no usable certificates")
        os.Exit(1)
    }
    client := &http.Client{
        Transport: &http.Transport{
            TLSClientConfig: &tls.Config{
                Certificates: []tls.Certificate{cert},
                RootCAs:      pool,
                MinVersion:   tls.VersionTLS12,
            },
        },
        Timeout: 10 * time.Second,
    }

    // ── Cursor (atomic, fsynced) and Tailer ─────────────────────────────
    // Cursor goes in /var/lib (persistent state). The lock file goes in
    // /var/run (tmpfs) — it only needs to exist while the process is alive,
    // and using a tmpfs path means a stale lock can't survive a hard reboot.
    cur, err := tail.NewFileCursor("/var/lib/myapp/cursor.json",
        tail.WithDirSync(true),
        tail.WithFlock("/var/run/myapp/cursor.lock"),
    )
    if err != nil {
        slog.Error("cursor", "err", err)
        os.Exit(1)
    }

    tr, err := tail.New(ctx, tail.Options{
        Source: tail.Lumberjack("/var/log/myapp.log",
            tail.WithLumberjackSkippedHook(func(path, reason string) {
                // A compressed backup means the cursor may be aging out
                // of reach. Surface it loudly.
                slog.Warn("lumberjack skipped backup", "path", path, "reason", reason)
            }),
        ),
        Cursor:        cur,
        RequireCursor: true,
        Logger:        slog.Default(),
        OnRotated: func(from, to tail.Position) {
            slog.Info("rotated", "from", from.File, "to", to.File)
        },
        OnTruncated: func(at tail.Position) {
            // Fires on real truncation events. If you see this without an
            // intentional reset, you may be tailing a copytruncate'd file —
            // see README "Log rotation guidance".
            slog.Warn("file truncated", "offset", at.Offset)
        },
    })
    if err != nil {
        slog.Error("tailer", "err", err)
        os.Exit(1)
    }
    defer tr.Close()

    // ── Sink: POST a JSON-array body to the ingest endpoint ─────────────
    sink := forward.SinkFunc[[]byte](func(ctx context.Context, batch [][]byte) error {
        body, err := json.Marshal(batch)
        if err != nil {
            // Marshal failure on []byte is essentially impossible; treat
            // as permanent so we don't loop forever on a corrupt batch.
            return fmt.Errorf("marshal batch: %w: %w", err, forward.ErrPermanent)
        }
        req, err := http.NewRequestWithContext(ctx, http.MethodPost,
            "https://ingest.example.com/logs", bytes.NewReader(body))
        if err != nil {
            return fmt.Errorf("build request: %w: %w", err, forward.ErrPermanent)
        }
        req.Header.Set("Content-Type", "application/json")
        resp, err := client.Do(req)
        if err != nil {
            return err // network error — retryable
        }
        defer resp.Body.Close()

        switch {
        case resp.StatusCode >= 200 && resp.StatusCode < 300:
            return nil
        case resp.StatusCode == http.StatusUnauthorized,
            resp.StatusCode == http.StatusForbidden:
            // Auth failure won't fix itself with retries.
            return fmt.Errorf("auth error %d: %w", resp.StatusCode, forward.ErrPermanent)
        case resp.StatusCode == http.StatusBadRequest:
            // Server rejected the payload; retrying sends the same bytes.
            return fmt.Errorf("bad request %d: %w", resp.StatusCode, forward.ErrPermanent)
        default:
            return fmt.Errorf("server error %d", resp.StatusCode) // retryable
        }
    })

    // ── Forwarder: batched delivery with bounded backoff ────────────────
    fwd, err := forward.New(forward.Options[[]byte]{
        Source: tr,
        // IdentityDecoderCopy is the safe default: each value owns its
        // bytes and can outlive Source.Next without worrying about
        // LineReader buffer reuse.
        Decoder:         forward.IdentityDecoderCopy,
        Sink:            forward.WithSinkTimeout[[]byte](8 * time.Second)(sink),
        MaxBatchRecords: 500,
        MaxBatchBytes:   1 << 20,           // 1 MiB
        MaxBatchAge:     5 * time.Second,
        InitialBackoff:  200 * time.Millisecond,
        MaxBackoff:      60 * time.Second,
        BackoffJitter:   0.2, // ±20% — avoid thundering herd across instances
        Logger:          slog.Default(),
        OnBatchSent: func(records, bytes int, pos tail.Position, latency time.Duration) {
            slog.Info("batch sent",
                "records", records, "bytes", bytes,
                "offset", pos.Offset, "latency_ms", latency.Milliseconds())
        },
        OnSendError: func(err error, attempt int, willRetry bool) {
            slog.Warn("send error", "attempt", attempt, "willRetry", willRetry, "err", err)
        },
    })
    if err != nil {
        slog.Error("forwarder", "err", err)
        os.Exit(1)
    }

    if err := fwd.Run(ctx); err != nil && ctx.Err() == nil {
        slog.Error("forwarder stopped", "err", err)
        os.Exit(1)
    }
}
```

## What to wire next

- **Metrics.** Pair the forwarder with the
  [Prometheus](../metrics-prometheus.md) or
  [OpenTelemetry](../metrics-otel.md) recipes; both wire the same hooks
  shown above.
- **Backpressure visibility.** `tr.Stats()` gives a point-in-time
  snapshot (lines, bytes, rotations, errors) safe to scrape from any
  goroutine, including after `Close`. Hook it into a `/metrics` endpoint
  or a periodic structured log.
- **Graceful shutdown.** Replace `defer tr.Close()` with
  `defer tr.CloseWithFlush(shutdownCtx)` if you want the cursor to
  persist the most-recently-yielded position before the process exits.
