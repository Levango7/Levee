#!/usr/bin/env python3
"""Fail-closed liveness check for `path:line` anchors in the live ledgers.

Why this exists: the repository's own ledgers are what the next agent steers by,
and a dead anchor sends them to a line that no longer exists.  One round of
review on 2026-11-01 turned up five ledger statements that had drifted from the
code, and none of them could have been caught by CI — CI checks code, not
documentation.  This is the cheap half of that gap: it proves an anchor still
*points somewhere*, not that the sentence around it is still *true*.

The other half lives in `CLAIMS` below.  For the assertions someone has curated,
it also proves the *sentence* — "X landed in file F", "Y is not implemented" —
still matches the code, by requiring a named token to be present (or absent) in
the named file.  Liveness and truth are separate because they fail for different
reasons and send the reader to different fixes: a dead anchor means the ledger
points at nothing, a broken claim means the code moved out from under a sentence
that reads as current.

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
from typing import NamedTuple

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


class Claim(NamedTuple):
    """One ledger assertion about the code, checkable by a token search.

    `ledger` and `what` are for the failure message — which live doc makes the
    claim, and what it asserts.  They are not themselves checked: a claim is
    *declared* here, not scraped from prose (scraping would guess a keyword per
    sentence and get the wrong one in both directions).  `path` is the file the
    claim turns on; `keyword` is the token that must appear in it when
    `mode == "present"`, or must not appear when `mode == "absent"`.

    A NamedTuple rather than a dataclass on purpose: the guard is loaded by
    tests through `importlib` under a name that is not in `sys.modules`, and a
    frozen dataclass then crashes in `dataclasses._is_type` reaching for the
    module it cannot find.
    """

    ledger: str
    what: str
    path: str
    keyword: str
    mode: str = "present"


# The curated half.  Every row was true when it was added and is re-checked on
# every CI run; a row going red means the code changed under a sentence the
# ledgers still read as current.  This is deliberately a subset of the ledgers'
# assertions — extend it as more claims acquire a machine-checkable shape.
#
# `drift scan` is the one negative row: `docs/levee-api.md` §6 and
# `docs/product-roadmap.md` both record that it was never registered, and the
# gate now fails if a `scan` command ever appears without the ledgers moving.
CLAIMS: tuple[Claim, ...] = (
    Claim(
        "docs/product-roadmap.md",
        "rollback 后验证已装配到每次 run",
        "internal/wiring/run.go",
        "NewPostRollbackVerifier",
    ),
    Claim(
        "docs/product-roadmap.md",
        "registry 与库存 labels 的不一致可被检出",
        "internal/inventory/ownership.go",
        "LabelConflicts",
    ),
    Claim(
        "docs/product-roadmap.md",
        "变更日历已装进 serve",
        "cmd/levee/cmd_serve.go",
        "WithChangeCalendar",
    ),
    Claim(
        "docs/product-roadmap.md",
        "日历门禁在 plan 与 apply 两处前向路径消费",
        "internal/wiring/calendar_gate.go",
        "AssertNotFrozen",
    ),
    Claim(
        "docs/product-roadmap.md",
        "回滚路径刻意豁免日历门禁（结构测试钉住）",
        "internal/wiring/calendar_gate_test.go",
        "TestCalendarGateIsForwardOnly",
    ),
    Claim(
        "docs/product-roadmap.md",
        "approval 观察者改为扇出（不再互相挤掉）",
        "internal/grpc/change_service.go",
        "WithApprovalCreateObserver",
    ),
    Claim(
        "docs/product-roadmap.md",
        "LE103：未声明幂等的 undo 步骤发编译期提示",
        "internal/dsl/compile_warnings.go",
        "codeNonIdempotentUndo",
    ),
    Claim(
        "docs/product-roadmap.md",
        "human 门禁的 approver 已装进 serve",
        "cmd/levee/cmd_serve.go",
        "WithGateApprover",
    ),
    Claim(
        "docs/product-roadmap.md",
        "插件执行面接缝存在（notifier/module/channel 共用）",
        "cmd/levee/cmd_serve.go",
        "installPluginExecutionSurfaces",
    ),
    Claim(
        "docs/product-roadmap.md",
        "input 取值供给已实现（LE004 随之撤回）",
        "internal/dsl/inputs.go",
        "func ResolveInputs",
    ),
    Claim(
        "docs/levee-api.md",
        "drift detect 是已注册命令",
        "cmd/levee/cmd_drift.go",
        '"detect"',
    ),
    Claim(
        "docs/blocked-items.md",
        "通道插件会话池已交付",
        "internal/plugin/session.go",
        "startPluginSession",
    ),
    Claim(
        "docs/levee-api.md",
        "drift scan 从未注册（负断言）",
        "cmd/levee/cmd_drift.go",
        '"scan"',
        mode="absent",
    ),
    # --- second round: the anchors the roadmap already cites, made checkable ---
    Claim(
        "docs/product-roadmap.md",
        "window.max_concurrency 是 DSL 接受的字段（存在但无消费）",
        "internal/dsl/parser.go",
        "max_concurrency",
    ),
    Claim(
        "docs/product-roadmap.md",
        "runToPB 的 Label 改用 dsl.DisplayName（不再是整段源）",
        "internal/grpc/change_service.go",
        "dsl.DisplayName",
    ),
    Claim(
        "docs/product-roadmap.md",
        "buildEngineServeOptions 是引擎 serve 选项的生产构造点",
        "cmd/levee/cmd_serve.go",
        "buildEngineServeOptions",
    ),
    Claim(
        "docs/product-roadmap.md",
        "取库决策收敛到 openStoreFromConfig 一处",
        "cmd/levee/helpers.go",
        "openStoreFromConfig",
    ),
    Claim(
        "docs/product-roadmap.md",
        "窗口关闭以 ErrWindowClosed 作为 deliberate refusal 传出",
        "internal/dsl/window.go",
        "ErrWindowClosed",
    ),
    Claim(
        "docs/product-roadmap.md",
        "CheckWindowForPlan 有生产调用点（plan 期消费窗口）",
        "internal/wiring/calendar_gate.go",
        "CheckWindowForPlan",
    ),
    Claim(
        "docs/product-roadmap.md",
        "状态词表 owner 导出 StepStatusNames（供 docgen 生成 TS）",
        "internal/state/vocabulary.go",
        "StepStatusNames",
    ),
    Claim(
        "docs/product-roadmap.md",
        "集群接管时间线消费 TakeoverEventDTO",
        "web/src/views/ClusterView.vue",
        "TakeoverEventDTO",
    ),
    Claim(
        "docs/blocked-items.md",
        "插件破坏性动作声明已持久化（D-1b）",
        "internal/plugin/registry.go",
        "DestructiveActions",
    ),
    Claim(
        "docs/blocked-items.md",
        "运行期通道 upload 动词已落地（D-1c）",
        "internal/plugin/protocol.go",
        "upload",
    ),
    Claim(
        "docs/product-roadmap.md",
        "human 门禁的 quorum 字段在（MinApprovers / ExcludeInitiator）",
        "internal/verify/human_gate.go",
        "ExcludeInitiator",
    ),
    Claim(
        "docs/product-roadmap.md",
        "docgen 生成 web/src/types/vocabulary.ts",
        "internal/docgen/docgen.go",
        "vocabulary.ts",
    ),
)

# Airbag, same idea as MIN_ANCHORS: a floor near the current row count (20 of
# ~25) is a ratchet — dropping assertions is possible, but only on purpose.
MIN_CLAIMS = 20


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


def check_anchors(root: Path, docs: tuple[str, ...] = LIVE_DOCS) -> tuple[list[str], int]:
    """Liveness: Return (problems, anchors found). Raises ScopeError when it cannot scan."""
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


def check_claims(root: Path, claims: tuple[Claim, ...] = CLAIMS) -> tuple[list[str], int]:
    """Truth: Return (problems, claims checked). Raises ScopeError on a malformed row.

    A row that names an unknown mode or a doc outside LIVE_DOCS is a declaration
    bug, not a code drift — that is why it is a ScopeError (exit 2), not a found
    problem (exit 1): the reader must fix the table, not the code.
    """
    problems: list[str] = []
    for claim in claims:
        if claim.mode not in ("present", "absent"):
            raise ScopeError(f"claim mode must be 'present' or 'absent', got {claim.mode!r}: {claim.what}")
        if claim.ledger not in LIVE_DOCS:
            raise ScopeError(f"claim names a ledger outside LIVE_DOCS: {claim.ledger!r} ({claim.what})")
        target = root / claim.path
        if not target.is_file():
            problems.append(f"[claim] {claim.ledger}: {claim.what!r} — target file missing: {claim.path}")
            continue
        found = claim.keyword in target.read_text(encoding="utf-8", errors="replace")
        if claim.mode == "present" and not found:
            problems.append(
                f"[claim] {claim.ledger}: {claim.what!r} — {claim.path} no longer contains {claim.keyword!r}"
            )
        elif claim.mode == "absent" and found:
            problems.append(
                f"[claim] {claim.ledger}: {claim.what!r} — {claim.path} now contains {claim.keyword!r}, "
                "so the negative claim is stale"
            )
    if len(claims) < MIN_CLAIMS:
        problems.append(
            f"only {len(claims)} claims declared (floor is {MIN_CLAIMS}) — the claim table has stopped "
            "carrying the assertions it exists to protect"
        )
    return problems, len(claims)


def check(root: Path, docs: tuple[str, ...] = LIVE_DOCS) -> tuple[list[str], int]:
    """Both halves: (problems, anchors found). Raises ScopeError when it cannot scan."""
    anchor_problems, seen = check_anchors(root, docs)
    claim_problems, _ = check_claims(root)
    return anchor_problems + claim_problems, seen


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

    print(
        f"DOC ANCHOR CHECK PASSED: {seen} anchors across {len(LIVE_DOCS)} live docs still resolve; "
        f"{len(CLAIMS)} claims still hold"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
