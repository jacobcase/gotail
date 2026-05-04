# gotail — Audit Context Model

- **Conducted:** 2026-05-04
- **Refreshed:** 2026-05-04 — re-verified against live tree; added drift since first pass (Stats, CloseWithFlush, OnInodeMismatch, cursor SyncMode, WithCursorMigration, Source.Done wiring, BackoffJitter, path-first findFileByInode tie-break). Removed the "no library-spawned long-lived goroutines" invariant (now false under SyncBackground / Forwarder.Run).
- **Scope:** `cmd/gotail`, `tail`, `watch`, `forward`, `internal/atomicwrite`, `tailtest`, `forwardtest` (v2 tree; `v1/` excluded).
- **Purpose:** Capture trust boundaries, data-flow surfaces, concurrent actors, load-bearing invariants, and reasoning hazards. **No findings, no fixes** — this is the model that future audit passes will reason against.

---

## 1. Architecture at a glance

Three layers; each is independently importable:

```
forward (L3)  — generic batched, retried at-least-once shipper to a Sink
   │
tail    (L2)  — durable cursor + multi-file series + rotation/truncate handling
   │
watch   (L1)  — file-as-stream: fd ownership, line framing, state events
```

The CLI (`cmd/gotail`) is a tiny consumer of L2. The forward layer’s canonical
`RecordSource` is `*tail.Tailer`, but the contract is an interface so a third
party can substitute any source with `Next/Commit/Done`.

Packages `tailtest` and `forwardtest` are test-only fakes (`MemorySource`,
`RecordingSink`, `FailingSink`) and are part of the public API.

---

## 2. Entrypoints

### 2.1 Process entrypoint
`cmd/gotail/main.go:27` — single CLI binary with three flags (`-start`, `-stop`,
positional path). Wires `signal.NotifyContext(SIGINT, SIGTERM)` to a
`tail.Tailer` and pipes lines to `bufio.NewWriter(os.Stdout)`. The CLI is the
*only* in-tree caller of L2; everything else is library API for embedders.

External inputs reaching this entrypoint:
- `os.Args` → `flag` → `path` (used verbatim as `tail.SingleFile(path)`).
- `os.Signal` from the kernel.
- `os.Stdout` (write side; not data input).

### 2.2 Library entrypoints (callable by embedders)
- `tail.New(ctx, Options) (*Tailer, error)` — primary L2 constructor. The
  `Options` struct is the **caller→library trust boundary** for L2 (paths,
  hooks, policies, file-mode, dir-sync flag, etc.).
- `tail.NewFileCursor(path, …Option) (Cursor, error)` — opens an on-disk JSON
  cursor file with optional sibling flock. Cursor options:
  `WithDirSync`, `WithFileMode`, `WithFlock`, `WithCursorMigration`,
  `WithSyncMode` (`SyncAlways` / `SyncOnCommit` / `SyncBackground`),
  `WithSyncBackgroundInterval`. `SyncBackground` starts a long-lived
  background flusher goroutine inside `NewFileCursor` (see §3.1).
- `tail.NewMemoryCursor()` — in-memory cursor for tests.
- `tail.SingleFile / StaticSource / Lumberjack / Logrotate / Glob` — `Source`
  factories. Each pulls inputs from the filesystem layout.
- `forward.New[T](Options[T]) (*Forwarder[T], error)` — L3 constructor.
- `forward.WithSinkTimeout[T](d) Sink-middleware`.
- Iterator entrypoints: `(*Tailer).Records(ctx) iter.Seq2[Record, error]` and
  `(*Tailer).Next(ctx)`.
- Tailer accessors callable from any goroutine: `(*Tailer).Position()`,
  `(*Tailer).Stats() Stats` (atomic counters: `BytesRead`, `LinesYielded`,
  `Rotations`, `Errors`, `Position`; counters survive `Close`),
  `(*Tailer).Done() <-chan struct{}` (closed only in `StopAtEOF` mode).
- Lifecycle: `(*Tailer).Close() error` (idempotent; discards uncommitted
  position) and `(*Tailer).CloseWithFlush(ctx) error` (idempotent — shares
  `closeOnce` with `Close`; saves the most-recently-yielded position via
  `Cursor.Save` before tearing down). The two are interchangeable on the
  `closeOnce` guard: only the first call wins, so a caller cannot undo a
  prior plain `Close` by calling `CloseWithFlush`.
- Cursor extension: `Syncer interface { Sync(ctx) error }` — implemented by
  `FileCursor` only when configured with `SyncOnCommit` or `SyncBackground`.
  Type-assert from `Cursor` to drive a manual flush.

### 2.3 Filesystem entrypoints (data-bearing)
Every byte that flows out as a `Record.Line` originated from one of these:
- The active log file `os.Open(path)` in `LineReader.switchToFile`
  (`watch/linereader.go:231`).
- Backup files enumerated by a `Source` and opened the same way after a rotation.
- A cursor file `os.ReadFile(c.path)` parsed as JSON in `FileCursor.Load`
  (`tail/cursor.go:113`).

### 2.4 OS-event entrypoints
- `fsnotify.Watcher` events on the watched **directory** (not the file)
  — `fw.Events`, `fw.Errors` (`watch/fsnotify_unix.go:200,207`). The watcher
  filters to entries whose `filepath.Clean(ev.Name) == w.target` and discards
  the rest. Errors are logged at warn level and the loop continues.
- Polling: `time.NewTimer(p.c.Interval)` (`watch/poll.go:196`). No external
  channel; just stat-driven.

### 2.5 Caller-supplied callable surfaces (executed inside library hot paths)
Every hook is **synchronous** and runs inside the read or batching loop. The
library treats hook authors as trusted.

`tail.Options`:
- `OnDropped(int)` — fired from `New` *before any Tailer struct exists*
  (during the `FallbackOldest` branch of cursor resolution; only the
  `opts` value has been bound). A hook closure that captures the soon-to-be
  Tailer pointer would observe a nil/zero target.
- `OnRotated(from, to Position)` — fired from `Tailer.advance`, holds the
  read loop until it returns.
