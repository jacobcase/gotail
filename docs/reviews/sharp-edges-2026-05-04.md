# gotail — Sharp Edges Audit

- **Conducted:** 2026-05-04
- **Re-examined:** 2026-05-04 (added §9–§11 covering Decoder retry-policy doc lie, `WithSyncBackgroundInterval` mode-coupling, and `Interval` validator inconsistency).
- **Skill:** trailofbits/sharp-edges
- **Scope:** Flag/config surface and exported APIs across `cmd/gotail`, `tail`, `watch`, `forward`, `internal/atomicwrite`. v2 tree only; `v1/` excluded.
- **Trust-boundary map:** `docs/reviews/audit-context.md` (Options structs are the caller→library trust boundary; everything caller-supplied is trusted, but the *shape* of that surface decides how easy the wrong call is).
- **Companion review:** `docs/reviews/insecure-defaults-2026-05-04.md` covers fail-open insecure defaults (symlink follow on cursor/lockfile open, `FailOnInodeMismatch=false`, unbounded retry). The findings below are the residual misuse-resistance gaps — places where the "easy path" silently produces the wrong behaviour even though no insecure default is technically in play.

## Scope notes (what does not apply)

The classic sharp-edge categories largely don't fire here:

- **No cryptographic API.** No keys, nonces, signatures, MAC verification, or hash selection.
- **No authentication or session API.** No tokens, no JWT-style alg field, no credential validation. The only "credentials" reference in the tree is a doc comment example showing how a *caller* might wrap a permanent sink error.
- **No SQL, no template rendering, no shell exec.** No string-concatenation injection surface.
- **No environment-variable config loading.** All config is in-process Options structs and `flag` package values.

What remains: configuration cliffs, ambiguous semantics on zero/empty values, doc/code divergence on enum-shaped config, and stringly-typed retry policy. The findings below are ordered roughly by severity.

---

### 1. `BackoffJitter == 0` — documented as "deterministic", silently normalized to 0.2 by `New`

- **File:** `forward/forward.go:69-73` (doc comment), `forward/forward.go:114-119` (validation), `forward/forward.go:298-317` (`jitteredBackoff`)
- **Severity:** Medium. Doc-vs-code divergence; an entire documented operating mode is unreachable.
- **Verdict:** **Confused-developer footgun.** Setting `BackoffJitter: 0` claims one behaviour and silently produces another, with no diagnostic.

**Pattern.**

```go
// BackoffJitter controls the fraction of the ceiling used for jitter.
// Must be in [0, 1]. 0 = deterministic (always ceiling). 1 = full jitter
// (current behaviour, rand in [0, ceiling)). Default is 0.2 (±20% around
// 0.8×ceiling). Negative or >1 is rejected by [New].
BackoffJitter float64
```

```go
// forward/forward.go:114-119
if opts.BackoffJitter < 0 || opts.BackoffJitter > 1 {
    return nil, fmt.Errorf("forward: BackoffJitter must be in [0, 1], got %g", opts.BackoffJitter)
}
if opts.BackoffJitter == 0 {
    opts.BackoffJitter = 0.2
}
```

The `jitteredBackoff` math is correct for `jitter=0`: `base = ceiling * (1 - 0) = ceiling`, `jitterRange = 0`, returns `base = ceiling`. The deterministic mode that the doc promises *exists in the implementation* — it is just unreachable through the constructor, because `New` overwrites the value before it ever gets there. There is no other in-range value that produces deterministic behaviour: `BackoffJitter = 1` is full-jitter `[0, ceiling)`; `BackoffJitter = ε` (e.g. `1e-9`) is *almost* deterministic but introduces a vanishing random term.

The codebase itself has noticed this. `forward/forward_test.go:823-834` opens the deterministic-mode test with a comment explaining the test author's workaround:

```go
// Jitter=0 is deterministic: always base=ceiling*(1-0)=ceiling.
// But 0 is the zero value which triggers default 0.2 — use 1e-9 instead.
// ...
// We cannot set 0 because it triggers the default.
```

The test then settles for `BackoffJitter: 1.0` (full jitter) and skips the deterministic case entirely.

**Misuse scenario.** A user who needs synchronous retry timing — for an integration test asserting backoff cadence, a load-test harness measuring tail-of-distribution latency, a coordinated rolling redeploy with N forwarders pointed at the same sink — reads the doc and writes:

```go
fwd, _ := forward.New(forward.Options[[]byte]{
    InitialBackoff: 5 * time.Second,
    MaxBackoff:     5 * time.Second,
    BackoffJitter:  0, // "deterministic, always ceiling" per doc
    ...
})
```

`New` succeeds. `Run` retries with `jitter = 0.2`, sleeping `[4s, 5s)`. The user's test case asserting `assert.Equal(t, 5*time.Second, observedSleep)` fails intermittently. The user has no way to reach the documented behaviour without either patching the library or sending an out-of-spec value (`1.0/3` is not deterministic; `1e-15` rounds to zero in IEEE-754 double mantissa games — fragile).

The compounding factor is operational: deterministic backoff is the right choice when many forwarders share an upstream sink, because the documented `0.2` jitter (range `[0.8·ceiling, ceiling)`) leaves the retry storm *highly* time-correlated — every instance retries within the same 200ms window. Insecure-defaults #4 already calls out the unbounded-retry stall mode; the inability to flatten the retry distribution amplifies it.

**Recommended fix.** Pick one of:

1. Match the doc — drop the `0 → 0.2` normalization. Document `BackoffJitter` zero-value as "deterministic; use the explicit literal `0.2` to opt into the historical default." This is the more honest fix; it surfaces the choice instead of silently defaulting it.
2. Match the code — change the doc to state "`0` selects the package default (`0.2`); to disable jitter, use a `*float64` pointer (added field) or a sentinel like `BackoffJitterNone`." This costs an API change but eliminates the trap.
3. Use a `*float64` (or a wrapper type) for the field so "unset" and "zero" are distinguishable; `*float64(nil)` defaults, `*float64(&zero)` is deterministic, `*float64(&one)` is full jitter.

