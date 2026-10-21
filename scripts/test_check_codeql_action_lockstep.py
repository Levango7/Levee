"""check_codeql_action_lockstep 的单测——守卫本身也要被守卫住。

钉住三件事：
1. 本仓当前状态是绿的（否则 CI 会因为守卫误判而红）。
2. 混用两个 SHA → 报错，且报错里指得出是哪两条不一致。
3. 混用两个版本注释 → 报错（注释是给人读的，注释不一致就是骗人）。
"""

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from check_codeql_action_lockstep import check_lockstep, collect_uses  # noqa: E402

REPO = Path(__file__).resolve().parent.parent
WORKFLOW_DIR = REPO / ".github" / "workflows"

GOOD = """
  - uses: github/codeql-action/init@6f2ac81f5cd81fe351a197ba0b00d91d03cd2aee  # v4.39.0
  - uses: github/codeql-action/analyze@6f2ac81f5cd81fe351a197ba0b00d91d03cd2aee  # v4.39.0
"""

MIXED_SHA = """
  - uses: github/codeql-action/init@24c54180a607b1449ed407dd24f251e4e9147c8d  # v4.38.3
  - uses: github/codeql-action/analyze@6f2ac81f5cd81fe351a197ba0b00d91d03cd2aee  # v4.39.0
"""

MIXED_VERSION = """
  - uses: github/codeql-action/init@6f2ac81f5cd81fe351a197ba0b00d91d03cd2aee  # v4.39.0
  - uses: github/codeql-action/upload-sarif@6f2ac81f5cd81fe351a197ba0b00d91d03cd2aee  # v4.38.2
"""


class CodeQLActionLockstep(unittest.TestCase):
    def test_repo_is_consistent(self):
        workflows = {p.name: p.read_text(encoding="utf-8") for p in sorted(WORKFLOW_DIR.glob("*.yml"))}
        self.assertEqual([], check_lockstep(workflows), "本仓的 codeql-action 引用必须同版本同 SHA")

    def test_all_same_is_green(self):
        self.assertEqual([], check_lockstep({"ci.yml": GOOD}))

    def test_mixed_sha_is_red(self):
        problems = check_lockstep({"ci.yml": MIXED_SHA})
        self.assertTrue(problems, "混用两个 SHA 必须报错")
        blob = "\n".join(problems)
        self.assertIn("24c54180", blob)
        self.assertIn("6f2ac81f", blob)

    def test_mixed_version_comment_is_red(self):
        problems = check_lockstep({"ci.yml": MIXED_VERSION})
        self.assertTrue(problems, "版本注释不一致必须报错")
        self.assertIn("版本注释不一致", "\n".join(problems))

    def test_no_usage_is_red(self):
        """零引用不能静默通过——那说明守卫读错了地方。"""
        problems = check_lockstep({"ci.yml": "steps:\n  - run: echo hi\n"})
        self.assertEqual(1, len(problems))

    def test_collect_uses_parses_action_sha_version(self):
        self.assertEqual(
            [
                ("init", "6f2ac81f5cd81fe351a197ba0b00d91d03cd2aee", "4.39.0"),
                ("analyze", "6f2ac81f5cd81fe351a197ba0b00d91d03cd2aee", "4.39.0"),
            ],
            collect_uses(GOOD),
        )


if __name__ == "__main__":
    unittest.main()