- `OnError(err)` — fired from `Tailer.Next` before returning a non-EOF error.
- `OnTruncated(at Position)` — fired by `LineReader.handleEvent` and by the
  late-truncation detector in `LineReader.Next` (the path that catches a
  truncate the watcher’s `pos` watermark missed). Fires *before* the offset
  reset, so a hook reading `Tailer.Position()` mid-fire sees the
  pre-reset offset.
- `OnCheckpoint(c Checkpoint)` — fired from `Tailer.Commit` /
  `CommitWithMeta` / `CloseWithFlush` after a successful `Cursor.Save`.
- `OnInodeMismatch(want, got uint64)` — fired by `tail.New` before the
  resume/fail decision is made (so observers see the mismatch even when
  `FailOnInodeMismatch` is set), and by the watcher's `openFirst` if the
  cursor's inode does not match the file currently at the path.
- Source-level hooks: `WithLumberjackSkippedHook`, `WithLogrotateSkippedHook`
  — fired synchronously from `Source.Enumerate`.

`forward.Options[T]`:
- `OnBatchSent(records int, bytes int, pos Position, latency time.Duration)`
  — fires after `Sink.Send` returns nil and `Source.Commit` runs; `latency`
  measures the *successful* `Send` call only (excludes batch fill time and
  excludes failed retry attempts).
- `OnSendError(err, attempt, willRetry)` — `willRetry=false` only on
  `ErrPermanent`; ctx cancellation during a backoff sleep returns `ctx.Err`
  from `sendWithRetry` *without* firing `OnSendError`.
- `OnCommitted(pos)` — fires after `Source.Commit` (a commit error is
  logged at warn but does not block the hook firing).
- `OnDecodeError(line, pos, err)` — fires synchronously when `Decoder`
  returns non-nil; `batchLastPos` advances and the loop continues.
- `OnBackoffSleep(d, attempt)` — fires before each retry sleep.

`forward.Decoder[T]` — runs once per record; arbitrary user code.
`forward.Sink[T]` — runs once per batch; arbitrary user code; the only
networked surface in the codebase. The library has no built-in HTTP/gRPC
sink (the cookbook documents one, but it is caller code).
`tail.CursorMigrator(version int, raw []byte) (Checkpoint, error)` —
caller-supplied; runs inside `FileCursor.Load` when the on-disk version
does not match `cursorVersion (=1)`. Output is persisted back via
`Cursor.Save` so subsequent loads bypass the migrator.

---

## 3. Actors and the goroutines they run on

The library is concurrency-light by design. Most state machines run on the
caller’s goroutine; a few extras come from `fsnotify` and `context.AfterFunc`.

### 3.1 Internal actors
| Actor | Goroutine | Owns |
|-------|-----------|------|
| **Reader/owner** — drives `Tailer.Next` (and so the `LineReader.Next` and `Watcher.Wait` calls) | The caller’s goroutine that ranges over `Records` or calls `Next` directly | Single-fd to the active file (`LineReader.f`), the read buffer (`LineReader.buf`), the watcher’s polling/fsnotify state, the `Tailer.lr` field, `Tailer.fileIdx`, `Tailer.atActive`, `Tailer.whenceUsed`, `pendingNewFile`, `pendingNewPos` |
| **Position-mutator** | Same goroutine as Reader (writes `Tailer.cur` under `t.mu`) | `Tailer.cur`, `Tailer.lastMeta` |
| **Position-reader / Committer** | Any goroutine | Reads `Tailer.cur`/`lastMeta` under `t.mu`; writes through `Cursor.Save` |
| **Stats reader** | Any goroutine; reads `tailerStats` atomics independently | Counters survive `Close`; snapshot is not transactionally consistent |
| **Closer** | Any goroutine; idempotent via `closeOnce` (shared between `Close` and `CloseWithFlush`) | Cancels `closeCtx`; awaits `activeNext` WaitGroup; then (CloseWithFlush only) saves position; then closes `lr` and `cursor` |
| **AfterFunc cancel** | The Go runtime runs this when `closeCtx` is cancelled | Cancels the per-call `callCtx` so a parked `LineReader.Next` returns |
| **fsnotify reader** | Inside `fsnotify` | Pushes events into `fw.Events`/`fw.Errors`; we read from those channels in the Reader goroutine |
| **Cursor background flusher** (only when `WithSyncMode(SyncBackground)`) | Library-spawned in `NewFileCursor`; lives until `Cursor.Close` (which `close(stopBg)` and waits on `bgDone`) | Calls `(*FileCursor).Sync(context.Background())` on every tick of the configured interval (default `DefaultSyncBackgroundInterval = 1s`), and once more on `stopBg` before exit |
| **Forwarder run-loop** | Whatever goroutine calls `Forwarder.Run(ctx)` | `batch`, `batchBytes`, `batchLastPos`, `batchStart`, retry loop |
| **Forwarder Done-watcher** | Library-spawned at the top of `Forwarder.Run`; lives until Run returns | Selects between `runCtx.Done()` and `Source.Done()`; on the latter calls `runCancel()` so the next `Source.Next` returns and a final `flush()` runs against the parent ctx. Joined via `<-doneWatcherDone` in Run's `defer` (LIFO order: `runCancel()` then await join) |

Library-spawned goroutines are bounded:
- `Forwarder.Run`'s Done-watcher exists only while `Run` runs.
- The cursor background flusher exists only when `WithSyncMode(SyncBackground)`
  was selected; its lifetime is bracketed by `NewFileCursor` →
  `(*FileCursor).Close`. Tests use `goleak` to verify shutdown.

`fsnotify` itself spawns its own internal readers; the library only consumes
its public channels (`fw.Events`, `fw.Errors`).

