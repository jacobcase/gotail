# Supply Chain Risk Report

**Conducted:** 2026-05-04

---

## Metadata

- **Scan Date**: 2026-05-04
- **Project**: gotail (`github.com/jacobcase/gotail/v2`)
- **Repositories Scanned**: 7 modules
- **Total Dependencies**: 2 compiled into the binary, 1 compiled into tests, 4 listed in `go.sum` only
- **Inputs**: `go.mod`, `go.sum`, `go list -deps ./...`, `go mod graph`, OSV.dev, GitHub API

---

## Executive Summary

gotail's runtime attack surface is **two well-maintained, organization-backed
modules**: `github.com/fsnotify/fsnotify` and `golang.org/x/sys`. Neither has a
known CVE at its pinned version, both have multi-contributor org ownership, and
fsnotify is being actively committed to (last commit on the day of this scan).

The only meaningful production hygiene finding is that `golang.org/x/sys` is
pinned at `v0.13.0` (Sep 2023), roughly 30 minor versions behind the current
`v0.43.0`. OSV reports no current vulnerabilities at this version, but the gap
is unusually wide for a Go project tracking the standard subrepos.

The remaining four flagged modules — `gopkg.in/yaml.v3`, `github.com/davecgh/go-spew`,
`github.com/pmezard/go-difflib`, and `github.com/stretchr/testify` — appear in
`go.sum` because they are declared by `go.uber.org/goleak`'s `go.mod`, but **none
of them are imported by any package in gotail or by `goleak`'s runtime code**
(verified with `go list -deps ./...`). They are test-of-test transitives that
never reach a compiled artifact. They still warrant the call-outs below
(archived upstream, single-maintainer staleness) because anything in `go.sum`
becomes a *potential* future supply-chain vector if a future version of `goleak`
or its replacement starts importing them — but at the pinned versions, they pose
**no current runtime risk to gotail**.

### Compiled Surface vs. `go.sum` Manifest

| Module                                 | Pinned Version          | Compiled Into | Risk Level (current) |
|----------------------------------------|-------------------------|---------------|----------------------|
| `github.com/fsnotify/fsnotify`         | v1.10.0                 | binary        | none                 |
| `golang.org/x/sys`                     | v0.13.0                 | binary        | low (stale pin)      |
| `go.uber.org/goleak`                   | v1.3.0                  | tests         | none                 |
| `github.com/stretchr/testify`          | v1.8.0                  | none          | none                 |
| `github.com/davecgh/go-spew`           | v1.1.1                  | none          | manifest only        |
| `github.com/pmezard/go-difflib`        | v1.0.0                  | none          | manifest only        |
| `gopkg.in/yaml.v3`                     | v3.0.1                  | none          | manifest only        |

### Counts by Risk Factor

| Risk Factor                           | Dependencies                                          | Total |
|---------------------------------------|-------------------------------------------------------|-------|
| Single maintainer (dominant author)   | `go-spew`, `go-difflib`, `yaml.v3`                    | 3     |
| Unmaintained / stale / archived       | `go-spew`, `go-difflib`, `yaml.v3`                    | 3     |
| Low popularity                        | `go-difflib`                                          | 1     |
| Past CVE                              | `yaml.v3` (fixed in pinned version), `golang.org/x/sys` (fixed before pinned version) | 2 |
| Stale pin (current version OK)        | `golang.org/x/sys`, `testify`                         | 2     |
| Absence of `SECURITY.md` in repo      | all 7                                                 | 7     |
| **Compiled-surface high-risk**        | —                                                     | **0** |

### High-Risk Dependencies (sorted by risk, highest first)

| # | Dependency Name | Compiled? | Risk Factors | Notes | Suggested Alternative |
|---|-----------------|-----------|--------------|-------|-----------------------|
| 1 | `gopkg.in/yaml.v3` v3.0.1 | No (manifest only) | archived upstream, single dominant maintainer, past CVE | Repo `go-yaml/yaml` archived 2025-04-01. Last content commit 2022-05-27. CVE-2022-28948 (panic on malformed input, HIGH) was fixed exactly in v3.0.1, so the pin is the patched version — but no future patches will ship. Gustavo Niemeyer authored 244 of ~305 contributions. ~7k stars. Not on any gotail code path. | **`go.yaml.in/yaml/v3`** — community-maintained continuation referenced by the archive notice; or **`sigs.k8s.io/yaml`** for stricter parsing. Only relevant if a future `goleak` (or its replacement) starts importing yaml. |
| 2 | `github.com/pmezard/go-difflib` v1.0.0 | No (manifest only) | unmaintained ~7y, single maintainer, low popularity | Last commit 2018-12-26. ~428 stars (lowest of all deps). pmezard authored 19 of ~24 contributions. Pulled in by testify only as a `go.mod` declaration. | **`github.com/google/go-cmp`** for newer assertion libraries; testify v1.10+ has begun migrating away from `go-difflib` for some output paths. No action needed for gotail (not compiled). |
| 3 | `github.com/davecgh/go-spew` v1.1.1 | No (manifest only) | unmaintained ~7y, single maintainer | Last commit 2018-08-30. v1.1.1 from 2018. davecgh authored 116 of ~130 contributions. ~6.4k stars. Pulled in by testify only as a `go.mod` declaration. | **`github.com/google/go-cmp`** + `cmp.Diff` for diffing values, or `github.com/davecgh/go-spew/spew` v1.1.2-pre forks if a maintained equivalent is needed. No action needed for gotail (not compiled). |
| 4 | `golang.org/x/sys` v0.13.0 | **Yes (prod)** | stale pin (~2.5y, ~30 minor versions behind) | Pinned via fsnotify's `go.mod`. Latest is v0.43.0 (2026-Q1). OSV reports zero vulnerabilities affecting v0.13.0 today. Historical CVE-2022-29526 (`Faccessat` privilege reporting) is fixed long before this pin. Multi-contributor Go-team-owned (tklauser, zx2c4, mdlayher, alexbrainman, ianlancetaylor). Org-backed by Google. | **`golang.org/x/sys` v0.43.0+** — same module, just a `go get -u` followed by `go mod tidy`. Bumping fsnotify from v1.10.0 to v1.10.1 (released today) will likely pull a newer `x/sys` indirectly. |

