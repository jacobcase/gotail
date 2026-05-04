# gotail v2 — Implementation Task List

This is the executable task list for implementing v2 of gotail. Read [V2_PLAN.md](./V2_PLAN.md) for the full design rationale; this file is the actionable checklist.

**Working agreement for the implementing agent:**

- Work phases in order. Each phase is an independently mergeable PR.
- Within a phase, follow the sub-task order; later sub-tasks usually depend on earlier ones.
- Every phase ends with all CI gates green: `go vet ./...`, `go test -race ./...`, `staticcheck ./...`, cross-compile to linux/amd64+arm64, darwin/arm64, windows/amd64, freebsd/amd64.
- Section references like `§4` or `§5.3` point into V2_PLAN.md.
- Section references like `tail.go:57` point into the v1 source files.
- Every public type gets a doc comment. Every package gets a `doc.go`.
- Use `slog` with consistent attribute keys: `path`, `inode`, `offset`, `attempt`, `err`, `latency_ms`.
- Hooks are nil-safe: `if h != nil { h(...) }`.
- Error returns use sentinel errors that compose with `errors.Is`/`errors.As`.

---

## Phase 0 — Project housekeeping

**Goal:** prepare the repo for v2 work. No library code yet.

- [ ] Create a `v2` branch off `main` (already on `v2-design-plan`; rebase or branch from there as appropriate).
- [ ] Bump `go.mod` directive to `go 1.26` (or later if 1.26 is GA at implementation time).
- [ ] Update module path to `github.com/jacobcase/gotail/v2` per Go module versioning convention (Decision #1).
- [ ] Remove `golang.org/x/sys` from `go.mod` (Decision #4 — stdlib `syscall` is sufficient).
- [ ] Add `.golangci.yml` enabling: `govet`, `errcheck`, `ineffassign`, `unused`, `gosimple`, `staticcheck`.
- [ ] Add `.github/workflows/ci.yml` with matrix: `{linux, macos, windows} × {1.26}`. Run `go vet`, `go test -race`, `staticcheck`, cross-compile check.
- [ ] (Optional) Move existing v1 code under `v1/` so tests still pass while new packages are built. Alternative: leave v1 on `main` and start v2 fresh.

**Deliverable:** repo builds clean on Go 1.26, CI is green on the empty (or v1-relocated) tree.

**Estimated:** ~50 LOC.

---

## Phase 1 — L1 `watch` package

**Goal:** primitives for watching a file across rotation and framing lines, with zero-alloc happy path.

### Package layout
```
watch/
├── doc.go
├── watch.go            # Watcher interface, Event, PreRotation, Position, Config
├── poll.go             # NewPolling implementation
├── stat_unix.go        # //go:build unix
├── stat_windows.go     # //go:build windows
├── linereader.go       # LineReader with owned buffer
├── fakewatcher.go      # FakeWatcher test helper
└── *_test.go
```

### Sub-tasks

- [ ] `watch/watch.go`: define `Position` struct (`File string`, `Inode uint64`, `Offset int64`) with JSON tags per §4. Add `Position.IsZero() bool` (§5.10 #11).
- [ ] `watch/watch.go`: define `Event` struct (`Path`, `Pos`, `ReOpened`, `Truncated`, `PreRotation *PreRotation`).
- [ ] `watch/watch.go`: define `PreRotation` struct (`Reader io.Reader`, `FinalSize int64`, `StartPos int64`). Reader valid until next `Wait` or `Close`.
- [ ] `watch/watch.go`: define `Watcher` interface: `Wait(ctx) (Event, error)`, `Close() error`.
- [ ] `watch/watch.go`: define `Config` (`Path`, `Interval`, `Whence`, `Resume *Position`, `StopAtEOF`, `Logger`, `NoInodeCheck`).
- [ ] `watch/watch.go`: define sentinel errors `ErrUnsupported`, `ErrInodeMismatch`, `ErrTruncated`, `ErrLineTooLong`.
- [ ] `watch/stat_unix.go` (`//go:build unix`): `func statInode(fi os.FileInfo) uint64` reading `syscall.Stat_t.Ino`. Stdlib only.
- [ ] `watch/stat_windows.go` (`//go:build windows`): `func statInode(f *os.File) uint64` calling `syscall.GetFileInformationByHandle` and combining `nFileIndexHigh<<32 | nFileIndexLow`. Doc that this is unstable on ReFS / some network FS.
- [ ] `watch/poll.go`: `NewPolling(c Config) (Watcher, error)`. Port from `poll_watcher.go` preserving the race-aware rotation logic at lines 131-143 (re-stat the open fd before rotating to drain trailing bytes — surface those bytes via `PreRotation.Reader`).
- [ ] `watch/poll.go`: drop the unused mutex (`poll_watcher.go:21`). Drop `cancel chan struct{}`; use ctx throughout.
- [ ] `watch/poll.go`: respect `StopAtEOF` — when set, return `io.EOF` once the file is exhausted instead of polling forever.
- [ ] `watch/poll.go`: respect `NoInodeCheck` — skip the inode equality check on resume and on rotation detection.
- [ ] `watch/linereader.go`: `LineReader` with **owned buffer** (§6 strategy 1). Fields: `buf []byte`, `head int`, `tail int`, `pos Position`. No `bufio.Reader`; manual scan via `bytes.IndexByte`.
- [ ] `watch/linereader.go`: `LineOptions` (`BufferSize`, `MaxLine` default 1 MiB, `KeepNewline` default false).
- [ ] `watch/linereader.go`: `NewLineReader(w Watcher, opts LineOptions) *LineReader`. LineReader opens its own `*os.File` against `Event.Path` — Watcher signals only.
- [ ] `watch/linereader.go`: `Next(ctx) (line []byte, pos Position, err error)`. Returned slice valid until next `Next` or `Close`. Trim CR (preserve `line_reader.go:140-144` behavior).
- [ ] `watch/linereader.go`: on rotation event, drain `Event.PreRotation.Reader` first, then reopen against new path; reuse the existing buffer (don't allocate a fresh `bufio.NewReader`) — §6 strategy 3.
- [ ] `watch/linereader.go`: enforce `MaxLine` — return `ErrLineTooLong`, advance past next newline so caller can resume.
- [ ] `watch/linereader.go`: `Position() Position`, `Close() error`.
- [ ] `watch/fakewatcher.go`: `FakeWatcher(path string, pos Position) Watcher` — emits one `ReOpened` event then EOF. For unit-testing LineReader without polling.
- [ ] `watch/doc.go`: package overview, vocabulary (Position only at this layer), buffer ownership rule.

### Tests (`watch/`)

- [ ] Port `TestReadAfterWatcher` (currently `tail_test.go:141`) — pins the rotation-race drain. Use `PreRotation.Reader`.
- [ ] `TestPollingWatcher_FileNotExistInitially` — start watching before file exists; verify pickup on create.
- [ ] `TestPollingWatcher_Truncation` — write, truncate, write; verify `Event.Truncated` fires and reading resumes from offset 0.
- [ ] `TestPollingWatcher_SymlinkSwap` — `os.Symlink` → fileA, tail, `os.Rename` symlink → fileB, verify rotation.
- [ ] `TestLineReader_LongLine` — line > MaxLine; verify `ErrLineTooLong` + recovery on next call.
- [ ] `TestLineReader_CRLF` — verify `\r\n` is stripped to bare line.
- [ ] `TestLineReader_BufferReuse` — capture returned slice, call `Next`, assert original slice contents are now invalid (write a probe byte).
- [ ] `TestLineReader_NoAlloc` — `testing.AllocsPerRun(1000, ...)` must report 0 allocs/op on the happy path.
- [ ] `TestContextCancellation` — `cancel()` mid-`Wait` returns promptly with `ctx.Err()`.
- [ ] Port `TestLineReaderRotate` (currently `line_reader_test.go:119-148`).
- [ ] `FuzzLineReader` — random `(input, splits)` → assert concatenation of yielded lines == input (§7).

### Benchmarks (`watch/bench_test.go`)

- [ ] `BenchmarkLineReader_NoAlloc` — target 0 allocs/op.
- [ ] `BenchmarkLineReader_LongLines` — <1 alloc/op for 1 MiB lines.
- [ ] `BenchmarkPolling_Overhead` — <100 ns/op steady state.
- [ ] `BenchmarkRotation_Latency` — <2× interval P99.

**Deliverable:** L1 standalone — bytes-and-lines, no checkpointing.

**Estimated:** ~600 LOC + ~400 LOC tests.

---

## Phase 2 — L2 `tail` package, single-file source

**Goal:** durable line-oriented tail with persistent cursor for a single file.

### Package layout
```
tail/
├── doc.go
├── tail.go             # Tailer, Options, Record, MissingPolicy
├── source.go           # Source interface, SingleFile, MemorySource
├── cursor.go           # Cursor interface, FileCursor, MemoryCursor
├── errors.go           # Sentinel errors
└── *_test.go

internal/
└── atomicwrite/
    └── atomicwrite.go  # tmp + fsync + rename + dirsync helper
```

### Sub-tasks

- [ ] `tail/tail.go`: type alias `Position = watch.Position` (Decision #21).
- [ ] `tail/tail.go`: define `Record` struct (`Line []byte`, `Pos Position`). Doc that `Line` is valid until next iteration.
- [ ] `tail/tail.go`: define `Options` per §4 (Source, Cursor, Logger, Interval, UseFsnotify=false, StopAtEOF, OnMissingCheckpoint, hooks: OnDropped, OnRotated, OnError, OnTruncated, OnCheckpoint).
- [ ] `tail/tail.go`: define `MissingPolicy` enum (`FallbackOldest` default, `Fail`, `SkipToActive`).
- [ ] `tail/source.go`: define `Source` interface: `Enumerate(ctx) ([]string, error)`. Order: oldest first, active last.
- [ ] `tail/source.go`: `SingleFile(path string) Source` — returns `[]string{path}`.
- [ ] `tail/source.go`: `MemorySource(paths []string) Source` — returns paths as-is (immutable test helper).
- [ ] `tail/cursor.go`: define `Checkpoint` struct (`Pos Position`, `Meta json.RawMessage`).
- [ ] `tail/cursor.go`: define `Cursor` interface: `Load`, `Save`, `Close`.
- [ ] `tail/cursor.go`: `NewMemoryCursor() Cursor` — in-memory implementation for tests.
- [ ] `tail/cursor.go`: define `SyncMode` enum (`SyncAlways` default, `SyncOnCommit`, `SyncBackground`).
- [ ] `tail/cursor.go`: `NewFileCursor(path string, opts ...FileCursorOption) (Cursor, error)`. Options: `WithDirSync(bool)` default on, `WithSyncMode`, `WithFileMode` default 0o600. Defer `WithFlock` to Phase 3.
- [ ] `tail/cursor.go`: cursor file format = JSON with `{pos, meta, version: 1}` (Decision #5). Reject `len(meta) > 64 KiB` (Decision #6).
- [ ] `internal/atomicwrite/atomicwrite.go`: `Write(path string, data []byte, mode os.FileMode, dirSync bool) error` — write to `path.tmp`, `f.Sync`, `os.Rename`, optional `dir.Sync`.
- [ ] `tail/tail.go`: `New(opts Options) (*Tailer, error)`. Constructs Watcher from Source's first file, loads Cursor, applies `MissingPolicy`.
- [ ] `tail/tail.go`: `Records(ctx) iter.Seq2[Record, error]` — Go 1.23+ range-over-func. **Cursor is NOT auto-advanced** (invariant #3).
- [ ] `tail/tail.go`: `Next(ctx) (Record, error)` — pull-style escape hatch.
- [ ] `tail/tail.go`: `Commit(ctx, pos) error` — calls `Cursor.Save` with current Meta preserved.
- [ ] `tail/tail.go`: `CommitWithMeta(ctx, pos, meta any) error` — JSON-encodes meta and saves.
- [ ] `tail/tail.go`: `Position() Position`, `Done() <-chan struct{}`, `Close() error` (idempotent).
- [ ] `tail/tail.go`: enforce all 7 Tailer invariants from §4 (position monotonicity, inode change resets offset, no auto-advance, missing-checkpoint policy, Done() one-shot, Close() terminal, lock continuous).
- [ ] `tail/errors.go`: `ErrSourceExhausted`, `ErrCheckpointMissing`. (`ErrLockHeld` defined here too but unused until Phase 3.)
- [ ] `tail/doc.go`: package overview, vocabulary table (Position/Checkpoint/Cursor), live-tail vs StopAtEOF backfill modes.

### Tests (`tail/`)

- [ ] `TestSingleFileSource` — basic enumeration.
- [ ] `TestFileCursor_AtomicSave` — kill mid-save by removing tmp; verify on-disk is fully old or fully new, never partial.
- [ ] `TestFileCursor_DirSync` — verify `WithDirSync(false)` threads through.
- [ ] `TestFileCursor_Meta_RoundTrip` — save with meta, load, unmarshal; assert equality.
- [ ] `TestFileCursor_OversizeMeta` — 65 KiB meta; assert error.
- [ ] `TestTailer_ResumeAcrossRestart` — write 20 lines, read 10, commit, close; new Tailer + same cursor; assert resume at line 11.
- [ ] `TestTailer_MissingCheckpoint_Fail` — cursor names a missing file; assert `ErrCheckpointMissing`.
- [ ] `TestTailer_MissingCheckpoint_SkipToActive` — same, `SkipToActive`; assert resume at active offset 0.
- [ ] `TestTailer_StopAtEOF_ClosesDone` — exhaust source; `<-Done()` returns.
- [ ] `TestTailer_Records_Iterator` — exercise `iter.Seq2` form.
- [ ] `TestTailer_Next_PullStyle` — exercise pull form.
- [ ] `TestTailer_Close_Idempotent` — `Close` twice; second is no-op.
- [ ] `TestTailer_CommitWithMeta` — round-trip user metadata via cursor reload.
- [ ] `FuzzCursorParse` — random JSON to `json.Unmarshal(data, &Checkpoint)`; must not panic.
- [ ] `TestTailer_PendingLineDiscardedOnClose` — uncommitted line is dropped per Decision #19.

### Benchmarks

- [ ] `BenchmarkCursor_Save` — <100 µs P50 (no fsync), <10 ms P99 (with fsync).

**Deliverable:** L2 single-file. Driving reqs 2, 6, 7 satisfied for the single-file shape.

**Estimated:** ~400 LOC + ~400 LOC tests.

---

## Phase 3 — L2 multi-file source + flock

**Goal:** lumberjack rotation chains, glob sources, and single-instance protection.

### Sub-tasks

- [ ] `tail/source.go`: `Lumberjack(activePath string) Source` — recognizes lumberjack v2 backup naming `<base>-YYYY-MM-DDTHH-MM-SS.<ext>` (with optional `.gz` deferred to v2.1 per Decision #12). Sort backups oldest first, append active last.
- [ ] `tail/source.go`: `Glob(active, backupGlob string) Source` — explicit glob for non-lumberjack rotators (e.g., `logrotate` numeric suffixes).
- [ ] `tail/flock_unix.go` (`//go:build unix`): acquire via `syscall.Flock(fd, LOCK_EX|LOCK_NB)`, release on close. Write holder PID into the lock file (best-effort, not load-bearing).
- [ ] `tail/flock_windows.go` (`//go:build windows`): acquire via `LockFileEx` with `LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY`. Range-lock the first byte.
- [ ] `tail/cursor.go`: implement `WithFlock(lockPath string) FileCursorOption`. Lock acquired in `NewFileCursor`, released in `Cursor.Close`. **Sibling `.lock` file**, never the cursor itself (rename-over-open loses Linux locks per §5.4).
- [ ] `tail/cursor.go`: `WithFlock("")` is a no-op (driving req allows lock-less).
- [ ] `tail/tail.go`: implement `FallbackOldest` policy — when cursor names a file no longer in `Source.Enumerate`, resume at oldest still-present (offset 0) and fire `OnDropped(n)` with count of aged-off backups.
- [ ] `tail/tail.go`: implement `WithoutInodeCheck()` option (delegates to `watch.Config.NoInodeCheck`).
- [ ] `tailtest/`: separate package with stateful `MemorySource` (`Add(path)`, `Prune(path)`) for mid-tail rotation scenarios.

### Tests

- [ ] `TestLumberjackSource_OrderedEnumeration` — `events.log` + 3 timestamped backups; assert oldest-first.
- [ ] `TestLumberjackSource_NamingEdgeCases` — paths with no extension, paths with multiple dots, paths with non-matching siblings (must be ignored).
- [ ] `TestGlobSource_Patterns` — verify glob matches expected files only.
- [ ] `TestFileCursor_Flock_Conflict` — open two cursors on the same lock; second returns `ErrLockHeld`.
- [ ] `TestFileCursor_Flock_ReleasedOnClose` — after `Close`, second cursor can acquire.
- [ ] `TestFileCursor_Flock_PIDInFile` — assert lock file contains caller PID (string-decimal).
- [ ] `TestTailer_MissingCheckpoint_FallbackOldest` — cursor names a deleted file; assert resume at oldest still-present, `OnDropped` fires with correct count.
- [ ] `TestTailer_RotatesAcrossLumberjackBackups` — start mid-backup, drain it, advance to next, then to active. Assert `OnRotated` fires per transition.
- [ ] Platform-tagged tests for flock semantics on linux/darwin/windows.

**Deliverable:** L2 satisfies driving reqs 1, 3, 4. A consumer can adopt at L2 without L3.

**Estimated:** ~300 LOC + ~250 LOC tests.

---

## Phase 4 — L2 rotation hardening

**Goal:** copytruncate, symlink swap, inode reuse, mid-write truncation — all correct with explicit hooks.

### Sub-tasks

- [ ] `watch/poll.go`: in `Wait`, when `currentSize < currentPosition`, emit `Event.Truncated = true`, reset position to 0, continue. Covers copytruncate, mid-write truncation, and inode reuse (where size shrinks).
- [ ] `tail/tail.go`: wire `Event.Truncated` to `OnTruncated(at Position)` hook.
- [ ] `watch/poll.go`: explicitly start the new file at offset 0 on rotation (don't consult `Resume` past the first open) — guards against rare inode-reuse hitting an old `SeekIfMatches` path. Already implicit per `poll_watcher.go:165`; make it a documented invariant with a regression test.
- [ ] Update README to remove the v1 caveat about truncation not working (`README.md:19-20`).

### Tests

- [ ] `TestRotation_RenameAndCreate` — already covered by ported `TestReadAfterWatcher`; double-check.
- [ ] `TestRotation_Copytruncate` — write, copy file content elsewhere, truncate original to 0, write new content. Assert: old content read once, then `OnTruncated`, then new content.
- [ ] `TestRotation_SymlinkSwap` — see Phase 1; ensure tail-layer rotation hook also fires.
- [ ] `TestRotation_InodeReuse` — write+delete fileA, create fileB at same path with same inode (use a deterministic test approach: in CI just assert "size-decrease check catches it" via a forced truncate scenario).
- [ ] `TestRotation_MidWriteTruncate` — writer calls `Truncate(0)` mid-stream; assert `OnTruncated` fires and reading resumes from offset 0.
- [ ] **Property-based** (`testing/quick`, stdlib only — Decision per §7): for any sequence of `(write, rotate, write, ...)`, concatenation of all yielded `Record.Line` values (with newlines re-added) equals the writer's full byte stream. Fuzz the timing of poll ticks.
- [ ] **Property-based**: for any sequence of `(write, commit, simulated-crash, restart)`, no byte yielded twice, no committed byte lost.
- [ ] **Property-based**: rotation race — write to old file *after* new file exists at path; assert all old bytes yielded before any new bytes.

**Deliverable:** rotation correctness across all common schemes.

**Estimated:** ~150 LOC + ~250 LOC tests.

---

## Phase 5 — L3 `forward` package

**Goal:** batched, retried, at-least-once shipper.

### Package layout
```
forward/
├── doc.go
├── forward.go          # Forwarder[T], Options[T], Sink[T], SinkFunc[T], RecordSource
├── decoders.go         # IdentityDecoder, IdentityDecoderCopy, JSONDecoder[T]
├── errors.go           # ErrPermanent
└── *_test.go

internal/
└── bufpool/
    └── bufpool.go      # sync.Pool of *[]byte

forwardtest/
└── sinks.go            # RecordingSink[T], FailingSink[T]
```

### Sub-tasks

- [ ] `forward/forward.go`: type alias `Position = tail.Position`.
- [ ] `forward/forward.go`: define `RecordSource` interface (`Records(ctx) iter.Seq2[tail.Record, error]`, `Commit(ctx, pos) error`, `Done() <-chan struct{}`). `*tail.Tailer` satisfies this; defining the interface keeps Forwarder testable in isolation.
- [ ] `forward/forward.go`: `Sink[T any]` interface with `Send(ctx, batch []T) error`. Document the three return contracts (nil → commit; `errors.Is(err, ErrPermanent)` → fatal; else → retry).
- [ ] `forward/forward.go`: `SinkFunc[T any] func(...) error` adapter.
- [ ] `forward/decoders.go`: `Decoder[T any] func(line []byte) (T, error)`.
- [ ] `forward/decoders.go`: `IdentityDecoder(line []byte) ([]byte, error)` — no copy. Loud doc warning that the slice aliases the LineReader buffer.
- [ ] `forward/decoders.go`: `IdentityDecoderCopy(line []byte) ([]byte, error)` — copies via bufpool.
- [ ] `forward/decoders.go`: `JSONDecoder[T any]() Decoder[T]` — `json.Unmarshal` to T.
- [ ] `internal/bufpool/bufpool.go`: `sync.Pool` of `*[]byte`, `Get() *[]byte`, `Put(b *[]byte)` (resets to `[:0]`).
- [ ] `forward/errors.go`: `ErrPermanent`.
- [ ] `forward/forward.go`: define `Options[T]` (Source, Decoder, Sink, MaxBatchRecords, MaxBatchBytes, MaxBatchAge, InitialBackoff, MaxBackoff, BackoffJitter default 0.2, hooks: OnBatchSent, OnSendError, OnCommitted, OnDecodeError, OnDropped, OnBackoffSleep, Logger).
- [ ] `forward/forward.go`: `New[T any](opts Options[T]) (*Forwarder[T], error)` — validate options.
- [ ] `forward/forward.go`: `Run(ctx) error` — main loop. One-shot per Decision #11. Returns nil on `Source.Done()`, `ctx.Err()` on cancel, the wrapped error on `ErrPermanent`.
- [ ] `forward/forward.go`: batching — flush when **any** of `MaxBatchRecords`, `MaxBatchBytes`, `MaxBatchAge` is met. Use a `time.Timer` for the age bound; reset on each new batch.
- [ ] `forward/forward.go`: retry — exponential backoff with full-jitter (`sleep = rand(0, min(MaxBackoff, InitialBackoff * 2^attempt))`). `OnBackoffSleep` fires before each sleep.
- [ ] `forward/forward.go`: on `Sink.Send` success → `Source.Commit(ctx, lastBatchPos)` → `OnCommitted(pos)` → `OnBatchSent(...)` → return pooled buffers.
- [ ] `forward/forward.go`: on `Decoder` error → `OnDecodeError(line, pos, err)` → skip line, advance position past it; do NOT include in batch.
- [ ] `forward/forward.go`: on `Sink` retryable error → `OnSendError(err, attempt, willRetry=true)` → backoff → retry **same batch**; do NOT advance cursor.
- [ ] `forward/forward.go`: on `Sink` permanent error → `OnSendError(..., willRetry=false)` → return wrapped error from `Run`.
- [ ] `forward/forward.go`: helper `WithSinkTimeout(d) func(Sink[T]) Sink[T]` — wraps a Sink in a per-call timeout context (§5.10 #7).
- [ ] `forwardtest/sinks.go`: `RecordingSink[T]` — captures all batches in a slice, exposes them to the test.
- [ ] `forwardtest/sinks.go`: `FailingSink[T]` — fails N times then succeeds; configurable `failWith error`.
- [ ] `forward/doc.go`: package overview.

### Tests (`forward/`)

- [ ] `TestForwarder_BatchByCount` — 25 records, `MaxBatchRecords=10` → batches of 10/10/5.
- [ ] `TestForwarder_BatchByBytes` — `MaxBatchBytes=N`; verify batch ends at byte threshold.
- [ ] `TestForwarder_BatchByAge` — `MaxBatchAge=100ms`; one record arrives, age fires, single-record batch sent.
- [ ] `TestForwarder_RetryOnError` — Sink returns non-permanent error twice, succeeds third; assert two `OnBackoffSleep` calls, cursor not advanced until success.
- [ ] `TestForwarder_PermanentErrorExits` — Sink returns `ErrPermanent`-wrapped error; `Run` returns that error.
- [ ] `TestForwarder_ContextCancellation` — cancel mid-retry; `Run` returns `ctx.Err()` promptly.
- [ ] `TestForwarder_DecodeErrorSkips` — decoder fails on line 5; assert line 5 skipped, cursor advanced past it, `OnDecodeError` fired.
- [ ] `TestForwarder_GenericTypes` — `Forwarder[MyEvent]` + `JSONDecoder[MyEvent]`; verify type flow.
- [ ] `TestForwarder_StopAtEOF` — Tailer exhausts; `Run` returns nil.
- [ ] `TestForwarder_RecordingSink` — `forwardtest.RecordingSink[T]` captures batches.
- [ ] **End-to-end**: real lumberjack writer → real `tail.Tailer` → `forward.Forwarder[[]byte]` → `httptest.Server` sink. Verify all writer bytes arrive at the server in order, exactly once.

### Benchmarks

- [ ] `BenchmarkForwarder_Throughput` — >100k records/sec on a no-op sink.

**Deliverable:** v2 feature-complete. Driving req 5 satisfied.

**Estimated:** ~400 LOC + ~400 LOC tests.

---

## Phase 6 — fsnotify backend (optional)

**Goal:** sub-millisecond latency for users who opt in. Build-tagged so default builds remain zero-dep.

### Sub-tasks

- [ ] `watch/fsnotify_unix.go` (`//go:build gotail_fsnotify && (linux || darwin || freebsd || netbsd || openbsd)`): `NewFsnotify(c Config) (Watcher, error)` using `github.com/fsnotify/fsnotify`. Watch parent directory (fsnotify can't watch nonexistent files); react to create/rename/write/remove on the named path.
- [ ] `watch/fsnotify_stub.go` (`//go:build !gotail_fsnotify`): `func NewFsnotify(c Config) (Watcher, error) { return nil, ErrUnsupported }`.
- [ ] `watch/watch.go`: `New(c Config) (Watcher, error)` — try `NewFsnotify`; on `ErrUnsupported` fall back to `NewPolling`. Log the choice via `c.Logger`.
- [ ] Add `github.com/fsnotify/fsnotify` to `go.mod` (only pulled in by build-tagged files; `go mod tidy` keeps it out of default builds via standard module graph rules — verify with a no-tag build).
- [ ] Wire `tail.Options.UseFsnotify` to switch between `NewPolling` and `New` (or `NewFsnotify` directly).

### Tests

- [ ] `TestFsnotify_FallbackToPolling` (build-tagged) — mock fsnotify error; assert fallback constructor falls back.
- [ ] `TestInotifyBackend` (`//go:build linux && gotail_fsnotify`) — basic write-detect.
- [ ] `TestKqueueBackend` (`//go:build darwin && gotail_fsnotify`) — basic write-detect.
- [ ] `TestWindowsFileID` (`//go:build windows`) — verify `GetFileInformationByHandle` produces stable IDs across opens.
- [ ] `TestWindowsRotation` (`//go:build windows`) — rename+create rotation on NTFS.

**Deliverable:** opt-in low-latency backend. Default builds unchanged.

**Estimated:** ~250 LOC + ~150 LOC tests.

---

## Phase 7 — Polish & docs

**Goal:** v2.0.0 release-ready.

### Sub-tasks

- [ ] Rewrite `README.md`: v2 package layout, quickstart per layer, v1→v2 migration table (§8), `gotail_fsnotify` build tag note, link to docs/.
- [ ] `docs/metrics-prometheus.md` — wire each L2/L3 hook to a Prometheus collector. Code-only docs, no library dep.
- [ ] `docs/metrics-otel.md` — same for OpenTelemetry.
- [ ] `docs/cookbook/https-forwarder.md` — full worked example: lumberjack writer + `tail.Lumberjack` + `forward.Forwarder` + mTLS HTTP sink.
- [ ] `docs/cookbook/backfill.md` — archived-file backfill with `StopAtEOF` and `<-Tailer.Done()`.
- [ ] `docs/cookbook/standalone.md` — slog-only writer mode, no Tailer.
- [ ] `cmd/gotail/main.go` (~50 LOC) — CLI binary mirroring `tail -f /var/log/foo.log`. Smoke-tests the L1/L2 stack.
- [ ] Commit benchmarks (Phase 1, 2, 5) and a `benchstat` baseline file in `docs/perf/`.
- [ ] godoc review pass: every exported type has a doc comment, every package has `doc.go` with overview and vocabulary table.
- [ ] Verify slog discipline: every library log line uses keys `path`, `inode`, `offset`, `attempt`, `err`, `latency_ms`. Add a doc note in each `doc.go`.
- [ ] Tag `v2.0.0` on the v2 branch; point `main` at v2.

**Deliverable:** release.

**Estimated:** ~100 LOC code + extensive prose.

---

## Out of scope for v2.0 (Phase 8 — design space for v2.1+)

Do not implement in v2.0. Listed here so reviewers know they were considered:

- Hybrid fsnotify+poll watcher (`WatchMode` enum: `Poll | Fsnotify | Hybrid`).
- Compressed backup file (`.gz`) source.
- Compat shim (`v2/compat`) — only ship if a v1 user materializes.
- Plugin cursors (Redis, SQL) as separate modules.
- Per-event filtering hooks at L2.
- `tail.WithCursorMigration(fn)` — design noted in §5.10 #9; implement if/when a schema bump forces it.

---

## Cross-cutting reminders

- **Allocations:** L1 `LineReader.Next` happy path = 0 allocs. Verify with `testing.AllocsPerRun` in every PR that touches it.
- **Buffer ownership:** any `[]byte` returned from L1 or L2 is invalidated by the next call. Document at the function and at the package level.
- **Context everywhere:** every blocking call takes `context.Context` and returns promptly on cancel.
- **No `golang.org/x/sys`:** stdlib `syscall` is the only system-call dep.
- **Hooks are nil-safe** and synchronous. Document that long-running hooks block the tail/forward loop.
- **No global state.** No `init()` registration. No package-level mutable variables.
- **Backwards compatibility is a non-goal across the v1 → v2 cut.** Hard cut per Decision (§8).
