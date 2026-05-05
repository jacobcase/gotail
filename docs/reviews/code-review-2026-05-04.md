# gotail v2 — End-to-End Review

- **Conducted:** 2026-05-04
- **Closed:** 2026-05-04

Overall this is a careful, well-structured codebase. The layering (watch / tail / forward) is honest — each layer is independently importable, the v1 race-aware drain semantics are preserved and pinned by tests, and the public API uses modern Go (`iter.Seq2`, generics on L3, `slog`, `ctx`-on-blocking, sentinel errors). What follows is concentrated on the things worth fixing.

## Correctness — file rotation / checkpointing (highest priority)

### 1. Windows inode is always 0 → rotation detection silently broken on Windows
`watch/stat_windows.go:13–21` returns 0 for `fileInode`. `fileInodeFromHandle` is defined immediately below it but **is never called from anywhere** (verified by grep). Consequences:
- `pollWatcher.openFirst` (`watch/poll.go:172`) saves `p.inode = 0`.
- `pollWatcher.isRotated` (`watch/poll.go:219–223`) computes `newInode != p.inode` → `0 != 0 == false`. **Rotation by inode change is never detected.**
- `findFileByInode` (`tail/tail.go:230`) treats the `0/0` case as "always match", so checkpoint resume on Windows lands on the first file in the series and offsets are applied blindly.
- The comment on `stat_windows.go:20` says "Callers that need stable identity on Windows should use NoInodeCheck or obtain the inode via fileInodeFromHandle (see poll.go)" — but poll.go does not call it.

This is the single most important bug. Either:
- wire `fileInodeFromHandle` through (open the file, then derive inode from the handle in `openFirst`/`fsnOpenFirst`/`isRotated`), or
- have the polling watcher auto-enable `NoInodeCheck` semantics on Windows when `fileInode` returns 0, and document that Windows callers must rely on size-based rotation detection.

