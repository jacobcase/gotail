# gotail Performance & Simplicity Review

Scope: `watch`, `tail`, `forward`, `internal/atomicwrite`, `cmd/gotail`,
`tailtest`, `forwardtest`. Focused on performance anti-patterns, channel
and goroutine value, dead/redundant code, and OS file-handling correctness.

Tests pass cleanly (`go test -count=1 -short ./watch/... ./tail/... ./forward/...`).

---

## High-impact findings

### H1. `forward.Forwarder.Run` feeder goroutine + buffered channel — dubious value, can be removed entirely

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

`tail/tail.go:284`: `t.opts.OnRotated(prev, Position{File: nextPath})` —
`Inode` and `Offset` are zero. This is asymmetric with `from = prev` which
is fully populated. Either populate from a quick `watch.StatInode(nextPath)`
(the next event from the LineReader will give it), or document that `to`
carries only `File` for backup-advance transitions. Tests at
`tail/tail_test.go:596-597` happen to only use `to.File`, so callers may
quietly depend on this.

### M3. `pollWatcher` Phase-3 truncation `Seek` is dead I/O

`watch/poll.go:103`, `watch/fsnotify_unix.go:106`. The watcher seeks its
own fd to 0 after detecting truncation, but never reads from this fd. The
seek doesn't affect the LineReader's separate fd. It's a no-op syscall.
Drop it (and the related error path).

### M4. Allocation in `LineReader.trimLine` when `KeepNewline=true`

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

`forward/forward.go:300`: `case <-time.After(d):`. Every retry leaks a
`*Timer` until it fires, even when the ctx-done case wins. On a
permanently-failing sink this builds up. Use `time.NewTimer(d)` +
`defer t.Stop()` per iteration.

### M7. `forwardtest.RecordingSink.All` reallocates on every call

`forwardtest/sinks.go:41-47` — appends without preallocating. Test-only,
but trivial: `out := make([]T, 0, totalLen)`.

---

## Low-impact / simplification

### L1. `pollWatcher.openFirst`'s `case`-based resume branch is awkward

`watch/poll.go:185-200` (and the fsnotify mirror). The
`switch { case A: ...; default: ...log; }` form would read more naturally
as an `if/else`. Stylistic only.

### L2. `Logrotate.Enumerate`'s redundant `HasPrefix` check

`tail/source.go:255`: `if !strings.HasPrefix(p, prefix)` —
`filepath.Glob(s.activePath + ".*")` already guarantees the prefix.
Defensive but redundant.

### L3. `tail.MemorySource` (frozen-slice constructor) vs `tailtest.MemorySource` (mutable)

Two types named `MemorySource` in two packages. Today this works but it's
a footgun in test imports. Consider `tail.StaticSource` or similar for the
immutable one. (Public API change — only worth doing pre-1.0.)

### L4. `cursor.go:131-133` — `maxMetaBytes` enforced before marshal

`Save` checks `len(cp.Meta) > maxMetaBytes` *before* marshalling, but the
limit really applies to the raw user-supplied JSON, not the wrapping
envelope. Either rename to `maxRawMetaBytes` to clarify intent, or move the
check post-marshal against the full payload.

### L5. `cmd/gotail/main.go:74-75` — two writes per record

`out.Write(rec.Line)` then `out.WriteByte('\n')`. Through `bufio.Writer`
this is fine (no syscall coalescing concern). Could enable `KeepNewline`
instead and emit one `Write`, but the gain is microscopic.

---

## Goroutines & channels — full inventory

| Site | Pattern | Verdict |
|------|---------|---------|
| `forward/forward.go:156` feeder goroutine + buffered `recCh` (default 16) | Convert blocking iterator into select-able stream | **Removable** — see H1 |
| `tail/tail.go:183` `done chan struct{}` (unbuffered, 0-size) | One-shot signal for `StopAtEOF` | Keep — already unbuffered |
| `tail/tail.go:323` `context.AfterFunc` for `closeCtx → cancel` | Plumbs Close into in-flight `Next`'s ctx | Keep — clean pattern |
| `tail/tail.go:437` `activeNext.Wait()` | Serializes Close with active Next | Keep |
| `pollWatcher.sleep` `time.NewTimer` | Cancellable sleep | Keep |
| `fsnotifyWatcher.fsnWait` select on lib's `Events`/`Errors` | Required by fsnotify API | Keep |
| `forward.sendWithRetry` `<-time.After(d)` | Backoff sleep | **Switch to NewTimer** — see M6 |

**Net**: the only owned channel/goroutine pipeline that warrants
questioning is the Forwarder feeder (H1). Everything else is either a
one-shot signal or library-imposed.

---

## File handling & rotation correctness

The state machine is correct, but the duplicate-fd architecture (H3)
creates a real (if narrow) race window between `Watcher.openFirst` and
`LineReader.switchToFile`: if rotation lands in that microsecond gap, the
two fds point at different inodes. The system mostly self-heals because
the next `Wait` will detect the inode mismatch and emit `ReOpened` — but
the LineReader's first read happens against a different file than the
Watcher's "old" fd. Fix is the fd-transfer refactor in H3.

The race-aware drain at `watch/poll.go:142-155` (re-stat after rotation
detection) is correct and the inline doc comment is good. The `oldFile`
lifecycle (kept open until next `Wait`) is correct.

The **copytruncate** path is double-defended:

1. Watcher's Phase-3 stat detects size shrink and emits `Truncated`.
2. LineReader's own EOF-stat-trunc check at `watch/linereader.go:149-162`
   catches the case where the LineReader hits EOF before the Watcher polls
   again.

Both reset to offset 0 and re-seek. Correct.

The **`StopAtEOF` for backups** at `tail/tail.go:211`
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

## Recommended order of attack

1. **H1** (drop Forwarder feeder) — biggest simplification, cuts the only
   owned goroutine pipeline.
2. **H6** (drop Tailer `mu` around `t.lr`) — pure cleanup, no behaviour
   change.
3. **M1, M3, M6** (closure wrap, dead seek, `time.After` → `NewTimer`) —
   small targeted fixes.
4. **H4, H5** (Unix stat-only inode, cache fsnotify target) — minor perf,
   easy.
5. **H3** (single-fd refactor + drop `PreRotation` from public API) —
   biggest design change; only do if you're willing to break the
   `watch.Event.PreRotation` contract pre-1.0.

Skip H1+H3 if you're past the point where you can take API-shape changes;
they're the high-value moves but the riskiest.