### 3.2 External actors (untrusted)
| Actor | Reaches us via |
|-------|----------------|
| **Log writer** (the application producing the watched file) | Bytes in the watched file → `LineReader.buf` → `Decoder` → `Sink`. The library does no content validation past `\n`/`\r` framing. |
| **Log rotator** (lumberjack v2 in-process, or external logrotate / docker / systemd-journald) | Renames active path away → `(stat path).inode` changes → `Watcher` fires `ReOpened` event → `LineReader` drains old fd then `os.Open`s the new path. |
| **External cursor-file editor** | Anything with write access to the cursor path can rewrite the JSON; on next `Load` we trust whatever is parseable. |
| **Adjacent-directory writer** | Can drop files matching the lumberjack/logrotate naming pattern into the watched directory; `Source.Enumerate` will pick them up. |
| **fsnotify event source (kernel)** | Inotify/kqueue events flow through the OS; the kernel decides what fires. We trust event ordering and `ev.Name` only after `filepath.Clean`-equality with our cached target. |
| **Sink callee** (HTTP API, message broker, etc.) | The Sink wraps it; its return value is interpreted (`nil` → commit, `errors.Is(_, ErrPermanent)` → abort, anything else → retry forever). |
| **Cursor lock-holder** | Another process holding the sibling flock causes `NewFileCursor` to fail with `ErrLockHeld`. |

---

## 4. Trust boundaries (where adversarial input crosses)

1. **Caller → library (Options structs).** Trusted. Hooks, paths, file modes,
   intervals come from the embedder.
2. **Filesystem → process (watched file content).** Hostile by default. Lines
   are framed by `\n`, optionally trimmed of trailing `\r`, then handed to
   `Decoder`. The library imposes:
   - `MaxLine` (default 1 MiB) on a single line; `ErrLineTooLong` recovers by
     skipping to the next `\n` (`watch/linereader.go:269`).
   - `BufferSize` initial allocation (default 64 KiB), grown to `MaxLine+1`.
   - **No** validation of line content otherwise.
3. **Filesystem → process (cursor file content).** JSON unmarshal in
   `FileCursor.Load`. Bound: `cursorVersion == 1` *unless* the embedder
   passed `WithCursorMigration(fn)`, in which case any other version
   triggers `fn(version, raw)` — the migrator receives the entire on-disk
   bytes verbatim and returns a `Checkpoint`; the result is then
   re-persisted via `(*FileCursor).Save` so subsequent loads bypass the
   migrator. `maxRawMetaBytes` (64 KiB) is enforced on **both** `Load`
   (post-unmarshal `len(cf.Meta)` check) and `Save`. The total cursor file
   size is otherwise unbounded — `os.ReadFile` reads whatever is on disk.
   The `Position.File` field is *not* used as a path within `LineReader`;
   `findFileByInode` walks the *enumerated* `files` list and matches
   inodes, so a rogue cursor cannot redirect reads to attacker-chosen paths
   *via the inode-match path*. However, when `NoInodeCheck` is set, the
   tie-break **does** consult `Position.File`: `findFileByInode` first
   tries to match the cursor's named path against `files`; only on miss
   does it fall back to the first existing file. The cursor's path therefore
   selects-among the source-enumerated paths but never escapes them. The
   `Inode` and `Offset` fields are uint64/int64 (encoded as quoted JSON
   strings to survive IEEE-754 doubles) and are used as-is for inode
   comparison and seek; resume-offset is clamped to the current file size
   at `openFirst` time.
4. **Filesystem layout → process (Source enumeration).** `os.ReadDir(dir)` for
   Lumberjack; `filepath.Glob(activePath + ".*")` for Logrotate; arbitrary
   `filepath.Glob(pattern)` for `Glob`. The set of enumerated paths is whoever
   populates the directory plus our active path.
5. **fsnotify events → process.** Events are filtered by exact
   `filepath.Clean` match against the watched path; non-matching events are
   discarded. Events have no length cap, but they’re dropped before any
   action. Symlink resolution is **not** performed (`filepath.Clean` only
   normalises lexical components).
6. **Sink → process (Forwarder.Run).** A misbehaving Sink can:
   - Block forever on `Send` — Forwarder offers no per-Send timeout unless
     the caller wraps it with `WithSinkTimeout`.
   - Return `ErrPermanent` to abort `Run` (single error sentinel).
   - Return any other error to trigger exponential-jittered backoff with no
     attempt cap (`forward/forward.go:206`); only `ctx.Done()` breaks the
     loop.
7. **Hooks → library state.** Hook code runs inside the read loop. A panic
   propagates up through `Next`/`Records`/`Run`. A blocking hook stalls
   record delivery, rotation handling, and cursor commits.
8. **CursorMigrator → library state.** The migrator receives raw on-disk
   bytes and returns a `Checkpoint`; on success its output is persisted
   back to disk via `(*FileCursor).Save` from inside `Load`. The Save
   write happens through the configured `SyncMode` — under
   `SyncOnCommit`/`SyncBackground`, the migrated checkpoint is buffered
   not flushed.

---

## 5. State mutations and ownership

### 5.1 `tail.Tailer`
- `t.lr` — single-writer (set by `New`, replaced by `advance` which only runs
  inside `Next`). `Close` reads it after `activeNext.Wait()` parks all
  in-flight `Next`s.
- `t.cur`, `t.lastMeta` — under `t.mu`. Written by `Next` (cur) and by
  `Commit`/`CommitWithMeta` (lastMeta). Read by `Position()`, `Commit()`,
  `CommitWithMeta()`.
- `t.fileIdx`, `t.atActive`, `t.whenceUsed` — read/written only on the
  reader goroutine (`Next`/`advance`/`openFile`).
- `t.done` — closed exactly once via `t.doneOnce` (in `Next` on active-file
  EOF, or in `advance` on series exhaustion in `StopAtEOF` mode).
- `t.closeCtx`, `t.closeCancel` — `closeCancel` invoked exactly once via
  `closeOnce`. Cancellation flows through `context.AfterFunc` to a per-call
  `cancel` registered at the top of `Next`.
- `t.activeNext` — `Add(1)` at the top of `Next`, `Done()` deferred.
  `Close` does `closeCancel()` first then `Wait()`.
- `t.stats` (`tailerStats`) — four `atomic.Int64`s (`bytesRead`,
  `linesYielded`, `rotations`, `errors`) read/written without `t.mu`.
  Reads happen via `Stats()` which loads each field independently — the
  snapshot is **not** transactionally consistent. Counters are not reset
  by `Close` and remain readable after teardown.

### 5.2 `watch.LineReader`
- Owns the only fd to the active file: `f`, `src`. After `ReOpened` during a
  rotation, the *old* fd remains open until EOF (kernel keeps the
  rotated-out inode alive while we hold the fd) — that is how trailing
  bytes drain. `pendingNewFile` / `pendingNewPos` hold the deferred new file.
