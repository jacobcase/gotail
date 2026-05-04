# Cookbook: Standalone slog writer

Use the `watch` package directly to tail a file and emit structured log records
without going through a `Tailer`. Useful for lightweight integrations that only
need raw lines, not durable checkpointing.

```go
package main

import (
    "context"
    "io"
    "log/slog"
    "os"
    "time"

    "github.com/jacobcase/gotail/v2/watch"
)

func main() {
    logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

    w, err := watch.NewPolling(watch.Config{
        Path:     "/var/log/app.log",
        Interval: 500 * time.Millisecond,
        Whence:   io.SeekEnd, // only new lines
    })
    if err != nil {
        logger.Error("watcher", "err", err)
        os.Exit(1)
    }

    lr := watch.NewLineReader(w, watch.LineOptions{})
    defer lr.Close()

    ctx := context.Background()
    for {
        line, pos, err := lr.Next(ctx)
        if err != nil {
            if ctx.Err() != nil {
                break
            }
            logger.Error("read error", "err", err)
            continue
        }
        logger.Info("line", "text", string(line), "offset", pos.Offset)
    }
}
```

## Notes

- `watch.LineReader` owns its read buffer; copy `line` if you need to retain it
  past the next `Next` call.
- `watch.LineOptions.KeepNewline` includes the trailing `\n` in the returned
  slice if you need it.
- Switch to `watch.New` instead of `watch.NewPolling` for OS-native,
  sub-millisecond event latency. The fsnotify backend is compiled in by
  default; `watch.New` falls back to polling when unavailable. Drop the
  dependency with `-tags gotail_nofsnotify` if needed.
