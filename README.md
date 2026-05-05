# gotail v2

[![Go Reference](https://pkg.go.dev/badge/github.com/jacobcase/gotail/v2.svg)](https://pkg.go.dev/github.com/jacobcase/gotail/v2)
[![CI](https://github.com/jacobcase/gotail/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/jacobcase/gotail/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/jacobcase/gotail/branch/main/graph/badge.svg)](https://codecov.io/gh/jacobcase/gotail)

A reliable, production-grade file tailing library for Go.

## Overview

gotail v2 is a layered library for tailing log files across rotation, truncation,
and restarts. It is designed for high-throughput log pipelines (edge proxy logs,
application journals) where durability and correctness matter more than minimal
dependencies.

```
┌──────────────────────────────────────────────────────────┐
│  forward  (L3)  batched at-least-once shipper to a Sink  │
├──────────────────────────────────────────────────────────┤
│  tail     (L2)  durable cursor, multi-file log series    │
├──────────────────────────────────────────────────────────┤
│  watch    (L1)  file-as-stream: events, line framing     │
└──────────────────────────────────────────────────────────┘
```

Use only the layer you need. Most callers use `tail` alone.

## Installation

```
go get github.com/jacobcase/gotail/v2
```

## Package layout

| Package | Layer | Purpose |
|---------|-------|---------|
| `watch` | L1 | `Watcher` (polling or fsnotify), `LineReader`, `Event` |
| `tail`  | L2 | `Tailer`, `Source`, `Cursor`, durable checkpoints |
| `forward` | L3 | Generic `Forwarder[T]`, `Sink[T]`, `Decoder[T]` |
| `tailtest` | — | `MemorySource` for unit tests |
| `forwardtest` | — | `RecordingSink[T]`, `FailingSink[T]` for unit tests |
| `cmd/gotail` | — | CLI binary (`tail -f` replacement) |

## Quick start — live tail

```go
tr, err := tail.New(ctx, tail.Options{
    Source:   tail.SingleFile("/var/log/app.log"),
    Interval: time.Second,
    Whence:   io.SeekEnd, // start at current end
})
if err != nil { ... }
defer tr.Close()

for rec, err := range tr.Records(ctx) {
    if err != nil { break }
    fmt.Println(string(rec.Line))
}
```

## Quick start — durable checkpoint (survive restarts)

```go
cur, err := tail.NewFileCursor("/var/run/app.cursor")
if err != nil { ... }

tr, err := tail.New(ctx, tail.Options{
    Source: tail.SingleFile("/var/log/app.log"),
    Cursor: cur,
})
if err != nil { ... }
defer tr.Close()

for rec, err := range tr.Records(ctx) {
    if err != nil { break }
    process(rec.Line)
    tr.Commit(ctx, rec.Pos) // write checkpoint after each line
}
```

## Quick start — multi-file log series (lumberjack rotation)

```go
tr, err := tail.New(ctx, tail.Options{
    Source:    tail.Lumberjack("/var/log/app.log"),
    StopAtEOF: true, // drain archived files then return nil from Run
})
if err != nil { ... }
defer tr.Close()

for rec, err := range tr.Records(ctx) {
    if err != nil { break }
    backfill(rec.Line)
}
<-tr.Done()
```

## Quick start — forward to HTTP sink

```go
tr, err := tail.New(ctx, tail.Options{Source: tail.SingleFile("/var/log/app.log")})
if err != nil { ... }
defer tr.Close()

fwd, err := forward.New(forward.Options[[]byte]{
    Source:          tr,
    Decoder:         forward.Decoder[[]byte](forward.IdentityDecoderCopy),
    Sink:            mySink,
    MaxBatchRecords: 500,
    MaxBatchAge:     5 * time.Second,
})
if err != nil { ... }

if err := fwd.Run(ctx); err != nil { ... }
```

## v1 → v2 migration

| v1 concept | v2 equivalent |
|-----------|---------------|
| `gotail.NewPoller(path)` → `io.ReadCloser` | `tail.New(ctx, opts)` → `*Tailer` |
| Manual rotation (v1 did not survive rename) | Automatic; `OnRotated` hook available |
| No checkpoints | `tail.FileCursor` / `tail.MemoryCursor` |
| No line framing | `watch.LineReader` (used internally by Tailer) |
| No multi-file backfill | `tail.Lumberjack` / `tail.Glob` |
| No batched forwarding | `forward.Forwarder[T]` |

## Platform support

| Platform | Polling | fsnotify (default) |
|----------|---------|--------------------|
| Linux | ✓ | ✓ (inotify) |
| macOS | ✓ | ✓ (kqueue) |
| FreeBSD / OpenBSD / NetBSD | ✓ | ✓ (kqueue) |
| Windows | ✓ | — (polling only) |

The fsnotify backend is compiled in by default and selected automatically
when supported, with transparent fallback to polling on Windows or when
events are unavailable. To force polling at runtime, set
`tail.Options{ForcePolling: true}`. To drop the `fsnotify` dependency
entirely (e.g., for distroless / minimal builds), opt out with the
`gotail_nofsnotify` build tag:

```
go build -tags gotail_nofsnotify ./...
```

## CLI

```
go install github.com/jacobcase/gotail/v2/cmd/gotail@latest
gotail /var/log/app.log              # tail from end, follow
gotail -start /var/log/app.log       # tail from beginning
gotail -stop /var/log/app.log        # drain to EOF and exit
```

## Development

The library is concurrency-heavy. Always verify changes under the race
detector, and exercise both build configurations so the fsnotify and
polling-only paths stay green:

```
go test -race ./...
go test -race -tags gotail_nofsnotify ./...
```

To generate a coverage profile (matches what CI uploads to Codecov):

```
go test -race -covermode=atomic -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html   # browseable report
go tool cover -func=coverage.out                     # per-function summary
```

CI runs both matrices on Linux, macOS, and Windows; goroutine leaks are
caught at suite teardown via [goleak](https://github.com/uber-go/goleak).
A separate `coverage` job uploads `coverage.out` as a build artifact and
to [Codecov](https://codecov.io/gh/jacobcase/gotail).

## Docs

- [Metrics: Prometheus](docs/metrics-prometheus.md)
- [Metrics: OpenTelemetry](docs/metrics-otel.md)
- [Cookbook: HTTPS forwarder](docs/cookbook/https-forwarder.md)
- [Cookbook: Backfill archived files](docs/cookbook/backfill.md)
- [Cookbook: Standalone slog writer](docs/cookbook/standalone.md)