- `pos` (logical position), `head`, `tail`, `buf` — all single-goroutine.
  Buffer aliasing rule: the slice returned by `Next` is invalidated by the
  next `Next` or `Close`.

### 5.3 `watch.pollWatcher` / `watch.fsnotifyWatcher`
- Internal state (`started`, `pos`, `inode`, `resume`, `whence`) is
  single-goroutine. `Wait` is documented not safe for concurrent use.
- `pos` is a *watermark*: the last file size we have **announced** to the
  consumer, not the consumer's read position.
- `resume` is a one-shot pointer; nilled out the first time it’s consumed.
- `whence` is reset to `io.SeekStart` after first use, so subsequent open
  events always start at offset 0.
- The `fsnotify` watcher caches `target := filepath.Clean(c.Path)` once at
  construction and uses it for every event match.

### 5.4 Cursor I/O
- `FileCursor.Save`:
  1. ctx-check.
  2. Reject if `len(cp.Meta) > maxRawMetaBytes`.
  3. Branch on `SyncMode`:
     - `SyncAlways` (default): call `flush(cp)` immediately.
     - `SyncOnCommit` / `SyncBackground`: take `mu`, write
       `c.pending = cp`, `c.dirty = true`, return nil — no disk I/O on
       this call.
- `FileCursor.flush(cp)` (called from Save under SyncAlways and from
  `Sync` under the buffered modes): marshal `cursorFile{Pos, Meta, Version}`
  → hand off to `atomicwrite.Write`: open `path+".tmp"` with mode (default
  `0o600`) → write → fsync → close → `os.Rename(tmp, path)` → optional
  parent-dir fsync (default on, `WithDirSync(false)` to disable).
- `FileCursor.Sync(ctx)` (Syncer extension): ctx-check; under `mu` snapshot
  `pending` and clear `dirty`; if not dirty return nil; otherwise call
  `flush(pending)` outside the lock. Implemented only by `FileCursor`;
  `MemoryCursor` does not satisfy `Syncer`.
- `FileCursor.Load`:
  1. ctx-check, `os.ReadFile`. NotExist → `(zero, false, nil)`.
  2. `json.Unmarshal` into `cursorFile`.
  3. `len(cf.Meta) > maxRawMetaBytes` → error.
  4. Version check: if `cf.Version != cursorVersion`:
     - No migrator → wrap and return `ErrUnsupportedCursorVersion`.
     - Migrator present → call `migrate(version, raw)`. On migrator error,
       wrap **both** the migrator error and `ErrUnsupportedCursorVersion`
       (`fmt.Errorf("…: %w: %w", merr, ErrUnsupportedCursorVersion)`) so
       existing `errors.Is(_, ErrUnsupportedCursorVersion)` callers still
       match. On migrator success, call `c.Save(ctx, migrated)` to persist
       in the new schema; if Save fails the *Save* error is returned
       (without ErrUnsupportedCursorVersion wrap).
  5. Return `Checkpoint{Pos, Meta}, true, nil`.
- Background flusher (only `SyncBackground`): ticker at the configured
  interval calls `_ = c.Sync(context.Background())`; on `<-stopBg`, runs
  one final `Sync(context.Background())` then closes `bgDone`. Note: the
  background flusher uses `context.Background()`, not any caller ctx —
  there is no propagated cancellation path for in-flight `flush` writes.
- `flock` (optional, sibling-of-cursor file): exclusive non-blocking
  advisory lock; `EWOULDBLOCK` (Unix) / `ERROR_LOCK_VIOLATION (=33)`
  (Windows) → `ErrLockHeld`. Best-effort PID write to the lockfile after
  the lock is acquired; PID is **not** load-bearing for correctness.
  Released in `Cursor.Close` *after* the background flusher has stopped.

### 5.5 `forward.Forwarder`
- Run-loop owns: `batch`, `batchBytes`, `batchLastPos`, `batchStart`. Reset
  inside `flush()`.
- Run wires three contexts:
  1. Parent `ctx` — the caller's. Used by `flush()` and `sendWithRetry`
     so an in-flight batch survives `Source.Done()` arrival.
  2. `runCtx, runCancel := context.WithCancel(ctx)` — cancelled on
     parent-ctx Done **or** `Source.Done()` channel close, by a watcher
     goroutine (see §3.1). `runCtx` is the default per-`Source.Next` ctx.
  3. `nextCtx` — derived from `runCtx` via
     `context.WithDeadline(runCtx, batchStart.Add(MaxBatchAge))` only
     when a non-empty batch is in flight and `MaxBatchAge > 0`. The
     local `cancel` is invoked immediately after each `Source.Next`
     (not deferred) to release timer resources.
- Error disambiguation after `Source.Next` returns err:
  - `ctx.Err() != nil` → return `ctx.Err()` (parent cancelled).
  - `runCtx.Err() != nil` (parent ok) → `Source.Done()` fired; flush and
    return `nil`.
  - `errors.Is(err, context.DeadlineExceeded)` → batch-age timeout; flush
    and continue.
  - `errors.Is(err, tail.ErrSourceExhausted)` → flush and return `nil`.
  - otherwise return `err`.
- `sendWithRetry` retry loop is unbounded except by `ctx.Done()` (parent
  ctx — `Source.Done()` does **not** abort in-flight retries). Backoff is
  computed by `jitteredBackoff(attempt)`:
  `ceiling = InitialBackoff << min(attempt, 62)`, clamped to
  `MaxBackoff`; `jitter = BackoffJitter` (validated to `[0, 1]`, default
  `0.2`); returned duration is
  `base + rand.Int64N(ceiling - base)` where
  `base = ceiling * (1 - jitter)`. So default behaviour is
  `[0.8·ceiling, ceiling)`; `BackoffJitter=1` gives full jitter
  `[0, ceiling)`; `BackoffJitter=0` is rejected, normalized to default
  before construction.
- On send success: `Source.Commit(ctx, pos)` is called. A commit error is
  logged at warn but does *not* prevent `OnCommitted` / `OnBatchSent` from
  firing.

---

## 6. Load-bearing invariants

Any audit reasoning that contradicts these has either found a real bug or has
misread the code.

