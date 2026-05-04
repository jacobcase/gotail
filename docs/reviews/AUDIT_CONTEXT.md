# gotail — Audit Context Model

- **Conducted:** 2026-05-04
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
  cursor file with optional sibling flock.
- `tail.NewMemoryCursor()` — in-memory cursor for tests.
- `tail.SingleFile / StaticSource / Lumberjack / Logrotate / Glob` — `Source`
  factories. Each pulls inputs from the filesystem layout.
- `forward.New[T](Options[T]) (*Forwarder[T], error)` — L3 constructor.
- `forward.WithSinkTimeout[T](d) Sink-middleware`.
- Iterator entrypoints: `(*Tailer).Records(ctx) iter.Seq2[Record, error]` and
  `(*Tailer).Next(ctx)`.

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
- `OnDropped(int)` — fired from `New` (yes, from inside the constructor, before
  the Tailer is fully wired).
- `OnRotated(from, to Position)` — fired from `Tailer.advance`, holds the
  read loop until it returns.
- `OnError(err)` — fired from `Tailer.Next` before returning a non-EOF error.
- `OnTruncated(at Position)` — fired by `LineReader.handleEvent` and by the
  late-truncation detector in `LineReader.Next` (the path that catches a
  truncate the watcher’s `pos` watermark missed).
- `OnCheckpoint(c Checkpoint)` — fired from `Tailer.Commit` /
  `CommitWithMeta` after a successful `Cursor.Save`.
- Source-level hooks: `WithLumberjackSkippedHook`, `WithLogrotateSkippedHook`
  — fired synchronously from `Source.Enumerate`.

`forward.Options[T]`:
- `OnBatchSent`, `OnSendError`, `OnCommitted`, `OnDecodeError`, `OnBackoffSleep`
  — all synchronous from `Forwarder.Run` / `sendWithRetry`.

`forward.Decoder[T]` — runs once per record; arbitrary user code.
`forward.Sink[T]` — runs once per batch; arbitrary user code; the only
networked surface in the codebase. The library has no built-in HTTP/gRPC
sink (the cookbook documents one, but it is caller code).

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
| **Closer** | Any goroutine; idempotent via `closeOnce` | Cancels `closeCtx`; awaits `activeNext` WaitGroup; then closes `lr` and `cursor` |
| **AfterFunc cancel** | The Go runtime runs this when `closeCtx` is cancelled | Cancels the per-call `callCtx` so a parked `LineReader.Next` returns |
| **fsnotify reader** | Inside `fsnotify` | Pushes events into `fw.Events`/`fw.Errors`; we read from those channels in the Reader goroutine |
| **Forwarder run-loop** | Whatever goroutine calls `Forwarder.Run(ctx)` | `batch`, `batchBytes`, `batchLastPos`, `batchStart`, retry loop |

There are **no library-spawned long-lived goroutines** in normal operation.
All event-driven work happens on the caller’s loop. `fsnotify` spawns its own
internal readers but the library only consumes its public channels.

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
   `FileCursor.Load`. Bound: `cursorVersion == 1`. There is no maximum file
   size on `os.ReadFile` of the cursor — `maxRawMetaBytes (64 KiB)` is only
   enforced on `Save`. The `Position.File` field is *not* used as a path;
   `findFileByInode` walks the *enumerated* `files` list and matches inodes,
   so a rogue cursor cannot redirect us to read attacker-chosen paths. The
   `Inode` and `Offset` fields are int64 (encoded as quoted JSON strings) and
   are used as-is for inode comparison and seek; resume-offset is clamped to
   the current file size at `openFirst` time.
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
  1. Marshal cursorFile struct.
  2. Reject if `len(cp.Meta) > maxRawMetaBytes`.
  3. Hand off to `atomicwrite.Write`: open `path+".tmp"` with mode (default
     `0o600`) → write → fsync → close → `os.Rename(tmp, path)` → optional
     parent-dir fsync (default on, `WithDirSync(false)` to disable).
- `FileCursor.Load`: `os.ReadFile`, `json.Unmarshal`, version check
  (`!= 1` → `ErrUnsupportedCursorVersion`).
- `flock` (optional, sibling-of-cursor file): exclusive non-blocking
  advisory lock; `EWOULDBLOCK` → `ErrLockHeld`. Best-effort PID write to
  the lockfile; PID is **not** load-bearing for correctness.

### 5.5 `forward.Forwarder`
- Run-loop owns: `batch`, `batchBytes`, `batchLastPos`, `batchStart`. Reset
  inside `flush()`.
