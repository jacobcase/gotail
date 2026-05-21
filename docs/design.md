# gotail — Design

**Module:** `github.com/jacobcase/gotail/v3`
**Target Go version:** 1.26 or later

This document is a snapshot of the current design — it describes what the
code does now, not how it got here. When a change alters a documented
choice, the relevant section is edited in place.

---

## 1. Overview

gotail is a layered file-tailing library. The canonical consumer shape (see §3) — a process that writes structured events to a lumberjack-rotated file and ships batches to a remote ingest endpoint — needs cursor persistence, rotation-spanning sources, batched delivery, single-instance protection, and metrics; gotail provides each as an opt-in layer so callers pay only for what they use.

**Design at a glance:**

1. **Three packages, three abstraction layers** — `watch` (primitives), `tail` (durable line-oriented tail), `forward` (batched at-least-once shipper). Callers pay only for what they use.
2. **Modern Go** — `context.Context`, `log/slog`, `iter.Seq2`, `errors.Is/As`, and generics where they earn their keep.
3. **Built-in checkpointing** — atomic, fsync'd, optional flock, pluggable `Cursor` interface, support for user-defined metadata persisted alongside the byte offset.
4. **Multi-file Source abstraction** — handles single files, lumberjack-style rotation chains, and arbitrary glob-based sets, with rotation-outran-outage fallback.
5. **File-watch strategies** — polling is the always-works default; a build-tagged fsnotify backend (with auto-fallback) is also provided, with documented per-platform behavior.
6. **Rotation handling** — explicitly models rename+create, copytruncate, symlink swap, inode reuse, and mid-write truncation with documented behavior in each case.
7. **Metrics hooks** — `OnX` callback fields, no concrete metrics dependency. Examples for Prometheus and OpenTelemetry shipped as docs only.
8. **Generics on L3** — `Sink[T]`, `Decoder[T]`, `Forwarder[T]`. L1 and L2 stay byte-oriented because lines are bytes.
9. **Memory-reuse hot paths** — owned line buffers, explicit buffer-ownership rules at every API boundary, zero-alloc happy path.
10. **Two-tier API** — L1 is the "give me bytes, I'll do the rest" primitive; L2/L3 are the "I have a job to get done" high-level shapes. Same library, different entry points.

---

## 2. Load-bearing invariants

These correctness properties are guaranteed by the implementation. They are the subtle parts; any refactor must preserve them.

