#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""断网恢复四步对账工具（Q4）的判据单测。

为什么要有这层守卫：Q4 的价值不在「多了一个脚本」，而在**四步各有机器判据**。
判据写在纯函数里（judge_locks / judge_step_rows / judge_recovery），这里用
单测把它们钉住；没有它们，脚本里任何一个判据被写坏（例如把「恰一行 success」
放宽成「至少一行」）都不会有人发现。

CI 门禁：python -m unittest discover -s scripts -p 'test_*.py'
"""

import importlib.util
import os
import unittest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TOOL = os.path.join(REPO, "deploy", "lab", "container-lab", "recover_reconcile.py")


def load_tool():
    spec = importlib.util.spec_from_file_location("recover_reconcile", TOOL)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


tool = load_tool()


class JudgeLocksTest(unittest.TestCase):
    def test_no_lock_row_for_target_is_clean(self):
        rows = [{"scope": "host:other", "owner": "x", "expires_at": "later"}]
        ok, detail = tool.judge_locks(rows, "levee-lab-1")
        self.assertTrue(ok, detail)

    def test_lock_row_for_target_is_not_clean(self):
        rows = [{"scope": "host:levee-lab-1", "owner": "exec-1",
                 "expires_at": "2026-11-15 10:00:00+08"}]
        ok, detail = tool.judge_locks(rows, "levee-lab-1")
        self.assertFalse(ok)
        self.assertIn("host:levee-lab-1", detail)

    def test_parse_locks_reads_the_saved_columns(self):
        text = ("host:levee-lab-1|exec-1|2026-11-15 09:00:00+08|2026-11-15 10:00:00+08\n"
                "(none)\n")
        rows = tool.parse_locks(text)
        self.assertEqual(1, len(rows))
        self.assertEqual("host:levee-lab-1", rows[0]["scope"])
        self.assertEqual("exec-1", rows[0]["owner"])


class JudgeStepRowsTest(unittest.TestCase):
    def test_exactly_one_success_per_step_passes(self):
        rows = [
            {"step_name": "write-marker", "host": "h1", "status": "success"},
            {"step_name": "hold", "host": "h1", "status": "success"},
        ]
        ok, detail, _ = tool.judge_step_rows(rows)
        self.assertTrue(ok, detail)

    def test_duplicate_success_rows_fail(self):
        rows = [
            {"step_name": "write-marker", "host": "h1", "status": "success"},
            {"step_name": "write-marker", "host": "h1", "status": "success"},
        ]
        ok, detail, _ = tool.judge_step_rows(rows)
        self.assertFalse(ok)
        self.assertIn("2 success row", detail)

    def test_zero_success_rows_fail(self):
        rows = [{"step_name": "write-marker", "host": "h1", "status": "failed"}]
        ok, detail, _ = tool.judge_step_rows(rows)
        self.assertFalse(ok)
        self.assertIn("0 success row", detail)

    def test_no_rows_at_all_fail(self):
        ok, detail, _ = tool.judge_step_rows([])
        self.assertFalse(ok)
        self.assertIn("no step evidence", detail)

    def test_parse_step_rows_reads_the_saved_columns(self):
        text = ("write-marker|levee-lab-1|success|2026-11-15 09:00:00+08|"
                "2026-11-15 09:00:05+08\n")
        rows = tool.parse_step_rows(text)
        self.assertEqual("write-marker", rows[0]["step_name"])
        self.assertEqual("success", rows[0]["status"])


class JudgeRecoveryTest(unittest.TestCase):
    def _rows(self):
        return [{"step_name": "write-marker", "host": "h1", "status": "success"}]

    def test_all_four_judgments_hold(self):
        checks = tool.judge_recovery(
            status="completed", step_rows=self._rows(),
            marker_before="100", marker_after="200",
            discarded_before=0, discarded_after=0)
        self.assertEqual(4, len(checks))
        self.assertTrue(all(ok for ok, _, _ in checks), checks)

    def test_not_completed_fails_the_status_judgment(self):
        checks = tool.judge_recovery(
            status="interrupted", step_rows=self._rows(),
            marker_before="100", marker_after="200",
            discarded_before=0, discarded_after=0)
        self.assertFalse(checks[0][0])
        self.assertTrue(all(ok for ok, _, _ in checks[1:]))

    def test_marker_not_advanced_fails(self):
        checks = tool.judge_recovery(
            status="completed", step_rows=self._rows(),
            marker_before="200", marker_after="200",
            discarded_before=0, discarded_after=0)
        self.assertFalse(checks[2][0])

    def test_missing_marker_value_fails_instead_of_passing(self):
        checks = tool.judge_recovery(
            status="completed", step_rows=self._rows(),
            marker_before=None, marker_after=None,
            discarded_before=0, discarded_after=0)
        self.assertFalse(checks[2][0], "unknown marker must not count as 'advanced'")

    def test_new_discarded_event_fails(self):
        checks = tool.judge_recovery(
            status="completed", step_rows=self._rows(),
            marker_before="100", marker_after="200",
            discarded_before=0, discarded_after=1)
        self.assertFalse(checks[3][0])

    def test_expected_step_missing_fails(self):
        checks = tool.judge_recovery(
            status="completed", step_rows=self._rows(),
            marker_before="100", marker_after="200",
            discarded_before=0, discarded_after=0,
            expected_steps=["write-marker", "hold"])
        self.assertFalse(checks[1][0])
        self.assertIn("hold", checks[1][2])


if __name__ == "__main__":
    unittest.main()
