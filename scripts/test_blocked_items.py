"""Guards for scripts/check_blocked_items.py — and for the ledger it protects.

Every detector has a paired sample (a fixture that must stay green and one that
must go red), so "it returned []" cannot pass by looking at nothing. The
emptiness-note rule in particular gets both directions: a category emptied of
its table is red unless it says `当前无`.
"""

import importlib.util
import re
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent


def _load(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)  # type: ignore[union-attr]
    return module


blocked = _load("check_blocked_items", REPO / "scripts" / "check_blocked_items.py")
CI_WF = REPO / ".github" / "workflows" / "ci.yml"
SUITE_NAME = "test_blocked_items"

HEAD = "# 搁置项\n\n"


def section(letter, rows):
    out = [f"## {letter}. 类别 {letter}", "", "| 项 | 缺什么 |", "| --- | --- |"]
    out += [f"| {rid} 事项 | 说明 |" for rid in rows]
    return "\n".join(out) + "\n\n"


# A small but valid document: one populated section, one explicitly-empty one.
VALID = HEAD + section("A", ["A-1", "A-2"]) + "## B. 类别 B\n\n（当前无。）\n\n"


class LiveBlockedItemsTest(unittest.TestCase):
    def test_the_ledger_passes(self):
        problems, counts, total = blocked.check(REPO)
        self.assertEqual([], problems, "blocked-items 结构有问题")
        self.assertGreaterEqual(total, blocked.MIN_ROWS)
        for letter in "ABCDEF":
            self.assertIn(letter, counts, f"section {letter} disappeared from the scan")


class DetectorTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        (self.root / "docs").mkdir()

    def tearDown(self):
        self._tmp.cleanup()

    def _check(self, body, min_rows=1):
        (self.root / "docs" / "blocked-items.md").write_text(body, encoding="utf-8")
        return blocked.check(self.root, min_rows=min_rows)

    def test_valid_fixture_passes(self):
        problems, counts, total = self._check(VALID)
        self.assertEqual([], problems, problems)
        self.assertEqual({"A": 2, "B": 0}, counts)
        self.assertEqual(2, total)

    def test_wrong_letter_id_is_flagged(self):
        body = HEAD + section("A", ["A-1"]) + section("B", ["B-1", "C-9"])
        problems, _c, _t = self._check(body)
        self.assertTrue(any("letter must match its category" in p and "C-9" in p for p in problems), problems)

    def test_duplicate_id_is_flagged(self):
        body = HEAD + section("A", ["A-1", "A-1"])
        problems, _c, _t = self._check(body)
        self.assertTrue(any("already used" in p for p in problems), problems)

    def test_empty_section_without_note_is_flagged(self):
        body = HEAD + section("A", ["A-1"]) + "## B. 类别 B\n\n（暂无。）\n\n"
        problems, _c, _t = self._check(body)
        self.assertTrue(any("explicitly empty" in p for p in problems), problems)

    def test_empty_section_with_note_passes(self):
        problems, counts, _t = self._check(VALID)
        self.assertEqual(0, counts["B"])
        self.assertEqual([], problems, problems)

    def test_row_without_id_is_flagged(self):
        body = HEAD + "## A. 类别 A\n\n| 项 | 缺什么 |\n| --- | --- |\n| 没有编号 | 说明 |\n"
        problems, _c, _t = self._check(body)
        self.assertTrue(any("has no" in p and "id" in p for p in problems), problems)

    def test_floor_catches_a_shrunk_scan(self):
        problems, _c, _t = self._check(VALID, min_rows=10)
        self.assertTrue(any("floor" in p for p in problems), problems)

    def test_no_category_sections_is_a_scope_error(self):
        with self.assertRaises(blocked.ScopeError):
            self._check("# 只有散文\n\n没有分类节。\n")

    def test_missing_file_is_a_scope_error(self):
        with self.assertRaises(blocked.ScopeError):
            blocked.check(self.root)


class ReleaseGateRunsThisSuiteTest(unittest.TestCase):
    def test_this_suite_is_named_in_the_release_gate_list(self):
        ci = CI_WF.read_text(encoding="utf-8").replace("\r\n", "\n")
        match = re.search(r"^\s*for suite in (.+?); do", ci, re.M)
        self.assertIsNotNone(match, "release-gate 不再有点名清单（`for suite in …; do`）")
        self.assertIn(SUITE_NAME, match.group(1).split(), "add this suite to the release-gate named-suite list")


if __name__ == "__main__":
    unittest.main()
