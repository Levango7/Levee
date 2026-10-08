"""release-gate 的单元测试步骤必须让失败真的失败。

ci.yml 的 release-gate 用 `python -m unittest discover … | tee /tmp/py.log` 跑
`scripts/` 下的门禁自检，再按点名清单 grep 日志确认每个套件都被收集到。点名清单
堵的是"discovery 空收集"（`Ran 0 tests / OK` 也能过），但**管道的退出码属于 `tee`**：
runner 的默认 shell 是 `bash -e`（没有 pipefail），于是 unittest 报 `FAILED` 时这一步
照样 exit 0——门禁只剩"套件名还在不在日志里"这一条判据，断言失败全部静默。

本仓实测（2026-10-09，PR #101）：CHANGELOG 结构被 `12c5b25` 改坏（`## [v1.20.0]`
出现两次，`test_changelog_structure` 报 `[] != ['v1.20.0 at lines 159 and 217']`），
而 release-gate 在 CI 上是**绿**的。复现：

    bash -e -c 'python -m unittest discover -s scripts -p "test_*.py" -v 2>&1 | tee /tmp/py.log'
    → STEP EXIT=0，PIPESTATUS 为 unittest=1 tee=0

同一命令加 `set -o pipefail` 后 exit 1。所以这里钉的不是"步骤还在不在"，而是三条
可复现的性质：那个步骤必须启用 pipefail；必须仍在把 unittest 输出 tee 进日志
（形状变了要人来重推，而不是让断言对着新形状空转）；且不得出现 `|| true` 这类把
退出码吞掉的写法。检测器对合成的坏/好样本各跑一遍——只会给"当前恰好干净"的仓库
打勾的检查，证明不了它看得见损坏。
"""

import re
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
CI_WF = REPO / ".github" / "workflows" / "ci.yml"
SUITE_NAME = "test_release_gate_step"

PIPEFAIL_RE = re.compile(
    r"set\s+-o\s+pipefail|set\s+-eo\s+pipefail|bash\s+-eo\s+pipefail|shell:\s*bash.*pipefail"
)


def _norm(text: str) -> str:
    """CRLF 工作副本必须与 LF 检出行为一致。

    `765d2c9` 补的守卫踩过这个坑：CRLF 下 `^…$` 静默匹配成空集，测试以"块形状变了"
    通过——守卫在那台机器上等于不存在。这里先归一，检测器在两种行尾下都做同一件事。
    """
    return text.replace("\r\n", "\n")


def _job_block(text: str, job: str) -> str:
    """Slice out one job's YAML block (2-space key to next 2-space key)."""
    m = re.search(rf"^  {re.escape(job)}:\n", text, re.M)
    assert m, f"{job!r} job not found"
    nxt = re.search(r"^  [A-Za-z][A-Za-z0-9_-]*:\n", text[m.end():], re.M)
    end = m.end() + (nxt.start() if nxt else len(text[m.end():]))
    return text[m.start():end]


def _discovery_step(text: str) -> str:
    """The release-gate step that runs the unittest discovery.

    解析不到必须是响的失败：一个找不到锚点的守卫会以"空集合通过"收场，那比没有守卫更坏。
    """
    block = _job_block(text, "release-gate")
    for step in re.split(r"^      - ", block, flags=re.M):
        if "unittest discover" in step:
            return step
    raise AssertionError("no step in the release-gate job runs `unittest discover`")


def pipefail_problems(text: str) -> list[str]:
    """This release-gate step could mask a failing suite — here is why ([] = it cannot)."""
    problems: list[str] = []
    step = _discovery_step(text)
    if not re.search(r"\|\s*tee\b", step):
        problems.append(
            "the step no longer pipes unittest into `tee` — re-derive this guard for "
            "the new shape instead of leaving it vacuous"
        )
    if not PIPEFAIL_RE.search(step):
        problems.append(
            "`unittest … | tee` without pipefail: the runner's `bash -e` reports a "
            "pipeline's LAST command, so a FAILED suite still exits 0"
        )
    if re.search(r"\|\|\s*true\b", step):
        problems.append("`|| true` inside the step swallows the exit status")
    return problems


GOOD_STEP = """jobs:
  release-gate:
    name: release gate policy
    steps:
      - name: release script self-tests
        run: |
          set -o pipefail
          failures=""
          python -m unittest discover -s scripts -p 'test_*.py' -v 2>&1 | tee /tmp/py.log \\
            || failures="unittest"
          for suite in test_changelog_structure; do
            grep -q "$suite" /tmp/py.log \\
              || { echo "::error::missing $suite"; failures="$failures missing:$suite"; }
          done
          [ -z "$failures" ] || { echo "::error::self-tests: $failures"; exit 1; }
"""

BAD_NO_PIPEFAIL = GOOD_STEP.replace("          set -o pipefail\n", "")
BAD_MASKED = GOOD_STEP.replace('| tee /tmp/py.log \\\n', '| tee /tmp/py.log || true \\\n')


class ReleaseGateStepTest(unittest.TestCase):
    def test_the_real_workflow_cannot_mask_a_failing_suite(self):
        problems = pipefail_problems(_norm(CI_WF.read_text(encoding="utf-8")))
        self.assertEqual([], problems, "release-gate 会对一个红的套件报绿")

    def test_this_suite_is_named_in_the_release_gate_list(self):
        """本文件必须出现在 release-gate 的点名清单里。

        discover 会自动收集新套件，但"收集到"和"被验证收集到"是两件事：清单不点名，
        这个套件被改名/删除时不会有任何东西变红（空收集仍以 `Ran 0 tests / OK` 通过）。
        断言落在**那条 `for suite in …` 的清单**上，而不是"文件里某处出现过这个名字"
        ——本文件的名字在注释里也有，那种写法等于没查。
        """
        ci = _norm(CI_WF.read_text(encoding="utf-8"))
        m = re.search(r"^\s*for suite in (.+?); do", ci, re.M)
        self.assertIsNotNone(m, "release-gate 不再有点名清单（`for suite in …; do`）")
        self.assertIn(
            SUITE_NAME,
            m.group(1).split(),
            "add this suite to the release-gate named-suite list in ci.yml",
        )


class DetectorIsNotVacuousTest(unittest.TestCase):
    """检测器自检：同一条判据必须能分出好样本与坏样本。"""

    def test_step_without_pipefail_is_flagged(self):
        problems = pipefail_problems(BAD_NO_PIPEFAIL)
        self.assertTrue(any("pipefail" in p for p in problems), problems)

    def test_masked_failure_is_flagged(self):
        problems = pipefail_problems(BAD_MASKED)
        self.assertTrue(any("|| true" in p for p in problems), problems)

    def test_hardened_step_is_clean(self):
        self.assertEqual([], pipefail_problems(GOOD_STEP))

    def test_missing_step_fails_loudly(self):
        with self.assertRaises(AssertionError):
            pipefail_problems("jobs:\n  release-gate:\n    steps:\n      - name: x\n        run: echo hi\n")


if __name__ == "__main__":
    unittest.main()
