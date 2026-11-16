#!/usr/bin/env python3
"""test_repro_probe.py — 复现探针的结构守卫。

Why: 探针的全部价值在于「复发时能拿到证据」。而证据是靠三件事成立的：
① 探针**重复跑**（一次 run 只能回答「这次红了」）；② 失败时**照样上传产物**
（`if: always()`；只在成功时上传等于永远没有证据）；③ 探针**不洗绿**、也**不
进门禁**（重试会把一次真崩溃洗成一次绿；探针红了不该挡住别人的合并）。

这三件事都只写在 workflow 与脚本的文本里，没有别的机制保证它们不被改掉：
有人把 `if: always()` 删掉、把循环改成跑一次、或者把探针加进主线的 required
checks，门禁都不会报——仓库里还有别的东西会先红，而「证据悄悄没了」正是
这个工作流最该防的失败模式。

所以这里读**入库文本**（workflow YAML + 两个探针脚本），断言它们仍然带着那些
性质。跑法：python -m unittest discover -s scripts -p 'test_*.py'
"""

from __future__ import annotations

import pathlib
import re
import unittest

REPO = pathlib.Path(__file__).resolve().parent.parent
WORKFLOW = REPO / ".github" / "workflows" / "repro-probe.yml"
CALENDAR_PROBE = REPO / "scripts" / "calendar-repro-probe.sh"
BROWSER_PROBE = REPO / "scripts" / "browser-check-probe.sh"
CI_WORKFLOW = REPO / ".github" / "workflows" / "ci.yml"


class ReproProbeWorkflowTest(unittest.TestCase):
    def setUp(self) -> None:
        self.text = WORKFLOW.read_text(encoding="utf-8")

    def test_the_workflow_exists_and_runs_on_a_schedule(self) -> None:
        self.assertIn("name: Repro Probe", self.text)
        self.assertIn("schedule:", self.text)
        self.assertRegex(self.text, r"cron: '\d+ \d+ \* \* \*'",
                         "the probe must run on a schedule — otherwise it waits for a manual trigger forever")
        self.assertIn("workflow_dispatch:", self.text,
                      "a manual trigger is the escape hatch when someone suspects a recurrence")

    def test_both_probes_are_present(self) -> None:
        self.assertIn("calendar-windows:", self.text)
        self.assertIn("browser-check-windows:", self.text)
        self.assertIn("scripts/calendar-repro-probe.sh", self.text)
        self.assertIn("../scripts/browser-check-probe.sh", self.text)

    def test_both_probes_run_on_windows(self) -> None:
        # 这两条偶发问题都只在 windows-latest 上出现过；探针跑在别的平台上
        # 等于什么都没盯。
        self.assertGreaterEqual(self.text.count("runs-on: windows-latest"), 2)

    def test_artifacts_are_uploaded_even_when_the_probe_fails(self) -> None:
        uploads = re.findall(
            r"- name: upload repro artifacts\s*\n\s*if: always\(\)", self.text)
        self.assertEqual(len(uploads), 2,
                         "both probes must upload their artifacts with if: always() — "
                         "the failure IS the evidence")

    def test_the_probe_is_not_part_of_the_merge_gate(self) -> None:
        # 主线的 required checks 不含本工作流：探针红了要报出来，但不能挡住
        # 别人的合并（那会把「等复发观察」变成「不许合并」）。
        self.assertNotIn("repro-probe", CI_WORKFLOW.read_text(encoding="utf-8"),
                         "the repro probe must stay OUT of the main CI gate")


