# gotail Performance & Simplicity Review

Scope: `watch`, `tail`, `forward`, `internal/atomicwrite`, `cmd/gotail`,
`tailtest`, `forwardtest`. Focused on performance anti-patterns, channel
and goroutine value, dead/redundant code, and OS file-handling correctness.

Tests pass cleanly (`go test -count=1 -short ./watch/... ./tail/... ./forward/...`).

## Status

| Item | Status | Commit |
|------|--------|--------|
| H1 — drop Forwarder feeder | **DONE** | `45e13fb` |
| H2 — `feederDrained` dead code | **DONE** (subsumed by H1) | `45e13fb` |
| H3 — single-fd architecture | **DONE** | `45e13fb` |
| H4 — Unix stat-only inode | **DONE** | `45e13fb` |
| H5 — cache fsnotify clean target | **DONE** | `45e13fb` |
| H6 — drop `Tailer.mu` around `t.lr` | **DONE** | `45e13fb` |
| M1 — `OnTruncated` closure wrap | **DONE** | `45e13fb` |
| M2 — half-populated `OnRotated` Position | **DONE** | `877e5d6` |
| M3 — dead Phase-3 `Seek` | **DONE** (eliminated by H3) | `45e13fb` |
| M4 — document `trimLine` in-place append | **DONE** | `877e5d6` |
| M5 — `jitteredBackoff` overflow loop | **DONE** | `45e13fb` |
| M6 — `time.After` → `time.NewTimer` | **DONE** | `45e13fb` |
| M7 — `RecordingSink.All` preallocation | **DONE** | `877e5d6` |
| L1 — `openFirst` switch → if/else | **DONE** (incidental in H3 rewrite) | `45e13fb` |
| L2 — `Logrotate.Enumerate` redundant `HasPrefix` | **DONE** | `655b3e2` |
| L3 — `MemorySource` naming collision | **DONE** | `655b3e2` |
| L4 — `maxMetaBytes` check before marshal | **DONE** (renamed to `maxRawMetaBytes`) | `655b3e2` |
| L5 — `cmd/gotail` two writes per record | **WONTFIX** — plumbing `KeepNewline` through `tail.Options` costs more than the gain (writes already coalesce through `bufio.Writer`) | — |

---

## High-impact findings

### H1. `forward.Forwarder.Run` feeder goroutine + buffered channel — dubious value, can be removed entirely

> **DONE** in `45e13fb`. `RecordSource` switched to `Next(ctx)`; `Forwarder.Run` uses per-call `context.WithDeadline` for `MaxBatchAge`. Feeder goroutine, `recCh`, `recItem`, `feederDrained`, and `PrefetchBuffer` removed.

`forward/forward.go:147` allocates a buffered `recCh` and spawns a feeder
goroutine (`forward/forward.go:156`). The justification is "let
`MaxBatchAge` interrupt the wait for the next record" and "let the feeder
read ahead during sink retry."

The age-timer concern can be solved without a goroutine by deriving a
per-`Next` deadline:

- **Empty batch**: `Next(ctx)` blocks until a record arrives — no timer needed.
- **Non-empty batch**: derive
  `deadlineCtx, cancel := context.WithDeadline(ctx, batchStart.Add(MaxBatchAge))`;
  if `Next` returns `ctx.DeadlineExceeded`, flush; if it returns a record, append.

`Tailer.Next` already cleanly honours ctx cancellation, so this works.
Eliminating the feeder removes:

- The goroutine and its `WaitGroup`.
- `recCh`, `PrefetchBuffer`, the `recItem` struct.
- `feederDrained atomic.Bool` (which is *almost dead code today* — see H2).
- The "feeder exited via ctx-done vs feeder drained naturally" reasoning at
  `forward/forward.go:216-238`.

The "read-ahead during sink retry" rationale is weak: while the sink is
failing you *want* backpressure, not a bigger queue. Pre-fetching during
retry just inflates memory residency and delays the moment the source
notices it should slow down. Where channels are still required, prefer
unbuffered — but here the channel itself is removable.

### H2. `feederDrained` is effectively dead with the standard `RecordSource`

> **DONE** in `45e13fb` (subsumed by H1 — feeder goroutine and atomic both removed).

`forward/forward.go:154-169`. The standard `tail.Tailer.Records` iterator
always yields an error before exiting (it never naturally exits without
yielding). So the feeder's range loop always exits via
`if err != nil { return }`, leaving `feederDrained == false`. The
`if feederDrained.Load() { return flush() }` branch at
`forward/forward.go:222` is unreachable for the in-tree source. The doc
comment says it defends against custom sources, but for the standard path
it is pure dead code.

If H1 is taken, this disappears. If not, the comment should make clear it
is custom-source defensive only, and you can sidestep the whole question by
having the consumer re-check via `errors.Is(item.err, tail.ErrSourceExhausted)`
exclusively.

