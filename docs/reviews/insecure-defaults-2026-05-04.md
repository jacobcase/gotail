# gotail — Insecure Defaults Audit

- **Conducted:** 2026-05-04
- **Skill:** trailofbits/insecure-defaults
- **Scope:** v2 tree (`cmd/gotail`, `tail`, `watch`, `forward`, `internal/atomicwrite`, `tailtest`, `forwardtest`, `watchtest`); `v1/` excluded.
- **Trust-boundary map:** `docs/reviews/AUDIT_CONTEXT.md` (filesystem input dominates; no env-based config, no auth, no HTTP server, no credentials).

## Scope notes (what does not apply)

The traditional fail-open surfaces this skill targets are absent here:

- **No environment-variable secret loading.** Production code (`grep -E '(env\.|os\.Getenv|LookupEnv)'`) returns zero matches outside one test helper (`tail/flock_unix_test.go:73`). No `SECRET = env.get(X) or 'default'` patterns exist because no env loading exists.
- **No authentication, no authorization, no session handling.** gotail is a log-tail library; the only "credential" reference in the tree is a doc comment showing how a *caller* might wrap a permanent sink error (`forward/errors.go:11`).
- **No debug toggles that change security posture.** `Debug` appears only as `slog.Logger.Debug(...)` calls — verbosity, not auth bypass.
- **No hardcoded credentials in test fixtures.** `forwardtest/`, `tailtest/`, `watchtest/` were inspected line-by-line. They contain no secrets, tokens, passwords, or admin paths. The packages are public (importable from production) but expose only `RecordingSink`, `FailingSink`, `MemorySource`, `FakeWatcher` — none of which leak privilege.
- **No permissive crypto, no permissive CORS, no `0o777` chmod.** Cursor file mode defaults to `0o600` (`tail/cursor.go:186`); lockfile mode is hardcoded to `0o600` (`tail/flock_unix.go:16`, `tail/flock_windows.go:31`). The library performs no crypto and exposes no network endpoints.

The findings below are the residual permissive defaults that survive scoping the skill to this codebase. Three are filesystem-trust defaults (symlink follow + identity check) and one is a forwarder-runtime default (unbounded retry).

---

### 1. atomicwrite opens cursor tmp-file without `O_NOFOLLOW`/`O_EXCL` — symlink follow on shared parent directory

- **File:** `internal/atomicwrite/atomicwrite.go:23`
- **Severity:** High when gotail runs as a privileged user (e.g., root, service-account writing into a shared `/var/lib/...` or `/tmp/...`); Low when the cursor directory is mode-`0o700` and owned by the tailing user only.
- **Verdict:** **Fail-open.** The write succeeds against the attacker-chosen target; gotail emits no error.

**Pattern:**

```go
// Write writes data to path atomically:
//  1. Write data to path+".tmp" with mode.
//  2. fsync the temp file ...
func Write(path string, data []byte, mode os.FileMode, dirSync bool) error {
    tmp := path + ".tmp"
    f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
```

The `tmp` path is deterministic (`<cursorPath>.tmp`) and the open uses neither `O_NOFOLLOW` nor `O_EXCL`. On every Unix the kernel resolves a pre-existing symlink at `tmp`, opens its target, and `O_TRUNC` truncates that target. On success the subsequent `os.Rename(tmp, path)` then renames the symlink **itself** into place — `rename(2)` operates on the directory entry, not the link target — so `<cursorPath>` becomes a symlink that redirects all future cursor writes too.

