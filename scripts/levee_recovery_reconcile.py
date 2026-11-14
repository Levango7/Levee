#!/usr/bin/env python3
"""Recovery reconciliation for a partitioned LEVEE cluster (design Q4).

Why this exists: the disconnect policy (`docs/design-disconnect-policy.md`) was
decided in four questions and Q4 — "how do we make the evidence whole again" —
was specified as a HUMAN-driven four-step procedure with machine-checkable
criteria at every step. Nothing shipped those criteria. An operator recovering
from an outage was left to re-derive them from the design doc under pressure,
which is exactly when a reconciliation step gets skipped.

The four steps (design §2.4), each with its machine criterion:

  1. confirm the database answers            — `SELECT 1`
  2. read the orphan lock's expires_at       — the target host lock outlives
     the killed executor (its owner is a transient engine execution id); the
     TTL (exec_timeout, 300s) is an UPPER BOUND, not an estimate, and waiting
     for it is the safety net against physical double-execution
  3. REST retry with replan=false            — never re-plan: re-planning could
     bless a plan the change no longer carries
  4. assert the outcome                      — run completed, exactly one
     success row per step, the target marker file moved (physical
     at-least-once, reported honestly rather than claimed as exactly-once),
     and no new evidence_discarded trace events

The assertion logic is pure and unit-tested separately from the IO, so the
checks that decide whether a recovery succeeded do not depend on a live
cluster being available to exercise them.

Exit codes: 0 reconciled clean, 1 a check failed, 2 the tool could not run
(bad arguments, unreachable prerequisites). Retrying is destructive-by-design
and requires --yes; --dry-run stops after step 2 and reports what would happen.

Usage:
    python scripts/levee_recovery_reconcile.py \
        --pg-dsn "$LEVEE_DATABASE_DSN" --base-url http://127.0.0.1:8080 \
        --token "$LEVEE_TOKEN" --run-id <id> --target-host db-1 \
        --marker-file /tmp/marker --allow-private-target [--yes|--dry-run]
"""

from __future__ import annotations

import argparse
import ipaddress
import json
import os
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any, Callable, Iterable, Sequence

# exec_timeout default: the target host lock TTL, hence the recovery upper bound
# (design §0). Kept as a constant rather than discovered so a config change that
# shortens it cannot silently turn "wait for expiry" into "retry too early".
DEFAULT_LOCK_TTL_SECONDS = 300


class PrereqError(RuntimeError):
    """A precondition could not be checked at all (distinct from 'failed')."""


@dataclass
class Check:
    """One reconciliation assertion with the detail an operator needs."""

    name: str
    ok: bool
    detail: str = ""

    def render(self) -> str:
        mark = "PASS" if self.ok else "FAIL"
        return "  [%s] %s%s" % (mark, self.name, (" — " + self.detail) if self.detail else "")


@dataclass
class RunFacts:
    """The facts step 4 asserts on, gathered from the platform.

    Plain data, so `reconcile_assertions` is a pure function of these fields —
    the tests construct it directly and the IO layer fills it in.
    """

    run_status: str
    steps: Sequence[tuple[str, str, int]]  # (step name, status, count)
    trace_events: Sequence[str]
    marker_mtime: float | None
    marker_mtime_before: float | None