Option 1 is least disruptive. The current behaviour also makes the `ErrPermanent` retry-policy surface (finding #6 below) harder to test — these two findings compound.

---

### 2. `WithFlock(cursorPath)` — using the cursor file itself as the lockfile silently breaks the lock on first Save

- **File:** `tail/cursor.go:113-119` (`WithFlock` doc), `tail/flock_unix.go:11-44` (`acquireFlock` — opens the path with `O_CREATE|O_RDWR`), `internal/atomicwrite/atomicwrite.go:42` (rename-over-path)
- **Severity:** High. The API silently fails to provide the protection it promises.
- **Verdict:** **Configuration cliff.** The "easy path" — pointing `WithFlock` at the cursor itself — passes lock acquisition and then collapses on the next Save with no diagnostic.

**Pattern.**

```go
// WithFlock acquires an exclusive advisory lock on lockPath before returning
// from [NewFileCursor], and releases it in [Cursor.Close]. An empty lockPath
// is a no-op. The lock file must be a sibling of the cursor file — never use
// the cursor file itself, because rename-over-open silently drops POSIX locks.
func WithFlock(lockPath string) FileCursorOption {
    return func(o *fileCursorOpts) { o.flockPath = lockPath }
}
```

The doc comment names the failure mode but there is no programmatic check anywhere in `NewFileCursor`/`acquireFlock`/`Save` that compares `lockPath` to `path`. A caller who writes:

```go
c, err := tail.NewFileCursor(
    "/var/lib/myapp/cursor.json",
    tail.WithFlock("/var/lib/myapp/cursor.json"), // ← same path as the cursor
)
```

gets a successful `acquireFlock` (the lock is held on the inode currently at `cursor.json`). Then the first `Cursor.Save` runs `atomicwrite.Write`:

1. Open `cursor.json.tmp`, write data, fsync, close.
2. `os.Rename("cursor.json.tmp", "cursor.json")` — the kernel atomically replaces the directory entry. The inode the flock holder has open is **orphaned** (no more directory entries point at it; it's kept alive only by the flock holder's fd). The new inode at `cursor.json` is the renamed-from `tmp` inode, **with no flock on it**.

A second process now runs `tail.NewFileCursor("/var/lib/myapp/cursor.json", tail.WithFlock("/var/lib/myapp/cursor.json"))`. `os.OpenFile` opens the new (rename-introduced) inode; `syscall.Flock(LOCK_EX|LOCK_NB)` succeeds because no flock is held on that new inode. The original holder's flock is still valid on the orphan inode — but no one will ever try to lock that inode again. The exclusive-lock guarantee `WithFlock` is documented to provide is silently void from the second `Save` onward.

**Misuse scenario.** A team adds `WithFlock` because they want to defend against two systemd unit instances accidentally tailing the same log:

```go
cursor, err := tail.NewFileCursor(
    cfg.CursorPath,
    tail.WithFlock(cfg.CursorPath), // matches what the README would suggest if not read carefully
)
```

In smoke tests this works perfectly: a second process gets `ErrLockHeld` immediately. They ship. In production, both instances run forever (each acquires the lock against a different inode after the first commit). Two writers race on `Source.Commit` → `Cursor.Save` → `atomicwrite.Write`; both renames succeed; whichever lands second clobbers the first's checkpoint, and on restart the cursor reflects whichever process committed last — losing ordering and (depending on positions) re-replaying or skipping data through the at-least-once guarantee. The team has no signal: no error log, no `OnError` fire, no `OnInodeMismatch`. The only symptom is downstream duplicate or missing records, attributed to upstream Sink retry rather than the lock having silently failed.

**Recommended fix.** In `NewFileCursor`, when `flockPath != "" && filepath.Clean(flockPath) == filepath.Clean(path)`, return an error like `tail.ErrLockfileEqualsCursor` (or wrap an existing sentinel). This is a one-line check and costs nothing — the constraint is already documented; programmatic enforcement just makes the doc self-protecting. A more aggressive fix is to *generate* the lockfile path from the cursor path inside `NewFileCursor` (`path + ".lock"`) and remove `WithFlock` as a knob, leaving only a `WithFlockEnabled(bool)` boolean — which eliminates the wrong-path footgun entirely. The caller-supplied path option exists today only to support test-isolated lock paths and shared-lock-across-different-cursors patterns, both of which can be served by an explicit `WithLockPath(path string)` that errors on equality with the cursor path.

---

### 3. `Whence == io.SeekCurrent` accepted as valid but silently treated as `SeekStart` — exposes existing log content the caller meant to skip

- **File:** `tail/tail.go:60-66` (Options.Whence doc), `watch/watch.go:55-57` (Config.Whence doc), `watch/poll.go:51-53` and `watch/fsnotify_unix.go:49-51` (validation), `watch/poll.go:185-187` and `watch/fsnotify_unix.go:182-184` (the SeekEnd-only special case)
- **Severity:** Medium. Data exposure: the file is read from offset 0 instead of the position the caller probably intended.
- **Verdict:** **Algorithm-selection footgun in disguise.** The API enumerates three values and validates them, but only two are semantically distinct. The third is accepted-but-no-op.

**Pattern.**

```go
// tail/tail.go:60-66
// Whence controls the initial seek position for the first file opened.
// Must be [io.SeekStart], [io.SeekCurrent], or [io.SeekEnd].
// Zero (io.SeekStart) reads from the beginning; [io.SeekEnd] skips
// existing content and tails only new data. Ignored when a Cursor
// provides a resume point.
Whence int
```

The doc lists three values but defines semantics for only two. Inside `pollWatcher.openFirst` (and the fsnotify mirror):

```go
} else if p.whence == io.SeekEnd {
    offset = size
}
// otherwise offset stays 0
```

Validation accepts `io.SeekCurrent` (= 1) explicitly:

```go
if c.Whence != io.SeekStart && c.Whence != io.SeekCurrent && c.Whence != io.SeekEnd {
    return nil, fmt.Errorf("watch: Config.Whence %v is invalid", c.Whence)
}
```

So `SeekCurrent` is accepted and silently behaves as `SeekStart` (`offset = 0`). On a freshly opened file, this is technically consistent with the io-package definition of `SeekCurrent` (a `Read`-then-`Seek(0, SeekCurrent)` returns the post-Read offset, which is 0 on a freshly opened fd) — but the API context is "initial seek position before any read," where `SeekCurrent` has no semantic meaning. The validation suggests it does.

**Misuse scenario.** A developer adopting gotail for the first time sees three constants in the doc and reaches for `io.SeekCurrent` thinking it means "start from where I am now" — i.e., resume-aware semantics:

```go
opts := tail.Options{
    Source: tail.SingleFile("/var/log/audit.log"),
    Cursor: tail.NewFileCursor("..."),  // resume from cursor
    Whence: io.SeekCurrent,             // "if no cursor, stay where I am"
}
```

The intent: "if the cursor is missing, don't replay six months of audit logs to the SIEM." The behaviour: with no cursor, `Whence = SeekCurrent` is treated as `SeekStart`, gotail opens the audit log at offset 0, and the entire historical content gets shipped. For a SIEM with cost-per-event billing this is a real budget event; for a downstream system that processes events transactionally (alerts, metrics, billing), it is a correctness event.

The same trap fires for any caller doing `case "tail-only": opts.Whence = io.SeekEnd; case "from-here": opts.Whence = io.SeekCurrent` — both branches "validate fine," but `from-here` silently means `from-start`.

**Recommended fix.** Either:

1. Reject `io.SeekCurrent` at `New`/`NewPolling`/`NewFsnotify` time with an error like `tail: SeekCurrent has no meaning on initial open; use SeekStart or SeekEnd`. This breaks no real caller, since today `SeekCurrent` is silently equivalent to `SeekStart`; loud rejection is strictly better than silent re-mapping.
2. Stop using `io.SeekStart`/`io.SeekEnd` for this enum and introduce an unambiguous `tail.WhencePolicy` type with `WhenceStart` / `WhenceEnd` only. Compare to `SkipExisting bool`, which the codebase already added as a "discoverable convenience for `io.SeekEnd`" — that convenience field exists *because* the integer-whence API is hard to read, and it is hard to read partly because the third accepted value has no effect.

Option 1 is the smaller change.

---

### 4. Batch-bound validation rejects only the all-zero case; any negative value passes and silently disables the bound

- **File:** `forward/forward.go:62-64` (Options doc), `forward/forward.go:111-113` (validation), `forward/forward.go:186` and `forward/forward.go:238-239` (consumption — all use `> 0` checks)
- **Severity:** Medium. A configuration that reads as "I am setting a cap" silently produces "no cap." Compounds with the unbounded-retry surface (insecure-defaults #4) into "Forwarder.Run never returns and OOMs."
- **Verdict:** **Configuration cliff with under-strict validation.** The invariant the doc and the constructor advertise — "at least one batch trigger must be set" — is only enforced for the literal `0` case.

**Pattern.**

```go
// Doc on Options[T]:
// Batching — at least one must be set; flush triggers when ANY bound fires.
MaxBatchRecords int           // flush when batch reaches this record count (0 = no limit)
MaxBatchBytes   int           // flush when batch reaches this byte size (0 = no limit)
MaxBatchAge     time.Duration // flush when oldest record in batch is this old (0 = no limit)
```

```go
// forward/forward.go:111-113
if opts.MaxBatchRecords == 0 && opts.MaxBatchBytes == 0 && opts.MaxBatchAge == 0 {
    return nil, errors.New("forward: at least one of MaxBatchRecords, MaxBatchBytes, MaxBatchAge must be set")
}
```

```go
// Consumption — all `> 0` not `!= 0`:
if len(batch) > 0 && f.opts.MaxBatchAge > 0 { ... }            // line 186
shouldFlush := (f.opts.MaxBatchRecords > 0 && ...) ||
               (f.opts.MaxBatchBytes > 0 && ...)              // lines 238-239
```

Result: `MaxBatchRecords: -1, MaxBatchBytes: -1, MaxBatchAge: -1` passes `New` (none of the fields equals zero) but every consumption check is `> 0`-gated, so no bound ever triggers a flush. The batch grows unboundedly until the parent ctx is cancelled or the source exhausts. Same effect for any subset combination of zero-and-negative values: the validation guard misses them.

**Misuse scenario.** A team has a config struct that maps YAML to `forward.Options`, and someone writes:

```yaml
forward:
  max_batch_records: -1   # "disable record cap; we only care about bytes"
  max_batch_bytes: 10485760
  max_batch_age: 5s
```

That config is fine. But if the YAML mapper has a bug, or someone copy-pastes from a different system where `-1` is the convention for "infinite," and lands on:

```yaml
forward:
  max_batch_records: -1
  max_batch_bytes: -1
  max_batch_age: -1
```

`forward.New` returns a Forwarder with no bounds. `Run` ingests records; `flush` is never called from any of the three triggers; the only way `flush` runs is if `Source.Next` returns `tail.ErrSourceExhausted`, which a live tailer never does. Memory grows linearly until OOM. The Forwarder logs nothing; its only signal is `Tailer.Stats()` showing record counts, which the caller has to be specifically watching.

**Recommended fix.** Tighten the validation:

```go
if opts.MaxBatchRecords <= 0 && opts.MaxBatchBytes <= 0 && opts.MaxBatchAge <= 0 {
    return nil, errors.New("forward: at least one of MaxBatchRecords, MaxBatchBytes, MaxBatchAge must be a positive value")
}
```

Bonus: reject any *individual* negative field as a programmer error — a negative cap is never meaningful; allowing it as a synonym for "no cap" hides typos. Each of `if opts.MaxBatchRecords < 0 { return nil, errors.New("forward: MaxBatchRecords must be ≥ 0") }` is two lines.

---

### 5. `SyncOnCommit` silently buffers checkpoints in memory — caller must remember to call `Sync(ctx)` or durability is lost

- **File:** `tail/cursor.go:32-35` (mode doc), `tail/cursor.go:62-68` (`Syncer` extension interface), `tail/cursor.go:271-289` (`Save` branching on mode), `tail/cursor.go:304-317` (`Sync` implementation)
- **Severity:** Medium. The cursor *appears* to be persisting — `Save` returns nil, `OnCheckpoint` fires — but on a crash the on-disk file is whatever was last `Sync`-flushed.
- **Verdict:** **Stringly-typed responsibility transfer with no enforcement and no diagnostic.** The Tailer's hot path calls `Save`; the embedder is silently expected to also call `Sync` from somewhere; nothing checks that they did.

**Pattern.**

```go
// SyncOnCommit buffers the latest checkpoint in memory; an explicit call to
// [Syncer.Sync] (type-asserted from the [Cursor] interface) flushes it.
// The Tailer's Commit calls only Save; the caller controls when Sync runs.
SyncOnCommit
```

```go
// Save body, tail/cursor.go:278-289
switch c.opts.syncMode {
case SyncOnCommit, SyncBackground:
    // Buffer; don't write to disk yet.
    c.mu.Lock()
    c.pending = cp
    c.dirty = true
    c.mu.Unlock()
    return nil
default: // SyncAlways
    return c.flush(cp)
}
```

`Cursor.Save` returns nil from a memory-only mutation. `OnCheckpoint` fires (`tail/tail.go:474-476` and `tail/tail.go:500-502`). The Tailer's `Stats.Position` advances. From the embedder's vantage point everything is committing. But on the disk, the file has not been touched since the last `Sync`. After a kernel panic, power loss, or `kill -9`, recovery loads the last-flushed checkpoint and replays everything written since.

The `Syncer` interface lives behind a type-assert:

```go
// audit-context §2.2:
//   Syncer interface { Sync(ctx) error } — implemented by FileCursor only when
//   configured with SyncOnCommit or SyncBackground. Type-assert from Cursor to
//   drive a manual flush.
```

A caller who configures `SyncOnCommit` but never reads the doc carefully enough to find the Syncer assertion has a fully working tail/forward pipeline with completely empty crash-window durability.

**Misuse scenario.** A team chooses `SyncOnCommit` for performance: their downstream sink batches at 1k records/s, and `SyncAlways` was hammering the SSD. They add the option:

```go
cursor, _ := tail.NewFileCursor(path,
    tail.WithSyncMode(tail.SyncOnCommit),
)
```

Their code calls `tailer.Commit(ctx, pos)` after each successful sink batch. Tests pass — the in-memory `MemoryCursor` gives the same observable behaviour. CI passes — short-lived runs with graceful `Tailer.Close()` (which does not flush; only `CloseWithFlush` does, and then only if a non-zero position has been yielded — see `tail/tail.go:546-580`) terminates fine. In prod, the host is rebooted for kernel patching at 03:00. After reboot, gotail reads the cursor file, finds the position from the last `Sync` — which never happened, so the file is whatever `NewFileCursor` first wrote, or doesn't exist. Replay starts at the source's oldest enumerated file. The downstream sink processes 6 hours of records as duplicates; the at-least-once guarantee technically holds, but the consumer's deduplication budget didn't anticipate this.

The compounding factor: the API name is `SyncOnCommit`. A reasonable reader parses this as "Sync runs at commit time" — the *opposite* of the actual contract ("Save is buffered; Sync is what you call separately"). The audit context (§7.6 #31) calls this out: "`OnCheckpoint` is *not* an fsync barrier."

**Recommended fix.** Two complementary changes:

1. Rename the mode for clarity. `SyncManual` (or `SyncBuffered`) describes the actual contract — "writes are buffered until you call Sync" — better than `SyncOnCommit`, which implies the opposite. Migration: keep `SyncOnCommit` as a deprecated alias for one release.
2. Add a default safety net: if `SyncMode == SyncOnCommit` and `Sync` has not been called within `2 * DefaultSyncBackgroundInterval` (say) since the last `Save`, log a `WARN` once. The library cannot enforce sync but it can make the missing-flush state visible without requiring a `*FileCursor` debug plumbing.

A more ambitious fix is to remove `SyncOnCommit` entirely — the use case it serves (batched fsync) is already served by `SyncBackground`, which has a default interval and does not require caller action.

---

### 6. Sink errors retry forever unless explicitly wrapped with `ErrPermanent` — wrong default polarity for caller-supplied errors

- **File:** `forward/errors.go:5-12` (the sole sentinel), `forward/forward.go:38-44` (Sink contract doc), `forward/forward.go:251-291` (retry loop)
- **Severity:** Medium. The default for any unknown error is "retry forever," which is the wrong choice for any 4xx-class HTTP failure or any auth/credential failure. Sink authors must remember the `ErrPermanent` sentinel and wrap correctly; the easy path is unsafe.
- **Verdict:** **Stringly-typed retry policy with default-permissive polarity.** Compounds with insecure-defaults #4 (no retry cap) into "any Sink that doesn't wrap errors correctly stalls Run forever on the first 401."

**Pattern.**

```go
// Sink accepts a decoded batch and delivers it to an external system.
// Return contracts:
//   - nil → commit the batch
//   - errors.Is(err, [ErrPermanent]) → non-retryable; Run returns the error
//   - any other error → retryable; Forwarder backs off and retries the same batch
type Sink[T any] interface {
    Send(ctx context.Context, batch []T) error
}
```

The contract is: the secure choice (don't retry indefinitely on auth failures, malformed credentials, schema rejections) requires the Sink author to remember `forward.ErrPermanent` and *wrap* — `fmt.Errorf("invalid creds: %w", forward.ErrPermanent)`. The unsafe choice (retry forever) is the behaviour for any error not so wrapped.

The `forward.ErrPermanent` example in `forward/errors.go:11` shows the pattern correctly. But:

- Many Sink authors copy-paste from existing Sink examples (e.g., the README cookbook). If those examples use `return err` without inspecting the error class, the unsafe default propagates.
- Common Go HTTP clients return errors that are *not* wrapped with anything meaningful — e.g. `if resp.StatusCode != 200 { return fmt.Errorf("status %d", resp.StatusCode) }`. A 401, 403, 404, 410, or 422 return value is a permanent condition; the bare `errors.New` formatted error is treated as transient.
- The `Decoder` surface has the *opposite* polarity: a decoder that returns a non-nil error skips that record and continues (`forward/forward.go:223-229`). So the Sink-default and Decoder-default are inverted: Sink errors retry forever, Decoder errors are silently skipped. Hard to remember which is which.

**Misuse scenario.** A team writes the canonical "POST batch as JSON to an HTTP endpoint" sink:

```go
sink := forward.SinkFunc[map[string]any](func(ctx context.Context, batch []map[string]any) error {
    body, _ := json.Marshal(batch)
    resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
    if err != nil {
        return err // network — transient, retry is correct
    }
    defer resp.Body.Close()
    if resp.StatusCode >= 200 && resp.StatusCode < 300 {
        return nil
    }
    return fmt.Errorf("upstream returned %d", resp.StatusCode) // ← unwrapped
})
```

In dev: works. In prod: the API key rotates, requests start returning 401. `sendWithRetry` wraps the 401, fires `OnSendError(err, attempt, true)`, sleeps `~MaxBackoff`, retries. Forever. With `BackoffJitter` defaulting to `0.2` (finding #1 makes this unavoidable), every forwarder retries within the same 200ms window every 30s, hammering the auth endpoint. The Tailer keeps reading and feeding the in-memory batch (via `Source.Next`), but the batch holds steady because flush never returns nil — actually wait, the batch is reset inside `flush` only after `sendWithRetry` returns nil, so the same batch retries. Memory is bounded but throughput is zero. Insecure-defaults #4 documents the no-retry-cap stall; this finding documents *why* the stall happens by default.

**Recommended fix.** Three options, in increasing impact:

1. Document at the type level. Add a paragraph at the top of `Options[T]` documenting that the *default* retry policy treats unknown errors as transient, and listing the canonical "wrap with `ErrPermanent`" pattern for status codes ≥ 400. Ship a `WithErrPermanentForHTTPStatus(codes ...int)` Sink-middleware that wraps. This is documentation, not API, change.
2. Add a contrarian sentinel — `ErrTransient` — and require Sink authors to declare *one* of `ErrPermanent` or `ErrTransient`. Any unwrapped error becomes a build-time assertion (impossible in Go without lint) or a runtime warning. This adds friction; it is the strict-typing fix.
3. Flip the default polarity: any error not wrapped with `ErrTransient` (or a `Retryable` interface) is treated as permanent. Stringly-typed becomes a positive opt-in. This is the "secure-by-default" fix; it breaks every existing caller.

Option 1 is the smallest improvement. Option 3 is the structurally right fix but is a v3 conversation.

---

### 7. `tail.Options.Cursor = nil` silently disables checkpointing — no warning, no error

- **File:** `tail/tail.go:48-51` (Options.Cursor field), `tail/tail.go:204` (`if opts.Cursor != nil` gate around Load), `tail/tail.go:462-464` (Commit no-op when Cursor is nil), `tail/tail.go:483-485` (CommitWithMeta no-op), `tail/tail.go:553-554` (CloseWithFlush no-op)
- **Severity:** Low. Easy to spot in code review, but there is no programmatic signal at runtime.
- **Verdict:** **Easy-path-is-wrong.** A reasonable embedder writes `tail.Options{Source: …}` (omitting Cursor) when they want a quick prototype, then ships a config that forgot to wire it back in.

**Pattern.**

```go
// Options.Cursor doc:
// Cursor persists checkpoints across restarts. Nil means no persistence.
Cursor Cursor
```

Every checkpointing call site silently no-ops on `Cursor == nil`:

```go
// Commit
func (t *Tailer) Commit(ctx context.Context, pos Position) error {
    if t.opts.Cursor == nil {
        return nil
    }
    ...
}
```

`tail.New` does not warn or error. There is no `RequireCursor bool` Options field. The Tailer behaves identically to one with a working cursor: `Stats` advances, `OnCheckpoint` never fires (because nothing was saved), `Position()` returns the in-memory position. After a process restart, replay starts at `Whence` semantics — `SeekEnd` if the old code-path `SkipExisting` was set, `SeekStart` otherwise — losing or replaying everything the cursor would have saved.

**Misuse scenario.** A YAML-driven config:

```yaml
gotail:
  source: { type: lumberjack, active: /var/log/app.log }
  # cursor: { type: file, path: /var/lib/gotail/cursor.json }   ← commented during dev
  whence: end
```

The dev who debugged a cursor permission issue locally commented the cursor block out of their environment-specific YAML and forgot to re-enable it before the PR landed. Or: the YAML mapper unmarshals an unknown `type:` value as the zero `Cursor`-shaped struct (`nil`-equivalent). Either way `tail.New` constructs a Tailer with `Cursor == nil`. Logs from that host are tailed and shipped, but a process restart for any reason (k8s evict, SIGTERM during deploy, OOM) loses the position and resumes from `Whence: end` — a window's worth of data is silently dropped before the next batch is sent.

The same trap fires for "I'll add the cursor later" prototypes that never get the cursor added.

**Recommended fix.** Add a `Options.RequireCursor bool` field; when true, `tail.New` errors on nil Cursor with a clear message. Default false (for backward compatibility). Document the new field in the godoc and in the README's getting-started example. Cost: ~5 lines.

A weaker fix is to add an `slog.Warn("tail: no cursor configured; checkpointing disabled")` once during `New`. That signals at runtime without a config change but pollutes the log of legitimate cursor-less callers.

---

### 8. `WithFileMode` accepts any `os.FileMode` including world-writable, setuid, and `0o000`

- **File:** `tail/cursor.go:108-111` (the option), `tail/cursor.go:184-187` (defaults set in NewFileCursor), `internal/atomicwrite/atomicwrite.go:23` (mode passed to `os.OpenFile`)
- **Severity:** Low. The default `0o600` is correct; this finding is about defensive validation, not a default-behaviour break.
- **Verdict:** **Unvalidated constructor parameter** (per the references' "config-patterns.md / unvalidated constructor parameters" pattern).

**Pattern.**

```go
// WithFileMode sets the permission bits for the cursor file. Default 0o600.
func WithFileMode(mode os.FileMode) FileCursorOption {
    return func(o *fileCursorOpts) { o.fileMode = mode }
}
```

`os.FileMode` is a `uint32` typedef; any value passes — `0o777`, `0o000`, `os.ModeSetuid|0o755`, `0o4777` (setuid + world-writable). The mode flows verbatim to `os.OpenFile` inside `atomicwrite.Write`. The `os.Rename` step preserves the tmp file's mode bits (POSIX rename does not touch perms), so the cursor's eventual mode is whatever the user passed — including setuid or world-writable. The umask intersects, but a process running with `umask=0` (common in containers and systemd units with `UMask=0000`) gets the literal value.

**Misuse scenario.** A caller debugging "cursor file has wrong perms":

```go
cursor, _ := tail.NewFileCursor(path, tail.WithFileMode(0o777))
```

…intending to fix a "permission denied" error that was actually about the *parent directory*. Now the cursor file is world-writable. Any local user can rewrite the cursor (with `Position.File = "/etc/shadow"`, `Inode = whatever`, `Offset = 0`, `Meta = "<their JSON of choice>"`). The audit context (§4 #3) already calls out that the cursor JSON is a trust boundary; this option lets the caller widen that trust boundary on a typo.

A more exotic case: `WithFileMode(os.ModeSetuid | 0o755)` would create a setuid cursor file. The cursor file is not executable, so this is benign on most filesystems, but the mode bits propagate and a defender investigating the filesystem state sees a setuid bit they cannot explain.

**Recommended fix.** Reject mode bits that fall outside the standard rwx perms and the sticky bit. Specifically reject any bit in `os.ModeType | os.ModeSetuid | os.ModeSetgid` and require `mode & 0o077 == 0` (no group/other write or read by default). The cost is 4 lines:

```go
func WithFileMode(mode os.FileMode) FileCursorOption {
    return func(o *fileCursorOpts) {
        if mode&^0o777 != 0 || mode&0o022 != 0 {
            // Validation occurs in NewFileCursor; record the bad value in opts
            // and surface a constructor error.
            ...
        }
        o.fileMode = mode
    }
}
```

A simpler approach: drop `WithFileMode` entirely and make the cursor mode hardcoded `0o600` (matching the lockfile, which already is hardcoded — `tail/flock_unix.go:16`). The use case for a non-`0o600` cursor mode is thin; unifying the mode removes a sharp edge for free.

---

### 9. `Decoder` doc promises `ErrPermanent` aborts the Forwarder; the code never inspects the wrapped error and always skips

- **File:** `forward/decoders.go:5-8` (`Decoder` doc), `forward/forward.go:222-229` (the consumption path), `forward/forward.go:37-44` (`Sink` doc, by contrast)
- **Severity:** Medium. Whichever direction the caller bets on, one is wrong: the `Sink` contract says `ErrPermanent` aborts; the `Decoder` contract *also* says `ErrPermanent` aborts. Only the Sink contract is implemented. A team relying on the documented decoder behaviour to fail fast on schema mismatch silently keeps running.
- **Verdict:** **Confused-developer footgun (doc-vs-code divergence on the polarity-twin of finding #6).**

**Pattern.**

```go
// forward/decoders.go:5-8
// Decoder converts a raw line from a [RecordSource] into a value of type T.
// A non-nil error causes the line to be skipped; wrap with [ErrPermanent] to
// abort the Forwarder.
type Decoder[T any] func(line []byte) (T, error)
```

```go
// forward/forward.go:222-229 — actual consumption
val, derr := f.opts.Decoder(rec.Line)
if derr != nil {
    if f.opts.OnDecodeError != nil {
        f.opts.OnDecodeError(rec.Line, rec.Pos, derr)
    }
    batchLastPos = rec.Pos
    continue
}
```

There is no `errors.Is(derr, ErrPermanent)` branch on the decode path. Every decode error — including ones the caller carefully wrapped with `ErrPermanent` — fires `OnDecodeError`, advances `batchLastPos`, and continues. The "`wrap with ErrPermanent to abort the Forwarder`" promise is unreachable. Audit-context §6 #24 (`"Decode errors are skipped, not retried"`) reflects the actual code; the doc reflects an aspiration that was never wired.

The polarity-twin issue is what makes this dangerous: the *Sink* contract (finding #6 above) says `ErrPermanent` aborts and any other error retries forever. A reasonable reader assumes the Decoder contract is symmetric — and the doc explicitly says it is. So a caller writes:

```go
opts.Decoder = func(line []byte) (Event, error) {
    var ev Event
    if err := json.Unmarshal(line, &ev); err != nil {
        return ev, fmt.Errorf("malformed: %w", forward.ErrPermanent)
    }
    if ev.Schema != currentSchema {
        return ev, fmt.Errorf("schema %d unsupported: %w", ev.Schema, forward.ErrPermanent)
    }
    return ev, nil
}
```

…intending: "if the decode fails in a way that means the upstream is producing garbage, halt the forwarder so an operator can investigate." Actual behaviour: every malformed or schema-mismatched record is silently skipped. The position still advances (audit-context §6 #24, §7.5 #26: at-least-once becomes at-most-zero for that record stream). The downstream consumer sees a gap; the operator sees no error.

**Misuse scenario.** A team rolls a new event-schema upgrade. The producer rolls first; gotail's decoder is configured to treat unknown schemas as permanent ("we do not want to silently misinterpret records"). After the producer rolls, gotail's `Decoder` returns a wrapped `ErrPermanent` for every line. The team expects `Run` to return that error and trigger their alerting. Actual behaviour: every line skipped, `OnDecodeError` fires (silenced if not wired), `Run` runs forever happily. By the time someone notices the data gap, hours of records have been silently abandoned and the cursor has advanced past them — replay won't recover them.

**Recommended fix.** Pick one:

1. Match the doc — add `if errors.Is(derr, ErrPermanent) { return derr }` (or wrap and return) on the decode path. One line plus a test. This honours the documented contract.
2. Match the code — remove "wrap with `ErrPermanent` to abort the Forwarder" from the `Decoder` doc, and explicitly state "decoder errors are *always* skipped; use a `Decoder`-side counter or the `OnDecodeError` hook to surface persistent failures." Matches today's behaviour but discards a useful failure-mode primitive.

Option 1 is strictly better — it converts the Decoder contract into a polarity-symmetric twin of the Sink contract (finding #6) instead of an asymmetric one. It also makes the cited test in `forward/forward_test.go` (currently exercising `OnDecodeError` only) extensible to permanent-failure assertions.

---

### 10. `WithSyncBackgroundInterval` is silently ignored unless `WithSyncMode(SyncBackground)` is also set

- **File:** `tail/cursor.go:138-143` (option doc + setter), `tail/cursor.go:199-207` (consumption inside `NewFileCursor`)
- **Severity:** Medium. Silent fall-back to `SyncAlways` defeats the perceived performance optimization and re-introduces fsync-per-Save for callers who thought they were buying a periodic background flush.
- **Verdict:** **Mode-coupled option (configuration cliff variant of finding #5).** A single `WithX` call is silently no-op'd unless paired with a *separate* `WithY` call. The dependency is documented but not enforced.

**Pattern.**

```go
// tail/cursor.go:138-143
// WithSyncBackgroundInterval overrides the flush interval used by
// [SyncBackground]. Zero or negative values use [DefaultSyncBackgroundInterval].
// Ignored when the sync mode is not [SyncBackground].
func WithSyncBackgroundInterval(d time.Duration) FileCursorOption {
    return func(o *fileCursorOpts) { o.syncInterval = d }
}
```

```go
// tail/cursor.go:199-207 — only consumed inside the SyncBackground branch
if o.syncMode == SyncBackground {
    interval := o.syncInterval
    if interval <= 0 {
        interval = DefaultSyncBackgroundInterval
    }
    c.stopBg = make(chan struct{})
    c.bgDone = make(chan struct{})
    go c.backgroundFlusher(interval)
}
```

The setter accepts the value; nothing checks at construction whether `syncMode == SyncBackground`. The default mode is `SyncAlways` (audit-context §5.4), so a caller who lifts the option from a snippet without lifting the matching `WithSyncMode(SyncBackground)` gets the default fsync-per-Save behaviour with no diagnostic.

**Misuse scenario.** A team is debugging "cursor SSD wear is too high":

```go
cursor, _ := tail.NewFileCursor(
    "/var/lib/myapp/cursor.json",
    tail.WithSyncBackgroundInterval(10 * time.Second), // "flush at most every 10s"
)
```

The option name reads as a self-contained throttle. The expectation: every commit accumulates in memory; once per 10s a goroutine writes to disk. Actual behaviour: `NewFileCursor` constructs `SyncAlways`; every `Cursor.Save` does a synchronous write+fsync+rename+dir-fsync (audit-context §5.4 step 3). The 10s interval is dead config. The team's wear-rate metric does not move. They tune harder, or add a YAML knob exposing `sync_background_interval: 1m`, but never realize they're missing the *mode* switch.

The compounding factor with finding #5: even if the team does discover `WithSyncMode`, they may pick `SyncOnCommit` (the more obviously-named one) — which finding #5 documents as silently buffering until manual `Sync`. Two adjacent footguns.

**Recommended fix.** In `NewFileCursor`, if `o.syncInterval != 0 && o.syncMode != SyncBackground`, return an error like `tail.ErrSyncIntervalRequiresBackground`:

```go
if o.syncInterval != 0 && o.syncMode != SyncBackground {
    return nil, fmt.Errorf("tail: WithSyncBackgroundInterval requires WithSyncMode(SyncBackground)")
}
```

This is a four-line check that converts a silent no-op into a loud constructor error. It also documents the dependency through the type system (the error message makes the relationship explicit) rather than only through the comment. Variant: collapse `WithSyncBackgroundInterval` into a single combined `WithSyncBackground(d time.Duration)` option that sets *both* mode and interval atomically and removes the trap entirely.

---

### 11. `tail.Options.Interval` validator silently coerces negatives to default; the underlying `watch.Config.Interval` rejects them

- **File:** `tail/tail.go:184-186` (Tailer-level validator), `watch/poll.go:45-47` (Watcher-level rejector), `watch/fsnotify_unix.go:55-57` (mirror — same `< 0` rejection)
- **Severity:** Low. Two layers of the same field disagree on the "what does negative mean" question. The looser layer always runs first, masking the stricter one.
- **Verdict:** **Inconsistent validation across layers.** A typo or config-mapping bug at the Tailer surface becomes silent default-restoration; the watcher's defensive check is dead code on this code path.

**Pattern.**

```go
// tail/tail.go:184-186 — Tailer surface, silent coercion
if opts.Interval <= 0 {
    opts.Interval = time.Second
}
```

```go
// watch/poll.go:45-50 — Watcher surface, strict rejection then zero-default
if c.Interval < 0 {
    return nil, fmt.Errorf("watch: Config.Interval must not be negative, got %v", c.Interval)
}
if c.Interval == 0 {
    c.Interval = time.Second
}
```

The Tailer always wins because `tail.New` constructs the watcher Config from `opts.Interval` (already coerced) before calling `watch.NewPolling`/`watch.NewFsnotify`. A negative `Interval` at the Tailer surface — say, from a YAML mapper that defaults a missing field to `-1`, or a test fixture that passes `time.Duration(-1)` to mean "unset" — becomes a silent 1-second default with no `OnError` fire and no log line. The watcher's strict guard, which exists specifically to catch this, never triggers.

The doc on `tail.Options.Interval` (`tail/tail.go:115-119`) says:

```go
// Interval controls how often the polling watcher checks the file. Ignored
// when an fsnotify-capable platform is available. Zero defaults to
// 1 second.
```

It says nothing about negative values. A reader who looks at the watcher's Config doc (`watch/watch.go:33-37`) sees the stricter rule but cannot reach it from the Tailer surface.

**Misuse scenario.** A team's YAML loader has a bug where a missing `interval:` field defaults to `time.Duration(-1)` instead of zero (for instance, a custom `time.Duration` unmarshaller that returns `-1` on parse error and the caller forgot to error-check). The Tailer constructs fine; the watcher polls every second. Later, when the team re-enables `interval: 5s` in the YAML, *that* value also flows through `<= 0` (it doesn't, but the team imagines it might), so the team adds a `5 * time.Second` minimum coercion thinking they're being defensive. The actual bug — the negative-default — was never surfaced.

A second case: a developer writes a regression test passing `Interval: -42` *expecting* a constructor error (because the watcher's doc says negative is invalid). The test asserts `require.Error(t, err)`. The test fails. The developer assumes the validator was recently relaxed and removes the assertion. Now the regression check is gone.

**Recommended fix.** Tighten the Tailer-level validator to match the watcher's:

```go
if opts.Interval < 0 {
    return nil, fmt.Errorf("tail: Options.Interval must not be negative, got %v", opts.Interval)
}
if opts.Interval == 0 {
    opts.Interval = time.Second
}
```

This aligns the two layers. Cost: three lines. Documentation: also update the godoc on `Options.Interval` to state "Zero defaults to 1 second; negative is an error." Variant: route the Tailer's Interval through the Watcher's Config validator instead of duplicating the check — e.g., make `tail.New` call `watch.NewPolling` *before* the silent-zero coercion, and let the watcher decide. That eliminates the dual-validation surface.

---

## Summary

| # | Finding | File | Verdict | Severity |
|---|---------|------|---------|----------|
| 1 | `BackoffJitter == 0` documented "deterministic", silently normalized to `0.2` | `forward/forward.go:117-119` | Confused-developer footgun | Medium |
| 2 | `WithFlock(cursorPath)` silently breaks the lock on first Save | `tail/cursor.go:117`, `internal/atomicwrite/atomicwrite.go:42` | Configuration cliff | High |
| 3 | `Whence == io.SeekCurrent` accepted but no-op (silent `SeekStart`) | `watch/poll.go:51`, `watch/fsnotify_unix.go:49` | Algorithm-selection footgun | Medium |
| 4 | Negative batch limits pass `New` and disable flushing | `forward/forward.go:111-113` | Configuration cliff (under-strict) | Medium |
| 5 | `SyncOnCommit` requires manual `Sync` calls; no diagnostic if missing | `tail/cursor.go:32-35`, `tail/cursor.go:278-289` | Stringly-typed responsibility transfer | Medium |
| 6 | Sink errors retry forever unless wrapped with `ErrPermanent` | `forward/forward.go:38-44`, `forward/errors.go:5-12` | Default-polarity sharp edge | Medium |
| 7 | `Cursor = nil` silently disables checkpointing | `tail/tail.go:48-51`, `tail/tail.go:462-464` | Easy-path-is-wrong | Low |
| 8 | `WithFileMode` accepts world-writable / setuid / `0o000` | `tail/cursor.go:108-111` | Unvalidated constructor parameter | Low |
| 9 | `Decoder` doc promises `ErrPermanent` aborts; code always skips | `forward/decoders.go:5-8`, `forward/forward.go:222-229` | Confused-developer footgun (doc-vs-code) | Medium |
| 10 | `WithSyncBackgroundInterval` silently ignored unless `SyncMode==SyncBackground` | `tail/cursor.go:138-143`, `tail/cursor.go:199-207` | Mode-coupled option | Medium |
| 11 | `tail.Options.Interval < 0` silently coerced; `watch.Config.Interval < 0` rejected | `tail/tail.go:184-186`, `watch/poll.go:45-47` | Inconsistent validation across layers | Low |

Findings #1, #2, #3, and #9 are documentation-vs-code or doc-vs-validation divergences: in each, the API surface advertises one behaviour while the implementation produces another, with no diagnostic. They are the most actionable: tightening validation or matching documentation costs only a few lines per case. #9 is the polarity-twin of #6 — both concern `ErrPermanent` semantics, but at opposite ends of the Forwarder pipeline (Sink vs. Decoder), with inverted code-vs-doc agreement on each side.

Findings #4, #5, and #10 are configuration-cliff sharp edges where the safety check exists but is too narrow (#4), absent entirely with the responsibility shifted to the caller (#5), or silently dependent on a sibling option (#10). #10 amplifies #5: a caller fleeing fsync-per-Save by reaching for `WithSyncBackgroundInterval` without `WithSyncMode` lands in #5's territory if they then also pick `SyncOnCommit`.

Finding #6 is a design-level retry-policy polarity question; the existing `ErrPermanent` sentinel is the right primitive but the *default* is wrong.

Finding #11 is an inconsistent-validator pair where the looser layer always shadows the stricter one; the watcher's defensive negative-rejection is dead code on the standard call path.

Findings #7 and #8 are unvalidated-constructor-parameter gaps that compound with the other findings (a `nil` cursor + a default `Whence` is a silent data-replay; a permissive cursor mode + the existing atomicwrite symlink-follow finding from `insecure-defaults-2026-05-04.md` #1 widens the cursor-write trust boundary).

### Considered but not elevated

The following surfaces were probed during the re-examination and rejected as not meeting the sharp-edge bar:

- `WithCursorMigration(nil)` silently disables migration (`tail/cursor.go:128-130`). A nil migrator passed to the option setter is accepted; `Load` then falls through to `ErrUnsupportedCursorVersion` on a version mismatch. Real but minor — caller would see the explicit `ErrUnsupportedCursorVersion` and investigate.
- `Glob` source accepts an empty `backupGlob`, degenerating to `SingleFile` semantics (`tail/source.go:195-197`). No correctness loss, just a misleading constructor.
- `Source` constructors (`SingleFile`, `Lumberjack`, `Logrotate`, `Glob`) accept empty `activePath`. Validation happens late inside `watch.NewPolling` (`watch/poll.go:42-44`) instead of at the source-constructor boundary; the error still surfaces, just with worse locality.
- CLI `-start` / `-stop` flag naming (`cmd/gotail/main.go:28-29`). Boolean flags whose names invite the misread that `-stop` is a shutdown signal. UX, not a security boundary.
- Endemic nil-hook-disables-signal pattern across `tail.Options`, `watch.Config`, `forward.Options[T]`. Documented as "all hooks optional and nil-safe" by design (`tail/tail.go:90-92`); only `OnInodeMismatch` and `OnError` carry security weight, and that distinction is already captured by finding #3 of `insecure-defaults-2026-05-04.md`.

No findings were identified for: cryptographic API misuse, authentication-mode selection, type-confusion between security-critical primitive types, JWT-style algorithm-confusion, or stringly-typed permission strings. These categories do not apply to this codebase as scoped.
