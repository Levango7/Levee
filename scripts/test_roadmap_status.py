"""Guards for scripts/check_roadmap_status.py — and for the roadmap it protects.

The checker earns its keep only if it can *see* the two failure modes it exists
for: a row whose status cell is missing or non-canonical, and a whole table
whose rows a naive scan would skip.  Every detector test here has a paired
sample, so "it returned []" cannot pass by looking at nothing.

The operations table (`| 项 | 目标 | … |`) gets its own tests because it is the
table a `^| P` count never saw — losing it silently is the exact bug the check
was written to end.
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


status = _load("check_roadmap_status", REPO / "scripts" / "check_roadmap_status.py")
CI_WF = REPO / ".github" / "workflows" / "ci.yml"
SUITE_NAME = "test_roadmap_status"

PRIORITY_TABLE = (
    "| 优先级 | 项 | 理由 | 依赖 | 状态 |\n"
    "| --- | --- | --- | --- | --- |\n"
    "| P0 | 甲 | r | d | closed |\n"
    "| P1 | 乙 | r | d | open |\n"
)
OPS_TABLE = (
    "| 项 | 目标 | 状态 |\n"
    "| --- | --- | --- |\n"
    "| 产品 demo 库 | g | blocked-on:人手 |\n"
)
HEAD = "# t\n\n"


def write_roadmap(root: Path, body: str) -> None:
    docs = root / "docs"
    docs.mkdir(parents=True, exist_ok=True)
    (docs / "product-roadmap.md").write_text(body, encoding="utf-8")


class LiveRoadmapTest(unittest.TestCase):
    def test_the_roadmap_passes(self):
        problems, counts, total = status.check(REPO)
        self.assertEqual([], problems, "roadmap 的状态列有问题")
        self.assertGreaterEqual(total, status.MIN_ROWS)
        # The operations rows are the ones a `^| P` scan could not see.
        self.assertIn("blocked-on:人手", counts)


class DetectorTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def _check(self, body, min_rows=1):
        write_roadmap(self.root, body)
        return status.check(self.root, min_rows=min_rows)

    def test_valid_fixture_passes(self):
        problems, counts, total = self._check(HEAD + PRIORITY_TABLE + "\n" + OPS_TABLE)
        self.assertEqual([], problems, problems)
        self.assertEqual({"closed": 1, "open": 1, "blocked-on:人手": 1}, counts)
        self.assertEqual(3, total)

    def test_ops_table_rows_are_counted(self):
        """The structural guard: dropping the ops table must change the count."""
        _p, counts, total = self._check(HEAD + PRIORITY_TABLE + "\n" + OPS_TABLE)
        self.assertEqual(3, total, "operations rows must be counted, not skipped")

    def test_missing_ops_table_is_a_scope_error(self):
        with self.assertRaises(status.ScopeError):
            self._check(HEAD + PRIORITY_TABLE)

    def test_header_without_status_column_is_flagged(self):
        body = (
            "# t\n\n"
            "| 优先级 | 项 | 理由 | 依赖 |\n"
            "| --- | --- | --- | --- |\n"
            "| P0 | 甲 | r | d |\n"
        )
        problems, _c, _t = self._check(body + "\n" + OPS_TABLE)
        self.assertTrue(any("lacks a `状态` column" in p for p in problems), problems)

    def test_non_canonical_status_is_flagged(self):
        body = PRIORITY_TABLE.replace("| P1 | 乙 | r | d | open |", "| P1 | 乙 | r | d | done |")
        problems, _c, _t = self._check(HEAD + body + "\n" + OPS_TABLE)
        self.assertTrue(any("not a canonical status" in p and "done" in p for p in problems), problems)

    def test_bare_blocked_prefix_is_flagged(self):
        body = PRIORITY_TABLE.replace("| P1 | 乙 | r | d | open |", "| P1 | 乙 | r | d | blocked-on: |")
        problems, _c, _t = self._check(HEAD + body + "\n" + OPS_TABLE)
        self.assertTrue(any("not a canonical status" in p for p in problems), problems)

    def test_multiline_row_reads_status_from_the_end(self):
        body = (
            "# t\n\n"
            "| 优先级 | 项 | 理由 | 依赖 | 状态 |\n"
            "| --- | --- | --- | --- | --- |\n"
            "| P0 | 甲 | 第一行\n第二行 | d | closed |\n"
        )
        problems, counts, total = self._check(body + "\n" + OPS_TABLE)
        self.assertEqual([], problems, problems)
        self.assertEqual(2, total, "one priority row + one operations row")
        self.assertEqual(1, counts.get("closed", 0))

    def test_floor_catches_a_silently_shrunk_scan(self):
        problems, _c, _t = self._check(HEAD + PRIORITY_TABLE + "\n" + OPS_TABLE, min_rows=10)
        self.assertTrue(any("floor" in p for p in problems), problems)

    def test_missing_roadmap_is_a_scope_error(self):
        with self.assertRaises(status.ScopeError):
            status.check(self.root)


class ReleaseGateRunsThisSuiteTest(unittest.TestCase):
    def test_this_suite_is_named_in_the_release_gate_list(self):
        ci = CI_WF.read_text(encoding="utf-8").replace("\r\n", "\n")
        match = re.search(r"^\s*for suite in (.+?); do", ci, re.M)
        self.assertIsNotNone(match, "release-gate 不再有点名清单（`for suite in …; do`）")
        self.assertIn(SUITE_NAME, match.group(1).split(), "add this suite to the release-gate named-suite list")


if __name__ == "__main__":
    unittest.main()