### H3. Watcher and LineReader hold two file descriptors per file (duplicate open + duplicate stat)

> **DONE** in `45e13fb`. Watchers no longer own an `*os.File`; rotation detected purely via inode comparison on `os.Stat(path)`. `watch.PreRotation` and `Event.PreRotation` removed from public API. Trailing-bytes drain now relies entirely on the LineReader's fd, which keeps the rotated-out inode alive until EOF.

`watch/poll.go:165` (Watcher first open) + `watch/linereader.go:230`
(LineReader `switchToFile` open) — every active file has two open fds, two
`Stat` calls per cycle, and a tiny race window where the Watcher's fd and
the LineReader's fd can diverge if rotation happens between them.

The Watcher's owned fd is load-bearing for one specific reason: detecting
"path now resolves to a new inode but my old fd still has bytes." But the
LineReader already does its own EOF-stat-trunc check
(`watch/linereader.go:149-162`) and the Watcher could in theory hand its fd
to the LineReader (transfer ownership) instead of having both sides open
the file independently. `PreRotation.Reader` is exported but **no in-tree
consumer uses it** — `LineReader.handleEvent` explicitly ignores it
(`watch/linereader.go:202-205`); the only callers are the watcher's own
unit tests.

Two viable simplifications:

- **Smaller**: have the Watcher transfer its fd to the LineReader on each
  `ReOpened`/initial-open event, so only one fd is ever open. The Watcher
  would then re-open the new path on rotation.
- **Larger** (probably wrong without more thought): drop the Watcher's
  owned fd entirely, replace `f.Stat()` with `os.Stat(path)`, and accept
  the trailing-bytes race. **Don't do this** — it would lose the
  race-aware drain that's the whole point of holding the fd.

The smaller change is worth pursuing. Drop `PreRotation` from the public
API in the same shot since nothing internal consumes it.

### H4. Rotation polling check opens + closes the path on every cycle (Unix could just stat)

> **DONE** in `45e13fb`. Added per-platform `statSizeInode`: single `os.Stat` on Unix, open-stat-close on Windows (file index requires a handle). Used by both watchers and `watch.StatInode`.

`watch/poll.go:230` and `watch/fsnotify_unix.go:214` —
`isRotated`/`fsnIsRotated` calls `os.Open(path)` + `defer Close()` to read
the inode of the path-side file. On Unix, the inode is available from a
plain `os.Stat(path)` via `Sys().(*syscall.Stat_t).Ino` — no fd needed. On
Windows the open is required (file index needs a handle). For the polling
watcher this fires every `Interval` while at EOF; for fsnotify only on
events, but events can be very frequent on busy logs.

Add a Unix-specific `statInode(path) (uint64, error)` (alongside the
existing `fileID(*os.File)`) and call it from `isRotated`. Same change
applies to `tail.findFileByInode`/`watch.StatInode` (`watch/watch.go:100-107`).

### H5. `fsnotify_unix.go:271`/`280` — `filepath.Clean` per event

> **DONE** in `45e13fb`. `target := filepath.Clean(c.Path)` cached on the watcher struct at construction.

```go
target := filepath.Clean(w.c.Path)
for {
    case ev, ok := <-w.fw.Events:
        if filepath.Clean(ev.Name) == target { return true }
```

`target` is recomputed each `fsnWait` call; `filepath.Clean(ev.Name)` runs
on every irrelevant sibling event. Cache `target` in the watcher struct at
construction time (since `c.Path` never changes), and skip the `Clean` on
`ev.Name` if you assume fsnotify already returns canonical paths (it does
on Linux; verify on macOS/BSD). At minimum, cache the target.

### H6. `Tailer.mu` around `t.lr` is unnecessary

> **DONE** in `45e13fb`. Locks dropped from `advance`, `Next`, and `Close`. Single-writer invariant documented on the field.

`tail/tail.go:257-259, 327-329, 355-357, 439-441`. `t.lr` is written in
`openFile` (called only from `New` and `advance`) and `advance` runs inside
`Next`. `Next` is documented as not safe for concurrent use; `Close` waits
for `activeNext.Wait()` before reading `t.lr`. So the `t.lr` accesses
inside `Next`/`advance` cannot race with `Close`, and the LineReader itself
enforces single-goroutine use. Drop the locks around `t.lr` reads — they
don't protect anything that isn't already serialized by `activeNext`. Keep
`mu` for `cur` and `lastMeta` only.

---

## Medium-impact findings

### M1. `tail.openFile` wraps `OnTruncated` in an identity closure

> **DONE** in `45e13fb`. Direct assignment now (`Position` is a type alias for `watch.Position`).

`tail/tail.go:225-229`:

