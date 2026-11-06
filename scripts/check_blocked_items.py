#!/usr/bin/env python3
"""Fail-closed structure + count check for `docs/blocked-items.md`.

Why this exists: `blocked-items.md` groups stalled work into lettered categories
(A–F), and "how many items are in each category" only ever appeared as a
point-in-time snapshot in `docs/project-status-2026-11.md` (§12 wrote
`A5 / B0 / C3 / D3 / E3 / F1`). Nothing derived that from the file itself, so the
snapshot could go stale with no gate noticing, and the category structure was
unpoliced: an item filed under the wrong letter, a duplicated id, or a category
silently emptied of its table all read as normal text.

This gate parses the file and prints the per-category counts — one answer from
one source — and enforces four invariants:
  1. each `## <L>. …` section letter is unique;
  2. every table row's id (`L-n…`) carries its section's letter;
  3. ids are unique across the whole file;
  4. a section is either non-empty (has a table row) or says so explicitly
     (contains the emptiness note), so losing a table is a red, not silence.

Exit codes follow the sibling checks: 0 clean, 1 problems found, 2 the check
itself could not run (missing file, no category sections).

Usage:
    python scripts/check_blocked_items.py [--root .]
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

BLOCKED = "docs/blocked-items.md"

SECTION_RE = re.compile(r"^## ([A-Z])\.\s")
SEPARATOR_RE = re.compile(r"^\|[\s:-]*\|")
ID_RE = re.compile(r"^([A-Z])-(\d+[a-z]?)")
EMPTY_NOTE = "当前无"

# Airbag: the six categories currently hold 16 rows. A floor below that means a
# parser that stops seeing them cannot pass as "nothing to report".
MIN_ROWS = 12


class ScopeError(RuntimeError):
    """The check could not run at all (distinct from "found problems")."""


def first_cell(line: str) -> str:
    """The first cell of a table line, honouring escaped pipes."""
    parts = [p.strip() for p in re.split(r"(?<!\\)\|", line)]
    if parts and parts[0] == "":
        parts = parts[1:]
    return parts[0] if parts else ""


def check(root: Path, *, min_rows: int = MIN_ROWS) -> tuple[list[str], dict[str, int], int]:
    """Return (problems, counts per section, total rows). Raises ScopeError.

    `min_rows` is the vacuity floor; tests lower it for toy fixtures.
    """
    doc = root / BLOCKED
    if not doc.is_file():
        raise ScopeError(f"missing blocked-items file: {BLOCKED}")
    lines = doc.read_text(encoding="utf-8").split("\n")

    sections: dict[str, dict] = {}
    order: list[str] = []
    current: str | None = None
    separator_seen = False

    for lineno, line in enumerate(lines, 1):
        m = SECTION_RE.match(line)
        if m:
            letter = m.group(1)
            if letter not in sections:
                sections[letter] = {"rows": [], "text": []}
                order.append(letter)
            current = letter
            separator_seen = False
            continue
        if line.startswith("## "):
            current = None  # a non-category section (e.g. the delivered list)
            continue
        if current is None:
            continue
        sections[current]["text"].append(line)
        if SEPARATOR_RE.match(line):
            separator_seen = True
            continue
        if separator_seen and line.startswith("|"):
            sections[current]["rows"].append((lineno, first_cell(line)))

    if not order:
        raise ScopeError("no `## <L>.` category sections found in blocked-items")

    problems: list[str] = []
    counts: dict[str, int] = {}
    total = 0
    seen_ids: dict[str, int] = {}

    for letter in order:
        sec = sections[letter]
        rows = sec["rows"]
        counts[letter] = len(rows)
        total += len(rows)
        if not rows and EMPTY_NOTE not in "\n".join(sec["text"]):
            problems.append(
                f"section {letter} has no table rows and no `{EMPTY_NOTE}` note — "
                "a category must be either populated or explicitly empty"
            )
        for lineno, cell in rows:
            m = ID_RE.match(cell)
            if not m:
                problems.append(f"L{lineno}: row under section {letter} has no `{letter}-n` id: {cell!r}")
                continue
            item_letter, item_num = m.group(1), m.group(2)
            item_id = f"{item_letter}-{item_num}"
            if item_letter != letter:
                problems.append(
                    f"L{lineno}: id {item_id} filed under section {letter} — the letter must match its category"
                )
            if item_id in seen_ids:
                problems.append(f"L{lineno}: id {item_id} already used at L{seen_ids[item_id]}")
            else:
                seen_ids[item_id] = lineno

    if total < min_rows:
        problems.append(
            f"only {total} item rows found (floor is {min_rows}) — the scan has stopped seeing the items"
        )
    return problems, counts, total


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=".", help="repository root (default: cwd)")
    args = parser.parse_args(argv)
    root = Path(args.root).resolve()
    try:
        problems, counts, total = check(root)
    except ScopeError as exc:
        print(f"BLOCKED-ITEMS CHECK COULD NOT RUN: {exc}", file=sys.stderr)
        return 2
    if problems:
        print(f"BLOCKED-ITEMS CHECK FAILED: {len(problems)} problem(s)", file=sys.stderr)
        for problem in problems:
            print(f"  {problem}", file=sys.stderr)
        return 1
    summary = " ".join(f"{k}={counts[k]}" for k in sorted(counts))
    print(f"BLOCKED-ITEMS CHECK PASSED: {total} items — {summary}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