**Exploit scenario.** gotail runs as a service account with write access to some shared resource (a config file, another service's data dir, `/etc/...` if running as root). The cursor lives at `/var/lib/myapp/cursor.json`, and the parent dir `/var/lib/myapp/` is group-writable or otherwise shared (a common deployment shape — `0o775`, group `adm`). A local attacker who can write to that dir runs:

```sh
ln -s /etc/cron.d/run-me /var/lib/myapp/cursor.json.tmp
```

When gotail next calls `FileCursor.Save` (under `SyncAlways`, every commit; under `SyncOnCommit`/`SyncBackground`, every `Sync`), `atomicwrite.Write` opens `cursor.json.tmp`, follows the symlink, truncates `/etc/cron.d/run-me`, and writes a JSON cursor record into it. After the rename, `cursor.json` is itself a symlink to `/etc/cron.d/run-me`; every subsequent commit overwrites it. The attacker now controls a privileged file with content of their choosing constrained only by the JSON envelope shape — which the attacker can pre-stage on disk under `Meta` (a raw `json.RawMessage` up to 64 KiB) by feeding a hostile cursor through one previous load cycle.

**Recommended fix.** Open the tmp file with `O_CREATE|O_EXCL|O_NOFOLLOW|O_WRONLY|O_TRUNC` (drop the `O_TRUNC` once `O_EXCL` is in place — they are mutually exclusive in intent; `O_EXCL` ensures we are creating the file, not opening an existing one). On `EEXIST` from a stale tmp file, `os.Remove(tmp)` and retry once; on a second `EEXIST` return an error. Even better: use a randomized tmp name (`path + ".tmp." + randomHex(8)`) so the attacker cannot pre-position the symlink. Document that the cursor file's parent directory must not be writable by untrusted principals.

---

### 2. Flock lockfile opens without `O_NOFOLLOW` — symlink follow on shared parent directory

- **File:** `tail/flock_unix.go:16`, `tail/flock_windows.go:31`
- **Severity:** Medium. The blast radius is smaller than #1 (only the PID string is written to the symlink target, and only after acquiring the lock), but the symlink-follow primitive is identical and the attack precondition (writable parent dir) is the same.
- **Verdict:** **Fail-open.** No error is surfaced; the lock is acquired against the attacker-chosen file.

**Pattern (Unix):**

```go
func acquireFlock(path string) (*flock, error) {
    f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
    ...
    if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
    ...
    // Write holder PID (best-effort; not load-bearing).
    _ = f.Truncate(0)
    _, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
```

Same pattern on Windows (`tail/flock_windows.go:31`) using `LockFileEx`.

**Exploit scenario.** Attacker plants a symlink at the lock path:

```sh
ln -s /var/log/audit/audit.log /var/lib/myapp/cursor.lock
```

When gotail's caller invokes `NewFileCursor(..., WithFlock("/var/lib/myapp/cursor.lock"))`, `acquireFlock`:

1. `os.OpenFile` follows the symlink, opening the **target** `audit.log` for read-write.
2. `syscall.Flock` takes `LOCK_EX` on the audit log's fd. POSIX advisory flock on `/var/log/audit/audit.log` does not block the audit subsystem (it ignores advisory locks), but it does mean the `gotail` user has write permission to the audit file. (If they don't, the open fails earlier — but a service account writing to its own data dir often has write to other service files via group membership.)
3. `f.Truncate(0)` blanks the audit log, then `WriteString(pid + "\n")` writes the PID.

The corruption survives until the audit subsystem rolls or the file is restored from backup. A defender investigating the gotail symptom only sees a PID line on disk; the connection to `WithFlock(...)` is not visible without source review.

**Recommended fix.** Open with `O_NOFOLLOW` on the Unix path; surface `ELOOP` as a distinct error (`ErrLockfileSymlink`) so deployments can detect it. On Windows, use `CreateFile` with `OPEN_EXISTING|FILE_FLAG_OPEN_REPARSE_POINT` semantics — i.e., do not traverse reparse points (the rough equivalent). Audit-context §11 already documents the lockfile-must-be-a-sibling rule; tightening the open flags makes that rule self-enforcing.

---

### 3. `FailOnInodeMismatch` defaults to `false` — gotail silently resumes through file-substitution

