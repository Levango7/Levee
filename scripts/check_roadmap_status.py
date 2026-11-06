#!/usr/bin/env python3
"""Fail-closed check for the canonical status column of `docs/product-roadmap.md`.

Why this exists: the roadmap is the page a reader steers by, and its rows used
to encode "done / open / decided" in at least five incompatible ways — a `✅`
in the priority cell, a `✅` in the trailing column, a `~~struck-through~~`
title, the words `已修` / `已定案并落地` / `已接线` in the priority cell, and a
bare `已完成` in the body.  A review round on 2026-11-05 found that counting
"how many rows are still open" gave a different answer under each reading
(11 / 1 / 1, and 6 by hand), and none of them was wrong — the criterion was not
self-evident.  Worse, rows that do not start with `| P` (the identity row and
the three operations rows) were invisible to every `^| P` count.

The fix is a single status column with one vocabulary, and this gate is what
keeps it single: every data row of every item table must carry exactly one
canonical status, so a fifth spelling cannot reappear, and the open count is
whatever this script says it is.

Vocabulary (single source is `STATUS_VOCAB` / `BLOCKED_PREFIX` below):
    closed            the item is done, or decided against (no action pending)
    open              the item is not done (including "watching: not reproduced")
    decided           a product decision was taken and its outcome recorded
    blocked-on:<who>  waiting on a named external actor (a person, a credential)

Shape notes:
  * A logical row may span several physical lines (a cell with newlines); the
    parser joins them, and every data row must then match its header's cell
    count.  A ragged row is a real defect, not a style choice: in GFM a row
    with MORE cells than its header drops the overflow (which is the status
    column), and one with FEWER leaves it blank — either way the status does
    not render.  The 2026-11-05 pass that added the column found five such rows
    and fixed them (an unescaped `|` inside a code span, a title merged into
    the priority cell, two over-long rows, and one four-line row).
  * An unescaped `|` inside a cell (e.g. in a code span) must be written `\|`;
    the cell splitter honours the escape.

Exit codes follow the sibling checks: 0 clean, 1 problems found, 2 the check
itself could not run (missing file, no item tables).

Usage:
    python scripts/check_roadmap_status.py [--root .]
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

ROADMAP = "docs/product-roadmap.md"

STATUS_VOCAB = ("closed", "open", "decided")
BLOCKED_PREFIX = "blocked-on:"
STATUS_HEADER_CELL = "状态"

# The header cells that mark an *item* table (the ones whose rows carry a status).
# The document's first table (`| 元信息项 | 内容 |`) is metadata, not items.
ITEM_TABLE_PRIORITY_CELL = "优先级"
ITEM_TABLE_OPS_HEADER = ("项", "目标")

# Airbag: the four item tables currently hold 53 rows. A floor close to the real
# count (50 of 53) means a parser that silently drops a whole table goes red —
# which is exactly how the operations table went uncounted for so long.
MIN_ROWS = 50

PIPE = re.compile(r"(?<!\\)\|")
SEPARATOR = re.compile(r"^:?-{2,}:?$")


class ScopeError(RuntimeError):
    """The check could not run at all (distinct from "found problems")."""


def logical_rows(lines: list[str]) -> list[tuple[int, int]]:
    """(start, end) 0-based inclusive line indices, one entry per logical row.

    A row begins on a `|`-leading line and continues while the following line is
    neither blank nor another `|`-leading line.
    """
    rows: list[tuple[int, int]] = []
    i = 0
    while i < len(lines):
        if lines[i].startswith("|"):
            j = i
            while j + 1 < len(lines) and lines[j + 1].strip() != "" and not lines[j + 1].startswith("|"):
                j += 1
            rows.append((i, j))
            i = j + 1
        else:
            i += 1
    return rows


def cells(text: str) -> list[str]:
    """The cells of a logical row, in order (handles escaped pipes)."""
    parts = [p.strip() for p in PIPE.split(text)]
    # A well-formed row is `| c1 | c2 |` → parts[0] and parts[-1] are empty.
    if parts and parts[0] == "":
        parts = parts[1:]
    if parts and parts[-1] == "":
        parts = parts[:-1]
    return parts


def is_separator(cell_list: list[str]) -> bool:
    return bool(cell_list) and all(SEPARATOR.match(c or "") for c in cell_list)


def is_ops_table(header: list[str]) -> bool:
    """The operations table `| 项 | 目标 | … |`.

    Its header has no `优先级`, which is exactly why a `^| P` scan never saw its
    rows; the prefix (not equality) is used so adding the `状态` column does not
    make the table disappear from the scan.
    """
    return header[: len(ITEM_TABLE_OPS_HEADER)] == list(ITEM_TABLE_OPS_HEADER)


def is_item_table(header: list[str]) -> bool:
    return ITEM_TABLE_PRIORITY_CELL in header or is_ops_table(header)


def status_ok(value: str) -> bool:
    if value in STATUS_VOCAB:
        return True
    if value.startswith(BLOCKED_PREFIX):
        return len(value) > len(BLOCKED_PREFIX)  # a named actor is required
    return False


def check(root: Path, *, min_rows: int = MIN_ROWS) -> tuple[list[str], dict[str, int], int]:
    """Return (problems, counts, total data rows). Raises ScopeError.

    `min_rows` is the vacuity floor; tests lower it for toy fixtures, production
    uses the module default.
    """
    doc = root / ROADMAP
    if not doc.is_file():
        raise ScopeError(f"missing roadmap: {ROADMAP}")
    lines = doc.read_text(encoding="utf-8").split("\n")
    rows = logical_rows(lines)

    problems: list[str] = []
    counts: dict[str, int] = {}
    total = 0
    current_header: list[str] | None = None
    seen_item_table = False
    seen_ops_table = False

    for idx, (start, end) in enumerate(rows):
        text = "\n".join(lines[start : end + 1])
        cell_list = cells(text)

        # A header is a row whose *next* row is a separator.
        nxt = rows[idx + 1] if idx + 1 < len(rows) else None
        if nxt is not None:
            nxt_text = "\n".join(lines[nxt[0] : nxt[1] + 1])
            if is_separator(cells(nxt_text)):
                current_header = cell_list
                if is_item_table(cell_list):
                    seen_item_table = True
                    if is_ops_table(cell_list):
                        seen_ops_table = True
                    if STATUS_HEADER_CELL not in cell_list:
                        problems.append(
                            f"L{start + 1}: item-table header lacks a `{STATUS_HEADER_CELL}` column: {cell_list}"
                        )
                continue
        if is_separator(cell_list):
            continue

        # A data row, but only inside an item table.
        if current_header is None or not is_item_table(current_header):
            continue

        total += 1
        if not cell_list:
            problems.append(f"L{start + 1}: empty data row")
            continue
        if len(cell_list) != len(current_header or []):
            problems.append(
                f"L{start + 1}: {len(cell_list)} cells, but the table header has {len(current_header or [])} — "
                "a ragged row (an unescaped `|`, a missing cell, or a wrapped line) does not render its status"
            )
            continue
        status = cell_list[-1]
        if not status_ok(status):
            problems.append(
                f"L{start + 1}: last cell is not a canonical status: {status!r} "
                f"(want one of {STATUS_VOCAB}, or {BLOCKED_PREFIX}<who>)"
            )
            continue
        counts[status] = counts.get(status, 0) + 1

    if not seen_item_table:
        raise ScopeError("no item tables found in the roadmap — the parser lost its target")
    if not seen_ops_table:
        raise ScopeError(
            "the operations table (`| 项 | 目标 | … |`) was not recognized — it is the table whose rows "
            "no `^| P` scan could see, so losing it silently is the failure this check exists to catch"
        )
    if total < min_rows:
        problems.append(
            f"only {total} data rows found (floor is {min_rows}) — the scan has stopped seeing the item rows"
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
        print(f"ROADMAP STATUS CHECK COULD NOT RUN: {exc}", file=sys.stderr)
        return 2
    if problems:
        print(f"ROADMAP STATUS CHECK FAILED: {len(problems)} problem(s) across {total} rows", file=sys.stderr)
        for problem in problems:
            print(f"  {problem}", file=sys.stderr)
        return 1
    summary = ", ".join(f"{k}={counts[k]}" for k in sorted(counts))
    print(f"ROADMAP STATUS CHECK PASSED: {total} rows, {summary}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
