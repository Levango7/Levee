#!/usr/bin/env python3
"""Tests for the Q4 recovery reconciliation (scripts/levee_recovery_reconcile.py).

The assertion battery is the point of the tool: it decides whether a recovery
actually made the evidence whole, and getting it wrong in either direction is
expensive — a false PASS tells an operator the change ran exactly once when it
may have run twice, and a false FAIL sends them back into a procedure they have
already completed.

Every case here is a pure RunFacts; none needs a cluster. The IO wrappers are
thin and covered separately by the lab drill (#96), which exercises the same
queries against a real PostgreSQL.

Also pinned: the target-validation guard. An operator-supplied base URL ends
up in a request carrying the deployment token, so a private/loopback host must
be acknowledged explicitly rather than silently allowed.
"""

from __future__ import annotations

import sys
import unittest

sys.path.insert(0, __file__.rsplit("/", 1)[0] if "/" in __file__ else ".")

from levee_recovery_reconcile import (  # noqa: E402
    PrereqError,
    RunFacts,
    main,
    reconcile_assertions,
    validate_base_url,
)


def clean_facts(**over):
    """A recovery that satisfies every criterion, as a starting point."""
    base = dict(
        run_status="completed",
        steps=[("add-column", "success", 1), ("verify", "success", 1)],
        trace_events=["run_started", "step_execute", "run_completed"],
        marker_mtime=2000.0,
        marker_mtime_before=1000.0,
    )
    base.update(over)
    return RunFacts(**base)


def failed_names(checks):
    return [c.name for c in checks if not c.ok]


class ReconcileAssertions(unittest.TestCase):

    def test_clean_recovery_passes_every_check(self):
        checks = reconcile_assertions(clean_facts(),
                                       expected_steps=["add-column", "verify"])
        self.assertEqual([], failed_names(checks))
        self.assertEqual(5, len(checks))

    def test_run_not_completed_fails(self):
        names = failed_names(reconcile_assertions(
            clean_facts(run_status="interrupted"),
            expected_steps=["add-column", "verify"]))
        self.assertIn("run status is completed", names)

    def test_duplicated_success_row_fails(self):
        # The ledger is exactly-once: two success rows for one step is a
        # double-write getting recorded, and the whole point of Q4 is to catch
        # it before the operator walks away.
        facts = clean_facts(steps=[("add-column", "success", 2)])
        names = failed_names(reconcile_assertions(
            facts, expected_steps=["add-column", "verify"]))
        self.assertIn("each step has exactly one success row", names)

    def test_planned_step_with_no_row_fails(self):
        # Without the plan there is no way to tell "recorded once" from "never
        # recorded at all"; with it, an absent planned step is a gap.
        facts = clean_facts(steps=[("verify", "success", 1)])
        names = failed_names(reconcile_assertions(
            facts, expected_steps=["add-column", "verify"]))
        self.assertIn("each step has exactly one success row", names)

    def test_unplanned_success_row_fails(self):
        # A success row for a step the plan never called for is somebody
        # writing outside the approved change; it must not pass silently.
        facts = clean_facts(steps=[("add-column", "success", 1),
                                   ("verify", "success", 1),
                                   ("ghost-step", "success", 1)])
        names = failed_names(reconcile_assertions(
            facts, expected_steps=["add-column", "verify"]))
        self.assertIn("each step has exactly one success row", names)

    def test_without_expected_steps_absent_rows_are_not_guessable(self):
        # Honesty about what the tool can know: without the plan it does NOT
        # claim a missing step, because "not in the list" would be a guess.
        facts = clean_facts(steps=[("verify", "success", 1)])
        names = failed_names(reconcile_assertions(facts))
        self.assertNotIn("each step has exactly one success row", names)

    def test_marker_that_did_not_move_fails(self):
        names = failed_names(reconcile_assertions(
            clean_facts(marker_mtime=1000.0, marker_mtime_before=1000.0)))
        self.assertIn("target marker file moved after retry", names)

    def test_marker_absent_fails_rather_than_skipping(self):
        names = failed_names(reconcile_assertions(clean_facts(marker_mtime=None)))
        self.assertIn("target marker file moved after retry", names)

    def test_new_discard_event_fails(self):
        facts = clean_facts(trace_events=["evidence_discarded", "run_completed"])
        names = failed_names(reconcile_assertions(facts, discarded_events_before=0))
        self.assertIn("no new evidence_discarded events", names)

    def test_pre_existing_discard_is_not_a_new_one(self):
        # A discard that happened during the partition is a fact of the outage,
        # not a failure of this recovery: only NEW discards mean a worker wrote
        # after being told it no longer owned the run.
        facts = clean_facts(trace_events=["evidence_discarded", "run_completed"])
        names = failed_names(reconcile_assertions(facts, discarded_events_before=1))
        self.assertNotIn("no new evidence_discarded events", names)

    def test_evidence_incomplete_event_fails(self):
        facts = clean_facts(trace_events=["evidence_incomplete", "run_completed"])
        names = failed_names(reconcile_assertions(facts))
        self.assertIn("no evidence_incomplete trace event", names)

    def test_failure_detail_names_the_offending_step(self):
        # An operator at 03:00 needs the step name, not a bare boolean.
        checks = reconcile_assertions(clean_facts(steps=[("add-column", "success", 3)]))
        bad = [c for c in checks if not c.ok][0]
        self.assertIn("add-column", bad.detail)


class BaseUrlValidation(unittest.TestCase):

    def test_rejects_non_http_scheme(self):
        # file:// would read local files with the operator's own privileges;
        # the scheme list is the whole defence, so it stays explicit.
        for url in ("file:///etc/passwd", "ftp://example.invalid/x", "gopher://x/"):
            with self.assertRaises(PrereqError, msg=url):
                validate_base_url(url, allow_private=False)

    def test_private_host_requires_acknowledgement(self):
        for url in ("http://127.0.0.1:8080", "http://localhost:9",
                    "http://192.168.1.10", "http://10.0.0.5", "http://[::1]:8080"):
            with self.assertRaises(PrereqError, msg=url):
                validate_base_url(url, allow_private=False)

    def test_private_host_allowed_when_acknowledged(self):
        self.assertEqual("http://127.0.0.1:8080",
                         validate_base_url("http://127.0.0.1:8080",
                                           allow_private=True))

    def test_public_host_needs_no_acknowledgement(self):
        # A public literal never resolves private; no DNS lookup needed.
        self.assertEqual("https://levee.example.com",
                         validate_base_url("https://levee.example.com",
                                           allow_private=False))


class MainGuard(unittest.TestCase):

    def test_refuses_to_retry_without_yes(self):
        argv = ["--pg-dsn", "postgres://x", "--base-url", "http://127.0.0.1:8080",
                "--token", "t", "--run-id", "r1", "--target-host", "db-1",
                "--marker-file", "/tmp/m", "--allow-private-target"]
        self.assertEqual(2, main(argv))

    def test_private_target_without_flag_exits_2(self):
        argv = ["--pg-dsn", "postgres://x", "--base-url", "http://127.0.0.1:8080",
                "--token", "t", "--run-id", "r1", "--target-host", "db-1",
                "--marker-file", "/tmp/m", "--dry-run"]
        self.assertEqual(2, main(argv))


if __name__ == "__main__":
    unittest.main()
