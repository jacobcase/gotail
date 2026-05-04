# Cookbook: Backfill archived log files

Use `StopAtEOF: true` and `<-tr.Done()` to drain a historical log series and
then stop cleanly.

```go
package main

import (
    "context"
    "fmt"
    "log/slog"
    "os"

    "github.com/jacobcase/gotail/v2/tail"
)

func main() {
    // Lumberjack discovers all rotated files alongside the active log and
    // sorts them oldest-first so the Tailer replays history in order.
    tr, err := tail.New(tail.Options{
        Source:    tail.Lumberjack("/var/log/app.log"),
        StopAtEOF: true,
        Logger:    slog.Default(),
        OnRotated: func(from, to tail.Position) {
            slog.Info("advancing to next file", "file", to.File)
        },
    })
    if err != nil {
        slog.Error("tailer", "err", err)
        os.Exit(1)
    }
    defer tr.Close()

    ctx := context.Background()
    var count int
    for rec, err := range tr.Records(ctx) {
        if err != nil {
            break // ErrSourceExhausted or ctx cancel
        }
        count++
        fmt.Println(string(rec.Line))
    }

    // tr.Done() is closed when StopAtEOF exhausts the source.
    <-tr.Done()
    slog.Info("backfill complete", "lines", count)
}
```

## Resumable backfill

Add a `FileCursor` so a restart picks up where it left off:

```go
cur, _ := tail.NewFileCursor("/var/run/backfill.cursor")

tr, err := tail.New(tail.Options{
    Source:    tail.Lumberjack("/var/log/app.log"),
    Cursor:    cur,
    StopAtEOF: true,
})

for rec, err := range tr.Records(ctx) {
    if err != nil { break }
    process(rec.Line)
    tr.Commit(ctx, rec.Pos)
}
```

If the process is interrupted, the next run resumes from the last committed
position rather than starting from the beginning.
