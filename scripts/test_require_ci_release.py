#!/usr/bin/env python3
"""Unit tests for the fail-closed release CI dependency gate."""
from __future__ import annotations

import argparse
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import require_ci_release as gate

SHA = "a" * 40
REF = "v1.2.3"


def run_doc(run_id: int, status: str, conclusion: str | None, branch: str = REF) -> dict:
    return {
        "id": run_id,
        "head_branch": branch,
        "head_sha": SHA,
        "status": status,
        "conclusion": conclusion,
        "html_url": f"https://example/actions/runs/{run_id}",
    }


def check_doc(check_id: int, status: str, conclusion: str | None, run_id: int, app: str = "github-actions") -> dict:
    return {
        "id": check_id,
        "name": gate.REQUIRED_CHECK_NAME,
        "status": status,
        "conclusion": conclusion,
        "details_url": f"https://example/actions/runs/{run_id}/job/{check_id}",
        "app": {"slug": app},
    }


class FakeAPI:
    def __init__(self, runs: list[dict], checks: list[dict] | None = None) -> None:
        self.repository = "acme/levee"
        self.runs = list(runs)
        self.checks = list(checks or [])
        self.run_calls = 0
        self.check_calls = 0

    def get(self, endpoint: str) -> dict:
        if "/actions/workflows/" in endpoint:
            value = self.runs[min(self.run_calls, len(self.runs) - 1)]
            self.run_calls += 1
            return value
        if "/check-runs" in endpoint:
            value = self.checks[min(self.check_calls, len(self.checks) - 1)]
            self.check_calls += 1
            return value
        raise AssertionError(f"unexpected endpoint: {endpoint}")


class FakeClock:
    def __init__(self) -> None:
        self.now = 0.0

    def __call__(self) -> float:
        value = self.now
        self.now += 2.0
        return value


class ReleaseGateTests(unittest.TestCase):
    def wait(self, api: FakeAPI, timeout: int = 4) -> None:
        gate.wait_for_release_ci(
            api,
            ref=REF,
            sha=SHA,
            workflow_file="ci.yml",
            timeout_seconds=timeout,
            poll_seconds=0,
            clock=FakeClock(),
            sleeper=lambda _: None,
        )

    def test_success(self) -> None:
        self.wait(FakeAPI(
            [{"workflow_runs": [run_doc(7001, "completed", "success")]}],
            [{"check_runs": [check_doc(8002, "completed", "success", 7001)]}],
        ))

    def test_pending_run_then_success(self) -> None:
        self.wait(FakeAPI(
            [
                {"workflow_runs": [run_doc(7001, "in_progress", None)]},
                {"workflow_runs": [run_doc(7001, "completed", "success")]},
            ],
            [{"check_runs": [check_doc(8002, "completed", "success", 7001)]}],
        ))

    def test_failed_ci_run_fails(self) -> None:
        with self.assertRaisesRegex(gate.GateError, "CI run 7001 concluded failure"):
            self.wait(FakeAPI([
                {"workflow_runs": [run_doc(7001, "completed", "failure")]}
            ]))

    def test_failed_aggregate_check_fails(self) -> None:
        with self.assertRaisesRegex(gate.GateError, "aggregate check concluded failure"):
            self.wait(FakeAPI(
                [{"workflow_runs": [run_doc(7001, "completed", "success")]}],
                [{"check_runs": [check_doc(8002, "completed", "failure", 7001)]}],
            ))

    def test_missing_run_times_out(self) -> None:
        with self.assertRaisesRegex(gate.GateError, "timed out"):
            self.wait(FakeAPI([{"workflow_runs": []}]))

    def test_old_success_run_does_not_match(self) -> None:
        with self.assertRaisesRegex(gate.GateError, "timed out"):
            self.wait(FakeAPI(
                [{"workflow_runs": [run_doc(6000, "completed", "success", branch="master")]}],
                [{"check_runs": [check_doc(8001, "completed", "success", 6000)]}],
            ))

    def test_old_check_does_not_match_current_run(self) -> None:
        with self.assertRaisesRegex(gate.GateError, "timed out"):
            self.wait(FakeAPI(
                [{"workflow_runs": [run_doc(7001, "completed", "success")]}],
                [{"check_runs": [check_doc(8001, "completed", "success", 6000)]}],
            ))

    def test_duplicate_checks_latest_wins(self) -> None:
        with self.assertRaisesRegex(gate.GateError, "concluded failure"):
            self.wait(FakeAPI(
                [{"workflow_runs": [run_doc(7001, "completed", "success")]}],
                [{"check_runs": [
                    check_doc(8002, "completed", "success", 7001),
                    check_doc(8003, "completed", "failure", 7001),
                ]}],
            ))

    def test_wrong_app_does_not_match(self) -> None:
        with self.assertRaisesRegex(gate.GateError, "timed out"):
            self.wait(FakeAPI(
                [{"workflow_runs": [run_doc(7001, "completed", "success")]}],
                [{"check_runs": [check_doc(8004, "completed", "success", 7001, app="other")]}],
            ))

    def test_api_error_fails_immediately(self) -> None:
        class BrokenAPI(FakeAPI):
            def get(self, endpoint: str) -> dict:
                raise gate.GateError("simulated API failure")

        with self.assertRaisesRegex(gate.GateError, "simulated API failure"):
            self.wait(BrokenAPI([]))

    def test_cli_rejects_invalid_tag(self) -> None:
        args = argparse.Namespace(
            repository="acme/levee", sha=SHA, ref="not-a-version", timeout=1, poll=0
        )
        with self.assertRaisesRegex(gate.GateError, "only accepts v"):
            gate.validate(args)


