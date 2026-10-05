#!/usr/bin/env python3
"""test_check_release_versions.py — 交付物版本门禁的自测。

Why: `check_release_versions.py` 是 `validate_delivery.sh` 的第一道门，而它自己此前
没有任何测试——规则被重构掉、被"顺手简化"成永远为真，都不会有东西发现。本文件给每条
规则配一个"必须失败"的用例，外加两条边界（空 tag 集合必须拒绝；历史无 tag 小节只点名
不阻断）。

其中 `test_image_tag_without_v_prefix_fails` 是 2026-10-06 那个交付缺陷的回归用例：
chart 的 `image.tag` 写了不带 v 的 `1.19.0`，而 release.yml 推的镜像 tag 就是 git tag
名（带 v），于是 `helm install` 默认值拉取一个从未构建的镜像。当时门禁放过了它，因为它
按 `v<image.tag>` 合成查询——两种拼写都算通过。

运行：python -m unittest scripts/test_check_release_versions.py
"""

from __future__ import annotations

import importlib.util
import pathlib
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPO = Path(__file__).resolve().parent.parent
SPEC_PATH = REPO / "scripts" / "check_release_versions.py"

_spec = importlib.util.spec_from_file_location("check_release_versions", SPEC_PATH)
crv = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(crv)

RELEASED = {"v1.18.0", "v1.19.0", "v1.17.0"}

CHART_OK = 'apiVersion: v2\nname: levee\ntype: application\nappVersion: "1.19.0"\nversion: 0.1.0\n'
VALUES_OK = ('image:\n  repository: ghcr.io/levango7/levee\n  # 逐字等于 release.yml 推的 tag\n'
             '  tag: "v1.19.0"\n  pullPolicy: IfNotPresent\n')
LOG_OK = "## [Unreleased]\n\n## [v1.19.0] - 2026-10-06\n\n- shipped\n"


def values_with_tag(tag: str) -> str:
    return f'image:\n  repository: ghcr.io/levango7/levee\n  tag: "{tag}"\n'


def chart_with_app_version(version: str) -> str:
    return f'apiVersion: v2\nname: levee\ntype: application\nappVersion: "{version}"\nversion: 0.1.0\n'


class GateRun(unittest.TestCase):
    """Run the gate on fixture files with a scripted tag set."""

    def run_gate(self, *, chart=CHART_OK, values=VALUES_OK, log=LOG_OK,
                 tags=RELEASED) -> tuple[int, list[str], list[str]]:
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            chart_dir = root / "chart"
            chart_dir.mkdir()
            (chart_dir / "Chart.yaml").write_text(chart, encoding="utf-8")
            (chart_dir / "values.yaml").write_text(values, encoding="utf-8")
            changelog = root / "CHANGELOG.md"
            changelog.write_text(log, encoding="utf-8")

            notes: list[str] = []
            crv.failures.clear()
            with mock.patch.object(crv, "tag_set", return_value=(set(tags), "fixture")), \
                 mock.patch.object(sys, "argv", ["check_release_versions.py", str(chart_dir), str(changelog)]), \
                 mock.patch.object(crv, "note", lambda msg: notes.append(msg)):
                rc = crv.main()
            return rc, list(crv.failures), notes

    # --- the state that must be accepted -------------------------------------

    def test_consistent_chart_passes(self) -> None:
        rc, failures, _ = self.run_gate()
        self.assertEqual(rc, 0, f"unexpected failures: {failures}")
        self.assertEqual(failures, [])

    def test_v_prefix_difference_is_not_a_failure(self) -> None:
        # appVersion 按 Helm 惯例不带 v，image.tag 按发布拼写带 v —— 同一个版本号。
        rc, failures, _ = self.run_gate(chart=chart_with_app_version("1.19.0"),
                                        values=values_with_tag("v1.19.0"))
        self.assertEqual(rc, 0, f"unexpected failures: {failures}")

    # --- rule 1: the chart must not claim one version while deploying another --

    def test_app_version_and_image_tag_disagree(self) -> None:
        rc, failures, _ = self.run_gate(values=values_with_tag("v1.18.0"))
        self.assertEqual(rc, 1)
        self.assertTrue(any("!= values.image.tag" in f for f in failures), failures)

    # --- rule 2a: the rendered image reference must exist ----------------------

    def test_image_tag_without_v_prefix_fails(self) -> None:
        """Regression: this is the spelling that shipped from v1.18.0 to v1.19.0."""
        rc, failures, _ = self.run_gate(values=values_with_tag("1.19.0"))
        self.assertEqual(rc, 1)
        self.assertTrue(any("is not a released tag name" in f for f in failures), failures)
        # The message must say what breaks, not just that something is off.
        self.assertTrue(any("helm install" in f for f in failures), failures)

    def test_image_tag_of_a_never_released_version_fails(self) -> None:
        rc, failures, _ = self.run_gate(chart=chart_with_app_version("1.20.0"),
                                        values=values_with_tag("v1.20.0"),
                                        log="## [Unreleased]\n")
        self.assertEqual(rc, 1)
        self.assertTrue(any("is not a released tag name" in f for f in failures), failures)
        self.assertTrue(any("has no tag v1.20.0" in f for f in failures), failures)

    # --- rule 3: a CHANGELOG section is a release claim ------------------------

    def test_section_newer_than_newest_tag_needs_the_pending_marker(self) -> None:
        claim = "## [Unreleased]\n\n## [v1.20.0] - 2026-10-07\n\n- done\n"
        rc, failures, _ = self.run_gate(log=claim)
        self.assertEqual(rc, 1)
        self.assertTrue(any("as a released section but tag v1.20.0 does not exist" in f
                            for f in failures), failures)

    def test_pending_marker_is_accepted_and_recorded(self) -> None:
        claim = ("## [Unreleased]\n\n## [v1.20.0] - 2026-10-07\n\n"
                 f"> 未切版\n\n- done\n")
        rc, failures, notes = self.run_gate(log=claim)
        self.assertEqual(rc, 0, f"unexpected failures: {failures}")
        self.assertTrue(any("declared 未切版" in n for n in notes), notes)

    def test_historical_untagged_section_is_recorded_not_fatal(self) -> None:
        claim = ("## [Unreleased]\n\n## [v1.19.0] - 2026-10-06\n\n- done\n\n"
                 "## [v1.7.0] - 2026-06-01\n\n- older than the newest tag\n")
        rc, failures, notes = self.run_gate(log=claim)
        self.assertEqual(rc, 0, f"unexpected failures: {failures}")
        self.assertTrue(any("no matching tag" in n for n in notes), notes)

    # --- rule 4: an empty tag set must never look like a pass ------------------

    def test_empty_tag_set_fails_instead_of_passing(self) -> None:
        rc, failures, _ = self.run_gate(tags=set())
        self.assertEqual(rc, 1)
        self.assertTrue(any("no v* tags are visible" in f.lower() for f in failures), failures)

    # --- the reader of the fields itself --------------------------------------

    def test_sidecar_tag_is_not_mistaken_for_the_image_tag(self) -> None:
        # `image_tag_of` must stay scoped to the `image:` block: a postgres tag
        # would otherwise satisfy the whole gate.
        values = VALUES_OK + "postgres:\n  image:\n    tag: \"9.9.9\"\n"
        rc, failures, _ = self.run_gate(values=values)
        self.assertEqual(rc, 0, f"unexpected failures: {failures}")
        self.assertFalse(any("9.9.9" in f for f in failures), failures)


if __name__ == "__main__":
    unittest.main()