- Per-Next deadline trick: when a non-empty batch is in flight and
  `MaxBatchAge > 0`, `Source.Next` is called with a derived
  `context.WithDeadline(ctx, batchStart.Add(MaxBatchAge))`. On
  `DeadlineExceeded`, the run loop flushes; on parent `ctx.Err() != nil`
  it aborts.
- `sendWithRetry` retry loop is unbounded except by `ctx.Done()`.

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
   FS); when set, `findFileByInode` returns the first existing path and
   resume offset is applied unconditionally if `r.Offset <= size`.
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
    fire if marshal or save errored.
19. **`Cursor` is owned by the Tailer’s `Close`.** `Tailer.Close` calls
    `Cursor.Close` after the last in-flight `Next` returns.

### Forwarder semantics
20. **At-least-once delivery.** `Source.Commit` only happens after
    `Sink.Send` returns nil. A Sink that succeeds but the commit fails
    leaves the cursor behind — next run replays the batch.
21. **Permanent-error escape hatch.** `errors.Is(err, ErrPermanent)` aborts
    `Run`. Any other Sink error retries forever (only `ctx.Done()` breaks
    it). The library never *generates* `ErrPermanent` itself.
22. **Decode errors are skipped, not retried.** `OnDecodeError` fires;
    `batchLastPos` advances; the loop continues.
23. **Batch flush triggers** are OR’d: `MaxBatchRecords`, `MaxBatchBytes`,
    `MaxBatchAge`. At least one must be > 0 (`forward.New` rejects all-zero).
    `MaxBatchBytes` is accumulated from `len(rec.Line)`, not from the
    decoded `T`.

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
4. **`OnDropped(1)` fires from inside `New`.** The Tailer is partially
   constructed (no `lr` yet — `openFile` runs *after* the hook). A hook that
   inspects the Tailer would see fields in flux.
5. **`OnRotated`/`OnTruncated`/`OnError` fire synchronously inside
   `Next`/`advance`.** A blocking hook stalls record delivery and rotation
   handling.

### 7.2 Inode / identity
6. **`findFileByInode` returns -1 vs first-existing depending on
   `noInodeCheck`.** When `noInodeCheck` is true, the *first existing* file
   in the enumerated list matches — resume lands at that path. On Windows
   with non-NTFS FS where `statSizeInode` returns inode 0, the inode-equality
   path can degenerate to “first 0==0 wins”. (Whether this is a bug is a
   review question; the model is: 0 inode is ambiguous.)
7. **`Position.File` from a cursor is advisory.** The path is stored, but
   resume targets are chosen by walking `Source.Enumerate` and comparing
   inodes — a malicious or stale path string in a cursor cannot redirect
   reads.
8. **fsnotify uses `filepath.Clean(c.Path)` only.** Symlinks are not
   resolved. If the parent dir contains a symlink that points the watched
   name elsewhere, `filepath.Clean` may not match the kernel-reported
   `ev.Name` for the resolved file.

### 7.3 Source enumeration
9. **Lumberjack file detection is suffix-based and content-blind.** Names
   matching `<stem>-<19-char-ts><ext>` are enumerated regardless of content.
   `<…>.gz` variants are detected by suffix and skipped via the
   `WithLumberjackSkippedHook`.
10. **Lumberjack timestamp parser accepts any well-formed `2006-01-02T15-04-05`
    string.** Future-dated and zero-dated names sort and enumerate normally.
11. **`Glob` source uses lexicographic sort.** `.10` < `.2` lexically; this
    is documented and superseded by `Logrotate` for numeric suffixes.
12. **`Logrotate` sort is descending by integer.** The largest `N` is
    treated as oldest.
13. **A new file appearing in the watched dir between rotations is
    enumeration-time data.** `Tailer.New` snapshots `files` once; `advance`
    walks that snapshot. Files added after construction are not visible
    until a fresh `New`.

### 7.4 Truncation and rotation
14. **Two truncation paths.** Watcher-detected truncation (`Event.Truncated`
    in `handleEvent`) and reader-detected late truncation (the EOF post-read
    `fi.Size() < l.pos.Offset` block in `LineReader.Next`). Both fire
    `OnTruncated` and reset to offset 0. The latter exists specifically for
    `copytruncate`-style rotations the watcher’s size-watermark missed.
15. **Order of detection: rotation before truncation.** The watcher checks
    inode-change first, then size-drop; a rotate-onto-smaller-file would
    otherwise look like a truncation.
16. **The `pendingNewFile` drain.** Between a `ReOpened` event and the next
    `io.EOF` on the old fd, the LineReader still reads the old (now-orphaned)
    inode. New events that arrive during the drain still land in the watcher
    state machine: subsequent `Wait` calls observe inode equal to the new
    inode (because we updated `inode` at `ReOpened`), so further `ReOpened`
    events on a *third* rotation would reset `pos` again — but the drained
    old fd does not re-open.