- **File:** `tail/tail.go:82-88` (Options field), `watch/watch.go:69-75` (Config field), `watch/poll.go:175-184` and `watch/fsnotify_unix.go:174-181` (the warn-and-continue path), `tail/tail.go:228-234` (`tail.New`'s mismatch branch)
- **Severity:** Medium when the watched file path is on a filesystem the embedder considers high-trust (logs from a privileged daemon being shipped to a SIEM); Low when the source filesystem is itself untrusted (the substitution event is ambient noise).
- **Verdict:** **Fail-open.** The mismatch is logged at `WARN` via `slog.Default()` and the watcher continues at offset 0 of whatever is at the path now. `OnInodeMismatch` is nil-safe — silent unless the embedder explicitly wired it.

**Pattern.**

```go
// FailOnInodeMismatch makes [New] return an error wrapping
// [ErrInodeMismatch] when the file at the cursor's path exists but has
// a different inode than the cursor recorded. Default behaviour is to
// log a warning and resume at offset 0, which is safer for most rotation
// patterns. ...
FailOnInodeMismatch bool
```

The "safer for most rotation patterns" justification holds for benign rotators (lumberjack/logrotate); it does not hold when an attacker can swap the file backing the watched path. The audit context (§4 #3, §6 #4, §7 #9-10) already documents that inode is the trust anchor for "same file" — but the trust-anchor invariant defaults to advisory.

**Exploit scenario.** A multi-tenant logging pipeline ingests `/var/log/tenants/<tenant>/app.log` from many tenants. Each tenant's directory is owned by the tenant. gotail runs as a privileged shipper that forwards every file to a SIEM. A tenant who wants to inject log entries attributable to a *different* tenant performs:

1. Wait for gotail to checkpoint a real log file (cursor records inode `I_real`).
2. `mv app.log app.log.real ; cp /tenant-other/log/forged.log app.log` — the path is preserved; the inode is different.
3. gotail's next watcher tick sees `inode != cursor.Inode`. With the default, it fires `OnInodeMismatch` (no-op if not wired), logs a warning, and resumes at offset 0 of `forged.log`. Every line in `forged.log` is shipped to the SIEM tagged with the original tenant's path. Detection requires either reading the slog warning or wiring `OnInodeMismatch` explicitly.

The same pattern applies to any deployment where the file backing the watched path is writable (or replaceable) by a less-trusted principal than the gotail process: container-mounted paths, shared NFS, FUSE mounts, etc.

**Recommended fix.** Two options, in order of preference:

1. Flip the default: `FailOnInodeMismatch = true`. Embedders who want lenient behaviour explicitly opt in. (Breaking change for v2 consumers; gate on a `tail.Options.AllowInodeMismatch` rename if wire-compat matters.)
2. If the default cannot flip, change the warn-only path to fire `OnInodeMismatch` *always* (it currently does) but also bump a `Stats.InodeMismatches` counter that scrapers can alert on, and document the security implication in the godoc for `FailOnInodeMismatch` (it currently only describes the rotation tradeoff).

---

### 4. `Forwarder.sendWithRetry` has no attempt cap and no default per-Send timeout

- **File:** `forward/forward.go:251-291` (the loop), `forward/forward.go:319-329` (`WithSinkTimeout` middleware — opt-in), `forward/forward.go:140-246` (Run wiring)
- **Severity:** Low (DoS / unbounded resource accumulation; not a confidentiality or integrity breach).
- **Verdict:** **Fail-open.** A misbehaving Sink stalls the forwarder until the parent context is cancelled.

**Pattern.**

```go
func (f *Forwarder[T]) sendWithRetry(ctx context.Context, batch []T, ...) error {
    for attempt := 0; ; attempt++ {
        ...
        err := f.opts.Sink.Send(ctx, batch)
        if err == nil { ... return nil }
        if errors.Is(err, ErrPermanent) { return ... }
        ...
        d := f.jitteredBackoff(attempt)
        ...
        select {
        case <-t.C:
        case <-ctx.Done():
            return ctx.Err()
        }
    }
}
```

There is no `MaxAttempts`, no `MaxRetryDuration`, no default `SinkTimeout`. `Source.Done()` does **not** abort `sendWithRetry` (the audit-context §6 #27 calls this out). The only way out is parent-ctx cancellation — and many embedders pass `context.Background()` to long-running daemons.

**Exploit scenario.** An attacker who can degrade (not break) the Sink's backend — e.g., saturate the receiving HTTP endpoint, force 503s, or hold TCP connections open without responding — keeps `sendWithRetry` spinning. With `WithSinkTimeout` not in use:

- A hung Sink (no response, no error) blocks `Send` indefinitely. The retry loop never reaches the backoff sleep. The active batch sits in memory, the cursor never advances, the Tailer's read loop blocks on `Source.Commit` only after a successful send (so it actually keeps reading and dropping records would require external pressure — but the in-flight batch holds memory and the Tailer's source eventually backpressures).
- A flapping Sink (returns transient errors) loops on `MaxBackoff = 30s` forever. The library logs at `Debug` (`forward/forward.go:278`) — not visible at default log levels — so the only signal is that no records are arriving downstream.

Both states persist until process restart or parent-ctx cancellation. Combined with `BackoffJitter` defaulting to `0.2` (jitter range `[0.8·ceiling, ceiling)`), the retry storm has high temporal correlation across multiple gotail instances pointed at the same Sink, amplifying the saturation.

**Recommended fix.** Add `Options.MaxRetryDuration` (cap on total time spent in `sendWithRetry` for a single batch) and/or `Options.MaxAttempts` (cap on retries before treating the error as permanent). When either fires, return a wrapped `ErrPermanent` so the existing escape-hatch flow (§4 #6, §6 #23) handles it. Separately, document `WithSinkTimeout` as required-not-optional in the `Forwarder.Run` godoc, or wire it implicitly when a non-zero `Options.SinkTimeout` field is set. Either change converts the failure mode from "stall forever" to "abort and surface".

---

## Summary

| # | Finding | File | Verdict | Severity |
|---|---------|------|---------|----------|
| 1 | atomicwrite tmp-file symlink follow | `internal/atomicwrite/atomicwrite.go:23` | Fail-open | High (privileged gotail + shared parent dir) |
| 2 | Lockfile symlink follow | `tail/flock_unix.go:16`, `tail/flock_windows.go:31` | Fail-open | Medium |
| 3 | `FailOnInodeMismatch` defaults false | `tail/tail.go:88`, `watch/watch.go:75` | Fail-open | Medium (security-sensitive deployments) |
| 4 | Forwarder retry has no attempt/duration cap | `forward/forward.go:251` | Fail-open | Low (DoS) |

Findings #1 and #2 are the only ones that materially extend an attacker's privilege; both are mitigated by a `0o700` cursor-file parent directory that the gotail user owns. Findings #3 and #4 are policy defaults that embedders should consciously override in adversarial deployments.

No findings were identified for: env-based fallback secrets, hardcoded credentials, debug toggles, fail-open authentication, or test fixtures leaking credentials to production. These categories do not apply to this codebase as scoped.