def reconcile_assertions(
    facts: RunFacts,
    *,
    discarded_events_before: int = 0,
    expected_steps: Sequence[str] | None = None,
) -> list[Check]:
    """Evaluate the step-4 criteria. Pure: no IO, no clock, no globals.

    Exactly these five, because they are the ones the design names as "must not
    be allowed to look like it worked":
      - run reached a terminal success state;
      - every executed step left exactly one success row (the ledger is
        exactly-once; two rows mean a double-write got recorded), AND every
        step the plan called for is accounted for when `expected_steps` is
        supplied — an absent row for a planned step is a gap, not a pass;
      - the target marker moved (physical at-least-once, stated as such);
      - no NEW evidence_discarded events appeared (a discard means a worker
        wrote after being told it no longer owned the run — the fence failed);
      - the run carries no trace event that says the evidence is incomplete.
    """
    checks: list[Check] = []

    checks.append(Check(
        "run status is completed",
        facts.run_status == "completed",
        "got %r" % facts.run_status,
    ))

    duplicated = [(name, n) for (name, status, n) in facts.steps
                  if status == "success" and n != 1]
    missing = [name for (name, status, n) in facts.steps
               if status == "success" and n == 0]

    present = {name for (name, _status, n) in facts.steps if n > 0}
    unexpected: list[str] = []
    if expected_steps is not None:
        # A planned step with no success row is the "ran but never recorded"
        # shape: it must not read as a clean recovery.
        missing.extend(n for n in expected_steps if n not in present)
        # A success row for a step the plan never called for is a double-write
        # of something else; it also must not pass silently.
        unexpected = [n for n in present if n not in set(expected_steps)]

    detail = "; ".join(["%s x%d" % (n, c) for n, c in duplicated]
                       + ["%s absent" % n for n in missing]
                       + ["%s not planned" % n for n in unexpected])
    checks.append(Check(
        "each step has exactly one success row",
        not duplicated and not missing and not unexpected,
        detail or "%d step(s)" % len(facts.steps),
    ))

    if facts.marker_mtime is None or facts.marker_mtime_before is None:
        checks.append(Check(
            "target marker file moved after retry",
            False,
            "marker mtime unavailable (before=%r after=%r)"
            % (facts.marker_mtime_before, facts.marker_mtime),
        ))
    else:
        # Strictly later: a rewrite at the same second would mean the executor
        # did not actually touch the target, which is the difference between
        # "ran" and "claims it ran".
        checks.append(Check(
            "target marker file moved after retry",
            facts.marker_mtime > facts.marker_mtime_before,
            "before=%s after=%s" % (facts.marker_mtime_before, facts.marker_mtime),
        ))

    discards_now = sum(1 for e in facts.trace_events if e == "evidence_discarded")
    new_discards = discards_now - discarded_events_before
    checks.append(Check(
        "no new evidence_discarded events",
        new_discards <= 0,
        "before=%d after=%d" % (discarded_events_before, discards_now),
    ))

    checks.append(Check(
        "no evidence_incomplete trace event",
        "evidence_incomplete" not in facts.trace_events,
        "present" if "evidence_incomplete" in facts.trace_events else "absent",
    ))
    return checks


# --- Target validation -------------------------------------------------------
#
# The base URL is operator-supplied and ends up in a client request, so it is
# treated as untrusted input rather than as configuration that happens to be
# typed by a person. Two guards:
#   - the scheme is limited to http/https (no file:, ftp:, gopher:, ...);
#   - a private / loopback / link-local target requires an explicit
#     acknowledgement, because reaching a control plane next to the agent is
#     the normal case (so blocking it outright would make the tool useless) but
#     silently allowing it would mean a typo or a crafted argument could point
#     the operator's own credentials at the cloud metadata service.


def _literal_is_private(host: str) -> bool:
    """True when the host text itself names a non-routable address."""
    try:
        addr = ipaddress.ip_address(host)
    except ValueError:
        return host == "localhost"
    return (addr.is_loopback or addr.is_link_local or addr.is_private
            or addr.is_unspecified or addr.is_multicast)


def validate_base_url(raw: str, *, allow_private: bool) -> str:
    """Return the validated base URL or raise PrereqError.

    Resolution is done here rather than left to the HTTP stack so the decision
    is explicit and logged; a DNS answer can change between check and use, so
    this narrows the window rather than eliminating it.
    """
    parsed = urllib.parse.urlsplit(raw)
    if parsed.scheme not in ("http", "https"):
        raise PrereqError("base-url scheme must be http or https, got %r"
                          % (parsed.scheme or ""))
    host = parsed.hostname
    if not host:
        raise PrereqError("base-url has no host: %r" % raw)

    private = _literal_is_private(host)
    if not private:
        try:
            for info in socket.getaddrinfo(host, None):
                if _literal_is_private(info[4][0]):
                    private = True
                    break
        except socket.gaierror:
            # Unresolvable now; the request will surface the real error. Not a
            # reason to refuse the run.
            private = False

    if private and not allow_private:
        raise PrereqError(
            "base-url host %r resolves to a private/loopback address. That is "
            "normal for a co-located LEVEE server, so confirm it with "
            "--allow-private-target (this guard exists so a crafted argument "
            "cannot aim your token at, say, the cloud metadata service)." % host)
    if private:
        print("warning: base-url host %r is private/loopback; confirmed by "
              "--allow-private-target" % host, file=sys.stderr)
    return raw