- **Race-aware rotation drain.** When a new inode appears at the watched path, the previous file descriptor is read to `io.EOF` before switching to the new file, so trailing bytes written between the last size check and rotation detection are still yielded. The Watcher emits state transitions only; the `LineReader` owns the fd and performs the drain (Decision #17). Pinned by the rotation-race tests (Invariant 3 in §7).
- **Inode-equality identity.** A position is `{file, inode, offset}`. Inode plus offset answers "where am I and which file is this"; a resume refuses to seek when the inode at the path no longer matches the saved cursor, preventing reads of the wrong file at a stale offset.
- **Byte-resolution position.** `Position.Offset` advances by the full consumed byte count after every yielded line, so it always points at the start of the next unread line — exactly what a checkpoint needs (and what makes the cursor line-aligned).
- **Zero-alloc steady state.** The read/frame/yield hot path owns its buffer and reuses it across reads and rotations (`head = tail = 0` on drain), so steady-state `LineReader.Next` and `Tailer.Next` allocate nothing.

---

## 3. Driving Requirements

gotail's API is anchored to a canonical consumer pattern that generalizes across most production users of a tail-and-ship library:

> A long-running process writes structured events (typically NDJSON via `log/slog`) to a `lumberjack.v2`-rotated file. A forwarder goroutine — in the same process or a sidecar — tails that file and POSTs batches to a remote ingest endpoint over HTTPS/mTLS. The on-disk file IS the buffer; the file format on disk equals the byte stream on the wire. The consumer also wants a "standalone" mode where events are written but no forwarder runs, so a separate agent can backfill the files later.

This shape covers log forwarders (Vector / Fluent Bit / Promtail-style), event-tap pipelines, audit-log shippers, observability collectors, and most ingest-side reliability tools. The concrete requirements below were derived from one such consumer.

| # | Requirement | Mechanism | Layer |
|---|---|---|---|
| 1 | Tail across rotation (active file + chronologically older lumberjack backups) | `tail.Source` interface; `tail.Lumberjack(activePath)` built-in | L2 |
| 2a | Persisted `{file, byte_offset}` checkpoint resumed across restarts | `tail.Cursor` interface; `tail.NewFileCursor(path)` | L2 |
| 2b | Atomic write (tmp + fsync + rename) | `FileCursor.Save` writes `<path>.tmp`, `fsync`, `os.Rename`, `fsync(parent_dir)` | L2 |
| 2c | Optional fsync-on-ack (cursor never lags by more than one syscall) | `FileCursor.Save` is synchronous; caller decides when to call it. `WithSyncMode(Always|OnCommit|Background)` for trade-off control | L2 |
| 3 | Single-instance lock (cursor or sibling `.lock`); held continuously while consumer is alive (acquired before declaring readiness, e.g. systemd `READY=1`) | `tail.WithFlock(lockPath)` cursor option; acquired in `tail.New`, released in `Tailer.Close`. Returns `ErrLockHeld` on conflict | L2 |
| 4 | Rotation-outran-outage fallback (resume oldest still-present, dropped-files counter) | `tail.Options.OnMissingCheckpoint` policy: `FallbackOldest` (default), `Fail`, `SkipToActive`. `OnDropped(n int)` callback fires the counter | L2 |
| 5a | Batched delivery (group by size or age), ship to a Sink | `forward.Forwarder[T]` with `MaxBatchRecords`, `MaxBatchBytes`, `MaxBatchAge` | L3 |
| 5b | On success commit position; on failure exponential backoff, don't advance | `Forwarder.Run` calls `Sink.Send`; on nil error calls `Source.Commit`; on error retries with exponential backoff bounded by `MaxBackoff` | L3 |
| 6 | Standalone-write mode (slog only, no forwarder, no cursor) | First-class shape: write via `slog.Handler` to a lumberjack file; do not construct a Tailer or Forwarder. gotail contributes the *shape* by making L1/L2/L3 independently importable so "skip the forwarder" is just "don't import `forward`" | L0 (consumer-side) |
| 7 | Backfill an archived/standalone file end-to-end | `tail.SingleFile(path)` source + `Options.StopAtEOF: true` + L3 forwarder. `Tailer.Done() <-chan struct{}` signals stream exhaustion | L2+L3 |
| ext | Modern Go (slog, ctx, generics, iter) | Go 1.26+, slog throughout, ctx on every blocking call, generics on L3, `iter.Seq2` for `Records()` | All |
| ext | Sentinel errors via `errors.Is/As` | `ErrCheckpointMissing`, `ErrInodeMismatch`, `ErrSourceExhausted`, `ErrLockHeld`, `ErrLineTooLong`, `ErrPermanent` | All |
| ext | Three packages not three structs | `github.com/jacobcase/gotail/v3/watch`, `.../tail`, `.../forward` | All |
| ext | Hooks not concrete metrics | `OnBatchSent`, `OnSendError`, `OnCommitted`, `OnDecodeError`, `OnDropped`, `OnError`, `OnCheckpoint` callback fields. No prom/otel deps | L2/L3 |
| ext | Memory-backed adapters for tests | `tail.NewMemoryCursor()`, `tail.StaticSource()`, `tailtest.MemorySource{}`, `watchtest.FakeWatcher()`, `forwardtest.RecordingSink[T]` / `FailingSink[T]` | All |
| ext | Iterator form | `Tailer.Records(ctx) iter.Seq2[Record, error]` | L2 |
| ext | Pull-style escape hatch | `Tailer.Next(ctx) (Record, error)` | L2 |
| ext | Inode opt-out for Windows/weird FS | `tail.Options.NoInodeCheck bool` | L2 |
| ext | Cursor format human-inspectable | JSON | L2 |

Additional wishlist items map cleanly:

| Request | Mechanism |
|---|---|
| Improved file watch strategies | Section 5.1 |
| Broader platform support | Section 5.2 |
| Built-in checkpointing | Section 5.3 (covers reqs 2a/2b/2c) |
| flock support | Section 5.4 (covers req 3) |
| Metrics hooks (BYO) | Section 5.5 |
| Better rotation tracking | Section 5.6 |
| Custom metadata in checkpoint | Section 5.7 |
| High-level API | L2 + L3 (sections 4, 5.8) |
| Generics where valuable | Section 5.9 |
| Two-tier API | Whole-architecture concern, sections 4 + 5.8 |

---

## 4. Architecture

### Package layout

```
github.com/jacobcase/gotail/v3/
├── watch/                  L1 — file-as-stream primitives
│   ├── watch.go            Watcher, Event, Position, Config, StatInode
│   ├── poll.go             Polling implementation (always available)
│   ├── fsnotify_unix.go    fsnotify implementation (default; opt out with gotail_nofsnotify)
│   ├── fsnotify_stub.go    Stub returning ErrUnsupported (Windows / opt-out)
│   ├── stat_unix.go        Inode extraction on Unix
│   ├── stat_windows.go     File-ID extraction on Windows (GetFileInformationByHandle)
│   └── linereader.go       Line framing on top of a Watcher
│
├── tail/                   L2 — durable line-oriented tail with cursor
│   ├── tail.go             Tailer, Options, Record
│   ├── source.go           Source interface; SingleFile, Lumberjack, Logrotate, Glob, StaticSource
│   ├── cursor.go           Cursor interface; FileCursor, MemoryCursor
│   ├── flock_unix.go       Advisory lock via syscall.Flock
│   ├── flock_windows.go    Advisory lock via LockFileEx
│   └── errors.go           Sentinel errors
│
├── forward/                L3 — batched at-least-once shipper
│   ├── forward.go          Forwarder[T], Options[T], Sink[T], SinkFunc[T], Decoder[T]
│   ├── errors.go           ErrPermanent sentinel
│   └── decoders.go         IdentityDecoder, JSONDecoder[T]
│
├── watchtest/              Test helpers for L1 (separate package)
├── tailtest/               Test helpers for L2: stateful MemorySource (Add/Prune)
├── forwardtest/            Test helpers for L3: RecordingSink[T], FailingSink[T]
│
└── internal/
    └── atomicwrite/        tmp+fsync+rename helper
```

External dependencies: **none** beyond stdlib if the fsnotify backend is build-tagged off by default. With it on: `github.com/fsnotify/fsnotify` only.

### Vocabulary

Three terms with disjoint meaning. Used consistently throughout the library and reflected in `doc.go`:

| Term | Meaning | Lives in |
|---|---|---|
| **Position** | A coordinate `{File, Inode, Offset}`. A pure value; no I/O. | `watch` (re-aliased into `tail` and `forward`) |
| **Checkpoint** | A persistable record `{Pos, Meta}`. What the storage port reads/writes. | `tail` |
| **Cursor** | The storage port itself — interface for loading/saving Checkpoints. | `tail` |

"Watermark" is *not* used. The L2 hook `OnCheckpoint` fires after `Cursor.Save`; the L3 hook `OnCommitted` fires after a successful `Sink.Send` + `Commit`. They are named distinctly — rather than `OnCheckpoint` at both layers — to keep the layer boundary clean.

**"Source" is intentionally overloaded across two layers** — `tail.Source` (file-set enumeration interface) and `forward.RecordSource` (record-stream interface). They're orthogonal concepts that happen to share the noun. Each is unambiguous in its own package; the field name `forward.Options[T].Source` (typed `RecordSource`) reads naturally at call sites. Keep this note here so future readers don't try to "fix" the overlap by renaming one of them — both names are right in their own context.

### Core types (signatures, not implementations)

#### L1: `package watch`

```go
package watch

// Position describes a point in a file series. Pure value; no I/O.
type Position struct {
    File   string `json:"file"`              // path as the consumer sees it
    Inode  uint64 `json:"inode,string"`      // 0 on platforms without stable inode
    Offset int64  `json:"offset,string"`     // byte offset within File
}

// Event describes a state transition observed by the Watcher. The Watcher
// emits events; it does NOT yield bytes. The consumer (typically LineReader)
// opens its own file handle against Event.Path for normal reads.
//
// LineReader keeps the previous file's fd open after a ReOpened event and
// drains it to io.EOF before opening the new path — this preserves the
// rotation-race trailing-bytes invariant without forcing the Watcher to
// expose an io.Reader.
type Event struct {
    Path      string    // current active path; only changes when ReOpened
    Pos       Position  // logical position at the time of the event
    ReOpened  bool      // first open or rotation detected
    Truncated bool      // size dropped below previous position
}

// Watcher emits events about file state transitions. Production
// implementations (NewPolling, NewFsnotify) own a *os.File internally and
// hide its lifecycle. Tests can use FakeWatcher.
type Watcher interface {
    Wait(ctx context.Context) (Event, error)
    Close() error
}

type Config struct {
    Path               string
    Interval           time.Duration  // poll interval; 0 = 1s default; negative rejected
    Whence             int            // io.SeekStart | io.SeekEnd; SeekCurrent rejected
    Resume             *Position      // optional resume point; subject to inode match
    StopAtEOF          bool
    Logger             *slog.Logger   // optional; default = slog.Default()
    NoInodeCheck       bool           // skip inode equality check (Windows / weird FS)
    AllowInodeMismatch bool           // default false: constructors error with ErrInodeMismatch
                                       // when Resume's inode does not match Path's current inode.
                                       // Set true to warn-and-resume.
    OnInodeMismatch    func(want, got uint64) // observation hook; fires before
                                              // AllowInodeMismatch policy decision
}

func NewPolling(c Config) (Watcher, error)
func NewFsnotify(c Config) (Watcher, error)  // ErrUnsupported on stub builds
func New(c Config) (Watcher, error)          // fsnotify with poll fallback

// FakeWatcher is a test helper; it lives in the watchtest sub-package so the
// production watch surface stays free of test-only symbols. It emits a single
// ReOpened event for path at pos, then EOF on subsequent Wait calls. Combine
// with a tmpfile populated by the test for full LineReader unit-testability
// without a real polling loop.
// (See watchtest.FakeWatcher.)

// StatInode returns the platform-specific inode/file-id for path without
// opening the file. Used by tail.findFileByInode to anchor a checkpointed
// inode against the current filesystem state; exported so callers wiring
// custom Sources can perform the same anchor check.
func StatInode(path string) (uint64, error)

// LineReader frames lines on top of a Watcher. It owns its own *os.File
// (opened against Event.Path) and its own bufio buffer. The Watcher only
// signals state transitions — it does not yield bytes.
type LineReader struct { /* unexported */ }

type LineOptions struct {
    BufferSize  int   // bufio buffer; 0 = 64 KiB
    MaxLine     int   // max line length before ErrLineTooLong; 0 = 1 MiB
    KeepNewline bool  // include trailing \n in returned bytes; default false

    // L1 carries two observability hooks. They are intentionally on the L1
    // surface so the LineReader can fire them at moments only it sees:
    OnTruncated func(at Position)            // late-detection copytruncate (§5.6)
    OnRotated   func(from, to Position)      // in-place rotation completion (§5.6)
}

func NewLineReader(w Watcher, opts LineOptions) *LineReader

// Next returns the next line. The returned slice is valid until the next call
// to Next or Close. Callers wanting to retain it must copy.
func (l *LineReader) Next(ctx context.Context) (line []byte, pos Position, err error)
func (l *LineReader) Position() Position
func (l *LineReader) Close() error

var (
    ErrUnsupported   = errors.New("watch: unsupported on this platform/build")
    ErrInodeMismatch = errors.New("watch: file inode no longer matches resume point")
    ErrTruncated     = errors.New("watch: file was truncated below current position")
    ErrLineTooLong   = errors.New("watch: line exceeds MaxLine")
)
```

L1 deliberately persists nothing.

**Why the Watcher/LineReader split:** the *driving* port (events) and the *driven* port (bytes) are different concerns. The Watcher signals state transitions; the LineReader owns the only fd and frames bytes. After a `ReOpened` event the LineReader keeps reading the previous fd until `io.EOF`, then `os.Open`s the new path — preserving the rotation-race trailing-bytes drain without crossing an `io.Reader` through the Watcher port. LineReader becomes unit-testable with `FakeWatcher` plus a tmpfile (no polling loop needed).

#### L2: `package tail`

```go
package tail

import (
    "context"
    "io"
    "iter"
    "log/slog"
    "time"

    "github.com/jacobcase/gotail/v3/watch"
)

// Re-export Position so L2 callers don't need to import watch.
type Position = watch.Position

// Source enumerates the files that make up a logical stream.
// Order: oldest first, active last. The Tailer treats the last element as
// the active file. Enumerate must be stable across calls until a file is
// either fully consumed past or pruned from disk.
type Source interface {
    Enumerate(ctx context.Context) ([]string, error)
}

// Built-in sources.
func SingleFile(path string) Source
func Lumberjack(activePath string, opts ...LumberjackOption) Source  // lumberjack-style timestamped backups
func Logrotate(activePath string, opts ...LogrotateOption) Source    // logrotate-style numeric-tail (.1, .2, ...)
func Glob(active string, backupGlob string) Source                   // explicit glob

// Compressed-backup behaviour: Lumberjack and Logrotate recognise .gz
// variants of their backup naming and skip them (the library does not
// decompress). Use WithLumberjackSkippedHook / WithLogrotateSkippedHook to
// observe skipped files for diagnostics; a checkpoint pointing at an aged-off
// .gz falls through to OnMissingCheckpoint policy.
func WithLumberjackSkippedHook(fn func(path string)) LumberjackOption
func WithLogrotateSkippedHook(fn func(path string)) LogrotateOption

// StaticSource returns the given paths as-is on every Enumerate. Use for
// fixed file sets that do not rotate at runtime. For mutable test scenarios
// (Add/Prune mid-tail), see tailtest.MemorySource.
func StaticSource(paths []string) Source

// Cursor persists a Checkpoint (Position + opaque user metadata).
type Cursor interface {
    Load(ctx context.Context) (Checkpoint, bool, error) // bool = found
    Save(ctx context.Context, c Checkpoint) error
    Close() error
}

// Checkpoint is what gets persisted. The Meta field is opaque user data,
// JSON-serialized as part of the cursor file.
type Checkpoint struct {
    Pos  Position        `json:"pos"`
    Meta json.RawMessage `json:"meta,omitempty"`
}

type FileCursorOption func(*fileCursorOpts)

func NewFileCursor(path string, opts ...FileCursorOption) (Cursor, error)
func NewMemoryCursor() Cursor

func WithDirSync(on bool) FileCursorOption                  // default: on
func WithFlock(lockPath string) FileCursorOption             // single-instance check; "" = no lock
func WithSyncMode(m SyncMode) FileCursorOption               // Always | OnCommit | Background
func WithSyncBackgroundInterval(d time.Duration) FileCursorOption // SyncBackground flush interval; 0 = 1s (Decision #23)

type SyncMode int
const (
    SyncAlways SyncMode = iota   // every Save fsyncs (default)
    SyncOnCommit                 // Save buffers; explicit Sync() flushes
    SyncBackground               // background flusher; bounded staleness
)

// Syncer is an extension interface implemented by *FileCursor when
// SyncOnCommit or SyncBackground is configured. The base Cursor interface
// is not extended (Decision: use extension interface to keep the contract
// intact for non-FileCursor implementations). Tailer.Commit calls Save only;
// callers drive Sync themselves.
type Syncer interface {
    Sync(ctx context.Context) error
}

// Record carries one line plus its position.
type Record struct {
    Line []byte   // valid until next iteration; copy if retaining
    Pos  Position
}

type Options struct {
    Source              Source
    Cursor              Cursor             // nil = no persistence (acts like L1)
    RequireCursor       bool               // when true, New errors if Cursor is nil
    Logger              *slog.Logger
    Interval            time.Duration       // 0 = 1s default; negative rejected
    ForcePolling        bool

    StopAtEOF           bool
    OnMissingCheckpoint MissingPolicy
    AllowInodeMismatch  bool               // default false: New errors when the cursor's
                                            // path exists with a different inode. Set true
                                            // to fall through to OnMissingCheckpoint policy.

    // Hooks. All optional; nil-safe.
    OnDropped       func(droppedFiles int)
    OnRotated       func(from, to Position)
    OnError         func(err error)
    OnTruncated     func(at Position)
    OnCheckpoint    func(c Checkpoint)
    OnInodeMismatch func(want, got uint64)  // observation hook; fires before
                                             // AllowInodeMismatch policy decision
}

type MissingPolicy int
const (
    FallbackOldest MissingPolicy = iota // resume at oldest still-present, offset 0
    Fail                                 // return ErrCheckpointMissing
    SkipToActive                         // jump to active file, offset 0
)

type Tailer struct { /* unexported */ }

// New constructs a Tailer. ctx governs only startup I/O (Source.Enumerate,
// Cursor.Load); the runtime loop uses the per-call ctx of Next.
func New(ctx context.Context, opts Options) (*Tailer, error)

// Records is the iterator form (Go 1.23+ range-over-func). Cursor is NOT
// auto-advanced — caller must call Commit explicitly.
func (t *Tailer) Records(ctx context.Context) iter.Seq2[Record, error]

// Next is the pull-style escape hatch.
func (t *Tailer) Next(ctx context.Context) (Record, error)

// Commit persists pos as a new Checkpoint with optional user meta.
func (t *Tailer) Commit(ctx context.Context, pos Position) error
func (t *Tailer) CommitWithMeta(ctx context.Context, pos Position, meta any) error

// Position returns the position of the most recently yielded record. Between
// Next calls this equals the next-commit point; mid-Next hooks (e.g.
// OnTruncated) see the pre-yield value, since t.cur is updated after the
// line is returned, not after each Watcher event.
func (t *Tailer) Position() Position

// Done is closed when Source is exhausted in StopAtEOF mode.
func (t *Tailer) Done() <-chan struct{}

func (t *Tailer) Close() error

var (
    ErrSourceExhausted    = errors.New("tail: source exhausted (StopAtEOF)")
    ErrCheckpointMissing  = errors.New("tail: checkpointed file no longer present")
    ErrLockHeld           = errors.New("tail: cursor lock held by another process")
)
```

#### L3: `package forward`

```go
package forward

import (
    "context"
    "encoding/json"
    "errors"
    "iter"
    "log/slog"
    "time"

    "github.com/jacobcase/gotail/v3/tail"
)

type Position = tail.Position

// RecordSource is the input port the Forwarder reads from. *tail.Tailer
// satisfies this interface; tests can supply a fake. Defining it here (rather
// than taking *tail.Tailer directly) keeps Forwarder testable in isolation
// and mirrors the symmetry of the Sink[T] output port. Pull-style: Run reads
// records one at a time and applies a per-call deadline derived from the
// MaxBatchAge timer to honour the age bound without a feeder goroutine.
type RecordSource interface {
    Next(ctx context.Context) (tail.Record, error)
    Commit(ctx context.Context, pos Position) error
    Done() <-chan struct{}
}

// Sink delivers a batch.
//   nil                              → batch durable downstream; advance cursor.
//   errors.Is(err, ErrPermanent)     → unrecoverable; Forwarder.Run returns err.
//   any other non-nil error          → retryable; Forwarder backs off and retries
//                                      the same batch (until ctx, until MaxAttempts,
//                                      or indefinitely if MaxAttempts == 0).
type Sink[T any] interface {
    Send(ctx context.Context, batch []T) error
}

type SinkFunc[T any] func(ctx context.Context, batch []T) error
func (f SinkFunc[T]) Send(ctx context.Context, batch []T) error { return f(ctx, batch) }

// Decoder converts a raw line to T. Most errors advance past the line so a
// malformed entry doesn't stall the pipeline. A decoder that returns an
// error wrapping ErrPermanent aborts Run without advancing the cursor —
// use that when the decoder cannot make progress (e.g. schema mismatch
// the caller wants to fix before resuming).
type Decoder[T any] func(line []byte) (T, error)

func IdentityDecoder(line []byte) ([]byte, error)   // no copy; warning in doc comment
func IdentityDecoderCopy(line []byte) ([]byte, error)
func JSONDecoder[T any]() Decoder[T]

type Options[T any] struct {
    Source  RecordSource    // typically *tail.Tailer; fakes for tests
    Decoder Decoder[T]
    Sink    Sink[T]

    // Batching — both bounds apply, whichever fires first.
    // All three reject negative values at construction time.
    MaxBatchRecords int
    MaxBatchBytes   int
    MaxBatchAge     time.Duration

    // Retry policy.
    InitialBackoff time.Duration
    MaxBackoff     time.Duration
    BackoffJitter  float64        // 0..1; 0 = deterministic (no implicit default).
                                  // 0.2 = conventional ±20% jitter.
    MaxAttempts    int            // 0 = unlimited

    // Hooks. OnCommitted fires after a successful Sink.Send + Source.Commit;
    // distinct from L2's OnCheckpoint (which fires on Cursor.Save).
    OnBatchSent    func(records int, bytes int, pos Position, latency time.Duration)
    OnSendError    func(err error, attempt int, willRetry bool)
    OnCommitted    func(pos Position)
    OnDecodeError  func(line []byte, pos Position, err error)
    // OnDropped intentionally absent at L3 — drops occur in the Tailer (L2)
    // before records reach the Forwarder. Use tail.Options.OnDropped instead.
    OnBackoffSleep func(d time.Duration, attempt int)

    Logger *slog.Logger
}

type Forwarder[T any] struct { /* unexported */ }

func New[T any](opts Options[T]) (*Forwarder[T], error)

// Run blocks until ctx canceled, Sink returns an error wrapping ErrPermanent,
// or Source signals exhaustion. Two exhaustion paths terminate Run cleanly:
// Source.Next returning tail.ErrSourceExhausted, and Source.Done() closing.
// Either path drains the in-flight batch with retries against the parent ctx
// so the last batch is still delivered. Returns nil on normal exhaustion,
// ctx.Err() on cancellation, or the Sink's permanent error otherwise. Run is
// one-shot: a Forwarder that has returned cannot be re-run; construct a new
// one to restart.
func (f *Forwarder[T]) Run(ctx context.Context) error

// ErrPermanent, when wrapped in a Sink.Send error, signals the Forwarder
// to stop retrying and exit Run with that error. Use for non-retryable
// failures (e.g. authentication failure, malformed payload schema).
var ErrPermanent = errors.New("forward: permanent sink error")
```

**Tailer invariants** (consolidated from §5.3, §5.6, §9 — the implementer must enforce all of these):

1. **Position monotonicity within an inode.** While `Position.Inode` is constant, `Position.Offset` is monotonically non-decreasing — except after a `Truncated` event, when it resets to 0. Tests must assert this never goes backwards otherwise.
2. **Inode change resets offset.** When the watcher reports rotation (new inode at path), the position emitted on the first record from the new file has `Offset = 0`. Cursor's saved position from the previous inode is irrelevant to the new file.
3. **Cursor never auto-advances.** A position is durable only after `Commit` or `CommitWithMeta` returns nil. `Records` and `Next` yield records but do not persist.
4. **Loaded checkpoint with stale inode triggers `OnMissingCheckpoint`.** If `Cursor.Load` returns a Checkpoint whose `Pos.Inode` doesn't match any current file in `Source.Enumerate`, the configured `MissingPolicy` decides resumption (default `FallbackOldest` — see §5.7).
5. **`Done()` is one-shot.** Only meaningful in `StopAtEOF: true` mode. Closes exactly once, after `Source.Enumerate` returns empty AND the active file's reader has hit EOF. Subsequent `Next` calls return `ErrSourceExhausted`.
6. **`Close()` is terminal.** After `Close`, all methods (`Next`, `Records`, `Commit`, `Position`, `Done`) return on a closed Tailer; `Close` itself is idempotent. Pending uncommitted line is discarded (see Decision #19).
7. **Single-instance lock held continuously.** When constructed with `WithFlock`, the lock is acquired before `tail.New` returns and released only by `Close` (via `Cursor.Close`). The lock is held continuously between those two points.

**Tailer covers two use cases with one type:** *live tail* (the default — Tailer runs forever until Close) and *one-shot backfill* (`StopAtEOF: true` — Tailer drains a fixed file then closes its `Done()` channel). The same struct serves both; only one method's behavior changes per mode (`Done()` only signals in StopAtEOF; `Close()`'s "discard pending uncommitted line" only matters for live tail, since backfill exits cleanly via Done). This is documented in the package doc; a single type avoids the constructor proliferation that two near-identical Tailer kinds would create.

### Two-tier API surface — what the user sees

| User goal | Entry point | Code shape |
|---|---|---|
| "Give me bytes from a file, follow rotation" | `watch.NewPolling` + manual `Read` loop | L1, ~10 LOC |
| "Give me lines, no persistence" | `watch.NewLineReader` | L1, ~5 LOC |
| "Give me lines, durable resume across restarts" | `tail.New` + `Records` | L2, ~15 LOC including cursor + flock |
| "Ship lines from disk to my HTTP endpoint, reliably" | `forward.New` + `forward.SinkFunc` | L3, ~20 LOC including decoder |
| "Backfill an archived file once" | L2 with `StopAtEOF: true` + L3 + `<-Tailer.Done()` | L2+L3, ~25 LOC |

---

## 5. Per-Feature Design

### 5.1 File-watch strategies

**Goal:** make the right thing happen by default; let users force a backend when they have a reason.

**Backends:**

| Backend | Available on | Latency | CPU at idle | Notes |
|---|---|---|---|---|
| `polling` | All platforms | `Interval` (default 1s) | One stat per interval per file | Always works. The default. |
| `fsnotify` | Linux (inotify), macOS (kqueue/FSEvents via fsnotify), *BSD (kqueue), Windows (ReadDirectoryChangesW) | <1 ms typical | ~0 | Default backend; falls back to polling on platforms where fsnotify reports unsupported. Opt out of the dep entirely with `-tags gotail_nofsnotify`. |

**Recommendation: fsnotify on by default with auto-fallback.** Distroless / minimal builds can drop the dep with `-tags gotail_nofsnotify`; runtime opt-out per-Tailer is `tail.Options.ForcePolling = true`.

**API sketch:**

```go
// Default constructor: tries fsnotify if available, falls back to polling.
func watch.New(c Config) (Watcher, error)

// Explicit constructors.
func watch.NewPolling(c Config) (Watcher, error)
func watch.NewFsnotify(c Config) (Watcher, error)  // ErrUnsupported if not built in
```

**Build tag scheme:**

- Default build: fsnotify backend included, `watch.New` prefers fsnotify with polling fallback.
- `-tags gotail_nofsnotify`: drops `github.com/fsnotify/fsnotify` from the module graph, `NewFsnotify` returns `ErrUnsupported`, `watch.New` falls back to polling.

This makes "fsnotify just works" the default while preserving an escape hatch for distroless / dependency-minimal builds. Document this in the README.

**Why not write fsnotify ourselves?** We could. Linux inotify is ~150 LOC of `unix.InotifyInit1` etc.; macOS kqueue is similar; Windows is `ReadDirectoryChangesW`. But fsnotify (the project) has fixed dozens of platform-specific edge cases over a decade — kqueue rename semantics, Windows path normalization, Linux event coalescing. Re-implementing risks reproducing bugs that are already fixed. Build-tag in fsnotify, but make the dep optional.

**Tradeoffs:**

- fsnotify on Linux misses changes when the watch limit (`fs.inotify.max_user_watches`) is hit. Polling doesn't have this failure mode. Document it. Consider a hybrid mode: fsnotify for the active file (where most events happen) + a slow poll (e.g. 10s) as a backstop.
- fsnotify on macOS via FSEvents has coarser granularity than inotify — events are coalesced. Latency is fine for tailing but not for sub-millisecond accuracy.
- fsnotify cannot watch a file that doesn't exist yet. We need to watch the *parent directory* and react to creates. Already what the fsnotify package does, but worth noting.

**Hybrid mode (not yet implemented):** fsnotify watches the parent directory for create/rename events; the active file is read until EOF on each notification; a 10s poll is a backstop for missed events. This is what `tail -F` does on modern systems. Kept out of the current surface to stay small; the API is designed so it can be added without breaking changes (a `WatchMode` enum: `Poll | Fsnotify | Hybrid`).

### 5.2 Platform support

**Goal:** Linux, macOS, *BSD work first-class. Windows works for the common case.

| Platform | Inode | Polling | fsnotify | Notes |
|---|---|---|---|---|
| Linux | `syscall.Stat_t.Ino` | Yes | Yes (inotify) | Reference platform. |
| macOS | `syscall.Stat_t.Ino` | Yes | Yes (kqueue/FSEvents) | First-class. |
| FreeBSD/NetBSD/OpenBSD | `syscall.Stat_t.Ino` | Yes | Yes (kqueue) | First-class. |
| Windows | `GetFileInformationByHandle` `nFileIndexHigh:nFileIndexLow` | Yes | Yes (ReadDirectoryChangesW) | Inode-equivalent but not stable across all filesystems (e.g. ReFS). Documented. |
| Plan 9, JS/WASM | n/a | No | No | Not supported. Build constraints exclude. |

**Implementation plan for inode/file-id:**

- `stat_unix.go` (build tag `unix`): pulls inode from `syscall.Stat_t.Ino`. No `golang.org/x/sys` dep needed; stdlib `syscall` suffices on the platforms we target.
- `stat_windows.go` (build tag `windows`): calls `GetFileInformationByHandle` via `syscall.GetFileInformationByHandle`. Combine `nFileIndexHigh<<32 | nFileIndexLow` into a `uint64`. Note in doc comment: stable on NTFS, *not* stable on ReFS or some network filesystems.
- `tail.Options.NoInodeCheck bool` — disables the equality check entirely. The flag lives on `Options` (not on `FileCursorOption`) because the inode comparison happens in the watcher / `findFileByInode`, not inside the cursor. Use case: Windows ReFS, FUSE mounts, anything where inode reuse is known to happen. Trades robustness against rotation for cross-platform predictability.

**Hard parts called out:**

- **Windows file locking semantics differ from Unix.** Default Windows opens are non-shared by default; we must use `FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE` so writers can keep appending while we read. Already what `os.Open` does via `syscall.Open` on Windows, but verify with a test.
- **Windows rename semantics.** On Windows you cannot rename over an open file; lumberjack on Windows handles this via `MoveFileEx`, but a naive rotation tool may fail. Out of scope to fix in gotail, but document that we follow whatever the writer does.
- **Symlink handling.** gotail opens with `os.Open`, which follows symlinks, so we tail the *target*, not the link. If the symlink is swapped (k8s ConfigMap-style), the inode changes and we treat it as rotation.
- **Plan 9 / WASM.** Excluded via build constraints. The `stat_*.go` files use `//go:build unix || windows`.

### 5.3 Built-in checkpointing

**Goal:** correctness against power loss, single-instance protection, optional user metadata, pluggable backend.

**Atomic write protocol:**

1. Open `<cursor_path>.tmp` with `O_WRONLY|O_CREATE|O_TRUNC|O_EXCL|O_NOFOLLOW`. A pre-existing symlink is refused; a stale regular tmp from a crashed prior write is unlinked first so `O_EXCL` succeeds. (Windows variants refuse reparse points via `os.Lstat` before opening.)
2. Write the cursor bytes.
3. `f.Sync()` (fsync the data + metadata of the temp file).
4. `f.Close()` — close before renaming (Windows requirement; also surfaces NFS/async-mount close-time errors).
5. `os.Rename(tmp, final)`.
6. (Optional, default on) `dir.Sync()` to fsync the directory entry. Required for power-loss durability of the rename itself on ext4/xfs/etc.

This is the standard atomic-update pattern. Implementation lives in `internal/atomicwrite/` so L2 and any future user can share it.

**Cursor file format (JSON):**

```json
{
  "pos": {
    "file": "/var/log/app/events.log",
    "inode": "12345678",
    "offset": "4096"
  },
  "meta": { "user-defined": "anything" },
  "version": 1
}
```

JSON is chosen because:
- The file is ~100-500 bytes, encoding cost is irrelevant.
- Human-inspectable for debugging (`cat events.cursor`).
- Schema evolution is trivial (`version` field; ignore unknown fields).
- Stdlib only — no protobuf, no msgpack.

**`SyncMode` trade-offs:**

| Mode | Save() durability | Save() latency | Loss on crash |
|---|---|---|---|
| `SyncAlways` (default) | Bytes are on platter when Save returns | ~5-50 ms (fsync cost) | None (last successful Save persisted) |
| `SyncOnCommit` | Save buffers in memory; explicit `Sync()` call flushes | ~µs for Save, fsync cost on Sync | Anything saved since last explicit Sync |
| `SyncBackground` | Background goroutine flushes every N ms | µs for Save | Up to N ms of saves |

Default is `SyncAlways` because the canonical consumer pattern requires "cursor never lags by more than one syscall" — losing one batch is acceptable, losing visibility into which batches were lost is not. Higher-throughput consumers can opt down.

**Custom metadata:**

`Tailer.CommitWithMeta(ctx, pos, meta any)` JSON-encodes `meta` into the `meta` field of the cursor file. Use cases:
- Forwarders: store the last-successful-batch's request ID for diagnostics or upstream idempotency.
- Aggregators: store a hash chain for tamper-evidence.
- Multi-stream: store per-source progress when one cursor tracks multiple logical streams.

The library does *not* interpret `meta`. Round-trips as `json.RawMessage`. On `Load`, callers `json.Unmarshal(c.Meta, &myType)`.

**API:**

```go
type Cursor interface {
    Load(ctx context.Context) (Checkpoint, bool, error)
    Save(ctx context.Context, c Checkpoint) error
    Close() error
}

type Checkpoint struct {
    Pos  Position
    Meta json.RawMessage
}

func NewFileCursor(path string, opts ...FileCursorOption) (Cursor, error)
func NewMemoryCursor() Cursor

// Options
WithDirSync(on bool)
WithFlock(lockPath string)        // must differ from cursor path (NewFileCursor errors on equality)
WithSyncMode(SyncMode)
WithFileMode(os.FileMode)         // default 0o600; rejects group/world-write
                                   // and special bits (setuid/setgid/sticky)
WithSyncBackgroundInterval(d)     // requires WithSyncMode(SyncBackground); errors otherwise
```

**Pluggability:** because `Cursor` is an interface, users can plug in alternatives — Redis-backed, SQL-backed, Consul KV, etc. The library ships file + memory; everything else is BYO.

### 5.4 flock / file locking

**Goal:** prevent two processes from sharing a state directory and double-shipping events.

**Design:**

`tail.WithFlock(lockPath string)` is a `FileCursorOption`. When set:

1. On `NewFileCursor`, open `lockPath` with `O_CREATE | O_RDWR | O_NOFOLLOW`, mode `0o600` (Unix); on Windows, refuse reparse points via `os.Lstat` before opening. Reject construction when `lockPath` equals the cursor path — renaming the cursor would lose the lock on Linux (the lock is on the inode, not the path).
2. Call `syscall.Flock(fd, LOCK_EX | LOCK_NB)` (Unix) or `LockFileEx` with `LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY` (Windows).
3. On lock failure, return `ErrLockHeld` wrapped in a descriptive error (`fmt.Errorf("lock held on %s: %w", lockPath, ErrLockHeld)`).
4. On `Close`, the file descriptor is closed; the OS releases the lock automatically.
5. Write the holder's PID into the lock file's content for diagnostics (best-effort, not used for logic).

**Why advisory not mandatory:**

- Mandatory locks on Linux are deprecated and require mount-time options.
- Advisory is enough: gotail processes *cooperate*; we're not defending against malicious processes.

**Why a separate lock file (`events.cursor.lock`) and not the cursor itself:**

- We rewrite the cursor file via `os.Rename`. Renaming over an open, locked file would lose the lock on Linux (the lock is on the inode, not the path). The lock file is never replaced.
- Driving requirement #3 allows either "flock on the cursor or a sibling `.lock`"; sibling is the safer choice for the reason above.

**Lifecycle:**

- Acquired in `NewFileCursor` (i.e. before `tail.New` returns).
- Released in `Cursor.Close` (called by `Tailer.Close`).
- Consumers should signal readiness to their supervisor (e.g. systemd `READY=1`) *after* `tail.New` returns successfully — so by construction the lock is held before the process declares readiness.

**Platform notes:**

- **Linux:** `syscall.Flock`. Stdlib. Lock survives `fork` but not `exec`.
- **macOS/BSD:** `syscall.Flock`. Same semantics as Linux.
- **Windows:** `LockFileEx`. Range-locks the first byte. Also stdlib (`syscall`).

### 5.5 Metrics hooks (BYO metrics lib)

**Goal:** the library exposes the events a metrics consumer cares about. The library ships zero metrics-lib dependencies. Consumers wire the hook callbacks to whatever they use (Prometheus, OTel, statsd, custom).

**Hook surface:**

| Layer | Hook | Signature | When |
|---|---|---|---|
| L2 | `OnDropped` | `func(droppedFiles int)` | Cursor's file is missing on resume; `droppedFiles` is always 1 (signals "a drop occurred"; precise historical count is not tracked) |
| L2 | `OnRotated` | `func(from, to Position)` | Fires on rotation. Two firing sites: (a) LineReader-detected in-place rotation (new inode at watched path), (b) Tailer.advance stepping to the next file in the Source enumeration |
| L2 | `OnTruncated` | `func(at Position)` | File size dropped below current position |
| L2 | `OnCheckpoint` | `func(c Checkpoint)` | Cursor.Save returned successfully |
| L2 | `OnError` | `func(err error)` | Non-fatal error during tail |
| L3 | `OnBatchSent` | `func(records int, bytes int, pos Position, latency time.Duration)` | Sink.Send returned nil |
| L3 | `OnSendError` | `func(err error, attempt int, willRetry bool)` | Sink.Send returned err |
| L3 | `OnCommitted` | `func(pos Position)` | Forwarder advanced cursor (post Sink.Send + Source.Commit success) |
| L3 | `OnDecodeError` | `func(line []byte, pos Position, err error)` | Decoder failed; line is being skipped |
| L3 | `OnBackoffSleep` | `func(d time.Duration, attempt int)` | About to sleep d before retry attempt |

**Why callbacks not channels:**

- Synchronous: a metrics increment is a non-blocking op in any sane lib (`prometheus.Counter.Inc()` is one atomic op). Channels add buffering, dropping policy, goroutine lifecycle — all worse.
- Nil-safe: every hook is checked `if h != nil { h(...) }`.
- Zero allocation: no closure box for nil hooks.

**Documentation:** ship a `docs/metrics-prometheus.md` and `docs/metrics-otel.md` showing how to wire each hook. No code dep, just example wiring.

**Example (Prometheus, in docs only):**

```go
batchesSent := prometheus.NewCounter(...)
batchBytes := prometheus.NewHistogram(...)

forward.New(forward.Options[[]byte]{
    OnBatchSent: func(records int, bytes int, pos forward.Position, latency time.Duration) {
        batchesSent.Inc()
        batchBytes.Observe(float64(bytes))
    },
    // ...
})
```

### 5.6 Better file rotation tracking

**Goal:** correct behavior against every common rotation scheme, with explicit documented behavior for each.

**Rotation schemes to handle:**

| Scheme | How it works | Detection | Behavior |
|---|---|---|---|
| **rename + create** (logrotate default, lumberjack) | Old file renamed to `.1`, new file created at original path | New inode at named path != currently-open inode | Drain old file to EOF, close, open new file at offset 0. |
| **copytruncate** (logrotate `copytruncate`) | Old file is `cp`'d to `.1`, then `:>` truncates the original to size 0; **inode unchanged** | File size < current position | Detect via stat, emit `OnTruncated`, reset position to 0 (or to the new size if mid-write), continue reading from current open fd. |
| **symlink swap** (k8s pod logs, atomic config swap) | Symlink target replaced atomically | New inode at named path != currently-open inode | Same as rename+create. |
| **inode reuse** | Old file deleted, new file created, OS reuses the same inode | Stat says same inode but size < position | Treat as truncation: same path as copytruncate. The "size < position" check handles it. |
| **mid-write truncation** | Process truncates own log without rotating | Size drops mid-tail | Emit `OnTruncated`; reset position to 0; continue. |
| **delete + recreate** | Old file unlinked, new file created at same path | New inode appears at path; we still hold the old fd open | Drain old file (via the trailing-bytes drain on the still-open fd), close, open new. |

**Implementation notes:**

- Truncation detection lives in `pollWatcher.Wait` next to the existing rotation logic. Pseudocode:
  ```
  if currentState.Size < currentState.Position:
      emit OnTruncated
      seek fd to 0 (or to new size, then rewind to 0 for safety)
      currentState.Position = 0
      continue read loop
  ```
- `OnTruncated` fires from two sites, by design: the watcher event loop catches the common case, and `LineReader.Next` catches copytruncate races the watcher missed (the `fi.Size() < l.pos.Offset` block — small windows where a truncation lands between watcher ticks and the LineReader is mid-refill). Hook authors must handle both invocation paths idempotently.
- copytruncate detection requires per-tick stat of the *open fd* (not just the path). `pollWatcher.Wait` (`watch/poll.go`) does this, handling `size < position` by truncating instead of declaring an error.
- Inode reuse + small-file edge case: if the new file is opened via the `rename+create` path but happens to inherit the same inode the old file had (rare but possible after long uptime), a resume-time inode check would otherwise seek into it. Mitigation: when crossing rotation, *always* start the new file at offset 0 regardless of any `Resume` point. This is how `pollWatcher.Wait` (`watch/poll.go`) behaves — it consults the `Resume` position only on the very first open.

**Open fd rotation hardening:** the old fd is closed as soon as rotation is confirmed — holding stale fds open delays the kernel reclaiming the disk space (the unlinked file can't be freed until all fds close). For multi-file `Source`s the lifecycle is per-file: each file gets its own open/read/close cycle.

**Test scenarios** (each becomes a test case):

1. Active file written to, rotated via rename+create, written to new file. Verify all bytes from old file are read before any from new file. (Already covered by `TestReadAfterWatcher`, `tail_test.go:141`.)
2. copytruncate: write, copy file content elsewhere, truncate original to 0, write new content. Verify old content read once, then truncation event, then new content.
3. Symlink swap: create symlink to fileA, tail, swap symlink to fileB atomically. Verify rotation detection.
4. Inode reuse: write+delete fileA, create fileB at same path with same inode (force via bind-mount in CI or accept "best effort" detection). Verify size-decrease check catches it.
5. Mid-write truncation: writer calls `Truncate(0)` mid-stream. Verify `OnTruncated` fires and reading resumes from offset 0.

### 5.7 Custom metadata in checkpoint

**Goal:** allow callers to persist their own state alongside the byte offset.

**API:**

```go
type Checkpoint struct {
    Pos  Position
    Meta json.RawMessage   // opaque
}

func (t *Tailer) CommitWithMeta(ctx context.Context, pos Position, meta any) error

func (c Cursor) Load(ctx context.Context) (Checkpoint, bool, error)
```

**Semantics:**

- `Meta` is JSON-encoded bytes. Library does not interpret.
- On `Commit` (no meta), the existing `Meta` is preserved. On `CommitWithMeta`, it's overwritten.
- Decoding is the caller's responsibility: `json.Unmarshal(checkpoint.Meta, &myStruct)`.
- Size limit: documented at 64 KiB to keep the cursor file small. Library returns an error on oversized meta to prevent accidental log-aggregation footguns (e.g. someone storing a recent batch in meta).

**Use cases:**

- Forwarders: persist the last successful upstream request ID. Useful when the upstream supports idempotency keys for at-most-once semantics.
- Multi-source aggregators: persist progress on multiple logical streams from one Tailer.
- Tamper-evidence: persist a running hash of all forwarded content.

**Why not give it a generic type parameter (`Cursor[M]`)?** Two reasons: the cursor is an interface a user might back with Redis or SQL, and forcing every backend to be parametric multiplies the surface. Storing as `json.RawMessage` keeps the seam clean. Callers who want type safety can wrap:

```go
type MyCursor struct{ tail.Cursor }
func (c *MyCursor) SaveMeta(ctx context.Context, pos Position, m MyMeta) error { ... }
```

### 5.8 High-level API encapsulating everything

The "easy" path for a forwarder-shaped consumer (lumberjack-rotated active log + persistent cursor + batched HTTPS sink):

```go
src := tail.Lumberjack("/var/lib/myapp/events.log")
cur, err := tail.NewFileCursor(
    "/var/lib/myapp/events.cursor",
    tail.WithFlock("/var/lib/myapp/events.cursor.lock"),
)
if err != nil { return err }

t, err := tail.New(ctx, tail.Options{
    Source: src,
    Cursor: cur,
    Logger: slog.Default(),
})
if err != nil { return err }
defer t.Close()

fwd, err := forward.New(forward.Options[[]byte]{
    Source:          t,                          // *tail.Tailer satisfies forward.RecordSource
    Decoder:         forward.IdentityDecoderCopy,
    Sink:            mySink,
    MaxBatchBytes:   1 << 20,
    MaxBatchAge:     5 * time.Second,
    InitialBackoff:  500 * time.Millisecond,
    MaxBackoff:      60 * time.Second,
})
if err != nil { return err }

return fwd.Run(ctx)
```

That's it. Every driving requirement from §3 (1–7) is satisfied by ~25 lines of glue. Each line maps to a specific feature:

| Line | Feature |
|---|---|
| `tail.Lumberjack(...)` | Multi-file rotation chain (5.6); driving req 1 |
| `NewFileCursor` | Persistent cursor (5.3); driving reqs 2a/2b/2c |
| `WithFlock` | Single-instance protection (5.4); driving req 3 |
| `Options.Source/Cursor` | Resumable tail across restarts (5.3) |
| `forward.New[[]byte]` | Generic forwarder (5.9) |
| `MaxBatchBytes/Age` | Batched delivery (driving req 5a) |
| `InitialBackoff/MaxBackoff` | Exponential retry (driving req 5b) |
| `IdentityDecoderCopy` | Memory-safety helper (§6) |
| Defaults for `OnMissingCheckpoint` | Rotation-outran-outage fallback (driving req 4) |

### 5.9 Generics where they add value

| Where | Why generics help | Where they don't |
|---|---|---|
| `Sink[T]` | Caller chooses record type (raw bytes, parsed struct, custom DTO) | n/a |
| `Decoder[T]` | Pairs with `Sink[T]`; type-safe pipeline | n/a |
| `Forwarder[T]` | Carries the type through the pipeline | n/a |
| `Options[T]` | Hooks reference T (`func(batch []T)`) | n/a |
| `Cursor` | Would force every backend to be parametric for one field | Use `json.RawMessage` instead |
| `Watcher` / `LineReader` | Lines are bytes; making them generic adds no value | Stay `[]byte` |
| `Tailer.Records` | Same — bytes-in, bytes-out | Stay `iter.Seq2[Record, error]` |

**Concretely:**

```go
// Ship raw bytes (forwarder default — file format == wire format).
fwd, _ := forward.New(forward.Options[[]byte]{
    Decoder: forward.IdentityDecoderCopy,
    Sink:    mySink, // forward.Sink[[]byte]
})

// Ship parsed events.
type Event struct{ Level, Msg string }
fwd, _ := forward.New(forward.Options[Event]{
    Decoder: forward.JSONDecoder[Event](),
    Sink:    myEventSink, // forward.Sink[Event]
})
```

**Avoid the generics-everywhere trap.** L1 and L2 stay byte-oriented. Generics at L3 only.

### 5.10 (Bonus) Additional improvements not on the user's list

Additional improvements beyond the core requirements:

1. **`watch.NewLineReader` honors `bufio.Scanner`-style buffer caps.** A configurable `MaxLine` (default 1 MiB) bounds line length, with `ErrLineTooLong` returned when exceeded; the `OnError` hook fires and the reader skips to the next newline. Without a cap, a pathologically long line would OOM the process.

2. **Header skipping / start-from-end-of-current.** Many log readers want "tail -n 0" (skip everything currently in the file, only return new lines). Already supported via `Whence: io.SeekEnd`, but make it discoverable in the high-level API: `tail.Options.SkipExisting bool`.

3. **Position invariant tests.** Property-based test: for any sequence of `(write, rotate, write, ...)` events, every byte written by the writer is yielded exactly once across all `Records` calls. Fuzz the timing of poll ticks. Catches whole classes of regressions in the rotation logic.

4. **Graceful shutdown semantics.** `Tailer.Close()` discards any pending uncommitted line and does not advance the cursor (Decision #19); it is terminal and idempotent. Callers that want the most-recently-yielded position persisted before exit use `Tailer.CloseWithFlush(ctx)`, which saves the cursor — and drives `Sync` under `SyncOnCommit`/`SyncBackground` — before the normal teardown.

5. **`Tailer.Stats()` snapshot.** A pull-style alternative to push-style hooks, for callers that prefer scraping. Returns counts of bytes read, lines yielded, rotations seen, errors, current position. Atomic snapshot; cheap.

6. **Backoff jitter.** Full-jitter exponential backoff with a configurable jitter factor. Prevents thundering-herd retries when a fleet of gotail-using processes sees the same upstream blip.

7. **Context-aware `Sink`.** The `Sink[T].Send(ctx, batch)` signature includes the context. A misbehaving sink that ignores ctx will hang the forwarder; document that ctx must be respected, and provide a helper `WithSinkTimeout[T](d)` that wraps a `Sink[T]` in a per-call context with deadline. The helper is parameterised on T so the wrapped sink retains its element type.

8. **`forward.RecordingSink[T]` and `FailingSink[T]` test helpers.** In a `forwardtest` subpackage. Saves every consumer from rewriting their own.

9. **`tail.WithCursorMigration(fn)`.** Lets a caller migrate an old cursor format to the new schema on `Load`. Hook fires when the on-disk `version` field is unknown; user-supplied function returns the migrated `Checkpoint`.

10. **Slog discipline.** Every log line in the library uses `slog` with consistent attribute keys: `path`, `inode`, `offset`, `attempt`, `err`, `latency_ms`. Documented in package doc.

11. **`watch.Position.IsZero()`.** Trivial, but having a canonical zero check prevents the bug where consumers compare positions field-by-field.

12. **No `golang.org/x/sys` direct dependency.** Inode stat uses stdlib `syscall.Stat_t`, which is sufficient on every platform we target; `x/sys` appears only as an indirect dep pulled by `fsnotify` (and is dropped entirely under `gotail_nofsnotify`).

---

## 6. Performance & Memory Strategy

**Hot path:** `LineReader.Next` is called once per line. For a 100k lines/sec log, this is the inner loop. Minimize allocations and syscalls per call.

### Allocation budget

Target: **zero allocations per line on the happy path** (line fits in buffer, no rotation, no error).

#### Strategy 1: Owned buffer, no allocation per line

Replace `bufio.Reader.ReadBytes('\n')` (which allocates a fresh `[]byte` per line) with manual scanning over a pooled buffer.

```go
// pseudocode
type LineReader struct {
    buf  []byte    // backing buffer, owned, reused across calls
    head int       // start of unconsumed data
    tail int       // end of unconsumed data
    line []byte    // returned slice; aliases buf[head:newline]
}

func (l *LineReader) Next(ctx context.Context) ([]byte, Position, error) {
    for {
        // Look for newline in buf[head:tail].
        if i := bytes.IndexByte(l.buf[l.head:l.tail], '\n'); i >= 0 {
            line := l.buf[l.head : l.head+i]
            l.head += i + 1
            l.pos.Offset += int64(i + 1)
            return trimCR(line), l.pos, nil
        }
        // No newline — refill.
        if err := l.refill(ctx); err != nil { return nil, l.pos, err }
    }
}
```

Buffer ownership rule: **the returned `[]byte` is valid until the next call to `Next` or `Close`.** Callers must copy if they retain. Document prominently. This matches `bufio.Scanner.Bytes()` semantics, which Go developers already know.

#### Strategy 2: `sync.Pool` for transient line copies — not used

The library does not pool line copies. `Forwarder[T]` is generic, so a pool can only target `T = []byte`, and there is no clean type-generic seam for returning a buffer to a pool after `Sink.Send`. `IdentityDecoderCopy` users who care about allocations already have the `Decoder[T]` callback as their BYO pool seam.

`BenchmarkForwarder_Throughput` (2026-05-04, Apple M4 Pro):

```
BenchmarkForwarder_Throughput-14   6930673   177.0 ns/op   5649159 records/s   224 B/op   4 allocs/op
BenchmarkForwarder_Throughput-14   6680863   177.7 ns/op   5626642 records/s   224 B/op   4 allocs/op
BenchmarkForwarder_Throughput-14   6646902   178.4 ns/op   5604866 records/s   224 B/op   4 allocs/op
```

~5.6M records/sec; 4 allocs/op on the hot path. Acceptable without pool complexity.

#### Strategy 3: Avoid `bufio.NewReader` reallocation on rotation

The `LineReader` owns its buffer; on rotation it just resets the buffer (`head = tail = 0`) — the buffer survives, only the underlying `io.Reader` changes. Allocating a fresh buffer per reopen would, under aggressive rotation, be a leak in slow-motion.

#### Strategy 4: Pool `Record`s (L2)

`Record{Line []byte, Pos Position}` is passed by value through `iter.Seq2[Record, error]`, so escape analysis should keep it on the stack. Verify with `go build -gcflags='-m'` and benchmark. If the iterator's closure forces escape, switch the internal representation to pointer-and-pool.

### Syscall budget

- **Polling watcher: 1 stat per poll interval per file.** Default 1s = 1 syscall/sec/file. Bounded.
- **fsnotify watcher: 0 syscalls at idle.** Events drive wakeups.
- **Reading: 1 read per buffer refill.** With 64 KiB buffer and 100-byte avg lines = 1 syscall per ~640 lines. Bounded.
- **Cursor save: 2-3 syscalls (write, fsync, rename, optionally fsync-dir).** Caller controls frequency via Commit calls.

### Benchmarks to add

`watch/bench_test.go`, `tail/bench_test.go`, `forward/bench_test.go`. Targets:

| Benchmark | Target | Reason |
|---|---|---|
| `BenchmarkLineReader_NoAlloc` | 0 allocs/op | Validate hot-path strategy 1 |
| `BenchmarkLineReader_LongLines` | <1 alloc/op for 1 MiB lines | Buffer growth path |
| `BenchmarkPolling_Overhead` | <100 ns/op steady state | Cost of one poll tick with no work |
| `BenchmarkRotation_Latency` | <2× interval P99 | Rotation detection responsiveness |
| `BenchmarkForwarder_Throughput` | >100k records/sec | End-to-end on a no-op sink |
| `BenchmarkCursor_Save` | <100 µs P50 (no fsync), <10 ms P99 (with fsync) | Commit cost |

Run as `go test -bench=. -benchmem` per package. CI gate: regression tolerance of 10% versus the prior commit (using `benchstat`).

### CPU profile checkpoints

After implementation, profile with:

```
go test -bench=BenchmarkForwarder_Throughput -cpuprofile=cpu.prof
go tool pprof cpu.prof
```

Top 5 functions should be: `bytes.IndexByte`, `os.(*File).Read`, `runtime.memmove`, `Tailer.Records`'s closure, `Forwarder.Run`'s loop. If `runtime.mallocgc` shows up in the top 5, the zero-alloc invariant is broken.

---

## 7. Testing Strategy

### Test layers

1. **Unit tests** — per-file, per-function. Keep the existing `TestReadAfterWatcher` (rename appropriately) and `TestLineReaderResume`; add coverage for everything else.
2. **Property-based tests** — single big invariant: every byte written is yielded exactly once.
3. **Platform-specific tests** — build-tagged.
4. **Integration tests** — real lumberjack writer + real `httptest.Server` sink + the L3 forwarder.
5. **Fuzz tests** — line framing, cursor format parsing.

### Required new unit tests (per package)

#### `watch/`

- Re-port `TestReadAfterWatcher` (currently `tail_test.go:141`) with the new API.
- `TestPollingWatcher_FileNotExistInitially` — start watching before the file exists; verify it picks up the file when created.
- `TestPollingWatcher_Truncation` — write, truncate, write; verify `OnTruncated`-equivalent path returns from offset 0 with `ErrTruncated` or via callback.
- `TestPollingWatcher_SymlinkSwap` — `os.Symlink` to fileA, tail, then `os.Rename` symlink to fileB; verify rotation detection.
- `TestLineReader_LongLine` — line longer than `MaxLine`; verify `ErrLineTooLong` and recovery.
- `TestLineReader_CRLF` — pins the CRLF stripping behavior.
- `TestLineReader_BufferReuse` — assert returned slice is invalidated by next `Next` call (use a probe that mutates).
- `TestLineReader_NoAlloc` — `testing.AllocsPerRun` must be 0 on the happy path.
- `TestContextCancellation` — `ctx.Cancel()` mid-`Wait` returns promptly with `ctx.Err()`.
- `TestFsnotify_FallbackToPolling` (build-tagged) — mock fsnotify error; verify fallback constructor falls back.

#### `tail/`

- `TestLumberjackSource_OrderedEnumeration` — given `events.log` and three timestamped backups, enumerate oldest-first.
- `TestSingleFileSource` — trivial, but serves as the reference implementation.
- `TestGlobSource_Patterns` — verify glob matching matches expected files only.
- `TestFileCursor_AtomicSave` — interrupt mid-save (kill -9 simulation via `os.Remove(tmp)`); verify on-disk file is either fully old or fully new.
- `TestFileCursor_DirSync` — verify `WithDirSync(false)` skips the parent fsync (use `strace`-equivalent or just verify the option threads through).
- `TestFileCursor_Flock_Conflict` — open two cursors on the same lock path; second returns `ErrLockHeld`.
- `TestFileCursor_Meta_RoundTrip` — save with meta, load, unmarshal, verify equality.
- `TestFileCursor_Migration` — save in v0 schema, load with `WithCursorMigration`, verify migration ran.
- `TestTailer_ResumeAcrossRestart` — write, read 10 lines, commit, close; new Tailer with same cursor; verify resumes at line 11.
- `TestTailer_MissingCheckpoint_FallbackOldest` — cursor names a deleted file; verify resumption at oldest still-present, `OnDropped` fires with correct count.
- `TestTailer_MissingCheckpoint_Fail` — same, but `MissingPolicy: Fail`; verify error.
- `TestTailer_MissingCheckpoint_SkipToActive` — same, but `SkipToActive`; verify resumes at active offset 0.
- `TestTailer_StopAtEOF_ClosesDone` — exhaust source; verify `<-Done()` returns.
- `TestTailer_Records_Iterator` — exercise the `iter.Seq2` form.
- `TestTailer_CommitWithMeta` — save user metadata, reload, verify round-trip.

#### `forward/`

- `TestForwarder_BatchByCount` — `MaxBatchRecords: 10`, send 25 records, verify three batches of 10/10/5.
- `TestForwarder_BatchByBytes` — `MaxBatchBytes`; verify batch ends at byte threshold.
- `TestForwarder_BatchByAge` — `MaxBatchAge: 100ms`; one record arrives, age timeout fires, batch sent with one record.
- `TestForwarder_RetryOnError` — Sink returns error twice, succeeds third; verify backoff sleeps observed via `OnBackoffSleep`, cursor not advanced until success.
- `TestForwarder_ContextCancellation` — cancel ctx mid-retry; verify Run returns ctx.Err() promptly.
- `TestForwarder_DecodeErrorSkips` — decoder returns error on line 5; verify line 5 skipped, cursor advanced past it, `OnDecodeError` fired.
- `TestForwarder_GenericTypes` — `Forwarder[MyEvent]` + `JSONDecoder[MyEvent]`; verify type flow.
- `TestForwarder_StopAtEOF` — Tailer exhausts; Run returns nil.
- `TestForwarder_RecordingSink` — `forwardtest.RecordingSink[T]` captures batches.

### Property-based tests

Use `testing/quick` (stdlib) or `github.com/leanovate/gopter` (one external dep, *for tests only*; `_test.go` doesn't affect lib dep count). Recommended: stdlib `testing/quick` to keep lib + tests dep-clean.

**Invariant 1 (offset accuracy):** for any random sequence of writes interleaved with rotations, the concatenation of all yielded `Record.Line`s (with newlines re-added) equals the concatenation of all bytes written by the writer.

**Invariant 2 (commit durability):** for any random sequence of (write, commit, simulated-crash, restart), no byte is yielded twice, and no committed byte is lost.

**Invariant 3 (rotation safety):** when the rotation race is provoked (write to old file *after* new file exists at path *before* old file's prior size was checked), all bytes from the old file are still yielded before any from the new file. The trailing-bytes drain on the still-open fd protects this; the test ensures we don't regress.

### Platform-specific tests

```go
//go:build linux

func TestInotifyBackend(t *testing.T) { ... }
```

```go
//go:build darwin

func TestKqueueBackend(t *testing.T) { ... }
```

```go
//go:build windows

func TestWindowsFileID(t *testing.T) { ... }
func TestWindowsRotation(t *testing.T) { ... }
```

CI matrix runs Linux + macOS + Windows + (optionally) FreeBSD. GitHub Actions free tier covers all four.

### Fuzz tests

```go
func FuzzLineReader(f *testing.F) {
    f.Fuzz(func(t *testing.T, input []byte, splits []byte) {
        // Feed input to LineReader in chunks of size splits[i].
        // Verify concatenation of yielded lines (with \n) == input.
    })
}

func FuzzCursorParse(f *testing.F) {
    f.Fuzz(func(t *testing.T, data []byte) {
        var c Checkpoint
        _ = json.Unmarshal(data, &c)  // must not panic
    })
}
```

### Fault injection

- Disk full mid-`Save` — verify atomic-write protocol leaves the old cursor file intact (no partial write at the final path).
- Kill -9 mid-tmp-write — verify recovery on next start ignores the orphan tmp file (or cleans it).
- Permission denied on cursor file — verify clear error, not a panic.

### CI gates

| Gate | Tool |
|---|---|
| `go vet ./...` | stdlib |
| `staticcheck ./...` | honnef.co/go/tools |
| `go test -race ./...` | stdlib |
| `go test -bench=. -benchmem` regression | benchstat |
| Cross-compile to `linux/amd64`, `linux/arm64`, `darwin/arm64`, `windows/amd64`, `freebsd/amd64` | stdlib |
| `golangci-lint run` (govet, errcheck, ineffassign, unused, gosimple) | golangci-lint |

---

## 8. Module versioning

- The module path carries the major version: `github.com/jacobcase/gotail/v3`.
- Sub-packages: `.../v3/watch`, `.../v3/tail`, `.../v3/forward`.
- A new major version — and the corresponding `/vN` path bump — is cut only for a backward-incompatible change to an exported identifier (verified with `apidiff` against the latest released tag). Additive changes ship as a minor release; bug fixes as a patch.

---

## 9. Decisions

Design decisions and their rationale.

| # | Question | Decision | Rationale |
|---|---|---|---|
| 1 | Module name: `github.com/jacobcase/gotail/vN` (vN suffix) or new repo? | `gotail/vN` suffix | Standard Go module-versioning idiom; preserves history. |
| 2 | Min Go version | **Go 1.26 or later** | User-specified. Comfortably above the 1.23 floor needed for `iter.Seq2`; gives access to all current stdlib improvements. |
| 3 | fsnotify: vendor it via build tag, or build our own? | Build tag in fsnotify. Default off. | Re-implementing risks reproducing platform bugs fsnotify already fixed. Build tag preserves zero-deps default. |
| 4 | Drop `golang.org/x/sys` as a direct dependency? | Yes. Stdlib `syscall` is sufficient on every platform we target. `x/sys` remains in `go.mod` as an indirect dep pulled by `fsnotify`; the `gotail_nofsnotify` build tag drops it entirely. | Direct-dep-clean is the goal; transitive pulls inherited from a vetted upstream are acceptable. |
| 5 | Cursor format: JSON or binary? | JSON | Human-inspectable, schema-evolution friendly, encoding cost is irrelevant at ~100 bytes. |
| 6 | Cursor file `Meta` size cap? | 64 KiB, return error on oversize | Prevents accidental log-aggregation footguns. Generous enough for any reasonable use. |
| 7 | Default poll interval? | 1 second | Balance of responsiveness and idle CPU. Override per-Tailer. |
| 8 | Default `MaxLine` for LineReader? | 1 MiB | Matches `bufio.MaxScanTokenSize` × 16. Most logs are <1 KiB; pathological cases exist; avoid OOM. |
| 9 | Should L2 expose `Tailer.Position()`? | Yes | Lets non-iterator users introspect without consuming a record. |
| 10 | Should L3 own the flock or L2? | L2 | Flock protects the cursor; cursor is L2's asset. Future L2-only consumers still get protection. |
| 11 | Should `Forwarder.Run` be re-entrant after returning? | No | One-shot. Document. Construct a new Forwarder if you want to restart. |
| 12 | Compressed backup file support (.gz)? | Detect and skip; decompression not implemented. `Lumberjack` and `Logrotate` recognise `.gz` variants and skip them, surfacing the skipped path via `WithLumberjackSkippedHook` / `WithLogrotateSkippedHook` for observability. A checkpoint pointing at an aged-off `.gz` falls through to `OnMissingCheckpoint` policy. | Skip-and-observe is cheap and prevents silent data omission; full decompression adds the bytes-vs-offset semantic axis and remains future work. |
| 13 | Hybrid fsnotify+poll mode? | Not implemented; future work | Single-mode is correct; hybrid is a latency optimization. Design API to allow later addition. |
| 14 | Should we ship a CLI binary (`cmd/gotail`)? | Yes, small one — `tail -F` shape only, L1/L2 path. End-to-end L3 coverage lives in the `forward` package tests (incl. `httptest.Server`-backed integration tests), not in the CLI. ~50–80 LOC. |
| 15 | Default `MissingPolicy` for `OnMissingCheckpoint`? | `FallbackOldest` | Matches driving requirement #4 (lose nothing, accept duplicates). Most graceful default for at-least-once shippers. |
| 16 | License? | See file `LICENSE` | — |
| 17 | Should the Watcher port expose an `io.Reader`? | No. The Watcher/LineReader split (§4 L1) means LineReader owns the only `*os.File`; the Watcher emits state-transition events only. After a `ReOpened` event the LineReader keeps reading the previous fd to `io.EOF` before opening the new path, preserving the rotation-race trailing-bytes drain without crossing an `io.Reader` through the port. | Single-fd, single-owner is simpler and removes the duplicate-fd race window. |
| 18 | Iterator semantics on error? | Yield `(zero, err)` then stop iteration | Matches `iter.Seq2` idiom; caller's `for line, err := range` checks err on each iteration. |
| 19 | What does `Tailer.Close()` do with a pending uncommitted line? | Discard. Cursor is *not* auto-advanced. | Caller's responsibility. Consistent with "library does not guess durability point". |
| 20 | `forwardtest` and `watchtest` — same module or separate? | Same module, separate package | One repo, one go.mod. Test helpers don't pollute the main package's surface. |
| 21 | `Position` type aliases collapse three layers' vocabularies — `forward.Position = tail.Position = watch.Position`. Keep or split per layer? | **Keep aliases.** | Position is the universal coordinate across all three layers; there is no value in re-shaping it per layer. The alias chain is a conscious decision, not an oversight. Documented so future reviewers don't try to "fix" it by introducing redundant types. Trade-off: a future shape change to `watch.Position` is a breaking change at L2 and L3 simultaneously — accepted. |
| 22 | `internal/atomicwrite` reachable only inside the module — fine, or expose? | **Keep `internal/`** | Out-of-tree cursor backends (e.g., a future `cursor/redis` plugin) must re-implement the ~30 LOC tmp+fsync+rename helper. Acceptable cost in exchange for keeping atomicwrite as a private implementation detail we can refactor freely. Revisit if a third-party cursor ecosystem materializes. |
| 23 | Default flush interval for `SyncBackground`? | **1 second** (`DefaultSyncBackgroundInterval`) | Matches the default poll interval (Decision #7); bounds cursor staleness to one second under normal operation. Exposed as a named constant so callers can reference it; overridable per-cursor via `WithSyncBackgroundInterval`. |

---

## 10. Out of scope / future work

Deliberately not implemented. The API is designed so each can be added without a breaking change.

- Hybrid fsnotify+poll watcher (a `WatchMode` enum: `Poll | Fsnotify | Hybrid`; see §5.1).
- Compressed backup file (`.gz`) decompression — `.gz` backups are currently detected and skipped (Decision #12).
- Plugin cursors (Redis, SQL) as separate modules.
- Per-event filtering hooks at L2.