```go
if t.opts.OnTruncated != nil {
    fn := t.opts.OnTruncated
    lrOpts.OnTruncated = func(at watch.Position) { fn(at) }
}
```

`Position = watch.Position` is a type alias (`tail/tail.go:19`), so
`t.opts.OnTruncated` already has the right signature. Replace with
`lrOpts.OnTruncated = t.opts.OnTruncated`. Saves a closure allocation on
each rotation/advance (rare but free).

### M2. `tail.advance` emits a half-populated `Position` to `OnRotated`

> **DONE** in `877e5d6`. Best-effort `watch.StatInode(nextPath)` populates `Inode`; `Offset` is genuinely zero for a backup-advance.

`tail/tail.go:284`: `t.opts.OnRotated(prev, Position{File: nextPath})` —
`Inode` and `Offset` are zero. This is asymmetric with `from = prev` which
is fully populated. Either populate from a quick `watch.StatInode(nextPath)`
(the next event from the LineReader will give it), or document that `to`
carries only `File` for backup-advance transitions. Tests at
`tail/tail_test.go:596-597` happen to only use `to.File`, so callers may
quietly depend on this.

### M3. `pollWatcher` Phase-3 truncation `Seek` is dead I/O

> **DONE** in `45e13fb` (eliminated by H3 — watchers no longer own an fd, so there's no fd to seek).

`watch/poll.go:103`, `watch/fsnotify_unix.go:106`. The watcher seeks its
own fd to 0 after detecting truncation, but never reads from this fd. The
seek doesn't affect the LineReader's separate fd. It's a no-op syscall.
Drop it (and the related error path).

### M4. Allocation in `LineReader.trimLine` when `KeepNewline=true`

> **DONE** in `877e5d6`. Documenting comment added explaining the in-place `\n` trick.

`watch/linereader.go:258`: `raw = append(raw, '\n')`. This is in-place when
`cap(raw)` is sufficient (the trailing `\n` byte is at exactly
`start+idx` in the buffer, so the append writes back where the `\n`
already was — zero-alloc). When the line ends with `\r\n` and
`KeepNewline=true`, the `\r` strip shrinks `raw` to `len = idx-1`, then
append writes `\n` at `start+idx-1` (overwriting `\r`) — also in-place. So
this is fine in practice, but the appended-byte position depends on buffer
geometry. Worth a one-line comment:
`// in-place: \n already lives at raw[len(raw)] in the read buffer`. No
code change needed; just don't accidentally "fix" it later.

### M5. `Forwarder.jitteredBackoff` overflow loop

> **DONE** in `45e13fb`. Replaced with bit-shift + 62-bit cap.

`forward/forward.go:309-322`. The loop multiplies by 2 each attempt, with
an overflow check inside. Cleaner and faster:

```go
shift := attempt
if shift > 62 { shift = 62 }
ceiling := f.opts.InitialBackoff << shift
if ceiling <= 0 || ceiling > f.opts.MaxBackoff {
    ceiling = f.opts.MaxBackoff
}
```

Not hot, just simpler.

### M6. `Forwarder.sendWithRetry` uses `time.After` (untracked timer)

> **DONE** in `45e13fb`. `time.NewTimer(d)` + `Stop()` per iteration.

`forward/forward.go:300`: `case <-time.After(d):`. Every retry leaks a
`*Timer` until it fires, even when the ctx-done case wins. On a
permanently-failing sink this builds up. Use `time.NewTimer(d)` +
`defer t.Stop()` per iteration.

### M7. `forwardtest.RecordingSink.All` reallocates on every call

> **DONE** in `877e5d6`. Replaced with `slices.Concat(s.batches...)`.

`forwardtest/sinks.go:41-47` — appends without preallocating. Test-only,
but trivial: `out := make([]T, 0, totalLen)`.

---

## Low-impact / simplification

### L1. `pollWatcher.openFirst`'s `case`-based resume branch is awkward

> **DONE** in `45e13fb` (incidental in the H3 rewrite — `openFirst` now uses `if/else`).

`watch/poll.go:185-200` (and the fsnotify mirror). The
`switch { case A: ...; default: ...log; }` form would read more naturally
as an `if/else`. Stylistic only.

### L2. `Logrotate.Enumerate`'s redundant `HasPrefix` check

> **DONE.** Removed; `filepath.Glob(s.activePath + ".*")` already guarantees the prefix.

`tail/source.go:255`: `if !strings.HasPrefix(p, prefix)` —
`filepath.Glob(s.activePath + ".*")` already guarantees the prefix.
Defensive but redundant.

### L3. `tail.MemorySource` (frozen-slice constructor) vs `tailtest.MemorySource` (mutable)

> **DONE.** Renamed `tail.MemorySource` → `tail.StaticSource` (matching the godoc, which already described it as "fixed, immutable"). `tailtest.MemorySource` keeps its name — it's the mutating one.

Two types named `MemorySource` in two packages. Today this works but it's
a footgun in test imports. Consider `tail.StaticSource` or similar for the
immutable one. (Public API change — only worth doing pre-1.0.)

### L4. `cursor.go:131-133` — `maxMetaBytes` enforced before marshal

> **DONE.** Renamed constant to `maxRawMetaBytes` with a doc comment clarifying that the limit is on user-supplied raw JSON, not the wrapping envelope. Error message updated to say "raw meta size". The pre-marshal check is the right place — running marshal just to reject would burn the work.

`Save` checks `len(cp.Meta) > maxMetaBytes` *before* marshalling, but the
limit really applies to the raw user-supplied JSON, not the wrapping
envelope. Either rename to `maxRawMetaBytes` to clarify intent, or move the
check post-marshal against the full payload.

### L5. `cmd/gotail/main.go:74-75` — two writes per record

> **WONTFIX.** The "fix" would require plumbing `KeepNewline` through `tail.Options` → `watch.LineOptions` (currently hardcoded `false`) — a meaningful surface-area change for zero observable gain. Both writes go through `bufio.Writer`, so there's no syscall difference. Closing as not worth the API cost.

`out.Write(rec.Line)` then `out.WriteByte('\n')`. Through `bufio.Writer`
this is fine (no syscall coalescing concern). Could enable `KeepNewline`
instead and emit one `Write`, but the gain is microscopic.

---

## Goroutines & channels — full inventory

| Site | Pattern | Verdict |
|------|---------|---------|
| ~~`forward/forward.go:156` feeder goroutine + buffered `recCh`~~ | ~~Convert blocking iterator into select-able stream~~ | **REMOVED in `45e13fb` (H1)** |
| `tail/tail.go` `done chan struct{}` (unbuffered, 0-size) | One-shot signal for `StopAtEOF` | Keep — already unbuffered |
| `tail/tail.go` `context.AfterFunc` for `closeCtx → cancel` | Plumbs Close into in-flight `Next`'s ctx | Keep — clean pattern |
| `tail/tail.go` `activeNext.Wait()` | Serializes Close with active Next | Keep |
| `pollWatcher.sleep` `time.NewTimer` | Cancellable sleep | Keep |
| `fsnotifyWatcher.fsnWait` select on lib's `Events`/`Errors` | Required by fsnotify API | Keep |
| ~~`forward.sendWithRetry` `<-time.After(d)`~~ | ~~Backoff sleep~~ | **`time.NewTimer` in `45e13fb` (M6)** |

**Net**: after H1+M6, no owned goroutines remain in the production code
paths. Channels in use are either one-shot signals or library-imposed.

---

## File handling & rotation correctness

After H3 the duplicate-fd architecture and its race window are gone.
The watcher stats the path; the LineReader holds the only fd to the
active file and its existing reads naturally drain trailing bytes on
the rotated-out inode (the kernel keeps the inode alive while the fd
is held).

The **copytruncate** path is still double-defended:

1. Watcher's Phase-4 stat detects size < watermark and emits `Truncated`.
2. LineReader's own EOF-stat-trunc check at `watch/linereader.go:149-162`
   catches the case where the LineReader hits EOF before the watcher's
   next poll/event fires.

Both reset to offset 0 and re-seek. Correct.

The **`StopAtEOF` for backups** in `Tailer.openFile`
(`!isActive || t.opts.StopAtEOF`) ensures rotation through old files
terminates cleanly. Correct.

The **lumberjack `.gz` skip** at `tail/source.go:114-120` reports
compressed files via the hook before the regular suffix check excludes
them. Correct, but the comment block could note that this means a
checkpoint resume targeting a compressed backup will fall to
`OnMissingCheckpoint` — that's the operationally interesting case the
hook lets users alert on.

The **`atomicwrite` sequence** (write → fsync file → close → rename →
fsync dir) at `internal/atomicwrite/atomicwrite.go:21-53` is the
textbook-correct durability ordering. The `os.Remove(tmp)` on every
failure path is correct. The dir-sync best-effort fallback for FAT32/SMB
is the right call.

---

## Outcome

All in-scope items addressed across three implementation commits plus this doc:

- `45e13fb` — H1, H2, H3, H4, H5, H6, M1, M3, M5, M6, L1.
  Net **−336 lines**; tests green under `-race` for both default and
  `gotail_nofsnotify` build tags.
- `877e5d6` — M2, M4, M7. Tiny follow-up batch.
- `655b3e2` — L2, L3, L4. Trivial cleanups + `tail.MemorySource` →
  `tail.StaticSource` rename to disambiguate from `tailtest.MemorySource`.

L5 closed as **wontfix** (plumbing cost > gain).
