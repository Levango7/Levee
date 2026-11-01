#!/usr/bin/env python3
"""Fail-closed liveness check for `path:line` anchors in the live ledgers.

Why this exists: the repository's own ledgers are what the next agent steers by,
and a dead anchor sends them to a line that no longer exists.  One round of
review on 2026-11-01 turned up five ledger statements that had drifted from the
code, and none of them could have been caught by CI — CI checks code, not
documentation.  This is the cheap half of that gap: it proves an anchor still
*points somewhere*, not that the sentence around it is still *true*.

Scope is declared, not discovered.  Scanning every markdown file under docs/
sounds better than it is: dated snapshots (docs/quality-hardening-2026-09.md,
docs/review-report.md, ...) are frozen records whose anchors are expected to rot,
and most "missing file" hits elsewhere are bare filenames (`store.go`) that match
a dozen packages.  Both classes would force churn on files whose whole point is
to stay as written.  So the live set below is explicit, anchors inside it must be
path-qualified, and widening the set is a one-line change — plus bumping
MIN_ANCHORS so an accidentally-empty scan cannot pass.

Deliberately NOT checked: fenced code blocks.  Samples inside ``` fences are
illustrations (`examples/workflows/x.yaml`) and treating them as references would
make the gate lie in the other direction.  That rule is enforced by a test with a
synthetic sample, so it cannot quietly disappear.

Exit codes follow internal/agentboard's convention: 0 clean, 1 problems found,
2 the check itself could not run (missing file, empty scope).  Reporting "every
anchor is dead" when the answer is "I could not read the docs" would send the
reader to the wrong fix.

Usage:
    python scripts/check_doc_anchors.py [--root .]
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

# The ledgers an agent actually steers by. Adding one here is the whole scope
# change; keep MIN_ANCHORS in step with it or the floor stops being meaningful.
LIVE_DOCS = (
    "docs/product-roadmap.md",
    "docs/blocked-items.md",
    "docs/levee-api.md",
    "docs/product-positioning.md",
    "AGENTS.md",
    "CONTRIBUTING.md",
)

# Anchors must carry at least one path separator: a bare `store.go:12` cannot be
# resolved unambiguously (the repo has a dozen), and guessing would produce the
# wrong verdict in both directions.
ANCHOR = re.compile(
    r"(?<![\w./-])"
    r"((?:[\w.-]+/)+[\w.-]+\.(?:go|ts|vue|proto|sql|md|yml|yaml|sh|json|toml|py|tpl))"
    r":(\d+)(?:-(\d+))?"
)

FENCE = re.compile(r"^\s*(```|~~~)")

# Airbag: the live set currently carries ~27 anchors. If a refactor silently
# stops matching them, "no problems" must not be the verdict.
MIN_ANCHORS = 20


class ScopeError(RuntimeError):
    """The scan could not be performed at all (distinct from "found problems")."""


def strip_fences(text: str) -> str:
    """Blank out fenced code blocks, keeping line numbers aligned."""
    out: list[str] = []
    in_fence = False
    for line in text.splitlines():
        if FENCE.match(line):
            in_fence = not in_fence
            out.append("")
            continue
        out.append("" if in_fence else line)
    return "\n".join(out)


def file_lines(path: Path) -> int:
    data = path.read_bytes()
    if not data:
        return 0
    return data.count(b"\n") + (0 if data.endswith(b"\n") else 1)


def scan_text(text: str, *, root: Path) -> list[str]:
    """Problems for one document's anchors ([] = every anchor still resolves)."""
    problems: list[str] = []
    for match in ANCHOR.finditer(strip_fences(text)):
        rel_path, start_s, end_s = match.group(1), match.group(2), match.group(3)
        start = int(start_s)
        end = int(end_s) if end_s else start
        target = root / rel_path
        if not target.is_file():
            problems.append(f"{match.group(0)}: no such file ({rel_path})")
            continue
        if end < start:
            problems.append(f"{match.group(0)}: range ends before it starts")
            continue
        total = file_lines(target)
        if end > total:
            problems.append(f"{match.group(0)}: line {end} is past EOF ({rel_path} has {total} lines)")
    return problems


def check(root: Path, docs: tuple[str, ...] = LIVE_DOCS) -> tuple[list[str], int]:
    """Return (problems, anchors found). Raises ScopeError when it cannot scan."""
    problems: list[str] = []
    seen = 0
    for rel in docs:
        doc = root / rel
        if not doc.is_file():
            raise ScopeError(f"declared live doc is missing: {rel}")
        text = doc.read_text(encoding="utf-8")
        seen += len(ANCHOR.findall(strip_fences(text)))
        for problem in scan_text(text, root=root):
            problems.append(f"{rel}: {problem}")
    if seen < MIN_ANCHORS:
        problems.append(
            f"only {seen} anchors found across {len(docs)} live docs (floor is {MIN_ANCHORS}) — "
            "the scan has stopped seeing the references it exists to protect"
        )
    return problems, seen


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=".", help="repository root (default: cwd)")
    args = parser.parse_args(argv)

    root = Path(args.root).resolve()
    try:
        problems, seen = check(root)
    except ScopeError as exc:
        print(f"DOC ANCHOR CHECK COULD NOT RUN: {exc}", file=sys.stderr)
        return 2

    if problems:
        print(f"DOC ANCHOR CHECK FAILED: {len(problems)} dead anchor(s) out of {seen}", file=sys.stderr)
        for problem in problems:
            print(f"  {problem}", file=sys.stderr)
        return 1

    print(f"DOC ANCHOR CHECK PASSED: {seen} anchors across {len(LIVE_DOCS)} live docs still resolve")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
