#!/usr/bin/env python3
"""Render the merged Semgrep SARIF as a deduplicated, severity-sorted markdown summary."""

import json
import sys
from collections import defaultdict
from pathlib import Path


SEVERITY_ORDER = {"error": 0, "warning": 1, "note": 2, "none": 3, "unknown": 4}
SEVERITY_LABEL = {
    "error": "HIGH",
    "warning": "MEDIUM",
    "note": "LOW",
    "none": "INFO",
    "unknown": "UNKNOWN",
}


def clean_rule_id(rid: str) -> str:
    """Strip the local-clone path prefix Semgrep injects for git-cloned rulesets."""
    marker = "repos."
    idx = rid.find(marker)
    if idx >= 0:
        return rid[idx + len(marker) :]
    return rid


def short_path(uri: str, project_root: str) -> str:
    if uri.startswith(project_root):
        return uri[len(project_root) :].lstrip("/")
    return uri


def main(sarif_path: str, out_path: str, project_root: str) -> None:
    sarif = json.loads(Path(sarif_path).read_text())

    # rule_id -> rule definition
    rules: dict[str, dict] = {}
    tools: list[str] = []
    for run in sarif.get("runs", []):
        driver = run.get("tool", {}).get("driver", {})
        tools.append(f"{driver.get('name', 'unknown')} {driver.get('semanticVersion', '')}".strip())
        for rule in driver.get("rules", []):
            rules[rule["id"]] = rule

    # Collect findings
    findings: list[dict] = []
    seen_keys: set[tuple] = set()

    for run in sarif.get("runs", []):
        for result in run.get("results", []):
            rule_id = result.get("ruleId", "unknown")
            rule = rules.get(rule_id, {})
            level = (
                result.get("level")
                or rule.get("defaultConfiguration", {}).get("level")
                or "unknown"
            )
            message = result.get("message", {}).get("text", "")
            loc = (result.get("locations") or [{}])[0].get("physicalLocation", {})
            uri = loc.get("artifactLocation", {}).get("uri", "")
            region = loc.get("region", {})
            start_line = region.get("startLine")
            end_line = region.get("endLine", start_line)
            snippet = region.get("snippet", {}).get("text", "").rstrip()

            dedup_key = (clean_rule_id(rule_id), uri, start_line, snippet)
            if dedup_key in seen_keys:
                continue
            seen_keys.add(dedup_key)

            props = rule.get("properties", {}) or {}
            tags = props.get("tags", []) or []
            help_uri = rule.get("helpUri", "")
            short_desc = rule.get("shortDescription", {}).get("text", "")
            full_desc = rule.get("fullDescription", {}).get("text", "")

            findings.append(
                {
                    "rule_id": clean_rule_id(rule_id),
                    "raw_rule_id": rule_id,
                    "level": level,
                    "severity_label": SEVERITY_LABEL.get(level, level.upper()),
                    "message": message,
                    "file": short_path(uri, project_root),
                    "start_line": start_line,
                    "end_line": end_line,
                    "snippet": snippet,
                    "tags": tags,
                    "precision": props.get("precision", ""),
                    "help_uri": help_uri,
                    "short_description": short_desc,
                    "full_description": full_desc,
                }
            )

    findings.sort(
        key=lambda f: (
            SEVERITY_ORDER.get(f["level"], 99),
            f["rule_id"],
            f["file"],
            f["start_line"] or 0,
        )
    )

    # Group counts
    by_severity: dict[str, int] = defaultdict(int)
    by_rule: dict[str, int] = defaultdict(int)
    for f in findings:
        by_severity[f["severity_label"]] += 1
        by_rule[f["rule_id"]] += 1

    # Render markdown
    lines: list[str] = []
    lines.append("# Semgrep findings — gotail")
    lines.append("")
    lines.append("- **Conducted:** 2026-05-04")
    lines.append("- **Tool:** " + ", ".join(sorted(set(tools))))
    lines.append("- **Mode:** Run all (no severity filter)")
    lines.append("- **Engine:** OSS")
    lines.append(f"- **Source SARIF:** `{Path(sarif_path).relative_to(project_root)}`")
    lines.append(
        "- **Rulesets:** p/golang, p/security-audit, p/owasp-top-ten, p/secrets, "
        "trailofbits/semgrep-rules (go/), dgryski/semgrep-go"
    )
    lines.append(f"- **Total findings:** {len(findings)} (deduplicated by rule + location + snippet)")
    lines.append("")

    lines.append("## Severity histogram")
    lines.append("")
    lines.append("| Severity | Count |")
    lines.append("| --- | --- |")
    for label in ("HIGH", "MEDIUM", "LOW", "INFO", "UNKNOWN"):
        if by_severity.get(label):
            lines.append(f"| {label} | {by_severity[label]} |")
    lines.append("")

    lines.append("## Findings by rule")
    lines.append("")
    lines.append("| Rule | Count | Severity |")
    lines.append("| --- | --- | --- |")
    rule_first_sev: dict[str, str] = {}
    for f in findings:
        rule_first_sev.setdefault(f["rule_id"], f["severity_label"])
    for rid in sorted(by_rule, key=lambda r: (SEVERITY_ORDER.get(
        next((f["level"] for f in findings if f["rule_id"] == r), "unknown"), 99
    ), r)):
        lines.append(f"| `{rid}` | {by_rule[rid]} | {rule_first_sev[rid]} |")
    lines.append("")

    lines.append("## Findings (sorted by severity, then rule, then file)")
    lines.append("")

    for i, f in enumerate(findings, 1):
        lines.append(f"### {i}. [{f['severity_label']}] `{f['rule_id']}`")
        lines.append("")
        lines.append(
            f"- **Location:** `{f['file']}:{f['start_line']}`"
            + (f"-{f['end_line']}" if f["end_line"] and f["end_line"] != f["start_line"] else "")
        )
        if f["precision"]:
            lines.append(f"- **Precision:** {f['precision']}")
        if f["tags"]:
            lines.append(f"- **Tags:** {', '.join(f['tags'])}")
        if f["help_uri"]:
            lines.append(f"- **Help:** {f['help_uri']}")
        lines.append("")
        if f["message"]:
            lines.append(f"**Message:** {f['message']}")
            lines.append("")
        if f["snippet"]:
            lines.append("```go")
            lines.append(f["snippet"])
            lines.append("```")
            lines.append("")

    lines.append("---")
    lines.append("")
    lines.append("## Notes")
    lines.append("")
    lines.append(
        "- `level` on each SARIF result was `null` in the merged file; severity was joined from "
        "each rule's `defaultConfiguration.level` (`error` -> HIGH, `warning` -> MEDIUM, `note` -> LOW)."
    )
    lines.append(
        "- Rule IDs from locally-cloned third-party rulesets had a `docs.reviews.semgrep-2026-05-04.repos.<repo>.` "
        "prefix injected by Semgrep (the path used as `--config`). The summary strips it for readability."
    )
    lines.append(
        "- Triage these for true vs. false positive — Semgrep's pattern matching is "
        "syntactic, so use of `unsafe`, `math/rand`, etc. is flagged regardless of context."
    )
    lines.append("")

    Path(out_path).write_text("\n".join(lines))
    print(f"Wrote {len(findings)} findings to {out_path}")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2], sys.argv[3])
