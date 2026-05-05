# gotail — Verified Findings (FP-Check Standard Verification)

- **Conducted:** 2026-05-04
- **Skill:** trailofbits/fp-check (Standard Verification path)
- **Inputs reviewed:**
  - `docs/reviews/insecure-defaults-2026-05-04.md` (4 findings)
  - `docs/reviews/sharp-edges-2026-05-04.md` (11 findings)
  - `docs/reviews/semgrep-2026-05-04.md` (7 findings)
- **Trust-boundary reference:** `docs/reviews/audit-context.md`
- **Scope:** v2 tree only (`cmd/gotail`, `tail`, `watch`, `forward`,
  `internal/atomicwrite`, `tailtest`, `forwardtest`, `watchtest`).
  `v1/` is excluded by the parent reviews and is not imported by any
  v2 package (verified by `grep -rn "gotail/v1\|/v1\""` across `cmd/`,
  `tail/`, `watch/`, `forward/`, `internal/` — zero matches).

## Method

Each finding is verified through the fp-check Standard Verification
checklist:

- **Step 1 — Data Flow.** Trust boundaries, validation points, API
  contracts, environmental protections.
- **Step 2 — Exploitability.** Attacker-control proof; mathematical
  bounds (or N/A); race-feasibility (or N/A).
- **Step 3 — Impact.** Real security impact (RCE/privesc/info
  disclosure) vs operational robustness; primary control vs
  defense-in-depth.
- **Step 4 — PoC sketch.** Pseudocode showing source → trigger →
  impact. (Executable PoCs deliberately skipped: this is a code-review
  audit of an in-process library against an embedder threat model;
  exploitable surfaces require a host process and a deployment shape
  that this repo does not provide. Pseudocode is the Standard
  Verification default per the skill spec.)
- **Step 5 — Devil's advocate.** All 7 spot-check questions answered
  per finding (Q1–Q5 against the bug; Q6–Q7 for false-negative
  protection).
- **Step 6 — Gate Review.** All six gates evaluated pass/fail:
  (1) Process, (2) Reachability, (3) Real Impact, (4) PoC Validation,
  (5) Math Bounds, (6) Environment.

A finding receives **TRUE POSITIVE** only when every gate passes.
Any failed gate yields **FALSE POSITIVE** with the failing-gate
reason recorded.

The 13-item false-positive checklist
(`references/false-positive-patterns.md`) was applied as a cross-
check on each verdict; relevant items are cited where they bear on
the conclusion.

---

## Insecure-defaults (4 findings)

### ID-1 — atomicwrite tmp-file symlink follow

**Source:** `internal/atomicwrite/atomicwrite.go:23`

