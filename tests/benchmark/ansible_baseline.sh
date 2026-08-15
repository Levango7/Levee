#!/bin/bash
# ansible_baseline.sh — Run an Ansible playbook against a target inventory and
# collect timing baseline data in JSON format.
#
# This script is part of the LEVEE benchmarking suite (W11-T091). It runs
# ansible-playbook with the given playbook and inventory, measures wall-clock
# time, and outputs a JSON result that can be consumed by compare.sh.
#
# Usage:
#   ./ansible_baseline.sh -i <inventory> -p <playbook> [-n <note>]
#
# Options:
#   -i <inventory>  Path to the Ansible inventory file or comma-separated
#                   host list (required).
#   -p <playbook>   Path to the Ansible playbook YAML file (required).
#   -n <note>       Optional note included in the JSON output.
#
# Output:
#   A single JSON object written to stdout:
#   {
#     "tool":          "ansible",
#     "playbook":      "<playbook path>",
#     "inventory":     "<inventory path>",
#     "target_count":  <number of hosts>,
#     "wall_seconds":  <elapsed wall-clock time in seconds>,
#     "exit_code":     <ansible-playbook exit code>,
#     "note":          "<optional note>",
#     "timestamp":     "<ISO-8601 UTC timestamp>"
#   }
#
# Dependencies: ansible-playbook, jq (optional, for host counting), date

set -euo pipefail

# --- defaults ----------------------------------------------------------------

INVENTORY=""
PLAYBOOK=""
NOTE=""
TIMESTAMP=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# --- argument parsing --------------------------------------------------------

usage() {
    echo "Usage: $0 -i <inventory> -p <playbook> [-n <note>]" >&2
    exit 1
}

while getopts ":i:p:n:" opt; do
    case "${opt}" in
        i) INVENTORY="${OPTARG}" ;;
        p) PLAYBOOK="${OPTARG}" ;;
        n) NOTE="${OPTARG}" ;;
        *) usage ;;
    esac
done
shift $((OPTIND - 1))

if [[ -z "${INVENTORY}" || -z "${PLAYBOOK}" ]]; then
    echo "Error: inventory (-i) and playbook (-p) are required." >&2
    usage
fi

if [[ ! -f "${PLAYBOOK}" ]]; then
    echo "Error: playbook file not found: ${PLAYBOOK}" >&2
    exit 1
fi

# --- count targets -----------------------------------------------------------

TARGET_COUNT=0
if command -v ansible-inventory &>/dev/null; then
    # Use ansible-inventory to count hosts if available.
    TARGET_COUNT=$(ansible-inventory -i "${INVENTORY}" --list 2>/dev/null \
        | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    hosts = data.get('_meta', {}).get('hostvars', {})
    if not hosts:
        # Fallback: count items in top-level groups (exclude _meta).
        hosts = {k: v for k, v in data.items() if k != '_meta' and isinstance(v, dict) and 'hosts' in v}
        seen = set()
        for g in hosts.values():
            for h in g.get('hosts', []):
                seen.add(h)
        print(len(seen))
    else:
        print(len(hosts))
except Exception:
    print(0)
" 2>/dev/null || echo 0)
fi

# If ansible-inventory failed or is not available, try a rough count from
# a static inventory file.
if [[ "${TARGET_COUNT}" -eq 0 && -f "${INVENTORY}" ]]; then
    # Count non-empty, non-comment lines as a rough estimate.
    TARGET_COUNT=$(grep -cve '^\s*$' -e '^\s*#' "${INVENTORY}" 2>/dev/null || echo 0)
fi

# --- run ansible-playbook with timing ----------------------------------------

START_EPOCH=$(date +%s%N)

set +e
ANSIBLE_OUTPUT=$(ansible-playbook -i "${INVENTORY}" "${PLAYBOOK}" 2>&1)
EXIT_CODE=$?
set -e

END_EPOCH=$(date +%s%N)
WALL_NS=$((END_EPOCH - START_EPOCH))
WALL_SECONDS=$(echo "scale=3; ${WALL_NS} / 1000000000" | bc -l 2>/dev/null \
    || echo "0")

# --- output JSON -------------------------------------------------------------

cat <<EOF
{
  "tool":          "ansible",
  "playbook":      "${PLAYBOOK}",
  "inventory":     "${INVENTORY}",
  "target_count":  ${TARGET_COUNT},
  "wall_seconds":  ${WALL_SECONDS},
  "exit_code":     ${EXIT_CODE},
  "note":          "${NOTE}",
  "timestamp":     "${TIMESTAMP}"
}
EOF

exit ${EXIT_CODE}