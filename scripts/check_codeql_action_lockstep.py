#!/usr/bin/env python3
"""codeql-action 的所有引用必须是同一个版本——本仓为此已经吃过一次亏。

**为什么需要这道守卫。** 2026-10-19，Dependabot 为 `github/codeql-action` 开了 **4 个**独立的
PR（init / autobuild / analyze / upload-sarif 各一个），因为本仓把 action 按 SHA 钉死
（`uses: github/codeql-action/init@<sha>`），而 `.github/dependabot.yml` 里当时**没有 groups**，
于是每个被引用的 action 各自变成一个 PR。四个 PR 单独看都是对的，**合任意一个都是红的**：

    Loaded a configuration file for version '4.38.3', but running version '4.39.0'.
    All steps in a workflow that use `github/codeql-action` must use the same version to work correctly.

——CodeQL 的报错原文。这不是 flaky，是配置形状错误：4 条引用各钉各的 SHA（3 条 v4.38.3、
1 条 v4.38.2），任何一次只升一条的更新都会让 workflow 立刻红。

**这道守卫钉住的不变量**：仓库里所有 `github/codeql-action/*` 引用
（1）钉同一个 commit SHA，（2）注释里标同一个版本号。
只升一条 → 立刻红，且报错说清是哪两条不一致；全升 → 绿。

**和 dependabot groups 的分工。** `.github/dependabot.yml` 里的 groups 让 Dependabot
**下次**自动把 4 条一起升；但 groups 管不到人手动改 workflow 的场景，所以两处都要有：
groups 减少来源，守卫钉住结果。

用法：`python scripts/check_codeql_action_lockstep.py`（在仓库根目录跑），有问题时退出码 1。
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
WORKFLOW_DIR = REPO / ".github" / "workflows"

# `uses: github/codeql-action/init@<sha>  # v4.38.3`
# 注释可省（钉 SHA 的写法本来就允许没有注释），但只要写了就必须和别处一致。
USES_RE = re.compile(
    r"uses:\s*github/codeql-action/(?P<action>[A-Za-z0-9_-]+)@(?P<sha>[0-9a-f]{7,40})"
    r"(?:\s*#\s*v?(?P<version>[0-9][0-9A-Za-z.\-]*))?"
)


def collect_uses(text: str) -> list[tuple[str, str, str | None]]:
    """从一段 workflow 文本里取出 (action, sha, version_comment) 列表。"""
    found: list[tuple[str, str, str | None]] = []
    for m in USES_RE.finditer(text):
        found.append((m.group("action"), m.group("sha"), m.group("version")))
    return found


def check_lockstep(workflows: dict[str, str]) -> list[str]:
    """返回问题清单；空列表 = 全部一致。

    `workflows` 是 {文件名: 文本}。分开收 SHA 和版本号两条不变量：
    混用两个不同 SHA 的引用必然是坏的，注释版本号不同步也是坏的
    （人看注释判断版本，注释骗人比机器骗人更难发现）。
    """
    problems: list[str] = []
    entries: list[tuple[str, str, str, str | None]] = []  # (file, action, sha, version)

    for name, text in sorted(workflows.items()):
        for action, sha, version in collect_uses(text):
            entries.append((name, action, sha, version))

    if not entries:
        problems.append(
            "仓库里一处 `github/codeql-action/*` 引用都没有——这条守卫要么读错了路径，"
            "要么 workflow 被人删干净了。两种都得让人看见，不许静默通过。"
        )
        return problems

    shas = {sha for _, _, sha, _ in entries}
    if len(shas) > 1:
        detail = "\n".join(f"  {f}: {a}@{sha}" + (f"  # v{v}" if v else "") for f, a, sha, v in entries)
        problems.append(
            "codeql-action 的引用钉了多个不同的 commit SHA："
            f" {', '.join(sorted(shas))}\n"
            "CodeQL 要求一个 workflow 里所有 codeql-action 步骤用同一版本"
            "（报错原文：All steps in a workflow that use `github/codeql-action` must use the same version）。\n"
            f"{detail}\n"
            "  修法：把上面每一条都升/降到同一个 SHA，然后一起推。"
        )

    versions = {v for _, _, _, v in entries if v is not None}
    if len(versions) > 1:
        detail = "\n".join(
            f"  {f}: {a}@{sha}" + (f"  # v{v}" if v else "  (无版本注释)") for f, a, sha, v in entries
        )
        problems.append(
            f"codeql-action 的版本注释不一致： {', '.join(sorted(versions))}\n"
            "注释里的版本号是人读的那一份，它必须和实际钉的 SHA 同步。\n"
            f"{detail}"
        )

    return problems


def main() -> int:
    if not WORKFLOW_DIR.is_dir():
        print(f"::error::找不到 workflow 目录：{WORKFLOW_DIR}")
        return 1

    workflows = {p.name: p.read_text(encoding="utf-8") for p in sorted(WORKFLOW_DIR.glob("*.yml"))}
    problems = check_lockstep(workflows)
    if problems:
        print("::error::codeql-action 版本不一致：")
        for p in problems:
            print(p)
        return 1

    total = sum(len(collect_uses(t)) for t in workflows.values())
    print(f"codeql-action lockstep OK：{total} 处引用，同一 SHA / 同一版本。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