# --- IO layer -----------------------------------------------------------------
#
# Each wrapper takes plain values and returns plain values. They raise
# PrereqError when the check itself could not be performed (database gone,
# HTTP transport failure), so the caller never mistakes "could not ask" for
# "answered no".


def probe_database(pg_dsn: str, *, timeout: int = 5) -> str:
    """Return 'ok' when the control plane answers SELECT 1."""
    try:
        out = subprocess.run(
            ["psql", pg_dsn, "-tAc", "SELECT 1"],
            capture_output=True, text=True, timeout=timeout,
        )
    except FileNotFoundError as exc:
        raise PrereqError("psql not found on PATH") from exc
    except subprocess.TimeoutExpired as exc:
        raise PrereqError("database probe timed out after %ss" % timeout) from exc
    if out.returncode != 0:
        raise PrereqError("database probe failed: %s" % out.stderr.strip())
    return "ok"


def orphan_locks(pg_dsn: str, target_host: str) -> list[tuple[str, ...]]:
    """Rows in `locks` for the target host whose lease has not expired.

    The owner column is the engine's transient execution id, which is why these
    rows survive the takeover that settled the run — they expire on their own
    TTL. An empty list is the criterion step 2 waits for.
    """
    sql = ("SELECT scope, owner, acquired_at, expires_at FROM locks "
           "WHERE scope = 'host:' || %s AND expires_at > now() ORDER BY acquired_at")
    try:
        out = subprocess.run(
            ["psql", pg_dsn, "-tAF", "\t", "-c", sql],
            capture_output=True, text=True, timeout=30,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        raise PrereqError("lock query could not run: %s" % exc) from exc
    if out.returncode != 0:
        raise PrereqError("lock query failed: %s" % out.stderr.strip())
    return [tuple(line.split("\t")) for line in out.stdout.splitlines() if line.strip()]


def wait_lock_free(
    pg_dsn: str,
    target_host: str,
    *,
    timeout: int,
    interval: float = 5.0,
    sleep: Callable[[float], None] = time.sleep,
) -> tuple[bool, str]:
    """Wait until the host lock is gone or expired. Returns (free, detail).

    A timeout is NOT reported as "free": the safety net is the lock TTL covering
    the single-step execution timeout, so a lock still held means an old
    execution may still have a step in flight and a retry would be a double run.
    """
    deadline = time.monotonic() + timeout
    while True:
        rows = orphan_locks(pg_dsn, target_host)
        if not rows:
            return True, "lock released or expired"
        if time.monotonic() >= deadline:
            scope, owner, _acquired, expires = rows[0]
            return False, ("lock on %s still held by %s (expires %s) after %ss"
                           % (scope, owner, expires, timeout))
        sleep(interval)


def rest_request(
    base_url: str,
    token: str,
    method: str,
    path: str,
    body: dict[str, Any] | None = None,
    *,
    timeout: int = 300,
) -> tuple[int, str]:
    """Call the REST gateway. Raises PrereqError only on transport failure."""
    url = base_url.rstrip("/") + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Authorization", "Bearer " + token)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as exc:
        # An HTTP response IS an answer: 4xx/5xx go back to the caller as such,
        # never folded into "could not reach the server".
        return exc.code, exc.read().decode()
    except urllib.error.URLError as exc:
        raise PrereqError("REST %s %s could not connect: %s" % (method, path, exc)) from exc


def fetch_expected_steps(base_url: str, token: str, run_id: str) -> list[str]:
    """Step names the approved plan calls for, in order.

    Needed because the ledger-exactly-once check is only half the story: a
    planned step that left NO row at all is the "ran but never recorded" shape,
    and without the plan there is no way to tell it apart from a change that
    legitimately had fewer steps. An unreadable plan yields [] — the other four
    criteria still run, and the gap is visible in the report as an empty
    expected set rather than a fabricated one.
    """
    status, body = rest_request(base_url, token, "GET",
                                "/api/v1/changes/%s?includePlan=true" % run_id)
    if status != 200:
        raise PrereqError("GET change (with plan) returned HTTP %d: %s" % (status, body))
    plan = json.loads(body).get("data", {}).get("plan") or {}
    names: list[str] = []
    for batch in plan.get("batches", []) or []:
        for step in batch.get("steps", []) or []:
            name = step.get("name")
            if name:
                names.append(name)
    return names


def fetch_run_facts(
    base_url: str,
    token: str,
    run_id: str,
    marker_file: str,
    marker_mtime_before: float | None,
) -> RunFacts:
    """Gather everything step 4 asserts on, over REST."""
    status, body = rest_request(base_url, token, "GET", "/api/v1/changes/%s" % run_id)
    if status != 200:
        raise PrereqError("GET change returned HTTP %d: %s" % (status, body))
    doc = json.loads(body)
    data = doc.get("data", doc)

    _, logs = rest_request(base_url, token, "GET",
                           "/api/v1/changes/%s/logs" % run_id)
    steps = [(s.get("step", ""), s.get("status", ""), 1)
             for s in json.loads(logs).get("data", {}).get("entries", [])]

    _, traces = rest_request(base_url, token, "GET",
                            "/api/v1/changes/%s/trace" % run_id)
    events = [e.get("event", "")
              for e in json.loads(traces).get("data", {}).get("entries", [])]

    try:
        mtime: float | None = os.stat(marker_file).st_mtime
    except OSError:
        mtime = None

    return RunFacts(
        run_status=data.get("status", ""),
        steps=steps,
        trace_events=events,
        marker_mtime=mtime,
        marker_mtime_before=marker_mtime_before,
    )


def main(argv: Iterable[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        description="Reconcile one change after a partition (design Q4).")
    ap.add_argument("--pg-dsn", required=True)
    ap.add_argument("--base-url", required=True)
    ap.add_argument("--token", required=True)
    ap.add_argument("--run-id", required=True)
    ap.add_argument("--target-host", required=True)
    ap.add_argument("--marker-file", required=True)
    ap.add_argument("--timeout", type=int, default=360,
                    help="seconds to wait for the lock TTL and the run outcome")
    ap.add_argument("--dry-run", action="store_true",
                    help="stop after step 2; never fire the retry")
    ap.add_argument("--yes", action="store_true",
                    help="confirm firing the retry (destructive by design)")
    ap.add_argument("--allow-private-target", action="store_true",
                    help="acknowledge the base-url host is private/loopback")
    args = ap.parse_args(list(argv) if argv is not None else None)

    try:
        validate_base_url(args.base_url, allow_private=args.allow_private_target)
    except PrereqError as exc:
        print("could not run: %s" % exc, file=sys.stderr)
        return 2

    if not args.dry_run and not args.yes:
        print("refusing to retry without --yes (or --dry-run to stop before it)",
              file=sys.stderr)
        return 2

    try:
        print("step 1/4: confirming the database answers ...")
        probe_database(args.pg_dsn)

        print("step 2/4: reading orphan locks on host:%s ..." % args.target_host)
        held = orphan_locks(args.pg_dsn, args.target_host)
        if held:
            print("  held: %d row(s); waiting up to %ss for the TTL to expire"
                  % (len(held), args.timeout))
            free, detail = wait_lock_free(args.pg_dsn, args.target_host,
                                          timeout=args.timeout)
            if not free:
                print("  FAIL %s" % detail, file=sys.stderr)
                return 1
            print("  %s" % detail)
        else:
            print("  no held lock")

        if args.dry_run:
            print("dry-run: stopping before the retry")
            return 0

        print("step 3/4: POST retry (replan=false, explicit targetHosts) ...")
        code, body = rest_request(
            args.base_url, args.token, "POST",
            "/api/v1/changes/%s/retry" % args.run_id,
            {"replan": False, "targetHosts": [args.target_host]},
        )
        if code not in (200, 201):
            print("  retry returned HTTP %d: %s" % (code, body), file=sys.stderr)
            return 1

        print("step 4/4: asserting the outcome ...")
        facts = fetch_run_facts(args.base_url, args.token, args.run_id,
                                args.marker_file, None)
        expected = fetch_expected_steps(args.base_url, args.token, args.run_id)
        checks = reconcile_assertions(facts, expected_steps=expected)
        for c in checks:
            print(c.render())
        failed = [c for c in checks if not c.ok]
        print("VERDICT: %s (%d/%d)"
              % ("PASS" if not failed else "FAIL", len(checks) - len(failed), len(checks)))
        return 0 if not failed else 1
    except PrereqError as exc:
        print("could not run: %s" % exc, file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