### Architectural / I/O
1. **Single fd per active file.** The watcher does *not* hold an fd to the
   active file; the LineReader does. `statSizeInode(path)` is the watcher’s
   only way to observe state. (On Windows `statSizeInode` is open-stat-close.)
2. **Trailing-bytes drain depends on kernel-keeping-rotated-inode-alive
   semantics.** When a rotation event arrives, the LineReader keeps reading
   the existing fd until `io.EOF` and only then opens the new path. This is
   correct on every Unix and on Windows with `FILE_SHARE_DELETE`-friendly
   handles; it is the central correctness property of L1.
3. **Watcher pos is a watermark, not the consumer’s position.** Reasoning
   about “did we miss data” must use `LineReader.pos.Offset` for what we have
   *consumed*, and `pollWatcher.pos` / `fsnotifyWatcher.pos` for what we
   have *announced as available*.
4. **Inode is the trust anchor for “same file”.** `findFileByInode`,
   resume-inode checks, and rotation detection all assume `StatInode` returns
   a stable identity. `NoInodeCheck` opts out (Windows ReFS, FUSE, network
   FS); when set, `findFileByInode` does a **path-first** tie-break — it
   prefers `wantPath` (the cursor's named file) when that path still exists
   in the source enumeration, and only falls back to the first-existing
   file otherwise. Resume offset is applied unconditionally if
   `r.Offset <= size` once the file has been chosen.
5. **Source.Enumerate is stable** until a file has been fully consumed and
   pruned (documented contract on `Source`, `tail/source.go:18`).
6. **Source returns oldest-first; active is last.** Lumberjack/Logrotate
   sort backups by age, append active.

### Concurrency / lifecycle
7. **`Tailer.lr` is single-writer.** Only `New` and `advance` set it;
   `advance` only runs inside `Next`. `Close` reads it after
   `activeNext.Wait()`.
8. **`Tailer.Close` cancels `closeCtx` *before* awaiting `activeNext`.**
   Any subsequent `Next` Adds to the WaitGroup, then immediately observes
   `closeCtx.Err()` at the top and returns `ErrSourceExhausted` (note:
   *not* `ctx.Err()`). The `WaitGroup.Add-after-Wait` race window is gated
   by this early check.
9. **`context.AfterFunc(closeCtx, cancel)` wires Close into the per-Next
   ctx.** A blocking `LineReader.Next` (parked on `Watcher.Wait`) is
   unblocked by Close. The post-EOF flow then sees `closeCtx.Err()` and
   reports `ErrSourceExhausted` instead of `ctx.Err()`, even when the
   original parent `ctx` is still live.
10. **`Tailer.done` is `Close`d at most once** via `doneOnce`. Done fires
    only in `StopAtEOF` mode (active-file EOF) or when `advance` walks off
    a finite source.
11. **`closeOnce` makes `Close` idempotent.** First call performs teardown;
    subsequent calls return the cached error.
12. **Buffer aliasing.** `LineReader.Next`’s returned `[]byte` is valid only
    until the next `Next`/`Close`. `IdentityDecoder` propagates this aliasing
    to the Sink. `IdentityDecoderCopy` and `JSONDecoder` break the alias.
    `RecordingSink` (test-only) also copies on `Send`.
13. **`Tailer` is not safe for concurrent `Next` callers**, but `Position`,
    `Commit`, `CommitWithMeta`, and `Close` may be called from any
    goroutine. (`mu` guards `cur`/`lastMeta`; `closeOnce` guards Close;
    `activeNext` synchronises Close vs Next.)
14. **`Forwarder.Run` is one-shot.** Re-running requires constructing a new
    Forwarder. There is no internal goroutine other than the caller’s.
15. **`flock` is per-fd and advisory.** Closing the fd or rename-over-the-
    cursor-file would silently drop POSIX flock; the lockfile must be a
    sibling, never the cursor file itself. The library enforces this only
    by API: `WithFlock(lockPath)` is separate from the cursor path.

### Durability
16. **`atomicwrite.Write` ordering** (post-perf-review): write → fsync →
    close → rename → optional parent-dir fsync. Tmp-file cleanup happens
    on every error path. Dir fsync is best-effort (silently skipped on FS
    that don’t support it).
17. **Cursor JSON schema is a wire commitment.** `Position.Inode` and
    `Position.Offset` are encoded as quoted strings so consumers using
    IEEE-754 doubles don’t lose precision past 2^53.
18. **`OnCheckpoint` only fires on a successful `Cursor.Save`.** It does not
    fire if marshal or save errored. Under `SyncOnCommit`/`SyncBackground`
    "successful Save" means "buffered in memory" — the on-disk file may
    still be a previous version; `OnCheckpoint` is *not* an fsync barrier.
19. **`Cursor` is owned by the Tailer's lifecycle.** Both `Tailer.Close`
    and `Tailer.CloseWithFlush` call `Cursor.Close` after the last
    in-flight `Next` returns; `closeOnce` is shared between them so only
    the first call's branch executes.
20. **Cursor migration is once-per-on-disk-file.** A successful migrator
    invocation calls `Save` from inside `Load`; the next `Load` sees the
    new schema and skips the migrator. Under `SyncOnCommit`/`SyncBackground`
    that "next Load" still sees the old on-disk bytes until `Sync`
    flushes the buffered Save — the migrator may run again on a cold
    process restart that follows a crash before flush.
21. **Cursor `Sync` is a no-op under `SyncAlways`** (every `Save` already
    fsyncs). Under the buffered modes it is idempotent: repeated `Sync`
    calls without an intervening `Save` skip the flush.

### Forwarder semantics
22. **At-least-once delivery.** `Source.Commit` only happens after
    `Sink.Send` returns nil. A Sink that succeeds but the commit fails
    leaves the cursor behind — next run replays the batch. The commit
    error is logged at warn level and **does not** abort the loop or
    suppress `OnCommitted`/`OnBatchSent`.
23. **Permanent-error escape hatch.** `errors.Is(err, ErrPermanent)` aborts
    `Run`. Any other Sink error retries forever (only parent `ctx.Done()`
    breaks it; `Source.Done()` does *not*). The library never *generates*
    `ErrPermanent` itself.
24. **Decode errors are skipped, not retried.** `OnDecodeError` fires;
    `batchLastPos` advances; the loop continues.
25. **Batch flush triggers** are OR'd: `MaxBatchRecords`, `MaxBatchBytes`,
    `MaxBatchAge`. At least one must be > 0 (`forward.New` rejects all-zero).
    `MaxBatchBytes` is accumulated from `len(rec.Line)`, not from the
    decoded `T`.
26. **`Source.Done()` semantics: one-shot exhaustion signal, not abort.**
    Closure of the Done channel triggers `runCancel()`, which causes the
    next `Source.Next` to return with `runCtx.Err()`; the loop then
    flushes the in-flight batch using the *parent* ctx and returns nil.
    Already-in-flight `sendWithRetry` is *not* aborted by Done.
27. **`BackoffJitter` validation.** `forward.New` rejects values outside
    `[0, 1]`; zero is normalized to `0.2`. `jitteredBackoff` uses
    `attempt` shifted up to 62 (`InitialBackoff << min(attempt, 62)`)
    before clamping to `MaxBackoff`, avoiding signed-shift undefined
    behaviour on long retry storms.

---

## 7. Reasoning hazards

Places where the model is subtle and easy to misread, ranked by how often
mistakes there would compound into a misdiagnosis.

### 7.1 Lifecycle and cancellation
1. **`Close()` swallows the user’s `ctx`.** When Close fires while `Next`
   is blocked, the caller sees `ErrSourceExhausted`, not `ctx.Err()`. Any
   audit that traces error contracts must remember: under Close, the
   sentinel wins.
2. **`AfterFunc` registration order matters.** `callCtx, cancel :=
   context.WithCancel(ctx); stop := context.AfterFunc(closeCtx, cancel);
   defer stop()`. If the order is changed (e.g. `stop` after the read), a
   leaked `AfterFunc` keeps the closure alive until `closeCtx` fires — a
   subtle GC/lifetime issue, not a correctness one.
3. **`activeNext.Add(1)` is fine after `Wait()` only because of the
   pre-check.** If the early `if closeCtx.Err() != nil` block is moved or
   removed, `Close` can race with a fresh `Next`.
4. **`OnDropped(1)` fires from inside `New` *before any Tailer struct
   exists*.** The call site is the `FallbackOldest` branch of cursor
   resolution, which runs before `t := &Tailer{…}` is constructed. A hook
   closure that captures the Tailer-to-be would see a nil/zero target;
   `tail.New`'s caller cannot pass `t` because they don't have it yet.
5. **`OnInodeMismatch` runs both at `tail.New` time (before the
   `OnMissingCheckpoint` resolution) and inside the watcher's `openFirst`
   (the first `Wait`).** A single Tailer construction can therefore fire
   the hook *twice* for the same logical event — once at the L2 surface
   when `findFileByInode` returns -1 but the cursor's path still resolves
   to a different-inode file, and once again inside the watcher when it
   stats the path. Hooks must be idempotent.
6. **`OnRotated`/`OnTruncated`/`OnError` fire synchronously inside
   `Next`/`advance`.** A blocking hook stalls record delivery and rotation
   handling.
7. **`Close` and `CloseWithFlush` share `closeOnce`.** A caller that
   `defer tr.Close()`s and then later calls `tr.CloseWithFlush(ctx)`
   gets a no-op flush — `Close` already won the once. The reverse holds
   too: `CloseWithFlush` followed by `Close` returns the cached error.
8. **`CloseWithFlush` saves position only if `t.cur != Position{}`.**
   A Tailer that hit `Close` before any record was yielded does not
   commit a zero position — but a zero position is also valid for
   "starting at offset 0 of inode 0", which is the genuine state right
   after a fresh `New`. The check is "did we yield anything", not
   "is the position meaningful".

### 7.2 Inode / identity
9. **`findFileByInode` behaviour depends on `noInodeCheck`.**
   - `noInodeCheck=false` (default): walks `files`, returns the index
     whose `StatInode` matches `want`, or -1. On Windows non-NTFS FS
     where `statSizeInode` returns inode 0 for *every* candidate, the
     inode-equality path degenerates to "first 0==want=0 wins".
   - `noInodeCheck=true`: **path-first tie-break**. First scans `files`
     for an entry equal to `wantPath` whose `StatInode` succeeds; if
     found, returns that index. Only on miss does it fall back to the
     first existing file. The cursor's `Position.File` therefore
     selects-among the source-enumerated paths but cannot escape them.
10. **`Position.File` from a cursor is dual-role.** It is *not* used as
    a path inside `LineReader` (paths come from `Source.Enumerate`). But
    it *is* consulted by the inode-mismatch detector
    (`watch.StatInode(cp.Pos.File)` inside `tail.New`'s mismatch branch)
    *and* by the `NoInodeCheck` tie-break above. A stale path string
    therefore can change which existing-source-file is chosen as
    "resume target" under `NoInodeCheck`, and can mis-trigger
    `OnInodeMismatch` if the named path no longer exists.
11. **fsnotify uses `filepath.Clean(c.Path)` only.** Symlinks are not
    resolved. If the parent dir contains a symlink that points the watched
    name elsewhere, `filepath.Clean` may not match the kernel-reported
    `ev.Name` for the resolved file.

### 7.3 Source enumeration
12. **Lumberjack file detection is suffix-based and content-blind.** Names
    matching `<stem>-<19-char-ts><ext>` are enumerated regardless of
    content. `<…>.gz` variants are detected by suffix and skipped via the
    `WithLumberjackSkippedHook`.
13. **Lumberjack timestamp parser accepts any well-formed `2006-01-02T15-04-05`
    string.** Future-dated and zero-dated names sort and enumerate normally.
14. **`Glob` source uses lexicographic sort.** `.10` < `.2` lexically; this
    is documented and superseded by `Logrotate` for numeric suffixes.
15. **`Logrotate` sort is descending by integer.** The largest `N` is
    treated as oldest.
16. **A new file appearing in the watched dir between rotations is
    enumeration-time data.** `Tailer.New` snapshots `files` once; `advance`
    walks that snapshot. Files added after construction are not visible
    until a fresh `New`.

### 7.4 Truncation and rotation
17. **Two truncation paths.** Watcher-detected truncation (`Event.Truncated`
    in `handleEvent`) and reader-detected late truncation (the EOF post-read
    `fi.Size() < l.pos.Offset` block in `LineReader.Next`). Both fire
    `OnTruncated` and reset to offset 0. The latter exists specifically for
    `copytruncate`-style rotations the watcher's size-watermark missed.
18. **Order of detection: rotation before truncation.** The watcher checks
    inode-change first, then size-drop; a rotate-onto-smaller-file would
    otherwise look like a truncation.
19. **The `pendingNewFile` drain.** Between a `ReOpened` event and the next
    `io.EOF` on the old fd, the LineReader still reads the old (now-orphaned)
    inode. New events that arrive during the drain still land in the watcher
    state machine: subsequent `Wait` calls observe inode equal to the new
    inode (because we updated `inode` at `ReOpened`), so further `ReOpened`
    events on a *third* rotation would reset `pos` again — but the drained
    old fd does not re-open.

### 7.5 Forward layer
20. **Indefinite retry under transient sink failure.** Without a parent
    `ctx` deadline or `WithSinkTimeout`, a hung Sink stalls `Run` forever.
21. **`MaxBatchAge` deadline is per-Next, not per-batch.** Implemented by
    deriving a context with deadline `batchStart + MaxBatchAge` and passed
    to `Source.Next`. The path that distinguishes "real ctx cancellation"
    vs "our derived deadline" is the `ctx.Err()` / `runCtx.Err()` pair —
    relying on the fact that the **parent** ctx's `Err()` is only non-nil
    if the parent itself is cancelled.
22. **Three-context disambiguation in Run.** Parent `ctx`, `runCtx`
    (parent OR Source.Done), and `nextCtx` (runCtx + per-Next deadline)
    are checked in that order after each `Source.Next` failure. Reordering
    or omitting any of these checks would mis-classify
    Source.Done-during-batch-age-deadline (or vice versa) and either
    drop the in-flight batch or retry forever.
23. **`OnSendError(err, attempt, willRetry)` semantics.** `willRetry=false`
    only on `ErrPermanent`; ctx cancellation during a backoff sleep returns
    `ctx.Err()` from `sendWithRetry` without firing `OnSendError`.
24. **`OnBatchSent.latency` measures only the successful Send.** Failed
    retries' durations are not summed in. A consumer treating `latency`
    as "total batch wall-time" will under-report under retry pressure.
25. **`MaxBatchBytes` is byte-of-line, not byte-of-T.** A Decoder that
    inflates a small line into a large T blows the byte budget downstream.
26. **Decoder failure does not retry, but the position still advances.**
    A persistent decode failure on every record will *commit* progress
    even though nothing was sent — at-least-once becomes at-most-zero for
    that record stream.
27. **`Source.Done()` does not abort `sendWithRetry`.** The Done-watcher
    cancels `runCtx`, but `sendWithRetry` is called with the *parent*
    ctx. A Source that signals exhaustion while a Sink is hung forever
    will still hang forever — Done is "no more new records", not
    "abort everything".
28. **Done-watcher goroutine ordering.** Run's defer is
    `runCancel(); <-doneWatcherDone` — the channel-receive happens
    *after* the cancel, so the watcher always observes runCtx.Done()
    on Run exit. Reordering would let the watcher leak past Run's
    return when Source.Done() never fires.

### 7.6 Cursor durability
29. **Cursor `Load` has no max file size.** `os.ReadFile(path)` reads
    whatever is there. (`maxRawMetaBytes` is enforced post-unmarshal on
    `len(cf.Meta)` only.)
30. **`maxRawMetaBytes` (64 KiB) is enforced on both Save and Load.**
    A cursor written by an older build with `Meta` larger than 64 KiB
    will fail Load.
31. **`SyncOnCommit`/`SyncBackground` introduce a crash window.** `Save`
    returns nil after writing `pending` in memory; the on-disk file lags
    until the next `Sync`. Crash window length is bounded by the user's
    Sync cadence (or `DefaultSyncBackgroundInterval = 1s`, configurable
    via `WithSyncBackgroundInterval`). `OnCheckpoint` fires per-Save in
    these modes and is therefore *not* an fsync barrier.
32. **Background flusher uses `context.Background()`.** The ticker calls
    `_ = c.Sync(context.Background())` regardless of any external ctx.
    A flush that hangs on a misbehaving filesystem will keep the
    goroutine wedged inside `atomicwrite.Write` until the OS unblocks
    it; `Cursor.Close` (which waits on `bgDone`) will block too.
33. **Cursor migrator runs Save inside Load.** A migrator that succeeds
    but whose Save fails returns the **save** error (not wrapped with
    `ErrUnsupportedCursorVersion`). The two error wrappings differ:
    migrator-call error wraps both `merr` and `ErrUnsupportedCursorVersion`
    (so `errors.Is` for the version sentinel still matches);
    migrator-Save error does *not* — callers using
    `errors.Is(err, ErrUnsupportedCursorVersion)` to detect "this was
    a version problem" will miss the post-migration save failure.
34. **Migration write goes through SyncMode.** Under
    `SyncOnCommit`/`SyncBackground` a migrated checkpoint is buffered
    rather than flushed; a crash before the next `Sync` re-presents the
    pre-migration on-disk bytes to `Load`, which will run the migrator
    again. Idempotent migrators are required.
35. **Lockfile PID is best-effort.** `f.Truncate(0)` then `WriteString(pid)`
    happen *after* the flock; failure is silently ignored. Audit reasoning
    must not treat the PID file as authoritative for who-holds-the-lock.
36. **`flock` on `!unix && !windows`** fails immediately with "not
    supported" (`tail/flock_other.go`).

### 7.7 Sizing and resource caps
37. **`tail.Options` does not surface `LineOptions.{BufferSize, MaxLine}`.**
    The Tailer constructs `LineReader` with zero-valued LineOptions
    (only `OnTruncated` is forwarded), so defaults apply: 64 KiB initial
    buffer, 1 MiB max line. Embedders cannot raise either limit without
    reaching into `watch.NewLineReader` directly (which would also
    require constructing the watcher manually — the Tailer's `openFile`
    is unexported).
38. **`os.ReadDir` in `Lumberjack.Enumerate` is unbounded.** A directory
    with millions of matching files would block at `New` time.
39. **`runtime.Stat` cost on Windows.** `findFileByInode` and `statSizeInode`
    each open + close a handle per candidate. With many backups this is
    measurable. Under `NoInodeCheck`, the path-first tie-break does *one*
    `StatInode` for the cursor's named path before falling back to scanning,
    so the common case is one open instead of N.

### 7.8 Hooks as covert paths
40. **Hooks are running with library-internal state held.** `OnTruncated`
    fires while `LineReader.Next` is on the path that resets `head`/`tail`/
    `pos.Offset`. A hook that calls back into `Tailer.Position()` sees the
    pre-reset position; the reset happens immediately after.
41. **`OnCheckpoint` runs after `Save` returns.** Under `SyncAlways` "Save
    returns" means "fsynced to disk". Under `SyncOnCommit`/`SyncBackground`
    it means "buffered in memory" — a hook that backs up the cursor file
    on every checkpoint will copy a stale on-disk version and miss the
    buffered update.
42. **Hooks may panic.** Panics propagate up through `Next` → `Records` →
    caller. There is no `recover` in the read or batching loops.
43. **`CursorMigrator` runs inside `Load` synchronously.** A migrator that
    blocks (e.g. on network I/O) blocks `tail.New`. A migrator that panics
    propagates out of `tail.New`. A migrator that mutates `raw` in place
    is unsafe — the slice aliases the bytes returned by `os.ReadFile`,
    which the library uses *before* the migrator returns to format error
    messages on failure paths.

---

## 8. Hostile-input reachability map

Following data from “untrusted source” to “effect we care about”:

| Source | First mediator | Final effect |
|--------|---------------|--------------|
| Watched-file bytes | `LineReader.buf` (line framing only) | Yielded as `Record.Line` to caller; passed verbatim to `Decoder` and `Sink` |
| Cursor JSON (matching version) | `FileCursor.Load` → `cursorFile` → `Checkpoint` | `Position.Inode` used in `findFileByInode`; `Position.File` consulted by `findFileByInode` under `NoInodeCheck` and by the inode-mismatch detector; `Position.Offset` used as `Resume.Offset` (clamped to file size); `Meta` returned to `OnCheckpoint` and re-saved verbatim (capped at 64 KiB) |
| Cursor JSON (mismatched version) | `WithCursorMigration(fn)` if registered, else `ErrUnsupportedCursorVersion` | Migrator receives raw on-disk bytes verbatim; its returned `Checkpoint` is `Save`d back to disk via `SyncMode` and then flows into the matching-version path above |
| Watched dir entries | `os.ReadDir` / `filepath.Glob` | Paths handed to `os.Open` in `LineReader.switchToFile` |
| fsnotify event Name | `filepath.Clean` equality with cached target | Drops non-matching events; otherwise unblocks `Wait` |
| Sink return value | `errors.Is(_, ErrPermanent)` and `ctx.Err()` checks | Permanent → abort `Run`; transient → backoff loop; nil → commit |
| `Source.Done()` channel | `Forwarder.Run`'s Done-watcher goroutine | Closure cancels `runCtx`; next `Source.Next` returns; in-flight batch flushes against parent ctx; Run returns nil. Does *not* abort `sendWithRetry`. |
| OS signals (CLI only) | `signal.NotifyContext` | Cancels `ctx`, which cascades to `Tailer.Next` |

The library does not parse, decompress, or interpret log content. There is no
HTTP server, no SQL surface, no template rendering, no shell exec, no XML/YAML
parsing, no compression. The attack surface is dominated by **file-system
input** (paths, contents, layouts) and **caller-supplied callbacks**.

---

## 9. Build-tag matrix

The same logic compiles into four meaningful flavours; each is part of the
audit surface:

| OS / build tag | Watcher selection | Inode strategy | Lock backend |
|----------------|-------------------|----------------|--------------|
| Linux / default | fsnotify (inotify), poll fallback | `syscall.Stat_t.Ino` (stat-only) | `syscall.Flock(LOCK_EX\|NB)` |
| macOS, *BSD / default | fsnotify (kqueue), poll fallback | `syscall.Stat_t.Ino` | `syscall.Flock` |
| Windows / default | poll (no fsnotify build) | `GetFileInformationByHandle` (open-stat-close) | `LockFileEx` |
| Any / `gotail_nofsnotify` | poll only | Same as platform | Same as platform |
| Plan-9 etc. (`!unix && !windows`) | poll | inode 0 | `acquireFlock` returns error |

Stub: `watch/fsnotify_stub.go` returns `ErrUnsupported` so `watch.New` falls
back to polling.

---

## 10. Test-only entrypoints (recap)

These are part of the library API and may be used in production tests, but are
not load-bearing for runtime correctness:

- `tail.StaticSource(paths)` — immutable.
- `tail.NewMemoryCursor()` — in-memory; mutex-guarded; does **not**
  implement `Syncer` (only `FileCursor` does).
- `tailtest.MemorySource{}` — mutable, supports mid-tail `Add` / `Prune`,
  uses `slices.Delete` on Prune. Snapshots return a copy.
- `forwardtest.RecordingSink[T]` — records batches; **copies on `Send`**
  (so it does not preserve buffer aliasing — a test relying on aliasing
  semantics needs a different sink).
- `forwardtest.NewFailingSink[T](n, failWith, inner)` — fails first `n`
  calls then delegates to `inner`. Default `failWith` is a generic
  errors.New value; default `inner` is a no-op success sink.
- `forward.IdentityDecoder` (aliasing) vs `forward.IdentityDecoderCopy`
  (allocates) vs `forward.JSONDecoder[T]()` (json.Unmarshal per line).

---

## 11. Drift from `V2_PLAN.md`

Moved to [`../V2_PLAN.md` §11 *Deviations*](../V2_PLAN.md#11-deviations) so the
plan and the deltas live alongside each other. Plan-vs-shipped reasoning that
used to live here now sits at the end of the design plan, with each deviation
linked back to the review section that drove it.

---

*End of context model. Findings, fixes, and recommendations belong in a
separate review document; this file is the shared mental model that those
documents reason against.*
