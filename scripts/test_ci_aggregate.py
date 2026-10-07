#!/usr/bin/env python3
"""test_ci_aggregate.py — CI 聚合门禁（`check (all jobs passed)`）的场景自测。

Why: 那个步骤是整个流水线的"总闸"，也是 `release.yml` 等的那颗 check。它的实现是一段
手写 bash：把 14 个 job 的结果拼成一个字符串再逐个裁决。今天它被两件事同时打中——
① runner 排队超时让 4 个 job 以 `cancelled` 结束，聚合报 "did not pass"，读起来像代码回归，
其实一条都没跑（实测：每个 job 都在排队 901 秒后被杀，steps=0）；② 这类"手抄清单 + 字符串
解析"的脚本本身就容易被改坏。所以聚合逻辑必须在 CI 里有自己的用例。

做法：从 `.github/workflows/ci.yml` 里**原样抽出**该步骤的 bash（只替换两处环境差异：
Actions 的 `${{ }}` 插值换成夹具，`jq` 这条本机/runner 都有的管道换成同形状的 echo），
再喂进伪造的 job 结果。测的是入库文本本身，不是另抄的一份。

运行：python -m unittest scripts/test_ci_aggregate.py（release-gate job 的 discover 会带上它）
"""

from __future__ import annotations

import os
import pathlib
import re
import subprocess
import unittest

REPO = pathlib.Path(__file__).resolve().parent.parent
YML = REPO / ".github" / "workflows" / "ci.yml"

# 与 ci.yml 里 needs(...) 清单同序；改那边必须同步改这里——而聚合步骤自带的漂移守卫
# 正是兜住这件事的那一道，本文件最后一个用例就在验它。
JOBS = ["release-gate", "vet", "lint", "frontend", "test", "integration", "proto",
        "docs", "smoke", "delivery", "build", "gosec", "govulncheck", "trivy"]


def extract_aggregate_script() -> str:
    """Return the aggregate step's bash body, with Actions-only syntax replaced."""
    text = YML.read_text(encoding="utf-8")
    lines = text.splitlines()
    anchor = next((i for i, l in enumerate(lines)
                   if "# The Actions engine interpolates" in l), None)
    assert anchor is not None, "aggregate step marker vanished from ci.yml"
    assert lines[anchor - 1].rstrip() == "        run: |", lines[anchor - 1]
    body_lines = []
    for line in lines[anchor:]:
        if line.strip() and not line.startswith(" " * 10):
            break
        body_lines.append(line[10:])
    body = "\n".join(body_lines)

    # ${{ needs.X.result }} -> RESULT_X （Actions 在进 bash 前就会插值，本地没有插值引擎）
    body, n_tok = re.subn(r"\$\{\{ needs\.([\w-]+)\.result \}\}",
                          lambda m: f"RESULT_{m.group(1)}", body)
    assert n_tok == len(JOBS), f"expected {len(JOBS)} result interpolations, got {n_tok}"
    # jq 在 Windows 开发机上不存在；换成同形状的 echo，tr/sort 语义保持不变。
    fixture = " ".join(sorted(JOBS))
    body, n_jq = re.subn(r"\$\(echo '[^']*' \| jq -r 'keys\[\]'([^)]*)\)",
                         lambda m: f"$(echo '{fixture}'{m.group(1)})", body)
    assert n_jq == 1, f"jq pipeline not found (matched {n_jq})"
    return body


SCRIPT = extract_aggregate_script()


class AggregateStepTests(unittest.TestCase):
    def run_case(self, results: dict[str, str], drop: tuple[str, ...] = ()):
        script = SCRIPT
        for job in JOBS:
            if job in drop:
                script = re.sub(rf" ?\b{job}:RESULT_{job}", "", script, count=1)
            else:
                script = script.replace(f"RESULT_{job}", results.get(job, "success"))
        # 写在仓库根、用相对路径交给 bash：Windows 的 tempfile 绝对路径（C:\Users\...）
        # 对 MSYS bash 没有意义，会报 "No such file or directory"。相对路径在 Linux CI 上
        # 同样成立，pid 后缀避免并发跑时互相覆盖。
        path = pathlib.Path(f".agg_case_{os.getpid()}.sh")
        path.write_text(script + "\n", encoding="utf-8", newline="\n")
        try:
            proc = subprocess.run(["bash", path.name], capture_output=True, text=True,
                                  encoding="utf-8", errors="replace")
        finally:
            path.unlink(missing_ok=True)
        return proc.returncode, proc.stdout + proc.stderr

    def test_all_success_is_green(self):
        rc, out = self.run_case({})
        self.assertEqual(rc, 0, out)
        self.assertIn("All required jobs passed", out)
        self.assertNotIn("::error::", out)

    def test_failure_is_reported_as_failure(self):
        rc, out = self.run_case({"lint": "failure"})
        self.assertEqual(rc, 1, out)
        self.assertIn("FAILED (failure)", out)
        self.assertIn("at least one required job failed", out)
        self.assertNotIn("UNVERIFIED", out)

    def test_cancelled_is_unverified_not_failed(self):
        """排队超时的 job 结论是 cancelled：必须说成"没有判定结果"，不能说成"没通过"。

        2026-10-06 实测：runner 饥饿时 4 个 job 各在排队 901s 后被杀、steps=0，聚合旧文案
        写成 "did not pass"，把人引向"找一次不存在的回归"。
        """
        rc, out = self.run_case({"lint": "cancelled", "docs": "skipped"})
        self.assertEqual(rc, 1, out)
        self.assertIn("UNVERIFIED, not failed", out)
        self.assertIn("jobs with no verdict: lint docs", out)
        self.assertIn("Re-run this workflow", out)

    def test_timeout_and_neutral_buckets_are_unverified_too(self):
        rc, out = self.run_case({"trivy": "timed_out", "vet": "neutral", "gosec": "stale"})
        self.assertEqual(rc, 1, out)
        self.assertIn("jobs with no verdict: vet gosec trivy", out)

    def test_list_drift_from_needs_is_red(self):
        """聚合清单少抄一项（历史上真发生过：docs 在 needs 里却不在清单里）必须变红。"""
        rc, out = self.run_case({}, drop=("docs",))
        self.assertEqual(rc, 1, out)
        self.assertIn("drifted from needs()", out)


if __name__ == "__main__":
    unittest.main()
