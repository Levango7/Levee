#!/bin/bash
# test_gate.sh — 10-minute test gate for LEVEE.
#
# This script runs the full test suite, static analysis, and format check
# for the LEVEE project. The entire gate must complete within 10 minutes
# (600 seconds). If any check fails or the time budget is exceeded, the
# gate reports FAIL; otherwise it reports PASS.
#
# This script is part of the LEVEE CI pipeline (W11-T093).
#
# Usage:
#   ./test_gate.sh [-t <timeout_seconds>]
#
# Options:
#   -t <timeout_seconds>  Maximum wall-clock time in seconds (default: 600).
#
# Exit codes:
#   0 — PASS (all checks passed within the time budget)
#   1 — FAIL (one or more checks failed or timed out)

set -euo pipefail

# --- defaults ----------------------------------------------------------------

TIMEOUT_SECONDS=600
GATE_START=$(date +%s)
FAILURES=0

# --- argument parsing --------------------------------------------------------

while getopts ":t:" opt; do
    case "${opt}" in
        t) TIMEOUT_SECONDS="${OPTARG}" ;;
        *) echo "Usage: $0 [-t <timeout_seconds>]" >&2; exit 1 ;;
    esac
done

# --- helper functions --------------------------------------------------------

remaining_seconds() {
    local now=$(date +%s)
    local elapsed=$((now - GATE_START))
    echo $((TIMEOUT_SECONDS - elapsed))
}

check_time_budget() {
    local remaining
    remaining=$(remaining_seconds)
    if [[ "${remaining}" -le 0 ]]; then
        echo "FAIL: time budget exceeded (${TIMEOUT_SECONDS}s)" >&2
        exit 1
    fi
}

run_check() {
    local name="$1"
    shift

    local remaining
    remaining=$(remaining_seconds)
    if [[ "${remaining}" -le 0 ]]; then
        echo "[TIMEOUT] ${name} — time budget exceeded before start" >&2
        FAILURES=$((FAILURES + 1))
        return 1
    fi

    echo ""
    echo "=== ${name} ==="
    echo "Time remaining: ${remaining}s"

    local check_start
    check_start=$(date +%s)

    set +e
    "$@"
    local exit_code=$?
    set -e

    local check_end
    check_end=$(date +%s)
    local check_elapsed=$((check_end - check_start))

    if [[ ${exit_code} -eq 0 ]]; then
        echo "[PASS] ${name} (${check_elapsed}s)"
    else
        echo "[FAIL] ${name} (${check_elapsed}s, exit=${exit_code})" >&2
        FAILURES=$((FAILURES + 1))
    fi

    return ${exit_code}
}

# --- preamble ----------------------------------------------------------------

echo "=========================================="
echo "  LEVEE 10-Minute Test Gate"
echo "=========================================="
echo "  Timeout: ${TIMEOUT_SECONDS}s"
echo "  Started: $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
echo "=========================================="

# --- check 1: go test --------------------------------------------------------

run_check "go-test" go test ./... -count=1 -timeout "${TIMEOUT_SECONDS}s" || true

check_time_budget

# --- check 2: go vet ---------------------------------------------------------

run_check "go-vet" go vet ./... || true

check_time_budget

# --- check 3: gofmt ----------------------------------------------------------

# gofmt -l lists files that need formatting; an empty output means all files
# are properly formatted.
FMT_OUTPUT=""
set +e
FMT_OUTPUT=$(gofmt -l .)
set -e

echo ""
echo "=== gofmt ==="
if [[ -z "${FMT_OUTPUT}" ]]; then
    echo "[PASS] gofmt (all files formatted)"
else
    echo "[FAIL] gofmt — the following files need formatting:" >&2
    echo "${FMT_OUTPUT}" >&2
    FAILURES=$((FAILURES + 1))
fi

# --- summary -----------------------------------------------------------------

GATE_END=$(date +%s)
GATE_ELAPSED=$((GATE_END - GATE_START))

echo ""
echo "=========================================="
echo "  Test Gate Summary"
echo "=========================================="
echo "  Total time:  ${GATE_ELAPSED}s / ${TIMEOUT_SECONDS}s"
echo "  Failures:    ${FAILURES}"
echo "=========================================="

if [[ "${FAILURES}" -eq 0 && "${GATE_ELAPSED}" -le "${TIMEOUT_SECONDS}" ]]; then
    echo ""
    echo "PASS"
    exit 0
else
    if [[ "${GATE_ELAPSED}" -gt "${TIMEOUT_SECONDS}" ]]; then
        echo "FAIL: time budget exceeded" >&2
    else
        echo "FAIL: ${FAILURES} check(s) failed" >&2
    fi
    echo ""
    echo "FAIL"
    exit 1
fi