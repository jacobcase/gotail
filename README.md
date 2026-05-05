# gotail v2

[![Go Reference](https://pkg.go.dev/badge/github.com/jacobcase/gotail/v2.svg)](https://pkg.go.dev/github.com/jacobcase/gotail/v2)
[![CI](https://github.com/jacobcase/gotail/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/jacobcase/gotail/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/jacobcase/gotail/branch/main/graph/badge.svg)](https://codecov.io/gh/jacobcase/gotail)

A reliable, production-grade file tailing library for Go.

## Overview

gotail v2 is a layered library for tailing log files across rotation,
truncation, and process restarts. It is built for high-throughput log
pipelines (edge proxy logs, application journals, audit shippers) where
durability and correctness matter.

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

- **L1 `watch`** — stat- or fsnotify-driven state machine plus a `LineReader`
  that frames newline-delimited lines on top of it. No persistence, no
  multi-file awareness.
- **L2 `tail`** — wraps L1 with a `Source` (file-set enumeration), a
  `Cursor` (atomic checkpoint persistence), and rotation/truncation
  handling. The default consumer surface.
- **L3 `forward`** — generic, batched, at-least-once shipper. Reads from a
  `RecordSource` (the canonical implementation is `*tail.Tailer`), decodes
  with a typed `Decoder[T]`, and delivers batches to a `Sink[T]` with
  exponential backoff.

## Installation

Add the library to your module:

```
go get github.com/jacobcase/gotail/v2
```

Install the bundled `gotail` CLI (a `tail -f` replacement that uses the
library):

```
go install github.com/jacobcase/gotail/v2/cmd/gotail@latest
```

The library has no required external dependencies beyond the Go standard
library; the `fsnotify` backend is on by default and contributes a single
direct dependency. See [Build settings](#build-settings) for how to drop
it.

## Getting started

A minimal, production-shaped live tail with a durable checkpoint and the
fsnotify backend (auto-fallback to polling on platforms or builds without
it):

```go
package main

import (
    "context"
    "fmt"
    "os/signal"
    "syscall"

    "github.com/jacobcase/gotail/v2/tail"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    cur, err := tail.NewFileCursor("/var/lib/myapp/cursor.json")
    if err != nil { panic(err) }

    tr, err := tail.New(ctx, tail.Options{
        Source:        tail.SingleFile("/var/log/myapp.log"),
        Cursor:        cur,
        RequireCursor: true, // fail fast if the cursor is misconfigured
    })
    if err != nil { panic(err) }
    defer tr.Close()

    for rec, err := range tr.Records(ctx) {
        if err != nil {
            return // ctx cancelled or source error; defer runs Close
        }
        fmt.Println(string(rec.Line))
        // Advance the cursor only after the line is durably handled.
        _ = tr.Commit(ctx, rec.Pos)
    }
}
```

Three knobs to know up front:

- **`Whence`** controls where the *first* file in the series opens when
  there is no checkpoint. `io.SeekStart` (the zero value) reads from the
  beginning; `io.SeekEnd` skips existing content and tails only new data.
  `SkipExisting: true` is a discoverable alias for `Whence: io.SeekEnd`.
  When a `Cursor` provides a resume point, `Whence` is ignored.
- **`Cursor`** is what makes the tail durable across restarts. Without one
  the Tailer behaves like L1: every restart starts at `Whence`. Set
  `RequireCursor: true` to refuse construction when `Cursor` is nil — a
  cheap guard against YAML mappers that forgot to wire it.
- **`Commit`** is explicit. Records are *never* auto-committed; commit
  *after* the line has been durably consumed (printed, written downstream,
  acknowledged by a sink). The default `SyncAlways` mode fsyncs every
  `Commit`; switch to `WithSyncMode(tail.SyncBackground)` if that becomes
  a bottleneck (see [Best practices](#best-practices)).

## Usage

### Live tail without a cursor

```go
tr, err := tail.New(ctx, tail.Options{
    Source:       tail.SingleFile("/var/log/app.log"),
    Whence:       io.SeekEnd, // tail only new lines
    Interval:     time.Second,
})
if err != nil { return err }
defer tr.Close()

for rec, err := range tr.Records(ctx) {
    if err != nil { return err }
    fmt.Println(string(rec.Line))
}
```

`rec.Line` is valid only until the next iteration step — copy it if you
need to retain the bytes.

### Multi-file series (lumberjack rotation)

`tail.Lumberjack` enumerates the active log plus all backup files named
`<stem>-YYYY-MM-DDTHH-MM-SS<ext>`, oldest-first. It is the recommended
shape for processes writing through
[`gopkg.in/natefinch/lumberjack.v2`](https://github.com/natefinch/lumberjack):

```go
tr, err := tail.New(ctx, tail.Options{
    Source: tail.Lumberjack("/var/log/app.log",
        tail.WithLumberjackSkippedHook(func(path, reason string) {
            slog.Warn("skipping rotated file", "path", path, "reason", reason)
        }),
    ),
    Cursor: cur,
})
```

The hook fires when a recognised backup cannot be exposed to the tailer —
the only reason today is `"compressed"` (lumberjack's `Compress: true`
mode produces `.gz` backups; gotail does not decompress on read).

### Multi-file series (logrotate)

`tail.Logrotate` handles logrotate's default numeric naming
(`app.log.1`, `app.log.2`, …). For `dateext`-style names use
`tail.Lumberjack` instead. For arbitrary patterns use `tail.Glob` —
note that lexicographic order does not match age order once you have ten
or more backups (`.10` sorts before `.2`); prefer `Logrotate` or
`Lumberjack` when the naming scheme matches.

### Backfill: drain archived files and stop

```go
tr, err := tail.New(ctx, tail.Options{
    Source:    tail.Lumberjack("/var/log/app.log"),
    Cursor:    cur,
    StopAtEOF: true,
})
if err != nil { return err }
defer tr.Close()

for rec, err := range tr.Records(ctx) {
    if errors.Is(err, tail.ErrSourceExhausted) { break }
    if err != nil { return err }
    process(rec.Line)
    tr.Commit(ctx, rec.Pos)
}
<-tr.Done() // closed when StopAtEOF exhausts the series
```

### Forward to an HTTP sink

```go
tr, err := tail.New(ctx, tail.Options{
    Source: tail.Lumberjack("/var/log/app.log"),
    Cursor: cur,
})
if err != nil { return err }
defer tr.Close()

fwd, err := forward.New(forward.Options[[]byte]{
    Source:          tr,
    Decoder:         forward.IdentityDecoderCopy, // safe to retain across batches
    Sink:            mySink,
    MaxBatchRecords: 500,
    MaxBatchBytes:   1 << 20, // 1 MiB
    MaxBatchAge:     5 * time.Second,
    BackoffJitter:   0.2, // ±20% jitter — avoid thundering-herd on retry
})
if err != nil { return err }

if err := fwd.Run(ctx); err != nil { return err }
```

Decoder choice:

- `forward.IdentityDecoder` returns the line as-is *without copying*. Only
  safe when each batch is fully consumed by `Sink.Send` before the next
  call to `Source.Next`. Saves an allocation per record.
- `forward.IdentityDecoderCopy` copies into a fresh slice; values are
  retainable across iterations. Use this unless you have measured the
  alloc cost as a bottleneck.
- `forward.JSONDecoder[T]()` parses each line as JSON into `T`.

The batched delivery loop has at-least-once semantics: a batch is only
committed (cursor advanced) after `Sink.Send` returns nil. Wrap a
non-retryable failure with `forward.ErrPermanent` to terminate `Run`;
everything else is retried with exponential backoff bounded by
`InitialBackoff`/`MaxBackoff` and jittered by `BackoffJitter`.

The full example with mTLS and rich metrics hooks lives in
[`docs/cookbook/https-forwarder.md`](docs/cookbook/https-forwarder.md).

## CLI

```
gotail /var/log/app.log              # tail from end, follow forever
gotail -start /var/log/app.log       # tail from beginning
gotail -stop /var/log/app.log        # drain to EOF and exit
```

Use the CLI for ad-hoc debugging or as a reference implementation. It
has no checkpoint, no metrics, and no forwarding; it is roughly 70 lines
of `cmd/gotail/main.go`.

## Build settings

The library is pure Go (no `cgo`) and cross-compiles cleanly to Linux,
macOS, FreeBSD, NetBSD, OpenBSD, and Windows on `amd64` and `arm64`.

### Build tags

| Tag | Effect |
|-----|--------|
| *(none, default)* | Compiles in the fsnotify backend; `watch.New` prefers fsnotify and falls back to polling on `ErrUnsupported`. |
| `gotail_nofsnotify` | Drops `github.com/fsnotify/fsnotify` from the module graph; `watch.NewFsnotify` always returns `ErrUnsupported`; `watch.New` returns the polling watcher. Use for distroless / minimal builds. |

```
go build -tags gotail_nofsnotify ./...
```

### Platform support

| Platform | Polling | fsnotify (default) | Stable inode |
|----------|---------|--------------------|--------------|
| Linux (any FS) | yes | yes (inotify) | yes (ext4, xfs, btrfs); see notes for tmpfs/overlay |
| macOS | yes | yes (kqueue) | yes |
| FreeBSD / OpenBSD / NetBSD | yes | yes (kqueue) | yes |
| Windows | yes | — | file index via `GetFileInformationByHandle`; unstable on ReFS |

Platform caveats:

- **Windows** has no fsnotify backend — `watch.New` automatically returns
  the polling watcher. Sub-second latency is achievable by lowering
  `Options.Interval`, at the cost of stat syscalls.
- **Windows ReFS, FUSE mounts, some network filesystems** do not expose a
  stable file identity. Set `Options.NoInodeCheck: true` to skip the
  identity check on resume and rotation; this trades robustness against
  rotation races for cross-platform predictability.
- **Symlink swap rotation** (`mv newlog activelog && ln -sf …`) is
  detected as a rotation event the same way as rename+create.
- **Atomic-rename durability** (`FileCursor.Save`) requires
  `WithDirSync(true)` on ext4/xfs/btrfs; this is the default. Some
  network filesystems silently no-op the directory fsync.

### `RequireCursor` and `AllowInodeMismatch`

Two opt-in safety flags that change `tail.New`'s behaviour:

- `RequireCursor: true` — refuses construction when `Cursor` is nil.
  Recommended for production.
- `AllowInodeMismatch: true` — when the cursor's path still exists but the
  inode has changed (rotation that reused the path while the consumer was
  down, or a copytruncate that replaced the file), fall back to the
  configured `OnMissingCheckpoint` policy instead of failing. Default is
  *false* (fail-safe): `New` returns `ErrInodeMismatch`. Flipping this
  on is appropriate only for trusted, single-tenant filesystems.

## Log rotation guidance

gotail is built around the **rename+create** rotation model used by
lumberjack, logrotate (the default), and most syslog daemons. Every other
strategy is a compromise.

### Recommended

- **`gopkg.in/natefinch/lumberjack.v2`** — in-process Go writer, atomic
  rename rotation, time-suffixed backup names. Pair with
  `tail.Lumberjack(activePath)`.
- **logrotate with `create`** (the default; produces `.1`, `.2`, …) —
  pair with `tail.Logrotate(activePath)`.
- **logrotate with `dateext`** (timestamp-suffixed backups) — pair with
  `tail.Lumberjack` if the timestamp format matches, otherwise
  `tail.Glob`.

For all three, gotail's race-aware drain semantics preserve trailing
bytes on the rotated-out file: when rotation is detected, the
`LineReader` continues reading its existing fd until EOF (the kernel
keeps the inode alive while we hold an fd) and only then opens the new
file. This is the v1 correctness property and survives unchanged.

### Avoid: `copytruncate`

logrotate's `copytruncate` directive **silently loses log lines** under
load and is incompatible with high-throughput tailing. The race is:

```
1. logrotate copies active.log → active.log.1     (in flight)
2. application appends lines L1, L2, L3            (during the copy)
3. logrotate truncates active.log to size 0        (lines may not be in the copy)
4. tail reader reaches the old position → EOF      (lines L1..L3 are gone)
```

gotail handles `copytruncate` defensively — it detects the size drop and
seeks back to offset 0 — but it cannot recover lines that were appended
to the original inode after the copy was taken and before the truncate.
**There is no software fix for this race; it is intrinsic to the
strategy.**

If you must use `copytruncate` (a constrained third-party process you
cannot reconfigure), expect:

- Up to one rotation interval of data loss per rotation, proportional to
  log velocity.
- `OnTruncated` hook fires; `Stats().Truncations` increments. Wire those
  up to alerting so the operational impact is visible.

### Avoid: in-place mid-file rewrites

gotail assumes append-only semantics on the active file. Rewrites in the
middle of the file (e.g. tools that "edit" log records) will be reported
as truncations and the reader will reset to offset 0.

### Recommended log rotation policy for high-throughput services

- Rotate by *size* (e.g. 100 MiB) rather than by time, so backups have
  predictable bounds.
- Keep enough backups on disk that an outage of one full retention window
  does not lose data — 7 backups at 100 MiB ≈ 700 MiB of buffer.
- Disable compression on the active rotation directory. Compressed `.gz`
  backups are *not* tailed by gotail; if a checkpoint resume targets a
  backup that has already been compressed and aged out of the source
  enumeration, the `OnMissingCheckpoint` policy decides what happens.
  Wire `WithLumberjackSkippedHook` (or `WithLogrotateSkippedHook`) to
  alerts so you see the gap before the cursor falls behind.

## Best practices

- **Always Commit explicitly.** Records are never auto-committed.
  Commit *after* the line has been durably handled. The cost of a
  too-late commit is replay (at-least-once); the cost of a too-early
  commit is data loss.
- **Use `RequireCursor: true` in production.** A nil cursor silently
  disables checkpointing and replays from `Whence` on every restart.
- **Use `BackoffJitter: 0.2`** (or higher) on `forward.Options` when
  multiple instances may retry at the same time. The zero default is
  deterministic for reproducibility in tests, not for production.
- **Pick `SyncMode` with intent.** `SyncAlways` (default) fsyncs every
  Commit and is the safest choice for durability — at most one
  uncommitted line is lost on power failure. `SyncBackground` (with
  `WithSyncBackgroundInterval`) trades bounded staleness for write
  throughput; suitable for very high-cardinality logs where per-line
  fsync becomes the bottleneck. `SyncOnCommit` lets the caller drive
  flush via the `tail.Syncer` extension interface.
- **Hooks must not block.** All hooks (`OnRotated`, `OnError`,
  `OnBatchSent`, …) fire synchronously from the read loop. Anything that
  does I/O — slog handlers writing over the network, blocking metric
  pushes — must hand off to a goroutine or buffered channel.
- **Pair `WithFlock` with a sibling lock path.** Never use the cursor
  file itself as the lock path: `Save`'s atomic rename will orphan the
  flock fd. `NewFileCursor` rejects this misconfiguration up front.
- **Long lines.** The default `MaxLine` is 1 MiB. Longer lines return
  `ErrLineTooLong` and the reader skips to the next newline.
- **Single-instance protection.** When two consumers may race for the
  same cursor, use `WithFlock`. Acquired before `tail.New` returns;
  released by `Tailer.Close` (via `Cursor.Close`).

## Project layout

```
github.com/jacobcase/gotail/
├── watch/        L1 — file-as-stream primitives (Watcher, LineReader, Position)
│   ├── poll.go              Always-available polling backend
│   ├── fsnotify_unix.go     OS-event backend (default; opt-out via build tag)
│   ├── fsnotify_stub.go     Stub returning ErrUnsupported (Windows / opt-out)
│   ├── linereader.go        Newline framing on top of a Watcher
│   └── stat_{unix,windows}  Inode / file-ID extraction
├── tail/         L2 — durable line-oriented tail (Tailer, Source, Cursor)
│   ├── tail.go              Tailer, Options, Stats, Records, Commit
│   ├── source.go            SingleFile, Lumberjack, Logrotate, Glob, StaticSource
│   ├── cursor.go            FileCursor (atomic JSON), MemoryCursor, Syncer
│   └── flock_{unix,windows} Advisory single-instance lock
├── forward/      L3 — batched at-least-once shipper (Forwarder[T], Sink[T], Decoder[T])
├── watchtest/    Test helpers for L1: FakeWatcher
├── tailtest/     Test helpers for L2: mutable MemorySource (Add/Prune)
├── forwardtest/  Test helpers for L3: RecordingSink[T], FailingSink[T]
├── cmd/gotail/   `tail -f` reference CLI (~70 LOC)
└── internal/     atomicwrite, bufpool — not part of the public API
```

| Package | Layer | Used when |
|---------|-------|-----------|
| `watch` | L1 | You want raw lines, no persistence, no multi-file awareness |
| `tail`  | L2 | You want durable resume across restarts (default for most callers) |
| `forward` | L3 | You want batched, retried, at-least-once delivery to an external system |
| `tailtest` / `watchtest` / `forwardtest` | — | Unit-test helpers — only import from `_test.go` |

## Development

The library is concurrency-heavy. Always exercise both build
configurations under the race detector — fsnotify and polling have
distinct code paths and both must stay green:

```
go test -race ./...
go test -race -tags gotail_nofsnotify ./...
```

Goroutine leaks are caught at suite teardown via
[goleak](https://github.com/uber-go/goleak); a stray goroutine fails the
test. Cross-compilation for the supported targets is verified in CI.

To generate a coverage profile (matches what CI uploads to Codecov):

```
go test -race -covermode=atomic -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html   # browseable report
go tool cover -func=coverage.out                     # per-function summary
```

CI runs the full matrix on Linux, macOS, and Windows with both build
tag configurations. A separate `coverage` job uploads `coverage.out` as
a build artifact and to [Codecov](https://codecov.io/gh/jacobcase/gotail).
`govulncheck` and `staticcheck` run on every push.

### Design plan

The detailed design lives in [`docs/v2-plan.md`](docs/v2-plan.md). When
the shipped code drifts from the plan, the divergence is recorded in
the plan's `## 11. Deviations` section.

## v1 → v2 migration

| v1 concept | v2 equivalent |
|------------|---------------|
| `gotail.NewPoller(path)` → `io.ReadCloser` | `tail.New(ctx, opts)` → `*Tailer` |
| Manual rotation handling | Automatic; `OnRotated` hook available |
| No checkpoints | `tail.NewFileCursor` / `tail.NewMemoryCursor` |
| No line framing | `watch.NewLineReader` (used internally by Tailer) |
| No multi-file backfill | `tail.Lumberjack`, `tail.Logrotate`, `tail.Glob` |
| No batched forwarding | `forward.Forwarder[T]` |

## Docs

- [Cookbook: HTTPS forwarder with mTLS](docs/cookbook/https-forwarder.md)
- [Cookbook: Backfill archived files](docs/cookbook/backfill.md)
- [Cookbook: Standalone watch usage](docs/cookbook/standalone.md)
- [Metrics: Prometheus](docs/metrics-prometheus.md)
- [Metrics: OpenTelemetry](docs/metrics-otel.md)
- [v2 design plan](docs/v2-plan.md)