### 2. `atomicwrite.Write` order leaves the tmp fd open during rename
`internal/atomicwrite/atomicwrite.go:18–50`: the sequence is Write → Sync → Rename → dirSync → Close. Conventional and safer order is Write → Sync → **Close** → Rename → dirSync. On Unix this happens to be safe because `rename(2)` is metadata-only; on Windows it has historically caused trouble, and even on Unix some kernels report fsync errors only at close-time, which would currently be lost (only the dir fd's sync error is even checked, and that's `_ =`'d). Closing before rename also lets you check the close error and surface it.

Also: `if err := f.Sync(); err != nil { f.Close(); os.Remove(tmp) ... }` — `Close()` can return an error in this path that is ignored. With the file still open during fsync, that's the most likely place for a delayed I/O failure to surface.

### 3. `TestFileCursor_AtomicSave` does not actually exercise crash-mid-rename
`tail/cursor_test.go:63–98` — the test removes the *post-rename leftover* tmp file and then asserts the final file matches the first checkpoint. There is no crash injection between Write and Rename; the test passes regardless of whether rename is atomic. It would be more honest to either (a) write a test that interrupts after the tmp write but before the rename (using a fault-injection wrapper or a test-only hook in `atomicwrite`), or (b) rename the test and remove the misleading commentary about "crash mid-rename".

### 4. `Lumberjack` `Source` rejects `.gz` backups silently
`tail/source.go:51–52` notes "Compressed (.gz) backups are not yet supported and are silently ignored." For a backfill use case (Requirement 7 in v2-plan.md) this is a real gap — a checkpoint that points at an already-compressed backup file produces `findFileByInode` mismatch and triggers `OnMissingCheckpoint` policy as if the file aged off, even though the data still exists. Either add `.gz` decompression support, or at minimum surface the skipped files to the caller (e.g. an `OnSkippedBackup` hook) so they're not invisible.

### 5. `Glob` source's "lexicographic = oldest-first" is wrong for double-digit numeric suffixes
`tail/source.go:117–124` and the doc example `tail.Glob("/var/log/app.log", "/var/log/app.log.[0-9]*")` — `sort.Strings` puts `.10` before `.2`, which inverts logrotate's age ordering once you have ≥10 backups. The test (`source_test.go:140–169`) only uses single-digit suffixes, so the regression hides. Either:
- document the limitation aggressively and reject `[0-9]*` patterns, or
- add a `numericSuffix bool` option (or pass a comparator), or
- offer a `tail.Logrotate` source that knows the numeric-tail convention.

### 6. `Tailer.openFile` mutates `t.opts.Whence` to express "one-shot"
`tail/tail.go:188–192`. Mutating an embedded options struct is subtle and the `t.opts` is also read elsewhere (`Records()` etc.). A separate `t.consumedWhence bool` or a local `firstOpen bool` flag would be clearer and less error-prone if the constructor ever needs to inspect the original options later.

### 7. Pre-existing-but-now-rotated `pollWatcher.openFirst` does not handle Resume gracefully
`watch/poll.go:175–186`: when `Resume.Inode != current_inode` and `NoInodeCheck` is false, the resume is silently dropped (the `if` body is skipped). The behavior — fall through to read from offset 0 — is reasonable, but it's silent. The `tail` layer relies on `findFileByInode` to detect this case before construction, so in normal use this branch is unreachable; if a custom Source were wrong, the error would be invisible. Minimum: log at Debug. Better: surface as `ErrInodeMismatch` so callers can choose.

## Concurrency safety

### 8. `forward.Forwarder.Run` leaks an unsupervised feeder goroutine on permanent error
`forward/forward.go:107–128, 240–243`. The feeder goroutine is started, then `Run` can return early via `return fmt.Errorf("forward: permanent sink error: %w", err)`. The feeder is not waited on (no `WaitGroup`, no `errgroup`). Because the feeder selects on `ctx.Done()` *and* on the channel send, if the *parent* of `Run` doesn't cancel ctx after `Run` returns (which is a perfectly normal pattern — many callers use a long-lived ctx and call `Run` synchronously), the feeder will park inside `f.opts.Source.Records(ctx)` consuming records until something else cancels the ctx.

Use `errgroup.WithContext` (or a private context cancelled in a defer) so `Run`'s return cancels the feeder. Per the `golang-concurrency` skill, "the goroutine that starts a goroutine is responsible for ensuring it exits."

### 9. `recCh` is unbounded against backpressure when sink stalls
Same area, `forward/forward.go:115`. `recCh := make(chan recItem, 16)` is fine, but combined with the at-most-once ageTimer and a slow Sink, what blocks: the feeder fills the channel, then blocks on send. Meanwhile the consumer side is sleeping in `time.After(d)` inside `sendWithRetry`. This is OK in practice because retries are bounded by ctx, but it's worth noting that `recCh` being non-trivially buffered creates a "16 records past the boundary already decoded" memory reservation per Forwarder. For typical use this is negligible; for very large records it isn't.

### 10. `fakeWatcher.Wait` doc/code mismatch + idle `select`
`watch/fakewatcher.go:19–40`. The doc says "returns [io.EOF] on subsequent Wait calls", but the second `Wait` blocks on `<-ctx.Done()`. Single-case `select { case <-ctx.Done(): }` is just `<-ctx.Done()`. Pick one behavior (returning EOF would actually serve the documented test semantics better, since `LineReader` already handles EOF correctly).

### 11. `Tailer.Next` reads `t.lr` under a lock, then operates on it without a lock
`tail/tail.go:310–314, 334–336, 408–413`. The pattern is "lock, snapshot pointer, unlock, use pointer". This is fine for concurrent `Close()` *as long as* `lr.Close()` is itself safe to call concurrently with `lr.Next()`. `LineReader.Close` (`watch/linereader.go:301`) just sets `l.f = nil` — there's no `select`, no atomic, and `l.f` is read in `Next` from `l.f.Stat()` (`linereader.go:146`). Concurrent `Close` while `Next` is mid-call is a data race on `l.f` and a likely nil-deref. The doc says `LineReader` is "not safe for concurrent use; Close may be called from any goroutine" — but the code does not actually make that promise true.

If you want to keep that contract, either:
- guard `l.f` with an atomic / sync.Once-style protection inside `LineReader.Close`, or
- fix the doc and have `Tailer.Close` race-free on top by serializing with a `cancel()` on a child context wired into `Next` (and not calling `lr.Close()` from a different goroutine than the one reading).

This is a real concurrency hazard given the documented contract.

### 12. `lumberjackSource`/`globSource` use `sort.Slice` / `sort.Strings`; modernize to `slices.SortFunc` / `slices.Sort`
`tail/source.go:106` and `:138`. Per `golang-modernize`, `slices.SortFunc` is preferred under Go 1.21+; you're on 1.26.

## Idiom & stdlib alignment

### 13. `forward.Forwarder.jitteredBackoff` shadows builtin `cap`
`forward/forward.go:264–277`. `cap := f.opts.InitialBackoff` shadows `builtin.cap`. `vet`/lint will flag it. Rename to `ceiling` or `limit`.

### 14. Old-style `for` loops where `for range N` works
`forward/forward.go:266`, `watch/linereader.go:279`. Both work but Go 1.22+ allows `for range attempt` / `for range n`. Modernize-linter material; trivial.

### 15. `errors.Is`/sentinel hygiene
- `forward/forward.go:316` (test) and similar use `err == io.EOF`. Strictly speaking that's fine because `io.EOF` is the literal value `*errors.errorString`, but the codebase otherwise uses `errors.Is` (e.g. `tail.go:316`). Pick one and apply it everywhere — consistency matters for the `golang-error-handling` skill.
- `tail/flock_unix.go:22`: `if err == syscall.EWOULDBLOCK` — should be `errors.Is(err, syscall.EWOULDBLOCK)` to handle wrapped errnos. (Though `syscall.Flock` is unlikely to wrap.)

### 16. `tailtest.MemorySource.Prune` uses pre-1.21 splice idiom
`tailtest/source.go:30–33`: `m.paths = append(m.paths[:i], m.paths[i+1:]...)`. Modern: `m.paths = slices.Delete(m.paths, i, i+1)`.

### 17. `internal/bufpool` is unused
`internal/bufpool/bufpool.go` — verified via grep, no callers anywhere in the tree. The v2-plan listed buffer reuse as a goal, the LineReader did the work in-place (`linereader.go:111–119`), and this package was left behind. Either delete or wire it in for the rotation case (`watch/linereader.go:226 switchToFile` allocates fresh state but reuses `l.buf`).

### 18. `forward.RecordSource` and `forward.Position` aliases are good — but the constraint is unstated
`forward/forward.go:21–25`. `RecordSource` requires `Records(ctx) iter.Seq2[tail.Record, error]`, importing `tail.Record`. That makes `forward` depend on `tail` for a type that is, in spirit, just `Record`-shaped. Consider declaring a local `forward.Record` and adapting in tests, or accept that `tail.Record` is the canonical shape and stop pretending the layering is one-way (the doc currently says forward depends on tail, so this is fine — just call it out explicitly in `forward/doc.go`).

### 19. Position is part of the JSON API; the `string` JSON tags are a permanent commitment
`watch/watch.go:13–17`. `Inode uint64 json:"inode,string"` and `Offset int64 json:"offset,string"` — encoding numbers as strings is the right call (large inodes survive JS clients, and JSON has no `int64`), but this is a wire-format commitment. Good idea to call it out explicitly in `tail/doc.go` "On-disk format" section, since `cursorFile` is your durability boundary.

### 20. `Checkpoint.Version` is on the wire but never validated on Load
`tail/cursor.go:83–87, 122–135`. `cursorFile.Version` is written as `1` but never read. If you ever bump the version, you have no way to detect old files. At minimum, validate `Version == 1` on Load and return a clear error otherwise; better, define the migration path now.

## Observability / API ergonomics

### 21. Hooks fire from the I/O hot path with no documentation about blocking
`tail/tail.go:74–80`, `forward/forward.go:62–68`. Hooks like `OnError`, `OnRotated`, `OnBatchSent`, `OnSendError` will be called synchronously from inside the tail/forward loops. A user who naively does an HTTP call in `OnError` will stall the pipeline. This isn't a bug but it's an obvious footgun — add a one-liner to `doc.go` saying "hooks must not block; do work asynchronously if needed."

### 22. `Tailer.Done()` is documented to close in StopAtEOF mode but advance() closes it on rotation past the end
`tail/tail.go:262–266`. `advance()` closes `done` when `nextIdx >= len(t.files)`. That happens during normal rotation in StopAtEOF mode (correct) — but it can also happen in *non*-StopAtEOF mode if the file series naturally terminates (e.g., custom `Source` with finite enumeration). The doc says "In live-tail mode it is never closed by the Tailer itself." That's not strictly accurate for custom finite Sources. Tighten the doc, or add a check that advance only closes done when StopAtEOF is true.

### 23. `Tailer.openFile` always uses polling when `UseFsnotify` is false; never uses `watch.New`'s auto-fallback
`tail/tail.go:204–208`. The `UseFsnotify=true` branch calls `watch.New`, which internally falls back to polling on `ErrUnsupported`. The `UseFsnotify=false` branch calls `NewPolling` directly — which is fine, but it means `tail.Options.UseFsnotify=false` actively disables the fsnotify path even when the binary was compiled with the build tag. That's exactly what the field name implies, so it's correct, but the v2-plan talks about "auto-fallback" — make sure the README explains which dial controls what.

### 24. `forward.Run` loses the feeder's `ctx.Err()` if the channel closes first
`forward/forward.go:174–181`. When the feeder closes `recCh` because `ctx.Done()` fired, the consumer sees `!ok` and returns `ctx.Err()` from `Run`. But there's also a window where the feeder closes `recCh` after the source iterator naturally returns (StopAtEOF). The current code returns `flush()`. That's the right semantics — but if `flush()` calls `sendWithRetry` and the context is already cancelled, the inner `select` returns `ctx.Err()`, masking the fact that it was a clean drain. Minor; only matters for callers who care about distinguishing "drained cleanly" from "ctx died at the very end."

## Small polish

- `tail/cursor.go:38–42`: `SyncOnCommit` and `SyncBackground` are documented "Not yet implemented; behaves like SyncAlways." Either implement or remove from the public enum. Public placeholder values are a maintenance liability.
- `tail/tail.go:121, 136`: `New` does I/O with `context.Background()`. The doc warns about it but the natural fix is to take a `ctx` parameter on `New` (or a separate `Open(ctx, opts)` constructor). At minimum, document that the timeout is the caller's responsibility, perhaps by exposing an `OpenContext` field on Options.
- `tail/tail.go:191`: `t.opts.Whence = 0 // consume` — mutating opts post-construction is the kind of thing a future maintainer will undo by mistake. Move to a private field.
- `watch/poll.go:170, 174`: `p.c.Resume = nil // one-shot` mutates options; same comment.
- `cmd/gotail/main.go:46–51` and `cmd/gotail/main.go:71–72`: writes `rec.Line + "\n"` to stdout in two `Write` calls. With buffered output (`bufio.NewWriter(os.Stdout)`) this would be much faster on high-rate logs, and a single `Write` per line is more atomic if anyone tee's output. Also, `KeepNewline: true` on `LineOptions` would reduce the second write to zero — but that's in the `tail` layer, not exposed.
- `cmd/gotail/main.go:1–11`: doc comment lists `-n int` flag that is not actually defined — dead docstring.
- `forward/forward.go:133`: `batchLastPos Position` declared with extra space (`batchLastPos    Position`) — gofmt should already handle this, but worth running `gofmt -s` to be sure.
- `watch/linereader.go:215–216`: `l.pendingNewPos = Position{}` — fine, but `Position{}` is the zero value which `IsZero` checks; consistent.

## Tests

The test suite is solid for what it covers — fuzz testing on LineReader, property-style "no byte yielded twice" with crash/restart, race-aware drain pinning. Gaps worth filling:
- **No fsnotify rotation test** — `fsnotify_test.go` covers basic write detection, not rotation drain. The `fsnoNotify` watcher's logic mirrors `pollWatcher` but tests don't exercise the race-aware drain in the fsnotify path. Port `TestReadAfterWatcher_RaceDrain` to the fsnotify backend.
- **No real test of `WithFlock` over `fork`/separate process** — `flock_unix_test.go` only tests within-process conflict, which on most Unixes is a no-op for `flock(2)` (LOCK_EX from the same fd is re-entrant). To pin the cross-process semantics, fork a helper process (e.g., `os/exec` with a `-test.run=TestHelperProcess` pattern) that holds the lock; assert the parent gets `ErrLockHeld`. The current test passes because the OS happens to give per-fd-table locks; on Linux that's actually correct because `flock(2)` is inherited per fd, but the test wouldn't catch a regression to e.g. `fcntl(F_SETLK)` (which has different semantics).
- **No goroutine-leak detection** — using `go.uber.org/goleak` (or a hand-rolled `pprof.Lookup("goroutine")` snapshot at TestMain start/end) would catch issue #8 above (and any future leaks). Especially valuable in `forward_test.go`.
- **No race-detector run wired into the test plan** — `go test -race ./...` should be the default for any concurrency-heavy library. CI config wasn't visible in the tree; add it.
- **`TestForwarder_RetryOnError`** asserts `failing.Attempts() != 3` but doesn't assert what *batch* was sent on the success — could miss a bug where a different batch gets re-decoded after retry.
- **No end-to-end multi-file rotation test exercising checkpoint resume** — `TestTailer_RotatesAcrossLumberjackBackups` rotates without restart; `TestTailer_ResumeAcrossRestart` uses a single file. The combined case (commit on backup file N, restart, resume across backups N→active) is the core advertised use case and isn't directly tested.

## Top three things to fix this week

1. **Windows inode (issue #1)** — wire `fileInodeFromHandle` through, or auto-fallback to size-based detection on Windows.
2. **`forward` feeder goroutine leak (issue #8)** — switch to `errgroup` so `Run` returns clean up the feeder.
3. **`LineReader.Close` race vs. `LineReader.Next` (issue #11)** — either fix the synchronization to match the documented "Close may be called from any goroutine" contract, or relax the contract.

Everything else is polish, modernization, or doc tightening. The bones are in good shape.
