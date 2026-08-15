#!/bin/bash
# compare.sh — Run LEVEE apply and Ansible baseline side-by-side and produce
# a comparison report.
#
# This script is part of the LEVEE benchmarking suite (W11-T091). It runs
# the same workflow through both LEVEE and Ansible, collects timing data, and
# prints a human-readable comparison table plus a JSON summary.
#
# Usage:
#   ./compare.sh -w <workflow.yaml> -i <inventory> -p <playbook.yaml> \
#                [-o <output.json>]
#
# Options:
#   -w <workflow.yaml>  LEVEE workflow file (required).
#   -i <inventory>      Ansible inventory file or comma-separated host list
#                       (required, shared by both tools).
#   -p <playbook.yaml>  Ansible playbook for the baseline run (required).
#   -o <output.json>    Path to write the JSON comparison result (optional;
#                       defaults to ./compare_result.json).
#
# Dependencies: levee (built binary), ansible-playbook, ansible_baseline.sh,
#               bc, jq (optional)

set -euo pipefail

# --- defaults ----------------------------------------------------------------

WORKFLOW=""
INVENTORY=""
PLAYBOOK=""
OUTPUT="compare_result.json"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- argument parsing --------------------------------------------------------

usage() {
    echo "Usage: $0 -w <workflow.yaml> -i <inventory> -p <playbook.yaml> [-o <output.json>]" >&2
    exit 1
}

while getopts ":w:i:p:o:" opt; do
    case "${opt}" in
        w) WORKFLOW="${OPTARG}" ;;
        i) INVENTORY="${OPTARG}" ;;
        p) PLAYBOOK="${OPTARG}" ;;
        o) OUTPUT="${OPTARG}" ;;
        *) usage ;;
    esac
done
shift $((OPTIND - 1))

if [[ -z "${WORKFLOW}" || -z "${INVENTORY}" || -z "${PLAYBOOK}" ]]; then
    echo "Error: workflow (-w), inventory (-i), and playbook (-p) are required." >&2
    usage
fi

# --- locate levee binary -----------------------------------------------------

LEVEE_BIN=""
if [[ -x "${SCRIPT_DIR}/../../levee" ]]; then
    LEVEE_BIN="${SCRIPT_DIR}/../../levee"
elif command -v levee &>/dev/null; then
    LEVEE_BIN="$(command -v levee)"
else
    echo "Error: levee binary not found. Build with 'go build -o levee ./cmd/levee'." >&2
    exit 1
fi

# --- run LEVEE apply with timing ---------------------------------------------

echo "=== LEVEE apply ==="

LEVEE_START=$(date +%s%N)
set +e
LEVEE_OUTPUT=$("${LEVEE_BIN}" apply -w "${WORKFLOW}" -i "${INVENTORY}" 2>&1)
LEVEE_EXIT=$?
set -e
LEVEE_END=$(date +%s%N)

LEVEE_WALL_NS=$((LEVEE_END - LEVEE_START))
LEVEE_WALL_SECONDS=$(echo "scale=3; ${LEVEE_WALL_NS} / 1000000000" | bc -l 2>/dev/null \
    || echo "0")

echo "LEVEE exited with code ${LEVEE_EXIT} in ${LEVEE_WALL_SECONDS}s"

# --- run Ansible baseline ----------------------------------------------------

echo ""
echo "=== Ansible baseline ==="

ANSIBLE_JSON=$("${SCRIPT_DIR}/ansible_baseline.sh" -i "${INVENTORY}" -p "${PLAYBOOK}" -n "compare-run" 2>&1 || true)
ANSIBLE_EXIT=$(echo "${ANSIBLE_JSON}" | grep -oP '"exit_code":\s*\K\d+' 2>/dev/null || echo "1")
ANSIBLE_WALL=$(echo "${ANSIBLE_JSON}" | grep -oP '"wall_seconds":\s*\K[0-9.]+' 2>/dev/null || echo "0")
TARGET_COUNT=$(echo "${ANSIBLE_JSON}" | grep -oP '"target_count":\s*\K\d+' 2>/dev/null || echo "0")

echo "Ansible exited with code ${ANSIBLE_EXIT} in ${ANSIBLE_WALL}s"

# --- compute speedup ---------------------------------------------------------

SPEEDUP="N/A"
if [[ "${ANSIBLE_WALL}" != "0" && "${ANSIBLE_WALL}" != "" ]]; then
    SPEEDUP=$(echo "scale=2; ${ANSIBLE_WALL} / ${LEVEE_WALL_SECONDS}" | bc -l 2>/dev/null \
        || echo "N/A")
fi

# --- print comparison report -------------------------------------------------

TIMESTAMP=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

echo ""
echo "=========================================="
echo "  LEVEE vs Ansible Comparison Report"
echo "=========================================="
echo "  Timestamp:    ${TIMESTAMP}"
echo "  Targets:      ${TARGET_COUNT}"
echo "------------------------------------------"
printf "  %-12s %12s %12s\n" "Metric" "LEVEE" "Ansible"
echo "------------------------------------------"
printf "  %-12s %12s %12s\n" "Wall (s)" "${LEVEE_WALL_SECONDS}" "${ANSIBLE_WALL}"
printf "  %-12s %12s %12s\n" "Exit code" "${LEVEE_EXIT}" "${ANSIBLE_EXIT}"
printf "  %-12s %12s\n"    "Speedup"    "${SPEEDUP}x"
echo "=========================================="

# --- write JSON result -------------------------------------------------------

cat > "${OUTPUT}" <<EOF
{
  "timestamp":       "${TIMESTAMP}",
  "target_count":    ${TARGET_COUNT},
  "levee": {
    "wall_seconds":  ${LEVEE_WALL_SECONDS},
    "exit_code":     ${LEVEE_EXIT}
  },
  "ansible": {
    "wall_seconds":  ${ANSIBLE_WALL},
    "exit_code":     ${ANSIBLE_EXIT}
  },
  "speedup":         "${SPEEDUP}"
}
EOF

echo ""
echo "JSON result written to ${OUTPUT}"