# Cookbook: Standalone watch usage

Use the `watch` package directly when you want raw lines from a single
file with no checkpoint persistence and no multi-file enumeration —
roughly the shape of `tail -F`. Most callers should reach for the L2
`tail` package instead; this cookbook is for cases where the extra
machinery is genuinely unwanted.

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "io"
    "log/slog"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/jacobcase/gotail/v2/watch"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    // watch.New auto-selects fsnotify (Linux/macOS/BSD) and falls back to
    // polling on Windows or when the gotail_nofsnotify build tag is set.
    w, err := watch.New(watch.Config{
        Path:     "/var/log/myapp.log",
        Interval: time.Second, // poll cadence; ignored when fsnotify is selected
        Whence:   io.SeekEnd,  // tail only new content (use io.SeekStart to read history)
    })
    if err != nil {
        slog.Error("watcher", "err", err)
        os.Exit(1)
    }

    lr := watch.NewLineReader(w, watch.LineOptions{
        // Defaults are 64 KiB read buffer and 1 MiB MaxLine. Override
        // only if you have measured a specific need.
        OnTruncated: func(at watch.Position) {
            slog.Warn("file truncated, resetting to offset 0", "prev_offset", at.Offset)
        },
    })
    defer lr.Close()

    for {
        line, pos, err := lr.Next(ctx)
        if err != nil {
            if errors.Is(err, ctx.Err()) || ctx.Err() != nil {
                return
            }
            if errors.Is(err, watch.ErrLineTooLong) {
                // Reader has already skipped to the next newline; keep going.
                slog.Warn("dropped over-long line", "offset", pos.Offset)
                continue
            }
            slog.Error("read error", "err", err)
            return
        }
        // line aliases LineReader's internal buffer — copy if you need to
        // retain it past the next Next call.
        fmt.Printf("%d\t%s\n", pos.Offset, line)
    }
}
```

## Notes

- **Buffer ownership.** `line` is valid only until the next call to
  `Next` or `Close`. Copy it before retaining.
- **Backend selection.** `watch.New` prefers fsnotify (sub-millisecond
  latency on Linux/macOS/BSD) and falls back to polling. Use
  `watch.NewPolling` directly to force polling on platforms that support
  fsnotify; use `watch.NewFsnotify` directly to surface
  `ErrUnsupported` rather than fall back.
- **`KeepNewline`.** Setting `LineOptions.KeepNewline = true` includes
  the trailing `\n` in returned slices. Useful when piping to writers
  that expect newline-terminated frames.
- **No persistence.** Restarting this program reopens the file at
  `Whence`; there is no resume across restarts. Use the `tail` package
  with a `Cursor` if you need durability — see the
  [HTTPS forwarder cookbook](https-forwarder.md) for an end-to-end
  example.