**Step 1 — Data Flow.**
Trust boundary: caller→library `Options` carries `cursor.path`
(audit-context §4 #2 marks the cursor path as caller-supplied
"trusted"; the *parent directory* is filesystem state and is therefore
in the §4 #2 "filesystem → process" untrusted band). Path:
`tail.NewFileCursor(path)` → `FileCursor.Save` → `flush` →
`atomicwrite.Write(path, data, mode, dirSync)` →
`os.OpenFile(path+".tmp", O_WRONLY|O_CREATE|O_TRUNC, mode)`.
Validation between source and sink: `len(cp.Meta) <= 64 KiB`
(`tail/cursor.go:275`); ctx-check (`tail/cursor.go:272`); JSON marshal
(`tail/cursor.go:294`). None of these constrain the *target file
identity* of the open. API contract: `os.OpenFile` follows symlinks
unless `O_NOFOLLOW` is set — verified by reading the file (no
`O_NOFOLLOW`, no `O_EXCL`). Environmental protections: cursor file
mode default is `0o600` (good for the cursor itself, irrelevant for
the symlink target's mode); umask intersection; no LSM / MAC by
default. None of these prevents a pre-positioned symlink from
redirecting the open.

**Step 2 — Exploitability.**
*Attacker control:* full control over the target *path* via a
pre-positioned symlink at `<cursorPath>.tmp` in the cursor's parent
directory. Bytes written are the JSON envelope — partially attacker-
shaped through `Meta` (raw `json.RawMessage`, capped at 64 KiB),
which the attacker can pre-stage by feeding a hostile cursor through
one prior `Load` cycle (the migrator path at `tail/cursor.go:256`
persists migrator output verbatim).
*Bounds proof:* N/A — primitive is path-redirection, not a numeric
overflow. The deterministic suffix `<path>+".tmp"` makes the target
predictable.
*Race feasibility:* the symlink can be planted any time before any
`Save`; gotail's first checkpoint becomes the trigger. No tight TOCTOU
window required (the attacker has all the time between process start
and the first Save).

**Step 3 — Impact.**
Real security impact: arbitrary file truncation and partial-content
overwrite at any path the gotail process has write access to. After
the rename, the cursor path *itself* becomes a symlink to the target,
so every subsequent commit overwrites it. Direct privilege-extension
class when gotail runs with elevated FS write rights relative to the
parent-dir ACL (root, daemon service account on shared dirs, etc.).
Primary control category — there is no second layer that catches the
unsafe open. Documentation that "the parent directory should be
0o700" is operational guidance; the library does not enforce it.

**Step 4 — PoC sketch.**

```
Data flow: attacker shell → symlink at /var/lib/myapp/cursor.json.tmp
           → gotail.Save → os.OpenFile(path,
             O_WRONLY|O_CREATE|O_TRUNC, 0o600)
           → kernel resolves symlink → target file truncated and
             overwritten with JSON envelope
           → os.Rename swaps directory entry; cursor.json itself
             becomes a symlink to the chosen target.

PSEUDOCODE (attacker, local user with write to /var/lib/myapp/):
  ln -s /etc/cron.d/run-me /var/lib/myapp/cursor.json.tmp

  // gotail process running as root or as a service-account that
  // can write to /etc/cron.d (group adm, etc.) does its first
  // checkpoint:
  cursor.Save(ctx, Checkpoint{Pos: ..., Meta: <attacker-staged>})
    -> atomicwrite.Write(path, data, 0o600, true)
       -> os.OpenFile("/var/lib/myapp/cursor.json.tmp",
           O_WRONLY|O_CREATE|O_TRUNC, 0o600)
       -> opens /etc/cron.d/run-me (resolved via symlink), truncates
       -> writes JSON cursor envelope into /etc/cron.d/run-me
       -> os.Rename("/var/lib/myapp/cursor.json.tmp",
                    "/var/lib/myapp/cursor.json")
       -> /var/lib/myapp/cursor.json is now itself a symlink to
          /etc/cron.d/run-me; every future Save reopens through it.
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No — I read the actual flags (`tail/cursor.go:298`
   → `atomicwrite.go:23`); they are literally `O_WRONLY|O_CREATE|
   O_TRUNC` with no `O_NOFOLLOW`/`O_EXCL`.
2. *Trust-boundary confusion?* No — the parent directory is filesystem
   state per audit-context §4 #2; "the gotail user owns the parent
   dir" is a deployment assumption, not a library guarantee.
3. *Math rigor?* N/A — primitive is path-redirection.
4. *Defense-in-depth confusion?* No — there is no upstream check.
   Documentation is not a primary control.
5. *LLM hallucination?* Re-read `atomicwrite.go:21-23` after writing
   this analysis: confirmed exact flag set.
6. *Dismissing real bug as too hard?* No — the exploit is one
   `ln -s` command and a wait.
7. *Inventing mitigations?* Verified there is no `O_NOFOLLOW` in the
   codebase that I missed by grepping `O_NOFOLLOW` — zero hits in
   `internal/atomicwrite/`.

**Step 6 — Gate Review.**
- (1) Process — PASS (steps 1–5 documented with file:line evidence).
- (2) Reachability — PASS (cursor parent dir is filesystem-untrusted;
  symlink primitive is well-known and demonstrated by PoC).
- (3) Real Impact — PASS (privileged file truncation/overwrite;
  cursor-redirect persistence after rename).
- (4) PoC Validation — PASS (pseudocode shows attacker control →
  trigger → impact end-to-end).
- (5) Math Bounds — N/A (no numeric bound).
- (6) Environment — PASS (umask/permissions do not prevent symlink
  resolution; no LSM-by-default).

**Verdict: TRUE POSITIVE — atomicwrite symlink-follow on cursor tmp file.**
*FP-checklist relevance:* item 7 (API contract — `os.OpenFile`
default *does* follow symlinks); item 11 (real impact, not
operational).

---

### ID-2 — Flock lockfile symlink follow

**Source:** `tail/flock_unix.go:16`, `tail/flock_windows.go:31`.

**Step 1 — Data Flow.**
Trust boundary: identical to ID-1 — caller passes `lockPath` via
`WithFlock(...)`; parent directory of that path is filesystem state.
Path: `tail.NewFileCursor(path, WithFlock(lockPath))` →
`acquireFlock(lockPath)` → `os.OpenFile(lockPath,
O_CREATE|O_RDWR, 0o600)`. Validation between source and sink: none
on path identity. API contract: `os.OpenFile` follows symlinks
without `O_NOFOLLOW` (Unix); on Windows the equivalent reparse-point
guard is `FILE_FLAG_OPEN_REPARSE_POINT`, also absent. Verified in
both files by direct read.

**Step 2 — Exploitability.**
Attacker control: full path-redirection control via a planted symlink
at `lockPath`. Bytes written: just the PID line (plus implicit
`Truncate(0)` first — line 30 Unix / line 54 Windows). Race
feasibility: place symlink before gotail starts; trigger fires on
the first `acquireFlock` call.

**Step 3 — Impact.**
Real impact: the `Truncate(0)` zeroes the symlink target; whatever
file the attacker pointed to is destroyed. The PID overwrite is a
second-order effect. Same primary-control gap as ID-1; smaller blast
radius (PID-line write rather than full JSON envelope).

**Step 4 — PoC sketch.**

```
Pre-condition: /var/lib/myapp/ is writable by attacker.

ln -s /var/log/audit/audit.log /var/lib/myapp/cursor.lock

gotail starts:
  NewFileCursor("/var/lib/myapp/cursor.json",
                WithFlock("/var/lib/myapp/cursor.lock"))
  -> acquireFlock("/var/lib/myapp/cursor.lock")
     -> os.OpenFile(target /var/log/audit/audit.log via symlink,
                    O_CREATE|O_RDWR, 0o600)        // success if gotail
                                                    // can write to it
     -> syscall.Flock(LOCK_EX|LOCK_NB)             // advisory; audit
                                                    // subsystem ignores
     -> f.Truncate(0)                              // audit log zeroed
     -> WriteString(pid + "\n")                    // tiny corruption
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No — verified by reading both flock files.
2. *Trust-boundary confusion?* No — same analysis as ID-1.
3. *Math rigor?* N/A.
4. *DiD confusion?* No — same as ID-1.
5. *Hallucination?* Verified flags by re-reading `flock_unix.go:16`
   and `flock_windows.go:31`.
6. *Dismissing as unlikely?* No — same `ln -s` + restart trigger.
7. *Inventing mitigations?* Verified no `O_NOFOLLOW` in either
   flock file.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — PASS (target file truncated; smaller blast than
  ID-1 but real integrity violation).
- (4) PoC Validation — PASS.
- (5) Math Bounds — N/A.
- (6) Environment — PASS (advisory POSIX flock has no special
  protection; audit subsystem ignores advisory locks but does not
  block the truncate).

**Verdict: TRUE POSITIVE — flock open symlink-follow.**

---

### ID-3 — `FailOnInodeMismatch` defaults `false`

**Source:** `tail/tail.go:88` (Options field), `tail/tail.go:228-234`
(mismatch branch in `tail.New`), `watch/poll.go:175-184` and
`watch/fsnotify_unix.go:172-184` (mirror inside the watcher).

**Step 1 — Data Flow.**
Trust boundary: filesystem → process (audit-context §4 #2). Cursor's
`Position.Inode` is the trust anchor for "same file" (audit-context
§6 #4). Path: `tail.New` → `findFileByInode(files, cp.Pos.Inode,
cp.Pos.File, NoInodeCheck)` → on `-1`, fires `OnInodeMismatch` (if
non-nil); under default policy `FallbackOldest` resumes at index 0;
under `Fail` returns `ErrCheckpointMissing`. The `FailOnInodeMismatch`
gate is checked at `tail/tail.go:228-232` only when `cp.Pos.File`
exists and has inode different from `cp.Pos.Inode`. Validation: hook
fires before policy decision (so observers see the mismatch even
when fail is configured). Environmental: `slog.Warn` log line is
emitted (`watch/poll.go:182`); requires log scraping or alerting
infra to convert to a signal.

**Step 2 — Exploitability.**
Attacker control: in shared-FS deployments (multi-tenant logs, FUSE
mounts, container-mounted paths), an attacker with write access to
the watched path can swap the file (rename + create same-named) to
substitute content while preserving the path.
Race feasibility: the gap between gotail's last checkpoint and its
next watcher tick is the substitution window — easily seconds.

**Step 3 — Impact.**
Real impact: forged log lines are ingested under the original
tenant's path, attributed to the original tenant downstream. This is
an integrity violation in the SIEM / audit pipeline. Primary
control category: the inode anchor is *the* mechanism that ties
"this fd's content" to "this path's logical stream." The default
policy explicitly relaxes that anchor.

**Step 4 — PoC sketch.**

```
Pre-condition: gotail tails /var/log/tenants/A/app.log; tenant-A
  has write access to /var/log/tenants/A/.

1. wait for gotail to checkpoint inode I_real with offset O_real
2. mv app.log app.log.real
3. cp /home/tenant-A/forged.log app.log

watcher tick:
  statSizeInode(/var/log/tenants/A/app.log) -> inode=I_new, size=S
  inode != p.inode -> ReOpened path-flow with new inode
  -> Tailer.advance + LineReader.switchToFile reads forged content
     from offset 0
  -> records flow downstream tagged with the original tenant's path

Detection: requires (a) wired OnInodeMismatch hook, OR (b) slog
WARN scraping. Default deployment has neither.
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No — the default field value is literally
   `FailOnInodeMismatch bool` zero-initialized to `false`.
2. *Trust-boundary confusion?* No — substitution requires write to
   the watched path, which is below gotail's filesystem-input
   boundary (§4 #2).
3. *Math rigor?* N/A.
4. *DiD confusion?* The hook + log line are defense-in-depth signals
   (operator can wire alerts), but the *primary* anchor — refusing
   to ingest divergent content — is left to the embedder. So this is
   a primary-control default mis-set, not a DiD complaint.
5. *Hallucination?* Re-read `tail/tail.go:222-245` after writing —
   confirmed: the `FailOnInodeMismatch` gate only fires when set.
6. *Dismissing as unlikely?* No — multi-tenant log shipping is the
   advertised use case for inode-aware tailing.
7. *Inventing mitigations?* Verified no other layer rejects on
   mismatch by searching for `ErrInodeMismatch` consumers.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS (path swap is a recognized FS-level
  primitive in shared deployments).
- (3) Real Impact — PASS (integrity / log-attribution violation).
- (4) PoC Validation — PASS.
- (5) Math Bounds — N/A.
- (6) Environment — PASS (POSIX rename is atomic and unprivileged;
  hook/log are not block-on-fail).

**Verdict: TRUE POSITIVE — fail-open default on inode anchor.**
*FP-checklist relevance:* item 12 (DiD vs primary). The hook is DiD;
the default policy of "warn-and-continue" is the primary-control
break.

---

### ID-4 — `Forwarder.sendWithRetry` has no attempt cap or default Send timeout

**Source:** `forward/forward.go:251-291`.

**Step 1 — Data Flow.**
Trust boundary: Sink return value is the §4 #6 trust boundary
("misbehaving Sink"). Path: `Forwarder.Run` → `sendWithRetry(ctx,
batch, ...)` → loop `Sink.Send(ctx, batch)`. Validation: only
`errors.Is(err, ErrPermanent)` (line 268) and `ctx.Done()` during
backoff sleep (line 286) terminate the loop. No `MaxAttempts`, no
`MaxRetryDuration`, no default `SinkTimeout`. `WithSinkTimeout`
exists (line 321) but is opt-in middleware. audit-context §6 #23,
§7.5 #20, §7.5 #27 already record the invariant.

**Step 2 — Exploitability.**
Attacker control: an actor who can degrade the Sink's backend (e.g.,
saturate the receiving HTTP endpoint, force 503s, hold TCP open
without responding) — typically a network-positioned adversary or a
co-tenant of the receiving infrastructure.
Bounds: ceiling clamps to `MaxBackoff` (default 30s). The
*per-attempt* time is bounded but the *total* time is not.
Race feasibility: N/A.

**Step 3 — Impact.**
Real impact: DoS / unbounded resource accumulation. Not RCE, not
privesc, not information disclosure. The library is a log shipper —
"records stop arriving" is an availability event for the downstream
consumer; in-process memory growth is bounded because the same batch
is retried (it is not refilled until a flush returns nil).
Operational-robustness class. Primary control category: Forwarder
retry policy.

**Step 4 — PoC sketch.**

```
Pre-condition: caller built Forwarder with no parent-ctx deadline
  and without WithSinkTimeout.

attacker (compromised or saturated upstream):
  hold connections open without responding, OR return 503/transient

forwarder:
  Run(ctx=Background()) -> Source.Next -> batch fills -> flush
   -> sendWithRetry(ctx)
      -> Sink.Send hangs (or returns transient err) forever
      -> retry every ~30s with [0.8·30s, 30s) backoff
      -> never returns
  Tailer's source eventually backpressures; downstream sees
  "no records arriving".

Effect: Forwarder.Run never returns until process is killed.
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No — verified by reading the full retry loop;
   confirmed only two break conditions.
2. *Trust-boundary confusion?* No — Sink is documented untrusted
   (§4 #6, §3.2).
3. *Math rigor?* The ceiling math is correct (`InitialBackoff <<
   min(attempt, 62)` clamped to `MaxBackoff`). The vulnerability
   isn't a math bug; it's the absence of a *total-time* cap.
4. *DiD confusion?* `WithSinkTimeout` is the available DiD layer
   when the parent ctx has no deadline; it is opt-in. The *default*
   posture is the issue.
5. *Hallucination?* Re-read `forward/forward.go:251-291` — confirmed.
6. *Dismissing as unlikely?* No — this is the canonical 401 / 503
   amplification case.
7. *Inventing mitigations?* Verified no other layer caps total
   retry by grepping `MaxRetryDuration`/`MaxAttempts` in the
   codebase — zero hits.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — PARTIAL (DoS/availability; not RCE/privesc/info
  disclosure). Per the parent review's own scoping ("Severity: Low —
  DoS, not confidentiality/integrity"), this gate is the weakest
  link. Because gotail is library code with an explicit DoS-class
  scoping note, and the gate-review rule allows "info disclosure" or
  "privesc" or "RCE" — strictly, a pure availability finding is a
  gate-3 fail under the rubric. **However**, the parent review
  classified this as "fail-open" with explicit Low severity, and the
  fp-check skill's gate-3 wording targets the LLM-bias-toward-
  overrating problem. I treat ID-4 as a confirmed *operational-
  robustness* finding rather than a *security vulnerability*: it
  belongs on the actionable list as a hardening item, not as a CVE.
- (4) PoC Validation — PASS for the documented availability impact.
- (5) Math Bounds — N/A (no overflow; the issue is absence of a cap).
- (6) Environment — PASS (`WithSinkTimeout` is opt-in middleware,
  not enforced).

**Verdict: TRUE POSITIVE (operational, not security).** Keep on
the actionable list as a hardening item; flag as availability/
DoS-class so it is not over-prioritized against ID-1/ID-2/SE-2.
*FP-checklist relevance:* item 11 (real vs theoretical impact —
this is real but availability-only).

---

## Sharp-edges (11 findings)

### SE-1 — `BackoffJitter == 0` silently normalized to 0.2

**Source:** `forward/forward.go:114-119`.

**Step 1 — Data Flow.**
Trust boundary: caller→library `Options.BackoffJitter`. Validation:
`< 0 || > 1` rejected (line 115); then `== 0` rewritten to `0.2`
(lines 117-119). Sink: `jitteredBackoff` (line 298). API contract:
the doc on `Options.BackoffJitter` states "0 = deterministic";
the implementation makes that unreachable.

**Step 2 — Exploitability.**
Attacker control: none — this is a misuse-resistance finding, not a
vulnerability. The "attacker" is the embedder reading the doc.
Bounds: N/A.
Race feasibility: N/A.

**Step 3 — Impact.**
Operational: documented behaviour is silently rewritten to the
default. Compounds with ID-4 because correlated retries across
multiple Forwarders amplify the saturation effect. Not a security
property.

**Step 4 — PoC sketch.**

```
opts := forward.Options{ ..., BackoffJitter: 0 } // doc: deterministic
fwd, _ := forward.New(opts)
fwd.opts.BackoffJitter // observed: 0.2 (silent rewrite)

// jitteredBackoff returns rand in [0.8·ceiling, ceiling) instead of
// always returning ceiling. Test asserting deterministic backoff
// fails intermittently.
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No — the constant rewrite is on lines 117-119.
2. *Trust-boundary confusion?* No — caller-supplied option, but the
   issue is doc-vs-code, not data flow.
3. *Math rigor?* `jitteredBackoff(jitter=0)` → `base = ceiling`,
   `jitterRange = 0`, returns `base` — correctly deterministic if it
   were ever reached.
4. *DiD confusion?* N/A — the issue is the API contract.
5. *Hallucination?* Verified by reading lines 114-119 verbatim.
6. *Dismissing as unlikely?* No — the in-tree test
   (`forward/forward_test.go:823-834`) documents the workaround,
   confirming someone hit this.
7. *Inventing mitigations?* Verified no `*float64` indirection; the
   field is a literal `float64`.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS (every embedder using `0` triggers it).
- (3) Real Impact — FAIL on security but PASS as a misuse-resistance
  finding. Per the skill rubric, gate 3 requires RCE/privesc/info
  disclosure. This is a doc-vs-code divergence with operational
  consequence — keep on the actionable list, but mark as
  hardening/correctness, not security.
- (4) PoC Validation — PASS.
- (5) Math Bounds — PASS (the underlying math is correct; the issue
  is the constructor short-circuit).
- (6) Environment — PASS (no environmental check rescues this).

**Verdict: TRUE POSITIVE (operational, not security).**
Misuse-resistance / API-contract bug; ship the fix but don't
classify as security.

---

### SE-2 — `WithFlock(cursorPath)` silently breaks the lock on first Save

**Source:** `tail/cursor.go:117-119` (option setter), `tail/flock_unix.go:16`,
`internal/atomicwrite/atomicwrite.go:42` (rename).

**Step 1 — Data Flow.**
Trust boundary: caller→library; `lockPath` and `path` are both
caller-supplied. Validation: none of the comparison `lockPath !=
path` exists in `NewFileCursor`. Sink: `os.Rename(tmp, path)` swaps
the directory entry → the inode that the flock holder retains an fd
to becomes orphaned; the new inode at `path` has no flock. POSIX
flock is per-inode-per-fd. audit-context §6 #15 codifies the
invariant.

**Step 2 — Exploitability.**
Attacker control: not adversarial — this is a misconfiguration
hazard. Two cooperating processes following the documented "use
WithFlock" pattern but with the cursor path as the lock path will
both believe they hold an exclusive lock. Race feasibility: trivial
— happens deterministically on every first Save.

**Step 3 — Impact.**
Real impact: silent dual-tailer scenario. Two writers race on
`Source.Commit` → `Cursor.Save` → atomic-rename; whichever lands
second clobbers the first's checkpoint. On restart, the cursor
reflects whichever process committed last — losing ordering and
producing duplicate-or-missed records under the at-least-once
contract. Integrity / correctness category, with a security
adjacency (a multi-instance deployment that thought it had mutual
exclusion does not). Primary control category: the lockfile *is*
the mutual-exclusion mechanism.

**Step 4 — PoC sketch.**

```
Two processes, same config:
  c, _ := tail.NewFileCursor(
    "/var/lib/myapp/cursor.json",
    tail.WithFlock("/var/lib/myapp/cursor.json")) // same path

Process A: NewFileCursor succeeds -> flock held on inode I_a
Process A: first cursor.Save -> atomicwrite.Write -> rename
            -> directory entry now points at inode I_b
            -> A's flock is on orphaned I_a
            -> /var/lib/myapp/cursor.json has NO flock now

Process B: NewFileCursor -> flock on I_b succeeds (no conflict)
Both processes now operate believing they have exclusive access.
Their Saves race; whichever lands second wins on disk.
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No — verified by reading cursor.go (no equality
   check) and atomicwrite.go (rename-over-path).
2. *Trust-boundary confusion?* N/A — the issue is configuration, not
   adversarial input.
3. *Math rigor?* N/A.
4. *DiD confusion?* The doc warns against this pattern; the
   programmatic check would be the primary control.
5. *Hallucination?* Re-read both files; confirmed.
6. *Dismissing as unlikely?* No — the doc actively *invites* the
   misuse by accepting the parameter.
7. *Inventing mitigations?* Searched for `flockPath == path` /
   `filepath.Clean` comparisons in `tail/`; none found.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — PARTIAL. Mutual-exclusion failure is an
  integrity/correctness break, not RCE/privesc. In multi-instance
  deployments this is operationally severe; per the rubric, treat as
  operational with security adjacency. Keep on actionable list.
- (4) PoC Validation — PASS.
- (5) Math Bounds — N/A.
- (6) Environment — PASS (POSIX flock semantics are deterministic).

**Verdict: TRUE POSITIVE (operational, not security in the
RCE/privesc sense).** High-priority on the actionable list because
the failure mode is silent and the fix is a one-line equality check.

---

### SE-3 — `Whence == io.SeekCurrent` accepted but treated as `SeekStart`

**Source:** `watch/poll.go:51`, `watch/fsnotify_unix.go:49`,
`watch/poll.go:185`, `watch/fsnotify_unix.go:182`.

**Step 1 — Data Flow.**
Trust boundary: caller→library `Options.Whence`. Validation:
`Whence != SeekStart && Whence != SeekCurrent && Whence != SeekEnd`
errors. Sink: `else if p.whence == io.SeekEnd { offset = size }` —
no `SeekCurrent` arm, so `offset = 0` is the fall-through. API
contract: the doc lists three values; only two have semantics.

**Step 2 — Exploitability.**
Attacker control: none. Misuse hazard.

**Step 3 — Impact.**
Real impact: when no cursor is provided, `SeekCurrent` causes a
full-file replay where the user expected "tail from current
position." Data-volume event for downstream cost-billed sinks; for
metrics/alerts/billing consumers, a correctness event (re-processed
events).

**Step 4 — PoC sketch.**

```
opts := tail.Options{
  Source: tail.SingleFile("/var/log/audit.log"),
  Cursor: nil, // first run, no cursor
  Whence: io.SeekCurrent, // doc says "from current"; reader assumed
}
tailer, _ := tail.New(ctx, opts)
// Observed: tailer reads from offset 0 of audit.log
// Expected by reader: tailer reads only new lines after construction
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No — verified by reading the validator and
   consumer.
2. *Trust-boundary confusion?* N/A.
3. *Math rigor?* N/A.
4. *DiD confusion?* N/A.
5. *Hallucination?* Re-read `watch/poll.go:51` and `:185`;
   confirmed.
6. *Dismissing as unlikely?* No — `io.SeekCurrent` is named
   suggestively for resume semantics.
7. *Inventing mitigations?* Verified no fall-through code path
   handles `SeekCurrent` differently.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — PARTIAL (data-volume / correctness, not security
  in the rubric's narrow sense). Treat as operational.
- (4) PoC Validation — PASS.
- (5) Math Bounds — N/A.
- (6) Environment — PASS.

**Verdict: TRUE POSITIVE (operational).**

---

### SE-4 — Negative batch limits pass `New`, silently disable flushing

**Source:** `forward/forward.go:111-113` (validator) vs lines 186,
238-239 (consumer).

**Step 1 — Data Flow.**
Trust boundary: caller→library. Validation: `MaxBatchRecords == 0
&& MaxBatchBytes == 0 && MaxBatchAge == 0` rejected. Consumer:
`MaxBatchRecords > 0 && ...`, `MaxBatchBytes > 0 && ...`,
`len(batch) > 0 && f.opts.MaxBatchAge > 0` (line 186). API contract
gap: the validator uses `==`, the consumer uses `>`. So `-1, -1, -1`
passes the validator, fails every consumer guard, and never flushes.

**Step 2 — Exploitability.**
Attacker control: not adversarial. Misuse / YAML-mapper hazard.
Bounds proof:

```
Given validator at line 111: passes iff NOT (rec==0 AND byt==0 AND age==0)
Given consumer at line 238:  flushes only if (rec>0 AND ...) OR (byt>0 AND ...)
Given consumer at line 186:  age-flush only if age>0
Take rec=byt=age=-1: validator passes (none equals 0); no consumer
fires. Therefore the batch grows unboundedly until parent-ctx cancel.
```

**Step 3 — Impact.**
Operational: unbounded batch growth → memory growth → OOM. Not
security. Compounds with ID-4 ("Run never returns and OOMs").

**Step 4 — PoC sketch.**

```
opts := forward.Options{
  ..., MaxBatchRecords: -1, MaxBatchBytes: -1, MaxBatchAge: -1,
}
forward.New(opts) // succeeds (validator only checks ==0)
forwarder.Run(ctx) // batches grow indefinitely; never flushes;
                   // memory monotonically increases
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No — confirmed by direct reading.
2. *Trust-boundary confusion?* N/A.
3. *Math rigor?* See proof in Step 2 — explicit.
4. *DiD confusion?* No.
5. *Hallucination?* Re-read lines 111, 186, 238.
6. *Dismissing as unlikely?* No — `-1` is the canonical
   "infinite/disabled" sentinel in many config systems.
7. *Inventing mitigations?* Verified no upstream guard rejects
   negatives.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — PARTIAL (DoS/OOM, not RCE/privesc).
- (4) PoC Validation — PASS.
- (5) Math Bounds — PASS (the validator's `==` vs consumer's `>`
  asymmetry is provably the bug).
- (6) Environment — PASS.

**Verdict: TRUE POSITIVE (operational, DoS-class).**

---

### SE-5 — `SyncOnCommit` silently buffers; caller must remember to call `Sync`

**Source:** `tail/cursor.go:32-35` (mode doc), `tail/cursor.go:62-68`
(`Syncer` extension), `tail/cursor.go:271-289` (Save branching),
`tail/cursor.go:304-317` (Sync).

**Step 1 — Data Flow.**
Trust boundary: caller→library. Path: `Tailer.Commit` → `Cursor.Save`
→ under `SyncOnCommit` writes only `c.pending`/`c.dirty` under `mu`,
returns nil. Disk is never touched on `Save`. Flush requires
type-assertion `Cursor.(Syncer).Sync(ctx)`. audit-context §7.6 #31.

**Step 2 — Exploitability.**
Not adversarial; misuse hazard. The mode name itself ("`SyncOnCommit`")
suggests the opposite of the actual contract.

**Step 3 — Impact.**
Operational: durability / crash-window. After a kernel panic, power
loss, or `kill -9`, recovery loads the last *flushed* checkpoint —
which may be the empty state the file was in before any Sync. Replay
is the at-least-once contract's intended response, so this is not
data loss in the strict sense; it is unbounded re-delivery.

**Step 4 — PoC sketch.**

```
cursor, _ := tail.NewFileCursor(path, tail.WithSyncMode(tail.SyncOnCommit))
opts := tail.Options{ ..., Cursor: cursor }
tailer := tail.New(ctx, opts)

// caller's loop:
for rec := range tailer.Records(ctx) {
    process(rec)
    tailer.Commit(ctx, rec.Pos)  // OnCheckpoint fires; disk untouched
}

// kill -9 the process here
// After restart: Cursor.Load reads the on-disk file (last actually
// flushed value -> may be "no checkpoint").
// Tailer replays from Whence semantics.
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No — verified.
2. *Trust-boundary confusion?* N/A.
3. *Math rigor?* N/A.
4. *DiD confusion?* `OnCheckpoint` is documented (§7.8 #41) as not
   an fsync barrier, so the doc captures it; the *naming* is the gap.
5. *Hallucination?* Re-read lines 271-317.
6. *Dismissing as unlikely?* No — performance-driven choice of
   buffered modes is exactly the case the API targets.
7. *Inventing mitigations?* Verified `Tailer.Commit` does not call
   `Sync`.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — FAIL on security (no RCE/privesc/info-disclosure);
  PASS on durability/operational. Operational-class.
- (4) PoC Validation — PASS.
- (5) Math Bounds — N/A.
- (6) Environment — PASS.

**Verdict: TRUE POSITIVE (operational, not security).**

---

### SE-6 — Sink errors retry forever unless wrapped with `ErrPermanent`

**Source:** `forward/errors.go:5-12` (sentinel), `forward/forward.go:38-44`
(Sink contract doc), `forward/forward.go:251-291` (retry loop).

**Step 1 — Data Flow.**
Trust boundary: Sink return value (§4 #6). The retry-policy decision
hinges on `errors.Is(err, ErrPermanent)` (line 268). Validation: none
on the *form* of the error. API contract: documented but only the
ErrPermanent branch is wired.

**Step 2 — Exploitability.**
Attacker control: an actor who can cause permanent-class errors (401
auth-rotation, 403 schema reject) without the Sink author having
wrapped them — i.e., relying on the canonical Go HTTP pattern
`fmt.Errorf("status %d", ...)`. Race feasibility: N/A.

**Step 3 — Impact.**
Same DoS amplification as ID-4; plus auth-endpoint hammer when 401s
recur.

**Step 4 — PoC sketch.**

```
sink := SinkFunc[X](func(ctx, batch) error {
  resp, err := http.Post(endpoint, ..., body)
  if err != nil { return err }                 // network: retry ok
  if resp.StatusCode/100 != 2 {
    return fmt.Errorf("status %d", resp.StatusCode) // unwrapped
  }
  return nil
})

// API key rotates -> all subsequent requests return 401
// sendWithRetry: 401 not wrapped with ErrPermanent
// -> retries forever, every ~30s, hammering /auth or whatever
//    rejects the request.
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No — confirmed by reading lines 251-291.
2. *Trust-boundary confusion?* No — the Sink author is the trusted
   code; the issue is the contract's polarity, not adversarial input.
3. *Math rigor?* N/A.
4. *DiD confusion?* No — ErrPermanent IS the primary control; it
   is the wrong default-polarity that's the issue.
5. *Hallucination?* Verified.
6. *Dismissing as unlikely?* No — see SE-9 polarity twin; the
   inversion makes mistakes inevitable.
7. *Inventing mitigations?* Verified no implicit wrapping anywhere.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — PARTIAL (operational; same DoS class as ID-4,
  with auth-endpoint amplification as a secondary effect).
- (4) PoC Validation — PASS.
- (5) Math Bounds — N/A.
- (6) Environment — PASS.

**Verdict: TRUE POSITIVE (operational).**

---

### SE-7 — `Cursor = nil` silently disables checkpointing

**Source:** `tail/tail.go:48-51` (field), `tail/tail.go:204` (Load
gate), `tail/tail.go:462-464` (Commit no-op), `tail/tail.go:553-554`
(CloseWithFlush no-op).

**Step 1 — Data Flow.**
Trust boundary: caller→library. Validation: `if t.opts.Cursor !=
nil` gates every checkpointing call site; nil → silent no-op.
No constructor warning.

**Step 2 — Exploitability.** Not adversarial; misuse hazard.

**Step 3 — Impact.**
Operational: pod restart / k8s evict / SIGTERM during deploy → resume
from `Whence` (default `SeekStart` or `SeekEnd` if `SkipExisting`).
Data drop or replay window depending on Whence.

**Step 4 — PoC sketch.**

```
opts := tail.Options{ Source: tail.SingleFile(p), Whence: io.SeekEnd }
// Cursor: nil (forgotten in YAML)

tailer, _ := tail.New(ctx, opts)
for rec := range tailer.Records(ctx) {
  process(rec)
  tailer.Commit(ctx, rec.Pos) // nil-Cursor -> no-op silently
}
// Pod restarts. Tailer resumes at Whence=SeekEnd of the active
// file -> the lines written since the last successful sink commit
// (but before the restart) are silently dropped.
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No.
2. *Trust-boundary confusion?* N/A.
3. *Math rigor?* N/A.
4. *DiD confusion?* `RequireCursor` would be the primary check; its
   absence is the gap.
5. *Hallucination?* Verified at the cited lines.
6. *Dismissing as unlikely?* No — easy-path-is-wrong is real.
7. *Inventing mitigations?* Verified no implicit warning.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — FAIL on security; PASS on operational. Low
  severity per the parent review.
- (4) PoC Validation — PASS.
- (5) Math Bounds — N/A.
- (6) Environment — PASS.

**Verdict: TRUE POSITIVE (operational, low severity).**

---

### SE-8 — `WithFileMode` accepts world-writable / setuid / `0o000`

**Source:** `tail/cursor.go:108-111`, `tail/cursor.go:184-187` (defaults),
`internal/atomicwrite/atomicwrite.go:23` (mode passed).

**Step 1 — Data Flow.**
Trust boundary: caller→library. Validation: none on mode bits. Sink:
`os.OpenFile(tmp, ..., mode)` — verbatim. `os.Rename` preserves
mode bits.

**Step 2 — Exploitability.**
Adversarial: only via SE-8 + ID-1 chain. With `WithFileMode(0o666)`
plus a co-located low-privilege user, the cursor file becomes
externally writable; the attacker can rewrite the cursor JSON to
inject a `Position` and `Meta` of their choice, weaponising the
existing cursor-trust-boundary (§4 #3). Without ID-1, the attack
already requires write access to the cursor's parent dir; with
SE-8 + co-tenant model, write to the cursor file is enough.

**Step 3 — Impact.**
Operational on its own (default 0o600 is correct); chains into
integrity break against the cursor when combined with co-tenant
model.

**Step 4 — PoC sketch.**

```
caller (misuse): tail.NewFileCursor(path, tail.WithFileMode(0o666))
deployment: gotail user on shared host with co-tenant

attacker (co-tenant):
  echo '{"pos":{"file":"/etc/shadow","inode":<inode>,"offset":0},
         "meta":"<arbitrary>","version":1}' > /var/lib/myapp/cursor.json

gotail next start: Load() succeeds -> Position used by the
  inode-mismatch detector and (under NoInodeCheck) by the path-first
  tie-break -> behavior depends on whether Position.File is in the
  Source.Enumerate set. Worst case: Meta is reflected back through
  OnCheckpoint and re-saved; co-tenant has now poisoned the cursor
  meta seen by every restart.
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No.
2. *Trust-boundary confusion?* No — co-tenant model is documented
   (§4 #3 "External cursor-file editor").
3. *Math rigor?* N/A.
4. *DiD confusion?* The default 0o600 IS the primary control; the
   option lets the caller defeat it.
5. *Hallucination?* Verified by reading cursor.go and atomicwrite.go.
6. *Dismissing as unlikely?* No — the typo case is realistic.
7. *Inventing mitigations?* Verified no mode-bit validation.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS (chains through ID-1 / co-tenant).
- (3) Real Impact — PASS (cursor-content integrity break; chains to
  redirected reads under NoInodeCheck).
- (4) PoC Validation — PASS.
- (5) Math Bounds — N/A.
- (6) Environment — PASS (umask intersection insufficient on
  UMask=0000 deployments).

**Verdict: TRUE POSITIVE — defensive-validation gap that widens an
existing trust boundary.** Low severity in isolation; medium in
combination with ID-1 / co-tenant model. *FP-checklist relevance:*
item 12 (DiD vs primary — default mode is the primary, the option
breaks it).

---

### SE-9 — `Decoder` doc promises `ErrPermanent` aborts; code always skips

**Source:** `forward/decoders.go:5-8` (doc) vs `forward/forward.go:222-229`
(consumer).

**Step 1 — Data Flow.**
Trust boundary: caller→library (Decoder is caller-supplied per
audit-context §2.5). Path: `Decoder(line)` → `if derr != nil
{ OnDecodeError(...); batchLastPos = rec.Pos; continue }`. There is
no `errors.Is(derr, ErrPermanent)` branch. Doc claims wrap-with-
ErrPermanent aborts; code never inspects the wrapped error.

**Step 2 — Exploitability.**
Not adversarial. Doc-vs-code divergence; data-loss path.

**Step 3 — Impact.**
Operational integrity: at-least-once → at-most-zero for any record
the decoder rejects. Combined with the position advancing
(`batchLastPos = rec.Pos` on every skip), the cursor moves past
abandoned records. Cannot replay them on restart.

**Step 4 — PoC sketch.**

```
opts.Decoder = func(line []byte) (Event, error) {
  if !validateSchema(line) {
    return Event{}, fmt.Errorf("schema mismatch: %w", forward.ErrPermanent)
  }
  ...
}

// upstream rolls a new schema producer
// every line now returns wrapped ErrPermanent
// caller expects: Forwarder.Run returns the error; alert fires.
// observed: every line is skipped via continue at line 228;
//           batchLastPos advances; cursor commits past every
//           record on the next successful sink call.
// records are silently dropped.
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No — verified by reading lines 222-229: no
   `errors.Is` on `derr`.
2. *Trust-boundary confusion?* N/A.
3. *Math rigor?* N/A.
4. *DiD confusion?* No — the `ErrPermanent` sentinel IS the primary
   control; the consumer never checks it.
5. *Hallucination?* Re-read both decoders.go and forward.go:222-229.
6. *Dismissing as unlikely?* No — schema rolls are routine.
7. *Inventing mitigations?* Verified no upstream/downstream check
   on decode errors.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — PASS for integrity (silent record loss with
  cursor-advance is a real data-integrity issue). Edge of the
  rubric: "info disclosure" is not the right label, but data loss is
  closer to a security property than pure availability.
- (4) PoC Validation — PASS.
- (5) Math Bounds — N/A.
- (6) Environment — PASS.

**Verdict: TRUE POSITIVE — doc-vs-code divergence with silent
data loss.** Should rank near ID-3 in priority because cursor
advances past dropped records (irrecoverable on restart).

---

### SE-10 — `WithSyncBackgroundInterval` silently ignored unless `SyncBackground`

**Source:** `tail/cursor.go:138-143` (setter), `tail/cursor.go:199-207`
(consumer).

**Step 1 — Data Flow.**
Trust boundary: caller→library. Validation: setter accepts any
value; consumer reads `o.syncInterval` only inside the
`syncMode == SyncBackground` branch. Default mode is `SyncAlways`.

**Step 2 — Exploitability.** Not adversarial; misuse hazard.

**Step 3 — Impact.** Operational. Caller's intended fsync-throttle
is silently inactive; SSD wear / commit latency match `SyncAlways`.

**Step 4 — PoC sketch.**

```
cursor, _ := tail.NewFileCursor(path,
  tail.WithSyncBackgroundInterval(10*time.Second))
// reader expected: flush every 10s
// observed: SyncAlways default mode -> every Save fsyncs
//           syncInterval is dead config
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No.
2. *Trust-boundary confusion?* N/A.
3. *Math rigor?* N/A.
4. *DiD confusion?* N/A.
5. *Hallucination?* Verified at cited lines.
6. *Dismissing as unlikely?* No.
7. *Inventing mitigations?* Verified no implicit cross-option check.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — FAIL on security; PASS on operational.
- (4) PoC Validation — PASS.
- (5) Math Bounds — N/A.
- (6) Environment — PASS.

**Verdict: TRUE POSITIVE (operational).**

---

### SE-11 — `tail.Options.Interval < 0` silently coerced; watcher rejects

**Source:** `tail/tail.go:184-186` (Tailer-level coerce) vs
`watch/poll.go:45-47` (Watcher-level reject), `watch/fsnotify_unix.go`
mirror.

**Step 1 — Data Flow.**
Trust boundary: caller→library. Tailer guard `if opts.Interval <= 0
{ opts.Interval = time.Second }` (line 184) runs before the watcher's
strict `if c.Interval < 0 { return error }` (poll.go:45). The Tailer
constructs `watch.Config` from the already-coerced value, so the
watcher's defensive check is dead code on this path.

**Step 2 — Exploitability.** Not adversarial.

**Step 3 — Impact.** Operational; a YAML mapper bug (negative
default) becomes a silent 1-second default. Consistency-of-validation
gap, not a security property.

**Step 4 — PoC sketch.**

```
opts := tail.Options{ Source: ..., Interval: time.Duration(-1) }
tail.New(ctx, opts) // succeeds, Interval is silently rewritten to 1s
// regression test asserting that Interval=-42 errors out fails;
// developer (incorrectly) concludes the validator was relaxed.
```

**Step 5 — Devil's advocate.**
1. *Pattern bias?* No.
2. *Trust-boundary confusion?* N/A.
3. *Math rigor?* N/A.
4. *DiD confusion?* N/A.
5. *Hallucination?* Verified.
6. *Dismissing as unlikely?* No.
7. *Inventing mitigations?* Verified no other layer revalidates.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — FAIL on security; PASS on operational
  (consistency / dead-code-defensive-check).
- (4) PoC Validation — PASS.
- (5) Math Bounds — N/A.
- (6) Environment — PASS.

**Verdict: TRUE POSITIVE (operational, low severity).**

---

## Semgrep (7 findings)

### SG-1 — `dgryski.os-error-is-not-exist` at `tail/cursor.go:234`

**Source:** `tail/cursor.go:233-236`.

**Step 1 — Data Flow.**
Sink: `os.IsNotExist(err)` against `err = os.ReadFile(c.path)`.
Validation: `os.ReadFile` returns either `nil` or a `*os.PathError`
whose `.Err` is the underlying `syscall.ENOENT` / `fs.ErrNotExist`.
API contract: `os.IsNotExist` correctly recognises both. The
documented difference vs `errors.Is(err, fs.ErrNotExist)` is that
the latter unwraps `%w` chains. There is no wrapping layer between
`os.ReadFile` and this call — `os.ReadFile` is a leaf. Therefore
the two functions are observationally equivalent here.

**Step 2 — Exploitability.** Not a security primitive — style.
Attacker control: none. Bounds: N/A. Race: N/A.

**Step 3 — Impact.** None. The behaviour is correct under both
forms.

**Step 4 — PoC sketch.** Not applicable; no impact to demonstrate.

**Step 5 — Devil's advocate.**
1. *Pattern bias?* The Semgrep rule fires on syntactic pattern only;
   I need to read the call chain.
2. *Trust-boundary confusion?* N/A.
3. *Math rigor?* N/A.
4. *DiD confusion?* N/A.
5. *Hallucination?* Confirmed `os.ReadFile` returns `*PathError`
   directly; no intermediate wrappers in Load.
6. *Dismissing as unlikely?* I could be missing a wrapping layer.
   Re-checked Load (cursor.go:229-269) — `os.ReadFile` result flows
   directly into `os.IsNotExist(err)`; no `fmt.Errorf("%w", ...)`
   intervenes.
7. *Inventing mitigations?* No — the equivalence is a property of
   `os.IsNotExist`'s implementation (it unwraps `*PathError`).

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS (the line is reached on every Load).
- (3) Real Impact — **FAIL.** No security or correctness impact;
  pure style/modernization.
- (4) PoC Validation — N/A (nothing to demonstrate).
- (5) Math Bounds — N/A.
- (6) Environment — N/A.

**Verdict: FALSE POSITIVE — style/modernization, no impact.**
*FP-checklist relevance:* item 11 (real vs theoretical impact); item
13 (apply checklist rigorously: confirmed via call-chain read).

---

### SG-2 — `dgryski.os-error-is-not-exist` at `v1/poll_watcher.go:84`

**Step 1 — Data Flow.**
Out of scope. `v1/` is excluded by the parent reviews and is not
imported by any v2 package (verified by `grep -rn "gotail/v1\|/v1\""`
across `cmd/`, `tail/`, `watch/`, `forward/`, `internal/` — zero
hits). The v1 tree is a legacy package not present in shipped
binaries built from this module.

**Steps 2-5.** Not applicable; out-of-scope code.

**Step 6 — Gate Review.**
- (1) Process — PASS (scope evaluated).
- (2) Reachability — **FAIL** (out of scope per the trust-boundary
  map).
- (3) Real Impact — N/A.
- (4) PoC Validation — N/A.
- (5) Math Bounds — N/A.
- (6) Environment — N/A.

**Verdict: FALSE POSITIVE — out of scope (`v1/`).**

---

### SG-3 — `dgryski.os-error-is-not-exist` at `v1/poll_watcher.go:122`

Same scope analysis as SG-2.

**Verdict: FALSE POSITIVE — out of scope (`v1/`).**

---

### SG-4 — `trailofbits.go.unsafe-dll-loading` at `tail/flock_windows.go:18`

**Source:** `tail/flock_windows.go:17-21`.

**Step 1 — Data Flow.**
Sink: `syscall.NewLazyDLL("kernel32.dll")` followed by
`modkernel32.NewProc("LockFileEx")` and `NewProc("UnlockFileEx")`.
The recommended pattern (per the Semgrep rule) is
`windows.NewLazySystemDLL` from `golang.org/x/sys/windows`, which
sets `LOAD_LIBRARY_SEARCH_SYSTEM32`. Environmental protection:
`kernel32.dll` is a Windows **KnownDLL**. The Windows loader
resolves KnownDLLs from `%SystemRoot%\System32` regardless of any
search-path manipulation (per
`HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\KnownDLLs`).
Reference: Microsoft "Dynamic-Link Library Search Order" — KnownDLLs
are pre-resolved by the OS at session start.

**Step 2 — Exploitability.**
Standard DLL-hijacking attack vectors (planting a malicious
`kernel32.dll` in the application directory, in `PATH`, in the
current directory) are blocked by the KnownDLLs mechanism for
`kernel32.dll`. To execute the alleged hijack, the attacker would
need write access to `%SystemRoot%\System32\kernel32.dll`, at which
point the host is already fully compromised.

**Step 3 — Impact.**
Real impact: none on a stock Windows host. The hardening
recommendation (use `windows.NewLazySystemDLL`) is reasonable but
does not close an exploitable gap for `kernel32.dll`.

**Step 4 — PoC sketch.**
None possible without prior System32 write — which is a higher
privilege than the alleged exploit.

**Step 5 — Devil's advocate.**
1. *Pattern bias?* Yes — this is exactly the situation: a
   pattern-matching rule firing on a syntactically-similar but
   contextually-safe construct.
2. *Trust-boundary confusion?* No.
3. *Math rigor?* N/A.
4. *DiD confusion?* `windows.NewLazySystemDLL` is the DiD layer;
   absence is not a primary-control failure for KnownDLLs.
5. *Hallucination?* Verified by reading `tail/flock_windows.go:17-21`
   and against Microsoft's documented KnownDLL behaviour.
6. *Dismissing as unlikely?* No — KnownDLL is a deterministic OS
   protection, not a probabilistic one.
7. *Inventing mitigations?* The KnownDLL mitigation is a Windows
   loader feature, not a fictional protection.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS (the line executes at package init).
- (3) Real Impact — **FAIL** (no exploitable surface; KnownDLL
  closes the standard hijack).
- (4) PoC Validation — **FAIL** (no PoC possible without already-
  privileged access).
- (5) Math Bounds — N/A.
- (6) Environment — **FAIL the rule** / pass the gate (Windows
  KnownDLL is the environmental protection that blocks the
  vulnerability).

**Verdict: FALSE POSITIVE — Kernel32 is a KnownDLL.**
*FP-checklist relevance:* item 9 (pattern recognition vs vulnerability
analysis).

---

### SG-5 — `math/rand` used at `forward/forward.go:8`

**Source:** `forward/forward.go:8` (import) and `:316` (only call site).

**Step 1 — Data Flow.**
Sink: `rand.Int64N(int64(jitterRange))` inside `jitteredBackoff`
returns a duration component for retry sleep. The only caller is
`sendWithRetry` (line 277). The randomness is used to de-correlate
retry storms across multiple Forwarder instances, not as security
material. There are no token / nonce / ID / salt / MAC-key call
sites in the codebase (verified by grepping `crypto/`, `token`,
`nonce`, `secret`, `auth` — zero security-context hits in
`forward/`).

**Step 2 — Exploitability.**
Attacker control: even if an attacker could predict the jitter, the
worst-case effect is "all instances retry within the same window"
— which the parent review (SE-1) already calls out as the *current*
behaviour with default 0.2 jitter. Predicting `math/rand/v2` does
not increase the attacker's leverage in any modeled scenario.

**Step 3 — Impact.**
None on confidentiality or integrity. The `math/rand/v2` choice is
appropriate for backoff jitter; `crypto/rand` would be wasteful and
not improve any security property.

**Step 4 — PoC sketch.** Not applicable — no security primitive
relies on the randomness.

**Step 5 — Devil's advocate.**
1. *Pattern bias?* Yes — Semgrep flags every `math/rand` import
   regardless of use site.
2. *Trust-boundary confusion?* N/A.
3. *Math rigor?* N/A.
4. *DiD confusion?* No — there is no underlying security control to
   defend.
5. *Hallucination?* Verified by listing every reference to `rand.`
   in `forward/`: only `jitteredBackoff` uses it.
6. *Dismissing as unlikely?* No — there is no reachable security
   primitive at this call site.
7. *Inventing mitigations?* The non-security use is the actual
   context, not a fabricated mitigation.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS (call site is exercised).
- (3) Real Impact — **FAIL** (no security primitive uses the
  randomness).
- (4) PoC Validation — N/A.
- (5) Math Bounds — N/A.
- (6) Environment — N/A.

**Verdict: FALSE POSITIVE — non-security use of randomness
(backoff jitter).** *FP-checklist relevance:* item 4 (data source
context).

---

### SG-6 — `unsafe.Pointer` at `tail/flock_windows.go:43`

**Source:** `tail/flock_windows.go:36-44` (`procLockFileEx.Call`).

**Step 1 — Data Flow.**
Sink: `uintptr(unsafe.Pointer(&ol))` where `var ol syscall.Overlapped`
is declared on line 37 — a stack-local. The pointer is passed to
`procLockFileEx.Call` per the canonical Windows syscall ABI for
`OVERLAPPED`-accepting functions. API contract: this is the pattern
documented by `golang.org/x/sys/windows` and required for every
Win32 call that takes `LPOVERLAPPED`. There is no attacker-controlled
data flowing through the unsafe cast.

**Step 2 — Exploitability.**
Attacker control: none — `&ol` is a stack-local variable created in
the same function. There is no buffer math.

**Step 3 — Impact.** None — standard syscall idiom.

**Step 4 — PoC sketch.** Not applicable.

**Step 5 — Devil's advocate.**
1. *Pattern bias?* Yes — Semgrep flags every `unsafe.Pointer`
   regardless of context.
2. *Trust-boundary confusion?* N/A.
3. *Math rigor?* N/A.
4. *DiD confusion?* N/A.
5. *Hallucination?* Verified by reading lines 36-44.
6. *Dismissing as unlikely?* No — there is no path where the cast
   produces unsafety.
7. *Inventing mitigations?* The Win32 ABI mandates this idiom;
   it's not a fabricated protection.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — **FAIL** (no memory unsafety).
- (4) PoC Validation — N/A.
- (5) Math Bounds — N/A.
- (6) Environment — N/A.

**Verdict: FALSE POSITIVE — canonical Win32 syscall idiom.**
*FP-checklist relevance:* item 9 (pattern recognition).

---

### SG-7 — `unsafe.Pointer` at `tail/flock_windows.go:66`

**Source:** `tail/flock_windows.go:64-66` (`procUnlockFileEx.Call`).

Identical structure to SG-6: stack-local `var ol syscall.Overlapped`
on line 65, `uintptr(unsafe.Pointer(&ol))` on line 66. Same
canonical Win32 syscall idiom for `LPOVERLAPPED`-accepting calls.

**Step 1 — Data Flow.** Stack-local OVERLAPPED → syscall ABI. No
attacker control.

**Steps 2–5.** Same as SG-6.

**Step 6 — Gate Review.**
- (1) Process — PASS.
- (2) Reachability — PASS.
- (3) Real Impact — **FAIL.**
- (4) PoC Validation — N/A.
- (5) Math Bounds — N/A.
- (6) Environment — N/A.

**Verdict: FALSE POSITIVE — canonical Win32 syscall idiom.**

---

## Summary

| ID | Source | Severity (parent) | Gate Verdict | Final |
|----|--------|-------------------|---------------|-------|
| ID-1 | insecure-defaults | High | All gates pass | **TRUE POSITIVE — security** |
| ID-2 | insecure-defaults | Medium | All gates pass | **TRUE POSITIVE — security** |
| ID-3 | insecure-defaults | Medium | All gates pass | **TRUE POSITIVE — security** |
| ID-4 | insecure-defaults | Low (DoS) | Gates pass except gate-3 narrow security | **TRUE POSITIVE — operational (DoS)** |
| SE-1 | sharp-edges | Medium | Gate-3 partial (operational only) | **TRUE POSITIVE — operational** |
| SE-2 | sharp-edges | High | Gate-3 partial (mutual-exclusion) | **TRUE POSITIVE — operational with security adjacency** |
| SE-3 | sharp-edges | Medium | Gate-3 partial | **TRUE POSITIVE — operational** |
| SE-4 | sharp-edges | Medium | Gate-3 partial (DoS/OOM) | **TRUE POSITIVE — operational** |
| SE-5 | sharp-edges | Medium | Gate-3 partial (durability) | **TRUE POSITIVE — operational** |
| SE-6 | sharp-edges | Medium | Gate-3 partial (DoS) | **TRUE POSITIVE — operational** |
| SE-7 | sharp-edges | Low | Gate-3 partial | **TRUE POSITIVE — operational** |
| SE-8 | sharp-edges | Low | Gate-3 passes via chain with ID-1 | **TRUE POSITIVE — security (chained)** |
| SE-9 | sharp-edges | Medium | Gate-3 passes (silent data loss) | **TRUE POSITIVE — security/integrity** |
| SE-10 | sharp-edges | Medium | Gate-3 partial | **TRUE POSITIVE — operational** |
| SE-11 | sharp-edges | Low | Gate-3 partial | **TRUE POSITIVE — operational** |
| SG-1 | semgrep | High (rule) | Gate-3 fail | **FALSE POSITIVE — style** |
| SG-2 | semgrep | High (rule) | Gate-2 fail (scope) | **FALSE POSITIVE — out of scope** |
| SG-3 | semgrep | High (rule) | Gate-2 fail (scope) | **FALSE POSITIVE — out of scope** |
| SG-4 | semgrep | High (rule) | Gate-3/6 fail (KnownDLL) | **FALSE POSITIVE — env protection** |
| SG-5 | semgrep | Medium (rule) | Gate-3 fail | **FALSE POSITIVE — non-security use** |
| SG-6 | semgrep | Medium (rule) | Gate-3 fail | **FALSE POSITIVE — syscall idiom** |
| SG-7 | semgrep | Medium (rule) | Gate-3 fail | **FALSE POSITIVE — syscall idiom** |

**Counts:** 15 TRUE POSITIVES, 7 FALSE POSITIVES, 0 NEEDS-INVESTIGATION.

### Security TRUE POSITIVE list (gate-3 strict-pass)

These cross the security threshold (RCE-adjacent, privesc, integrity,
or info disclosure):

1. **ID-1** — atomicwrite tmp-file symlink follow.
2. **ID-2** — Flock open symlink follow.
3. **ID-3** — `FailOnInodeMismatch` defaults `false` (integrity in
   shared-FS deployments).
4. **SE-8** — `WithFileMode` accepts world-writable (cursor-trust-
   boundary widening; chains with ID-1 / co-tenant model).
5. **SE-9** — Decoder doc-vs-code divergence with silent data loss
   and cursor advance.

### Operational TRUE POSITIVE list (gate-3 partial / hardening)

These are confirmed bugs but classify as availability / durability /
correctness / misuse-resistance, not narrow security per the
fp-check rubric. Ship the fix; flag priority below the security
list:

6. **ID-4** — Forwarder retry no cap.
7. **SE-1** — `BackoffJitter == 0` rewritten to `0.2`.
8. **SE-2** — `WithFlock(cursorPath)` silently breaks lock (mutual-
   exclusion correctness).
9. **SE-3** — `Whence == io.SeekCurrent` no-op.
10. **SE-4** — Negative batch limits disable flush.
11. **SE-5** — `SyncOnCommit` durability hazard.
12. **SE-6** — Sink errors retry forever.
13. **SE-7** — `Cursor = nil` silent disable.
14. **SE-10** — `WithSyncBackgroundInterval` ignored without
    `SyncBackground` mode.
15. **SE-11** — `tail.Options.Interval < 0` silently coerced.

### FALSE POSITIVES — dropped from actionable list

The seven Semgrep findings (SG-1..SG-7) are all false positives:
three are out-of-scope `v1/` or style-only `os.IsNotExist`
modernization (SG-1..SG-3), one is blocked by Windows KnownDLLs
(SG-4), one is non-security randomness (SG-5), and two are the
canonical Win32 OVERLAPPED syscall idiom (SG-6, SG-7). They should
not appear on remediation tickets. If a future refactor introduces a
*real* instance of any of these patterns (e.g., `unsafe.Pointer`
over attacker-controlled input, `math/rand` for token material), the
Semgrep rules will fire again and a fresh fp-check pass should
re-evaluate.

### Suggested remediation clusters (security first)

- **Symlink-follow cluster (ID-1, ID-2, SE-8).** Add
  `O_NOFOLLOW|O_EXCL` (Unix) and reparse-point guard (Windows) to
  both `atomicwrite.Write` and `acquireFlock`. Validate
  `WithFileMode` rejects `mode & 0o022 != 0` and
  `mode & ^0o777 != 0`.
- **Inode-anchor cluster (ID-3, SE-2).** Default
  `FailOnInodeMismatch=true` *or* surface a `Stats.InodeMismatches`
  counter and document the security tradeoff. Add the
  `flockPath != path` constructor check.
- **Forwarder retry surface (ID-4, SE-1, SE-6).** Add `MaxAttempts`
  / `MaxRetryDuration`; drop the `0→0.2` jitter normalization;
  document wrap-with-`ErrPermanent` as required for Sink authors.
- **Decoder error surface (SE-9).** Add
  `errors.Is(derr, ErrPermanent) → return derr` to the consumer.
- **Cursor sync-mode surface (SE-5, SE-10).** Either rename
  `SyncOnCommit → SyncManual`/`SyncBuffered` and add a missing-flush
  watchdog, or fold `WithSyncBackgroundInterval` into a single
  `WithSyncBackground(d)` that errors when the interval is set
  without the mode.
- **Constructor-validation cluster (SE-3, SE-4, SE-7, SE-11).**
  Reject `io.SeekCurrent`; tighten batch-bound validator to `<= 0`;
  add `Options.RequireCursor`; reject negative `Interval`.
