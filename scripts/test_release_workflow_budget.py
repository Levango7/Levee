"""release.yml 的等待预算必须自洽——这是切 tag 之前就该发现的那类错。

`ci-gate` job 里跑 `require_ci_release.py`：脚本会自己轮询，等 tag 提交那一笔的
`check (all jobs passed)` 变绿（预算 = CI_TIMEOUT_SECONDS）；而 job 本身还有一层
GitHub 的 `timeout-minutes`。**两个数一旦反了，先到的那个才算真预算**——脚本那段等待
就成了永远不可能生效的死配置。

本仓实测踩过：原配置是 job `timeout-minutes: 30`(=1800s) 配 `CI_TIMEOUT_SECONDS:
'3600'`(=3600s)，而 2026-10-07 那天 master 一次完整绿 run 用了 **58.5 分钟**
（`8eced6eb`，07:53:58Z → 08:52:27Z，来自 Actions API 的 run_started_at/updated_at）。
按那组配置，谁在那天切 tag 都会得到一条**必然**在 30 分钟处被杀的 Release run——
不是"可能失败"，是算术上不可能成功。

所以这里钉的不是"数字够不够大"，而是三条可复现的关系：job 预算 > 脚本预算 >
实测最慢 run，且发布 job 确实挂在这道门后面。
"""

import re
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
RELEASE_WF = REPO / ".github" / "workflows" / "release.yml"
CI_WF = REPO / ".github" / "workflows" / "ci.yml"
GATE_SCRIPT = REPO / "scripts" / "require_ci_release.py"

# 2026-10-07 实测：master 一次完整绿 run 的墙钟时间（含所有 job 排队）。
# 这是"预算必须超过它"的外界事实依据，不是估的。
MEASURED_MASTER_RUN_SECONDS = 58 * 60 + 30  # 3510s ≈ 58.5 分钟


def _job_block(text: str, job: str) -> str:
    """Slice out one job's YAML block (2-space key to next 2-space key)."""
    m = re.search(rf"^  {re.escape(job)}:\n", text, re.M)
    assert m, f"{job!r} job not found"
    nxt = re.search(r"^  [A-Za-z][A-Za-z0-9_-]*:\n", text[m.end():], re.M)
    end = m.end() + (nxt.start() if nxt else len(text[m.end():]))
    return text[m.start():end]


def _job_timeout_seconds(text: str, job: str) -> int:
    block = _job_block(text, job)
    m = re.search(r"^\s*timeout-minutes:\s*(\d+)", block, re.M)
    assert m, f"job {job!r} has no timeout-minutes"
    return int(m.group(1)) * 60


def _env_int(text: str, name: str) -> int:
    m = re.search(rf"{name}:\s*'(\d+)'", text)
    assert m, f"{name} not set in release.yml"
    return int(m.group(1))


class ReleaseWorkflowBudget(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.release = RELEASE_WF.read_text(encoding="utf-8")
        cls.script = GATE_SCRIPT.read_text(encoding="utf-8")

    def test_job_budget_strictly_exceeds_script_wait(self):
        """job 被杀之前，脚本必须有机会自己等到绿——否则等待预算是死配置。"""
        job_s = _job_timeout_seconds(self.release, "ci-gate")
        script_s = _env_int(self.release, "CI_TIMEOUT_SECONDS")
        self.assertGreater(
            job_s, script_s,
            f"ci-gate timeout-minutes ({job_s}s) must exceed CI_TIMEOUT_SECONDS "
            f"({script_s}s); the smaller one is the real budget and the script's "
            "patience can never fire")
        self.assertGreaterEqual(job_s - script_s, 300,
                                "leave at least 5 minutes of slack for checkout and the run itself")

    def test_script_default_fallback_also_fits_under_job_budget(self):
        """env 行被删时，脚本的内置默认值同样要落在 job 预算之内。"""
        job_s = _job_timeout_seconds(self.release, "ci-gate")
        m = re.search(r'os\.environ\.get\("CI_TIMEOUT_SECONDS",\s*"(\d+)"\)', self.script)
        assert m, "require_ci_release.py no longer reads CI_TIMEOUT_SECONDS with a numeric default"
        default_s = int(m.group(1))
        self.assertGreater(job_s, default_s,
                           f"job budget {job_s}s must exceed the script's built-in default {default_s}s")

    def test_budgets_cover_the_measured_slowest_master_run(self):
        """等待预算必须大于"master 跑完整绿实测要多久"，否则这道门在池紧张时必然超时。"""
        job_s = _job_timeout_seconds(self.release, "ci-gate")
        script_s = _env_int(self.release, "CI_TIMEOUT_SECONDS")
        self.assertGreater(script_s, MEASURED_MASTER_RUN_SECONDS,
                           f"CI_TIMEOUT_SECONDS={script_s}s does not cover the measured "
                           f"{MEASURED_MASTER_RUN_SECONDS}s master run")
        self.assertGreater(job_s, MEASURED_MASTER_RUN_SECONDS)

    def test_poll_interval_actually_polls(self):
        """预算除以轮询间隔必须留下足够多次轮询——否则一次排队抖动就判死。"""
        script_s = _env_int(self.release, "CI_TIMEOUT_SECONDS")
        poll_s = _env_int(self.release, "CI_POLL_SECONDS")
        self.assertGreater(poll_s, 0)
        self.assertLessEqual(poll_s, 60, "polling faster than 15s/60s just burns API quota")
        self.assertGreaterEqual(script_s // poll_s, 60,
                                f"only {script_s // poll_s} polls within the wait budget")

    def test_publish_job_is_gated_on_the_ci_check(self):
        """goreleaser 必须挂在这道门后面：门禁存在但不拦住发布，等于没有。"""
        block = _job_block(self.release, "goreleaser")
        self.assertRegex(block, r"needs:\s*ci-gate",
                         "goreleaser must depend on ci-gate, otherwise a tag can publish unverified code")

    def test_new_python_suite_is_named_in_ci(self):
        """本文件必须出现在 release-gate 的点名清单里。

        discover 会自动收集新套件，但"收集到"和"被验证收集到"是两件事：清单不点名，
        这个套件被改名/删除时不会有任何东西变红（空收集仍以 `Ran 0 tests / OK` 通过）。
        """
        ci = CI_WF.read_text(encoding="utf-8")
        self.assertIn("test_release_workflow_budget", ci,
                      "add this suite to the release-gate named-suite list in ci.yml")


if __name__ == "__main__":
    unittest.main()
