"""Guards for scripts/check_doc_anchors.py — and for the ledgers it protects.

The checker only earns its keep if it can *see* damage.  Every detector test here
has a paired sample: a dead anchor that must be flagged and a live one in the same
text that must not be, so "it returned []" cannot pass by looking at nothing.

The rule that needed a sample of its own is the fenced-code exemption: samples
inside ``` fences are illustrations, not references.  Without a test, that rule is
one refactor away from silently swallowing every anchor in the file.

The checker has two halves and both are exercised here: `scan_text`/`check_anchors`
(liveness — an anchor must still resolve) and `check_claims` (truth — a declared
assertion's token must still be present, or still absent, in the code it names).
Each detector has a paired satisfied sample, so a green run cannot come from
checking nothing.
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


anchors = _load("check_doc_anchors", REPO / "scripts" / "check_doc_anchors.py")
CI_WF = REPO / ".github" / "workflows" / "ci.yml"
SUITE_NAME = "test_doc_anchors"


class LiveLedgersAreLiveTest(unittest.TestCase):
    def test_declared_ledgers_have_no_dead_anchor(self):
        problems, seen = anchors.check(REPO)
        self.assertEqual([], problems, "台账里有指向不存在位置的锚点")
        self.assertGreaterEqual(seen, anchors.MIN_ANCHORS)

    def test_the_scan_is_not_vacuous(self):
        """A scan that sees nothing must not report success.

        MIN_ANCHORS is the airbag; this asserts the airbag is wired to the real
        count rather than comparing a constant with itself.
        """
        _problems, seen = anchors.check(REPO)
        self.assertGreater(seen, 0, "扫描一个锚点都没看到——判据已空转")
        # Compared against the real count, not a literal: a hardcoded number goes
        # stale the moment a ledger gains or loses an anchor.
        self.assertLessEqual(
            anchors.MIN_ANCHORS, seen, f"下限 {anchors.MIN_ANCHORS} 高于真实锚点数 {seen} 会让门禁永远红"
        )


class DetectorSeesDamageTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        (self.root / "sub").mkdir()
        (self.root / "sub" / "three.go").write_text("a\nb\nc\n", encoding="utf-8")

    def tearDown(self):
        self._tmp.cleanup()

    def test_missing_file_is_flagged(self):
        problems = anchors.scan_text("see `sub/nope.go:1`.", root=self.root)
        self.assertEqual(1, len(problems), problems)
        self.assertIn("no such file", problems[0])

    def test_line_past_eof_is_flagged(self):
        problems = anchors.scan_text("see `sub/three.go:9`.", root=self.root)
        self.assertEqual(1, len(problems), problems)
        self.assertIn("past EOF", problems[0])

    def test_reversed_range_is_flagged(self):
        problems = anchors.scan_text("see `sub/three.go:3-1`.", root=self.root)
        self.assertEqual(1, len(problems), problems)
        self.assertIn("ends before", problems[0])

    def test_live_anchor_is_not_flagged(self):
        """The control: the same text with a real anchor must stay green."""
        self.assertEqual([], anchors.scan_text("see `sub/three.go:2`.", root=self.root))
        self.assertEqual([], anchors.scan_text("see `sub/three.go:1-3`.", root=self.root))

    def test_bare_filename_is_not_an_anchor(self):
        """`three.go:1` cannot be resolved unambiguously, so it is not a claim."""
        self.assertEqual([], anchors.scan_text("see `three.go:9`.", root=self.root))

    def test_fenced_samples_are_ignored_but_unfenced_are_not(self):
        fenced_only = "```yaml\nsub/three.go:9\n```\n"
        self.assertEqual([], anchors.scan_text(fenced_only, root=self.root))

        both = "```yaml\nsub/three.go:9\n```\nand later `sub/three.go:9`.\n"
        problems = anchors.scan_text(both, root=self.root)
        self.assertEqual(1, len(problems), f"围栏外的同一锚点必须仍被检查：{problems}")

    def test_missing_live_doc_is_a_scope_error_not_a_pass(self):
        with self.assertRaises(anchors.ScopeError):
            anchors.check(self.root, docs=("docs/does-not-exist.md",))


class ClaimsAreCheckedTest(unittest.TestCase):
    """The truth half: a declared claim must match the code it names.

    Each detector assertion has a paired sample (a satisfied claim that must stay
green, a broken one that must go red), so "it returned []" cannot pass by
    checking nothing.
    """

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        (self.root / "code").mkdir()
        (self.root / "code" / "present.go").write_text("package x\nfunc Want() {}\n", encoding="utf-8")
        (self.root / "code" / "absent.go").write_text("package x\n// nothing declared here\n", encoding="utf-8")

    def tearDown(self):
        self._tmp.cleanup()

    def _claim(self, **kw):
        base = dict(
            ledger="docs/product-roadmap.md",
            what="synthetic",
            path="code/present.go",
            keyword="func Want",
            mode="present",
        )
        base.update(kw)
        return anchors.Claim(**base)

    def _filled(self, *first):
        """`first`, padded with satisfied fillers to clear the table floor."""
        fillers = tuple(self._claim() for _ in range(anchors.MIN_CLAIMS - len(first)))
        return tuple(first) + fillers

    def test_present_claim_satisfied(self):
        problems, checked = anchors.check_claims(self.root, claims=self._filled())
        self.assertEqual([], problems, problems)
        self.assertEqual(anchors.MIN_CLAIMS, checked)

    def test_present_claim_violated_is_flagged(self):
        bad = self._claim(keyword="func Missing")
        problems, _ = anchors.check_claims(self.root, claims=self._filled(bad))
        self.assertTrue(any("no longer contains" in p for p in problems), problems)

    def test_absent_claim_satisfied(self):
        ok = self._claim(path="code/absent.go", keyword="Oops", mode="absent")
        problems, _ = anchors.check_claims(self.root, claims=self._filled(ok))
        self.assertEqual([], problems, problems)

    def test_absent_claim_violated_is_flagged(self):
        bad = self._claim(path="code/present.go", keyword="func Want", mode="absent")
        problems, _ = anchors.check_claims(self.root, claims=self._filled(bad))
        self.assertTrue(any("negative claim is stale" in p for p in problems), problems)

    def test_missing_target_is_flagged(self):
        bad = self._claim(path="code/nope.go")
        problems, _ = anchors.check_claims(self.root, claims=self._filled(bad))
        self.assertTrue(any("target file missing" in p for p in problems), problems)

    def test_unknown_mode_is_a_scope_error(self):
        bad = self._claim(mode="maybe")
        with self.assertRaises(anchors.ScopeError):
            anchors.check_claims(self.root, claims=self._filled(bad))

    def test_claim_naming_a_foreign_ledger_is_a_scope_error(self):
        bad = self._claim(ledger="docs/not-live.md")
        with self.assertRaises(anchors.ScopeError):
            anchors.check_claims(self.root, claims=self._filled(bad))

    def test_live_claim_table_holds_and_clears_the_floor(self):
        problems, checked = anchors.check_claims(REPO)
        self.assertEqual([], problems, "声明的 CLAIMS 里有断言已不成立")
        self.assertLessEqual(
            anchors.MIN_CLAIMS, checked, f"下限 {anchors.MIN_CLAIMS} 高于真实条数 {checked} 会让门禁永远红"
        )


class ReleaseGateRunsThisSuiteTest(unittest.TestCase):
    def test_this_suite_is_named_in_the_release_gate_list(self):
        """Being collected by `unittest discover` is not the same as being verified
        collected: the named list is what turns a renamed/deleted suite red."""
        ci = CI_WF.read_text(encoding="utf-8").replace("\r\n", "\n")
        match = re.search(r"^\s*for suite in (.+?); do", ci, re.M)
        self.assertIsNotNone(match, "release-gate 不再有点名清单（`for suite in …; do`）")
        self.assertIn(SUITE_NAME, match.group(1).split(), "add this suite to the release-gate named-suite list")


if __name__ == "__main__":
    unittest.main()
