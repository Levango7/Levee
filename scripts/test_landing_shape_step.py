"""master 的落地形状必须是一父链——这条规则不能只写在 CONTRIBUTING.md §5.4 里。

§5.4 把 PR 落地方式从 merge commit 改成 `gh pr merge --rebase`。规则本身写在文档里
就会腐烂：谁来合、用什么旗标合，都不在 PR 的 diff 里，CI 的 pull_request 事件根本
看不见。唯一能证明的时刻是 push 到 master 之后——那时 HEAD 就是落地那一笔提交。

本测试因此只做一件事：**钉住 ci.yml 里那一步的存在与形状**，而不是自己去数父提交
（CI 上是浅克隆，而且 PR 事件里 master 的提交压根不在树上，去数就成了随机结果）。

它存在的理由是这个仓反复付过学费的那一类：一个守卫"全绿地检查了它自己看不见的事"。
具体三条各自的死法——

- 步骤被删：规则变成文档里的一句话，没人拦得住一次 `--merge`。
- 条件写宽（比如在 pull_request 上也断言父数）：GitHub 的
  `refs/pull/N/merge` 合成引用天生有两个父，于是每条 PR 必红——一个会响的假警报
  和一个不响的真守卫一样糟。
- 条件写在 **job** 上而不是 step 上：PR 事件里这个 job 变 `skipped`，扇入
  `check (all jobs passed)` 走的是 "produced no verdict (skipped) — UNVERIFIED"
  那一条并 rc=1，于是所有 PR 因一道"只在 master 才有意义"的检查而红。

所以断言的是：步骤名、step 级 `if` 的两个条件、用 `git rev-list --parents -1` 取父数、
与 "1" 比较、失败路径 `exit 1`，以及 release-gate 这个 job 自己没有 job 级 `if`。

DetectorIsNotVacuous 用合成文本跑同一组表达式：只盯着一份当下干净的文件断言，无法
说明它能不能看见损坏。
"""

import re
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
CI_WF = REPO / ".github" / "workflows" / "ci.yml"

STEP_NAME = "landing shape (master stays linear)"


def job_block(text, job):
    """The lines of one job, from its key to the next top-level key."""
    m = re.search(rf"^  {re.escape(job)}:\n", text, re.M)
    if not m:
        return None
    nxt = re.search(r"^  [A-Za-z][A-Za-z0-9_-]*:\n", text[m.end():], re.M)
    return text[m.start():m.end() + (nxt.start() if nxt else len(text) - m.end())]


def step_block(gate, name):
    """One step's text inside the release-gate job, keyed on its display name."""
    m = re.search(rf"^      - name: {re.escape(name)}\n", gate, re.M)
    if not m:
        return None
    nxt = re.search(r"^      - name: ", gate[m.end():], re.M)
    return gate[m.start():m.end() + (nxt.start() if nxt else len(gate) - m.end())]


def step_code(step):
    """The step's `run: |` body with comment lines removed — and only the body votes.

    Not tidiness: the step carries a comment that NAMES the depth-sensitive probe it
    avoids, so scanning the whole step reports a guard that does not exist. The same
    blindness runs the other way too — a comment mentioning `exit 1` could satisfy a
    required pattern while the executed body never exits. Strip comments and require
    the patterns in what actually runs.
    """
    m = re.search(r"^        run: \|\n((?:.*\n)*)", step, re.M)
    if not m:
        return ""
    return "\n".join(l for l in m.group(1).split("\n") if not l.strip().startswith("#"))


def findings(gate):
    """Every reason the landing-shape guard would not fire when it should."""
    problems = []
    step = step_block(gate, STEP_NAME) if gate else None
    if step is None:
        return ["step %r is gone from the release-gate job" % STEP_NAME]

    cond = re.search(r"^        if: (.+)$", step, re.M)
    if not cond:
        problems.append("the step lost its `if:` — it would then run on pull_request, where the"
                        " synthetic merge ref has two parents and the check is meaningless")
    else:
        expr = cond.group(1)
        if "github.event_name == 'push'" not in expr:
            problems.append("`if:` no longer restricts to push events: %s" % expr)
        if "refs/heads/master" not in expr:
            problems.append("`if:` no longer restricts to the master ref: %s" % expr)

    code = step_code(step)
    if not code.strip():
        return problems + ["the step has no `run: |` body to inspect"]
    if "git cat-file commit HEAD" not in code:
        problems.append("the step no longer counts parents off the commit object")
    if "grep -c '^parent '" not in code:
        problems.append("the parent count is no longer taken from `^parent ` lines")
    # Measured, not theoretical: under this job's default fetch-depth of 1 the
    # history-boundary form answers ZERO for a commit that has two parents
    # (`git rev-list --parents -1 HEAD` on a shallow clone of 385d772 → 0, while
    # `git cat-file commit` → 2). A probe whose value depends on how much history
    # was fetched cannot express "exactly one parent".
    if "git rev-list --parents" in code:
        problems.append("the step counts parents with a depth-sensitive probe (`git rev-list --parents`);"
                        " under fetch-depth: 1 it reports 0 for a real two-parent commit")
    if '!= "1"' not in code:
        problems.append('the step no longer compares the count against "1"')
    if "exit 1" not in code:
        problems.append("the failure path no longer exits non-zero — the step prints and passes")
    # A job-level `if` would make the whole job skipped on PRs, which the aggregate
    # scores as UNVERIFIED and rc=1: every PR would go red because of this guard.
    head = gate[:gate.index("      - name: ")] if "      - name: " in gate else gate
    if re.search(r"^    if:", head, re.M):
        problems.append("release-gate gained a JOB-level `if:` — a skipped job yields no verdict")
    return problems


