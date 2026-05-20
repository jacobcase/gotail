# Cookbook: Backfill archived log files

Use `StopAtEOF: true` and `<-tr.Done()` to drain a historical log series
to its current end and then stop cleanly. Pair with a `Cursor` so a
restart picks up where the last run left off rather than re-reading the
whole archive.

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "log/slog"
    "os"
    "os/signal"
    "syscall"

    "github.com/jacobcase/gotail/v3/tail"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    cur, err := tail.NewFileCursor("/var/lib/myapp/backfill.cursor")
    if err != nil {
        slog.Error("cursor", "err", err)
        os.Exit(1)
    }

    tr, err := tail.New(ctx, tail.Options{
        // Lumberjack discovers all <stem>-<timestamp><ext> backups
        // alongside the active log and orders them oldest-first, so
        // the Tailer replays history in chronological order.
        Source:        tail.Lumberjack("/var/log/myapp.log"),
        Cursor:        cur,
        RequireCursor: true,
        StopAtEOF:     true,
        Logger:        slog.Default(),
        OnRotated: func(from, to tail.Position) {
            slog.Info("advancing to next file", "file", to.File)
        },
        OnDropped: func(droppedFiles int) {
            // Cursor named a file that no longer exists. With the default
            // FallbackOldest policy, the tailer resumed at the oldest
            // still-present backup. Surface the gap.
            slog.Warn("checkpoint dropped files", "count", droppedFiles)
        },
    })
    if err != nil {
        slog.Error("tailer", "err", err)
        os.Exit(1)
    }
    defer tr.Close()

    var count int
    for rec, err := range tr.Records(ctx) {
        if err != nil {
            if errors.Is(err, tail.ErrSourceExhausted) {
                break // backfill complete
            }
            slog.Error("read", "err", err)
            os.Exit(1)
        }
        process(rec.Line)
        // Commit after the line has been durably handled. With the default
        // SyncAlways mode this fsyncs every call; for very high-volume
        // backfills consider WithSyncMode(tail.SyncBackground).
        if err := tr.Commit(ctx, rec.Pos); err != nil {
            slog.Error("commit", "err", err, "offset", rec.Pos.Offset)
            os.Exit(1)
        }
        count++
    }

    <-tr.Done() // closed when StopAtEOF exhausts the source
    slog.Info("backfill complete", "lines", count)
}

func process(line []byte) {
    // ... insert into your downstream system ...
    fmt.Println(string(line))
}
```

## When to use this shape

- **Replaying an archive into a new system.** Point a backfill run at the
  full lumberjack/logrotate set, drain to EOF, exit. The cursor lets you
  resume across restarts.
- **One-shot ETL.** Read every line from a known-finite log set and stop.
  `StopAtEOF` is the signal; `<-tr.Done()` is the synchronisation point.
- **Catching up after an outage.** Combined with a live tailer, run a
  `StopAtEOF` pass first to drain backlog, then start a follow-mode
  tailer (no `StopAtEOF`) for ongoing data.

## Performance notes

- `forward.IdentityDecoderCopy` (or an explicit `copy`) is required if
  you hand `rec.Line` to a goroutine — the slice aliases the
  `LineReader`'s buffer and is overwritten on the next iteration.
- For very high-cardinality backfills where per-line `Commit` fsync
  becomes the bottleneck, switch the cursor to
  `tail.WithSyncMode(tail.SyncBackground)` or commit only every N lines.
  At-least-once still holds; the reduction is in fsync cost, not in
  durability guarantees against the *most-recently-committed* position.