### 7.5 Forward layer
17. **Indefinite retry under transient sink failure.** Without a parent
    `ctx` deadline or `WithSinkTimeout`, a hung Sink stalls `Run` forever.
18. **`MaxBatchAge` deadline is per-Next, not per-batch.** Implemented by
    deriving a context with deadline `batchStart + MaxBatchAge` and passed
    to `Source.Next`. The path that distinguishes “real ctx cancellation”
    vs “our derived deadline” is `if ctx.Err() != nil` — relying on the
    fact that the **parent** ctx’s `Err()` is only non-nil if the parent
    itself is cancelled.
19. **`OnSendError(err, attempt, willRetry)` semantics.** `willRetry=false`
    only on `ErrPermanent`; ctx cancellation during a backoff sleep returns
    `ctx.Err()` from `sendWithRetry` without firing `OnSendError`.
20. **`MaxBatchBytes` is byte-of-line, not byte-of-T.** A Decoder that
    inflates a small line into a large T blows the byte budget downstream.
21. **Decoder failure does not retry, but the position still advances.**
    A persistent decode failure on every record will *commit* progress
    even though nothing was sent — at-least-once becomes at-most-zero for
    that record stream.

### 7.6 Cursor durability
22. **Cursor `Load` has no max file size.** `os.ReadFile(path)` reads
    whatever is there.
23. **`maxRawMetaBytes` is 64 KiB and applies only on `Save`.** A larger
    cursor on disk loads fine.
24. **Lockfile PID is best-effort.** `f.Truncate(0)` then `WriteString(pid)`
    happen *after* the flock; failure is silently ignored. Audit reasoning
    must not treat the PID file as authoritative for who-holds-the-lock.
25. **`flock` on `!unix && !windows`** fails immediately with “not
    supported” (`tail/flock_other.go`).

### 7.7 Sizing and resource caps
26. **`tail.Options` does not surface `LineOptions.{BufferSize, MaxLine}`.**
    The Tailer constructs `LineReader` with zero-valued LineOptions, so
    defaults apply: 64 KiB initial buffer, 1 MiB max line. Embedders cannot
    raise either limit without reaching into `watch.NewLineReader` directly
    (which would also require constructing the watcher manually — the
    Tailer’s `openFile` is unexported).
27. **`os.ReadDir` in `Lumberjack.Enumerate` is unbounded.** A directory
    with millions of matching files would block at `New` time.
28. **`runtime.Stat` cost on Windows.** `findFileByInode` and `statSizeInode`
    each open + close a handle per candidate. With many backups this is
    measurable.

### 7.8 Hooks as covert paths
29. **Hooks are running with library-internal state held.** `OnTruncated`
    fires while `LineReader.Next` is on the path that resets `head`/`tail`/
    `pos.Offset`. A hook that calls back into `Tailer.Position()` sees the
    pre-reset position; the reset happens immediately after.
30. **`OnCheckpoint` runs after `Save` returns.** A hook that itself touches
    the cursor file (e.g. takes its own backup) races with future `Save` calls.
31. **Hooks may panic.** Panics propagate up through `Next` → `Records` →
    caller. There is no `recover` in the read or batching loops.

---

## 8. Hostile-input reachability map

Following data from “untrusted source” to “effect we care about”:

| Source | First mediator | Final effect |
|--------|---------------|--------------|
| Watched-file bytes | `LineReader.buf` (line framing only) | Yielded as `Record.Line` to caller; passed verbatim to `Decoder` and `Sink` |
| Cursor JSON | `FileCursor.Load` → `cursorFile` → `Checkpoint` | `Position.Inode` used in `findFileByInode`; `Position.Offset` used as `Resume.Offset` (clamped to file size); `Meta` returned to `OnCheckpoint` and re-saved verbatim |
| Watched dir entries | `os.ReadDir` / `filepath.Glob` | Paths handed to `os.Open` in `LineReader.switchToFile` |
| fsnotify event Name | `filepath.Clean` equality with cached target | Drops non-matching events; otherwise unblocks `Wait` |
| Sink return value | `errors.Is(_, ErrPermanent)` and `ctx.Err()` checks | Permanent → abort `Run`; transient → backoff loop; nil → commit |
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
- `tail.NewMemoryCursor()` — in-memory; thread-safe.
- `tailtest.MemorySource{}` — mutable, supports mid-tail `Add` / `Prune`.
- `watch.FakeWatcher(path, pos)` — emits one ReOpened then EOF.
- `forwardtest.RecordingSink[T]` — records batches; copies on `Send`.
- `forwardtest.NewFailingSink[T](n, failWith, inner)` — fails first `n` calls.

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