class ActionPinsMatchTheMainWorkflowTest(unittest.TestCase):
    """本工作流里的 action 必须与主线 CI 用**同一个** SHA。

    Why: 2026-11-16 实跑探针时两个 job 都在「Set up job」就红了——
    `Unable to resolve action actions/checkout@3d3c42e5aac2ba...`，
    比对后发现手抄 SHA 时少了一个字符（`aac5ba` 抄成 `aac2ba`）。
    actionlint 看不出来：截断后的 SHA 形状仍然是一个 SHA，它只校验格式。
    所以这类错**只有真跑才会暴露**，而真跑一次要等一个 runner。

    这条守卫把「抄错」变成构建时就红：同名的 action，本工作流用的 SHA
    必须逐字出现在 ci.yml 里。它不判断版本新旧（那是 Dependabot / 人工
    的事），只判断「我们到底钉的是不是同一个提交」。
    """

    def setUp(self) -> None:
        self.probe = WORKFLOW.read_text(encoding="utf-8")
        self.ci = CI_WORKFLOW.read_text(encoding="utf-8")

    def test_every_action_pin_names_the_same_commit_as_the_main_workflow(self) -> None:
        used = re.findall(r"uses:\s*(\S+@[0-9a-f]{40})", self.probe)
        self.assertGreaterEqual(len(used), 3,
                                "the probe is expected to pin checkout / setup-go|setup-node / upload-artifact")
        for pin in used:
            name = pin.split("@")[0]
            self.assertIn(pin, self.ci,
                          f"{name} is pinned to a SHA the main CI does not use "
                          f"(hand-copied pins drift by a character and only fail at runtime): {pin}")

    def test_pins_look_like_full_commit_shas(self) -> None:
        # A truncated or short pin resolves to nothing at runtime; the regex in
        # the test above already demands 40 hex chars, so this one asserts the
        # probe did not grow an unpinned `@v3` style reference either.
        for ref in re.findall(r"uses:\s*\S+@(\S+)", self.probe):
            self.assertRegex(ref, r"^[0-9a-f]{40}$",
                             f"an action must be pinned to a full SHA, found {ref!r}")


class ReproProbeScriptsTest(unittest.TestCase):
    def test_calendar_probe_loops_and_records_both_cover_modes(self) -> None:
        text = CALENDAR_PROBE.read_text(encoding="utf-8")
        self.assertIn("for i in $(seq 1 \"$RUNS\")", text,
                      "the probe must repeat — one run cannot answer 'how often'")
        self.assertIn("go test -count=5 -v", text,
                      "the repeated form the台账 asks for")
        self.assertIn("go test -cover -count=5 -v", text,
                      "the -cover form must also run: that is what answers "
                      "'is the crash always tied to the coverage instrumentation'")
        self.assertIn("GOTRACEBACK=all", text,
                      "a crash without a dump is the reason this investigation stalled")

    def test_calendar_probe_does_not_stop_at_the_first_failure(self) -> None:
        text = CALENDAR_PROBE.read_text(encoding="utf-8")
        # 早停会把「十次红一次」与「十次红九次」混成同一件事。
        self.assertNotIn("exit 1\n  fi", text)
        self.assertIn("plain_failures=$((plain_failures + 1))", text)

    def test_calendar_probe_fails_loudly_when_it_reproduces(self) -> None:
        text = CALENDAR_PROBE.read_text(encoding="utf-8")
        self.assertIn("REPRODUCED", text)
        self.assertIn("exit 1", text)

    def test_browser_probe_loops_and_keeps_per_round_evidence(self) -> None:
        text = BROWSER_PROBE.read_text(encoding="utf-8")
        self.assertIn("for i in $(seq 1 \"$RUNS\")", text)
        self.assertIn("BROWSER_CHECK_DUMPIO=1", text,
                      "the browser process's own stdout/stderr is the evidence the "
                      "launch-failure mode was missing")
        self.assertIn("run-${i}.log", text)
        self.assertIn("run-${i}", text)
        self.assertIn("settle_failures", text)
        self.assertIn("launch_failures", text)

    def test_browser_probe_fails_loudly_when_it_reproduces(self) -> None:
        text = BROWSER_PROBE.read_text(encoding="utf-8")
        self.assertIn("REPRODUCED", text)
        self.assertIn("exit 1", text)


if __name__ == "__main__":
    unittest.main()
