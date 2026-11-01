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



G06_START = "# --- G-06"
G06_NEXT = re.compile(r"^# --- G-0", re.M)


def g06_block(text: str) -> str:
    """Slice the G-06 section out of release_gate.sh.

    锚点找不到必须响：一个只会对"没找到"保持沉默的检测器，会在脚本改名的那天
    变成一条永远为真的断言（比没有门禁更糟，因为它会替损坏的状态背书）。
    """
    norm = _norm(text)
    m = re.search(rf"^{re.escape(G06_START)}", norm, re.M)
    assert m, "release_gate.sh has no `# --- G-06` section -- re-derive this guard"
    nxt = G06_NEXT.search(norm, m.end() + 1)
    end = nxt.start() if nxt else len(norm)
    return norm[m.start():end]


def _code_only(block: str) -> str:
    """Drop comment-only lines before scanning.

    G-06 自己就踩过这个：修好之后写的那段说明里引用了旧写法 `go test … || true`，
    于是一条**解释**被判成了**缺陷**。判据必须只看会被 shell 执行的那些行——否则下一个
    在门禁脚本里写注释的人会得到一条与自己无关的红。
    """
    kept = [ln for ln in block.splitlines() if not ln.lstrip().startswith("#")]
    return "\n".join(kept)


def g06_problems(text: str) -> list[str]:
    """This G-06 could report PASS for a test run that never happened -- here is why."""
    block = _code_only(g06_block(text))
    problems: list[str] = []

    captured = re.findall(r"\|\|\s*([A-Za-z_][A-Za-z0-9_]*)=\$\?", block)
    if re.search(r"\|\|\s*true\b", block):
        problems.append("`|| true` in G-06 discards `go test`'s exit status")
    if not captured:
        problems.append("G-06 never captures `go test`'s exit status (`|| NAME=$?`), "
                        "so a run that did not happen is indistinguishable from a fast one")

    budget = re.search(r"\$\{?DURATION_G6\}?\s*-(?:gt|ge|le|lt)\s*\d+", block)
    if not budget:
        problems.append("G-06 no longer compares the measured duration against a budget "
                        "-- the gate lost the dimension it is named after")

    name = captured[0] if captured else None
    judged = name is not None and re.search(
        r"\$\{?" + re.escape(name) + r"\}?\s*-(?:ne|eq)\s*0", block)
    if name is not None and not judged:
        problems.append(f"G-06 captures {name} but never branches on it: timing still decides "
                        "the verdict, which is the false-positive this gate exists to close")
    return problems


G06_GOOD = """# --- G-06: 测试门禁 10 分钟 ---------------------------------------------------

START_G6=$(date +%s)
RC_G6=0
go test ./... -count=1 > /dev/null 2>&1 || RC_G6=$?
END_G6=$(date +%s)
DURATION_G6=$((END_G6 - START_G6))
if [[ $RC_G6 -ne 0 ]]; then
    FAIL=$((FAIL + 1))
elif [[ $DURATION_G6 -gt 600 ]]; then
    FAIL=$((FAIL + 1))
else
    PASS=$((PASS + 1))
fi

# --- G-07: 二进制可构建 -------------------------------------------------------
"""

G06_BAD_MASKED = """# --- G-06: 测试门禁 10 分钟 ---------------------------------------------------

START_G6=$(date +%s)
go test ./... -count=1 > /dev/null 2>&1 || true
END_G6=$(date +%s)
DURATION_G6=$((END_G6 - START_G6))
if [[ $DURATION_G6 -le 600 ]]; then
    PASS=$((PASS + 1))
fi

# --- G-07: 二进制可构建 -------------------------------------------------------
"""

G06_BAD_UNJUDGED = G06_GOOD.replace("if [[ $RC_G6 -ne 0 ]]; then", "if false; then")
G06_BAD_NO_BUDGET = G06_GOOD.replace("elif [[ $DURATION_G6 -gt 600 ]]; then", "elif false; then")


class G06JudgesRunNotOnlyClockTest(unittest.TestCase):
    """G-06 必须"跑成且计时"，不能只计时。

    本仓 2026-10-31 实测过的假阳性：`GOSUMDB=off` 挡住 go.mod 声明的 1.26.10 工具链
    切换时，同一张报告里 G-01/02/03/07 全红而 G-06 是 PASS（1s）。用替身 `go` 现场
    复现过四组：旧脚本 + go_rc=1 → PASS（ bug ）；新脚本 + go_rc=1 → FAIL(rc=1)；
    新脚本 + go_rc=0 → PASS；把预算改成 -1 → FAIL(时长分支仍可达)。
    """

    def test_the_real_gate_cannot_pass_a_run_that_did_not_happen(self):
        gate = REPO / "scripts" / "release_gate.sh"
        problems = g06_problems(gate.read_text(encoding="utf-8"))
        self.assertEqual([], problems, "G-06 会对没跑成的测试报 PASS：" + "; ".join(problems))

    def test_masked_exit_status_is_flagged(self):
        problems = g06_problems(G06_BAD_MASKED)
        self.assertTrue(any("|| true" in p for p in problems), problems)
        self.assertTrue(any("exit status" in p for p in problems), problems)

    def test_captured_but_unjudged_status_is_flagged(self):
        problems = g06_problems(G06_BAD_UNJUDGED)
        self.assertTrue(any("branches on it" in p for p in problems), problems)

    def test_lost_time_budget_is_flagged(self):
        problems = g06_problems(G06_BAD_NO_BUDGET)
        self.assertTrue(any("budget" in p for p in problems), problems)

    def test_hardened_shape_is_clean(self):
        self.assertEqual([], g06_problems(G06_GOOD))

    def test_prose_mentioning_the_bad_token_is_not_flagged(self):
        """注释里出现 `|| true` 不算缺陷。

        反向样本：把旧写法的**引用**作为注释加进已经修好的形状，判据必须仍给绿。
        没有这一条，检测器就退化成子串扫描器——而它第一个咬到的就是本笔自己的说明注释。
        """
        commented = G06_GOOD.replace(
            "START_G6=$(date +%s)",
            "# 这里原先写成 `go test … || true`，现在改成捕获 rc 再判。\nSTART_G6=$(date +%s)",
            1,
        )
        self.assertEqual([], g06_problems(commented))

    def test_missing_anchor_fails_loudly(self):
        with self.assertRaises(AssertionError):
            g06_problems("#!/bin/bash\necho nothing here\n")

if __name__ == "__main__":
    unittest.main()