class TaggedCommitLinearityTests(unittest.TestCase):
    """A release tag must point at a single-parent commit (CONTRIBUTING §5.4).

    `ci.yml`'s landing-shape step asserts the same thing for pushes to master,
    but its condition is push + refs/heads/master, so on a `v*` tag push the step
    is skipped and nothing else would notice a tag cut on a merge commit.
    """

    def test_single_parent_passes(self) -> None:
        api = CommitAPI(["b" * 40])
        gate.assert_linear_tagged_commit(api, sha=SHA)
        self.assertEqual(api.calls, [f"repos/acme/levee/commits/{SHA}"])

    def test_merge_commit_fails(self) -> None:
        api = CommitAPI(["b" * 40, "c" * 40])
        with self.assertRaisesRegex(gate.GateError, "has 2 parents"):
            gate.assert_linear_tagged_commit(api, sha=SHA)

    def test_parentless_commit_fails(self) -> None:
        api = CommitAPI([])
        with self.assertRaisesRegex(gate.GateError, "has 0 parents"):
            gate.assert_linear_tagged_commit(api, sha=SHA)

    def run_main(self, api: "CommitAPI") -> tuple[int, list[str]]:
        """Run main() end to end with the API and the CI wait stubbed out."""
        order: list[str] = []
        original_api, original_wait = gate.GitHubAPI, gate.wait_for_release_ci

        class StubAPI(api.__class__):  # type: ignore[misc, valid-type]
            def __init__(self, **_kwargs: object) -> None:
                super().__init__(api.parents)

        gate.GitHubAPI = StubAPI  # type: ignore[assignment]
        gate.wait_for_release_ci = lambda *_a, **_k: order.append("wait")  # type: ignore[assignment]
        try:
            rc = gate.main([
                "--repository", "acme/levee", "--sha", SHA, "--ref", REF,
                "--token", "t", "--timeout", "0", "--poll", "0",
            ])
        finally:
            gate.GitHubAPI, gate.wait_for_release_ci = original_api, original_wait
        return rc, order

    def test_main_refuses_merge_commit_without_waiting(self) -> None:
        rc, order = self.run_main(CommitAPI(["b" * 40, "c" * 40]))
        self.assertEqual(rc, 1)
        self.assertEqual(order, [], "the CI wait must not run for a non-linear tag")

    def test_main_waits_when_tag_is_linear(self) -> None:
        # Control: without this, "wait never ran" would also pass if main() simply
        # stopped calling the wait at all.
        rc, order = self.run_main(CommitAPI(["b" * 40]))
        self.assertEqual(order, ["wait"])


class CommitAPI:
    def __init__(self, parents: list[str]) -> None:
        self.repository = "acme/levee"
        self.parents = list(parents)
        self.calls: list[str] = []

    def get(self, endpoint: str) -> dict:
        self.calls.append(endpoint)
        if "/commits/" not in endpoint:
            raise AssertionError(f"unexpected endpoint: {endpoint}")
        return {"sha": SHA, "parents": [{"sha": p} for p in self.parents]}


if __name__ == "__main__":
    unittest.main()