class LandingShapeStepTest(unittest.TestCase):
    def setUp(self):
        self.text = CI_WF.read_text(encoding="utf-8")
        self.gate = job_block(self.text, "release-gate")
        self.assertIsNotNone(self.gate, "no release-gate job in ci.yml")

    def test_guard_present_and_shaped(self):
        problems = findings(self.gate)
        self.assertEqual([], problems, "the landing-shape guard is not the guard §5.4 assumes:\n" + "\n".join(problems))

    def test_self_tests_roster_names_this_suite(self):
        # The gate job greps each expected suite out of the discovery log, because an
        # empty discovery still exits 0. If this file is renamed or dropped from the
        # list, nobody checks it — including the check that nobody checks it.
        m = re.search(r"for suite in ([^;]+); do", self.text)
        self.assertIsNotNone(m, "the roster loop disappeared from ci.yml")
        self.assertIn("test_landing_shape_step", m.group(1).split(),
                      "this suite is not in the release-gate roster")

    def test_release_gate_still_feeds_the_aggregate(self):
        # Read the needs list OF THE AGGREGATE JOB. A file-wide search for the first
        # `    needs: [...]` was my first attempt here and it landed on CodeQL's
        # `needs: [vet, lint, test]`, which would have made this assertion pass for
        # the wrong reason — or fail when an unrelated job's list changed.
        check = job_block(self.text, "check")
        self.assertIsNotNone(check, "no `check` job in ci.yml")
        needs = re.search(r"^    needs: \[([^\]]+)\]", check, re.M)
        self.assertIsNotNone(needs, "the aggregate job has no needs list")
        self.assertIn("release-gate", needs.group(1).split(", "),
                      "release-gate left the fan-in, so its verdict can no longer block a merge")

    def test_no_job_level_condition_on_release_gate(self):
        head = self.gate[:self.gate.index("      - name: ")]
        self.assertNotRegex(head, r"(?m)^    if:",
                            "release-gate must run on every event: a skipped job gives the aggregate no verdict")


class DetectorIsNotVacuousTest(unittest.TestCase):
    """The expressions above must be able to *see* the damage they exist for."""

    def good(self):
        return job_block(CI_WF.read_text(encoding="utf-8"), "release-gate")

    def test_good_fixture_passes(self):
        self.assertEqual([], findings(self.good()))

    def test_missing_step_detected(self):
        gate = self.good()
        broken = gate[:gate.index("      - name: landing shape")] + "      - name: unrelated\n        run: echo hi\n"
        self.assertTrue(any("gone" in p for p in findings(broken)), "deleting the step went unnoticed")

    def test_missing_step_condition_detected(self):
        gate = self.good().replace("        if: github.event_name == 'push' && github.ref == 'refs/heads/master'\n", "")
        self.assertTrue(any("lost its `if:`" in p for p in findings(gate)),
                        "a step that also runs on pull_request went unnoticed")

    def test_condition_widened_detected(self):
        gate = self.good().replace("github.event_name == 'push' && github.ref == 'refs/heads/master'",
                                   "github.ref == 'refs/heads/master'")
        self.assertTrue(any("push events" in p for p in findings(gate)),
                        "widening the event condition went unnoticed")

    def test_soft_failure_detected(self):
        gate = self.good().replace("            exit 1\n", "            echo warning-only\n")
        self.assertTrue(any("exits non-zero" in p for p in findings(gate)),
                        "a guard that only prints went unnoticed")

    def test_depth_sensitive_probe_detected(self):
        # The exact regression this file exists to catch: swapping the commit-object
        # read back for a history-walking one compiles, runs, and silently changes
        # meaning under fetch-depth: 1 (0 parents reported for a real merge commit).
        gate = self.good().replace("parents=$(git cat-file commit HEAD | grep -c '^parent ')",
                                   "parents=$(git rev-list --parents -1 HEAD | awk '{print NF-1}')")
        problems = findings(gate)
        self.assertTrue(any("depth-sensitive" in p for p in problems),
                        "the probe became fetch-depth dependent and nothing said so: %s" % problems)

    def test_job_level_condition_detected(self):
        gate = self.good()
        idx = gate.index("\n    steps:\n")
        broken = gate[:idx] + "\n    if: github.event_name == 'push'\n" + gate[idx:]
        self.assertTrue(any("JOB-level" in p for p in findings(broken)),
                        "a job-level `if` (which makes the aggregate report UNVERIFIED) went unnoticed")


if __name__ == "__main__":
    unittest.main()
