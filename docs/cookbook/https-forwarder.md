# Cookbook: HTTPS forwarder with mTLS

A complete example: lumberjack log writer → `tail.Lumberjack` source →
`forward.Forwarder` → mTLS HTTPS sink.

```go
package main

import (
    "context"
    "crypto/tls"
    "crypto/x509"
    "encoding/json"
    "fmt"
    "log/slog"
    "net/http"
    "os"
    "time"

    "github.com/jacobcase/gotail/v2/forward"
    "github.com/jacobcase/gotail/v2/tail"
)

func main() {
    ctx := context.Background()

    // ── TLS config ──────────────────────────────────────────────────────
    cert, err := tls.LoadX509KeyPair("client.crt", "client.key")
    if err != nil {
        slog.Error("load cert", "err", err)
        os.Exit(1)
    }
    caCert, _ := os.ReadFile("ca.crt")
    pool := x509.NewCertPool()
    pool.AppendCertsFromPEM(caCert)
    tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool}

    client := &http.Client{
        Transport: &http.Transport{TLSClientConfig: tlsCfg},
        Timeout:   10 * time.Second,
    }

    // ── Tailer ──────────────────────────────────────────────────────────
    cur, err := tail.NewFileCursor("/var/run/app.cursor",
        tail.WithDirSync(true),
    )
    if err != nil {
        slog.Error("cursor", "err", err)
        os.Exit(1)
    }

    tr, err := tail.New(ctx, tail.Options{
        Source: tail.Lumberjack("/var/log/app.log"),
        Cursor: cur,
        Logger: slog.Default(),
        OnRotated: func(from, to tail.Position) {
            slog.Info("rotated", "from", from.File, "to", to.File)
        },
    })
    if err != nil {
        slog.Error("tailer", "err", err)
        os.Exit(1)
    }
    defer tr.Close()

    // ── Sink ────────────────────────────────────────────────────────────
    sink := forward.SinkFunc[[]byte](func(ctx context.Context, batch [][]byte) error {
        b, _ := json.Marshal(batch)
        req, _ := http.NewRequestWithContext(ctx, "POST",
            "https://ingest.example.com/logs", nil)
        req.Body = http.NoBody // set properly in production
        _ = b
        resp, err := client.Do(req)
        if err != nil {
            return err // retryable
        }
        defer resp.Body.Close()
        if resp.StatusCode == 401 || resp.StatusCode == 403 {
            return fmt.Errorf("auth error %d: %w", resp.StatusCode, forward.ErrPermanent)
        }
        if resp.StatusCode >= 500 {
            return fmt.Errorf("server error %d", resp.StatusCode) // retryable
        }
        return nil
    })

    // ── Forwarder ───────────────────────────────────────────────────────
    fwd, err := forward.New(forward.Options[[]byte]{
        Source:          tr,
        Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
        Sink:            forward.WithSinkTimeout[[]byte](8 * time.Second)(sink),
        MaxBatchRecords: 500,
        MaxBatchBytes:   1 << 20, // 1 MiB
        MaxBatchAge:     5 * time.Second,
        InitialBackoff:  200 * time.Millisecond,
        MaxBackoff:      60 * time.Second,
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

    if err := fwd.Run(ctx); err != nil {
        slog.Error("forwarder stopped", "err", err)
        os.Exit(1)
    }
}
```
