#!/usr/bin/env python3
"""Fail-closed release dependency gate for GitHub Actions tag workflows.

A v* tag starts both CI and Release. This gate waits for the CI workflow run
created for the exact tag + commit, then verifies that run's aggregate
GitHub Actions check completed successfully. A successful branch check for the
same commit is deliberately not accepted.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import time
from typing import Any, Callable

REQUIRED_CHECK_NAME = "check (all jobs passed)"
REPOSITORY_RE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
TAG_RE = re.compile(r"^v[0-9A-Za-z._-]+$")
SHA_RE = re.compile(r"^[0-9a-fA-F]{40}$")


class GateError(RuntimeError):
    """A non-retryable release gate failure."""


def log(message: str) -> None:
    print(f"[release-ci-gate] {message}", flush=True)


def select_ci_run(document: dict[str, Any], *, ref: str, sha: str) -> dict[str, Any] | None:
    """Return the newest CI run for the exact ref and commit, if visible yet."""
    runs = [
        run
        for run in document.get("workflow_runs", [])
        if run.get("head_branch") == ref
        and str(run.get("head_sha", "")).lower() == sha.lower()
    ]
    if not runs:
        return None
    return max(runs, key=lambda run: int(run.get("id", 0)))


def select_aggregate_check(document: dict[str, Any], *, run_id: int) -> dict[str, Any] | None:
    """Return the newest aggregate check created by this exact workflow run."""
    fragment = f"/actions/runs/{run_id}/"
    checks = [
        check
        for check in document.get("check_runs", [])
        if check.get("name") == REQUIRED_CHECK_NAME
        and (check.get("app") or {}).get("slug") == "github-actions"
        and fragment in str(check.get("details_url", ""))
    ]
    if not checks:
        return None
    return max(checks, key=lambda check: int(check.get("id", 0)))


class GitHubAPI:
    """Thin gh api adapter; no GitHub network access in unit tests."""

    def __init__(self, *, repository: str, token: str, executable: str = "gh") -> None:
        self.repository = repository
        self.token = token
        self.executable = executable

    def get(self, endpoint: str) -> dict[str, Any]:
        env = os.environ.copy()
        if self.token:
            env["GH_TOKEN"] = self.token
            env["GITHUB_TOKEN"] = self.token
        proc = subprocess.run(
            [self.executable, "api", endpoint],
            check=False,
            capture_output=True,
            text=True,
            env=env,
        )
        if proc.returncode != 0:
            detail = proc.stderr.strip() or proc.stdout.strip() or f"exit {proc.returncode}"
            raise GateError(f"GitHub API failed for {endpoint}: {detail}")
        try:
            value = json.loads(proc.stdout)
        except json.JSONDecodeError as exc:
            raise GateError(f"GitHub API returned invalid JSON for {endpoint}: {exc}") from exc
        if not isinstance(value, dict):
            raise GateError(f"GitHub API returned a non-object payload for {endpoint}")
        return value


def wait_for_release_ci(
    api: GitHubAPI,
    *,
    ref: str,
    sha: str,
    workflow_file: str,
    timeout_seconds: int,
    poll_seconds: int,
    clock: Callable[[], float] = time.monotonic,
    sleeper: Callable[[float], None] = time.sleep,
) -> None:
    started = clock()
    while clock() - started <= timeout_seconds:
        runs = api.get(
            f"repos/{api.repository}/actions/workflows/{workflow_file}/runs"
            f"?head_sha={sha}&event=push&per_page=100"
        )
        run = select_ci_run(runs, ref=ref, sha=sha)
        if run is None:
            log(f"waiting for CI run for tag={ref} commit={sha[:12]}")
        else:
            run_id = int(run["id"])
            status = str(run.get("status") or "")
            conclusion = str(run.get("conclusion") or "none")
            log(
                f"found CI run {run_id} status={status} conclusion={conclusion} "
                f"url={run.get('html_url', '')}"
            )
            if status != "completed":
                log(f"CI run is still {status}")
            elif conclusion != "success":
                raise GateError(
                    f"CI run {run_id} concluded {conclusion}, expected success"
                )
            else:
                checks = api.get(
                    f"repos/{api.repository}/commits/{sha}/check-runs?per_page=100"
                )
                check = select_aggregate_check(checks, run_id=run_id)
                if check is None:
                    log(f"CI run succeeded; waiting for aggregate check {REQUIRED_CHECK_NAME!r}")
                else:
                    check_id = int(check["id"])
                    check_status = str(check.get("status") or "")
                    check_conclusion = str(check.get("conclusion") or "none")
                    log(
                        f"aggregate check id={check_id} status={check_status} "
                        f"conclusion={check_conclusion} url={check.get('details_url', '')}"
                    )
                    if check_status != "completed":
                        log(f"aggregate check is still {check_status}")
                    elif check_conclusion != "success":
                        raise GateError(
                            "aggregate check concluded "
                            f"{check_conclusion}, expected success"
                        )
                    else:
                        log("PASS: exact tag CI run and aggregate check succeeded")
                        return
        if poll_seconds:
            sleeper(poll_seconds)
    raise GateError(
        f"timed out after {timeout_seconds}s waiting for successful CI "
        f"for tag={ref} commit={sha}"
    )



def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository", default=os.environ.get("GITHUB_REPOSITORY", ""))
    parser.add_argument("--sha", default=os.environ.get("GITHUB_SHA", ""))
    parser.add_argument("--ref", default=os.environ.get("GITHUB_REF_NAME", ""))
    parser.add_argument("--token", default=os.environ.get("GITHUB_TOKEN", ""))
    parser.add_argument("--gh", default=os.environ.get("GH_BIN", "gh"))
    parser.add_argument(
        "--workflow", default=os.environ.get("CI_WORKFLOW_FILE", "ci.yml")
    )
    parser.add_argument(
        "--timeout", type=int, default=int(os.environ.get("CI_TIMEOUT_SECONDS", "3600"))
    )
    parser.add_argument(
        "--poll", type=int, default=int(os.environ.get("CI_POLL_SECONDS", "15"))
    )
    return parser.parse_args(argv)


def validate(args: argparse.Namespace) -> None:
    if not REPOSITORY_RE.fullmatch(args.repository):
        raise GateError("invalid GITHUB_REPOSITORY")
    if not SHA_RE.fullmatch(args.sha):
        raise GateError("GITHUB_SHA must be a full 40-character commit SHA")
    if not TAG_RE.fullmatch(args.ref):
        raise GateError("release gate only accepts v* tags")
    if args.timeout < 0 or args.poll < 0:
        raise GateError("timeout and poll must be non-negative")


def main(argv: list[str] | None = None) -> int:
    try:
        args = parse_args(argv)
        validate(args)
        api = GitHubAPI(repository=args.repository, token=args.token, executable=args.gh)
        wait_for_release_ci(
            api,
            ref=args.ref,
            sha=args.sha,
            workflow_file=args.workflow,
            timeout_seconds=args.timeout,
            poll_seconds=args.poll,
        )
        return 0
    except GateError as exc:
        print(f"[release-ci-gate] ERROR: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