The following two modules have a single mild factor each but did not warrant a row above:

- `github.com/stretchr/testify` v1.8.0 — pin is ~3 minor versions behind current v1.11.1; not compiled.
- `go.uber.org/goleak` v1.3.0 — release cadence slow (no new release in 2.5 years despite recent master commits); compiled into tests only, no security implications for the binary.

### Per-Dependency Detail

| Module | Owner Type | Top Author Share | Last Commit | Latest Tag | OSV Vulns @ Pin | Stars | LOC (non-test) | Typosquat Risk |
|--------|-----------|-------------------|-------------|------------|------------------|-------|----------------|----------------|
| `github.com/fsnotify/fsnotify` v1.10.0 | Org (`fsnotify`) | 175/~520 (arp242, ~34%) | 2026-05-04 | v1.10.1 (today) | 0 | ~10.6k | 4,412 | None — canonical |
| `golang.org/x/sys` v0.13.0 | Org (`golang`/Google) | 596/~1,100 (tklauser, ~54%) | 2026-04-23 | v0.43.0 | 0 | ~1.3k | 223,643 (full); 192,264 (`unix/`) | None — official Go subrepo |
| `go.uber.org/goleak` v1.3.0 | Org (`uber-go`) | 29/~70 (prashantv, ~41%) | 2025-12-10 | v1.3.0 (Oct 2023) | 0 | 851 | None — Uber vanity domain → canonical |
| `github.com/stretchr/testify` v1.8.0 | Org (`stretchr`) | 129/~variable (ernesto-jimenez, multi-maintainer) | 2026-04-08 | v1.11.1 | 0 | ~26k | 10,096 | None — canonical |
| `github.com/davecgh/go-spew` v1.1.1 | User (`davecgh`) | 116/~130 (davecgh, ~89%) | 2018-08-30 | v1.1.1 (2018) | 0 | ~6.4k | 2,199 | None — canonical |
| `github.com/pmezard/go-difflib` v1.0.0 | User (`pmezard`) | 19/~24 (pmezard, ~79%) | 2018-12-26 | v1.0.0 (2016) | 0 | ~428 | 772 | None — canonical |
| `gopkg.in/yaml.v3` v3.0.1 | Org (`go-yaml`), **archived** | 244/~305 (niemeyer, ~80%) | 2025-04-01 (archive) | v3.0.1 (May 2022) | 0 (CVE fixed in v3.0.1) | ~7k | 11,285 | None — canonical (vanity → `github.com/go-yaml/yaml`) |

### Typosquatting Assessment

All seven module paths are the canonical, long-established import paths for
their projects. Vanity URLs (`go.uber.org/goleak`, `golang.org/x/sys`,
`gopkg.in/yaml.v3`) all redirect via the project's own `<meta name="go-import">`
tags to the corresponding GitHub repositories listed above. None of the names
are confusingly similar to other widely-used Go modules. **No typosquatting
risk identified.**

---

## Suggested Alternatives

The two manifest-only "high-risk" rows (`go-spew`, `go-difflib`, `yaml.v3`)
become Real Issues only if some future version of `go.uber.org/goleak`
introduces actual imports of them. Given goleak's stable, narrow surface
(goroutine leak detection in tests), this is unlikely. **No action recommended
today** beyond awareness.

For `golang.org/x/sys`, the recommended action is a routine version bump,
ideally bundled with the next `go.mod` housekeeping pass.

---

## Recommendations

1. **Bump `golang.org/x/sys`** to a current minor version (≥v0.30, ideally v0.43).
   Direct command: `go get golang.org/x/sys@latest && go mod tidy`. This is the
   only finding that touches gotail's compiled binary. (Note: bumping fsnotify
   to v1.10.1 — released today, 2026-05-04 — will likely pull a newer `x/sys`
   indirectly; consider both bumps in one PR.)

2. **Optional: bump `github.com/fsnotify/fsnotify` to v1.10.1.** The new release
   landed on the day of this scan. Diff is small; review release notes before
   merging since gotail leans on fsnotify behavior heavily (per `docs/v2-plan.md`).

3. **No action on `goleak`, `testify`, `go-spew`, `go-difflib`, `yaml.v3`.**
   They are not on any code path that ships or runs in gotail's tests.

4. **Add `govulncheck` to CI** to get continuous Go vulnerability database
   coverage. `go install golang.org/x/vuln/cmd/govulncheck@latest && govulncheck ./...`
   would catch any future advisory affecting the pinned versions, including
   ones that might appear in `go.sum`-only modules if their import status ever
   changes.

5. **Re-evaluate `goleak` if the v1.3.0 release stays frozen.** Master is
   active but the last tagged release is from October 2023. If a v1.4.0 has
   not shipped within the next 6 months, consider migrating to direct
   `go test -race` + `runtime.NumGoroutine()` assertions (already common in
   gotail's existing test patterns) to drop one transitive subtree entirely.

---

## Report Generated By

Supply Chain Risk Auditor Skill
Generated: 2026-05-04
