#!/usr/bin/env python3
"""Guard the machine-readable action/environment/evidence matrix."""

from __future__ import annotations

import copy
import json
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[1]
MATRIX = ROOT / "docs" / "compatibility-matrix.json"
MODULES = {
    "file": "file/file.go",
    "mysql": "mysql/mysql.go",
    "pg": "pg/pg.go",
    "pkg": "pkg/pkg.go",
    "shell": "shell/shell.go",
    "svc": "svc/svc.go",
    "user": "user/user.go",
}
LEVELS = {"unit", "lab", "real-single", "unverified", "unsupported"}


def go_actions(root: Path, module: str) -> set[str]:
    source = (root / "internal" / "executor" / "modules" / MODULES[module]).read_text(encoding="utf-8")
    match = re.search(r"func \(Module\) Actions\(\) \[\]string\s*\{(.*?)\n\}", source, re.S)
    if not match:
        raise ValueError(f"cannot read Actions() for {module}")
    actions = set(re.findall(r'"([a-z_]+)"', match.group(1)))
    if not actions:
        raise ValueError(f"empty Actions() for {module}")
    return {f"{module}.{action}" for action in actions}


def validate_matrix(data: dict, root: Path) -> None:
    if data.get("schema_version") != "levee.compatibility/v1":
        raise ValueError("schema_version must be levee.compatibility/v1")
    actions = data.get("actions")
    if not isinstance(actions, dict) or not actions:
        raise ValueError("actions must be a non-empty object")
    expected = set().union(*(go_actions(root, module) for module in MODULES))
    if set(actions) != expected:
        raise ValueError(f"action set drift: matrix-only={sorted(set(actions) - expected)}, code-only={sorted(expected - set(actions))}")

    for name, spec in actions.items():
        environments = spec.get("environments")
        if not isinstance(environments, dict) or not environments:
            raise ValueError(f"{name}: environments must be non-empty")
        for env, level in environments.items():
            if level not in LEVELS:
                raise ValueError(f"{name}/{env}: unknown evidence level {level}")
            if env == "windows" and level != "unsupported":
                raise ValueError(f"{name}: POSIX action must not claim Windows support")
            if level == "real-single":
                evidence = spec.get("evidence") or []
                if not any("replay" in item or "REAL_" in item for item in evidence):
                    raise ValueError(f"{name}/{env}: real-single requires replay evidence")

    if any("apk" in spec["environments"] for spec in actions.values()):
        source = (root / "internal" / "executor" / "modules" / "pkg" / "pkg.go").read_text(encoding="utf-8")
        if "type apkPM struct{}" not in source or "apk info -e" not in source:
            raise ValueError("apk is claimed without apkPM implementation")
    pg_source = (root / "internal" / "executor" / "modules" / "pg" / "schema_ddl.go").read_text(encoding="utf-8")
    if "pg.schema_snapshot_ddl" in actions and "--restrict-key" not in pg_source:
        raise ValueError("PG DDL snapshot lost strict pg_dump compatibility check")
    if "pg.schema_restore" in actions and "set -o pipefail" not in pg_source:
        raise ValueError("PG dump pipeline must preserve pg_dump failures")


class MatrixGateTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.data = json.loads(MATRIX.read_text(encoding="utf-8"))

    def test_current_matrix_matches_implementation_and_evidence(self) -> None:
        validate_matrix(self.data, ROOT)

    def test_windows_cannot_be_supported_by_posix_actions(self) -> None:
        changed = copy.deepcopy(self.data)
        changed["actions"]["pkg.install"]["environments"]["windows"] = "unit"
        with self.assertRaisesRegex(ValueError, "must not claim Windows"):
            validate_matrix(changed, ROOT)

    def test_real_single_requires_replay_evidence(self) -> None:
        changed = copy.deepcopy(self.data)
        changed["actions"]["pg.schema_restore"]["evidence"] = ["internal/executor/modules/pg"]
        with self.assertRaisesRegex(ValueError, "real-single requires replay evidence"):
            validate_matrix(changed, ROOT)

    def test_action_set_drift_is_rejected(self) -> None:
        changed = copy.deepcopy(self.data)
        del changed["actions"]["pkg.upgrade"]
        with self.assertRaisesRegex(ValueError, "action set drift"):
            validate_matrix(changed, ROOT)

    def test_unknown_evidence_level_is_rejected(self) -> None:
        changed = copy.deepcopy(self.data)
        changed["actions"]["pkg.install"]["environments"]["apk"] = "production"
        with self.assertRaisesRegex(ValueError, "unknown evidence level"):
            validate_matrix(changed, ROOT)


if __name__ == "__main__":
    unittest.main()
