#!/usr/bin/env bash
# sql-review-chain.sh — the §8.6 step-4 demo: 高危拦截 + 强制审批 + 事后审计追溯.
#
# Runs the whole SQL-review chain offline against a throwaway data dir:
#
#   act 0  the rule engine standalone (levee sqlreview)
#   act 1  high-risk SQL is BLOCKED at plan time, before any approval exists
#   act 2  warn-level SQL passes but is stamped on the plan artifact; the run
#          needs a two-person approval (initiator excluded) before apply
#   act 3  the audit report shows the verdict, the plan hash the approval
#          signed, and the approval chain
#
# No host is ever contacted: apply runs status-only because this build wires
# no execution engine. The only binary needed is levee (LEVEE_BIN or PATH).
set -euo pipefail

LEEVE="${LEVEE_BIN:-levee}"
command -v "$LEEVE" >/dev/null 2>&1 || {
  echo "sql-review-chain: levee binary not found (build: go build -o levee ./cmd/levee, or set LEVEE_BIN)" >&2
  exit 2
}

DEMO_DIR="$(mktemp -d)"
mkdir -p "$DEMO_DIR/data" "$DEMO_DIR/templates"
trap 'echo "sql-review-chain: command failed at line $LINENO (exit $?)" >&2' ERR
export LEVEE_SERVER_DATA_DIR="$DEMO_DIR/data"
export LEVEE_ACTOR="${LEVEE_ACTOR:-demo-operator}"
# Keyed seals from the first audit write onward. Setting this env var only
# halfway through a run's life seals early records one way and re-reads them
# another, and the sealer then refuses with "hash chain exists but is broken".
export LEVEE_AUDIT_HMAC_KEY="${LEVEE_AUDIT_HMAC_KEY:-demo-key-not-for-production-32b}"

say() { printf '\n=== %s ===\n' "$*"; }
pass() { printf 'PASS: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# assert_exit <expected> <actual> <what>
assert_exit() {
  [ "$1" -eq "$2" ] || fail "$3: expected exit $1, got $2"
}
# assert_contains <file> <pattern> <what>
assert_contains() {
  grep -qF -- "$2" "$1" || fail "$3: expected output to contain '$2' (see $1)"
}

# --- inventory: one pretend database host, never contacted -------------------
cat >"$DEMO_DIR/hosts.yaml" <<'EOF'
groups:
  - name: db
targets:
  - address: db-01.prod
    port: 3306
    channel_type: mysql
    group: db
    labels:
      env: prod
EOF

say "inventory + templates"
"$LEEVE" target import --file "$DEMO_DIR/hosts.yaml" >"$DEMO_DIR/00-import.txt" 2>&1
assert_contains "$DEMO_DIR/00-import.txt" "Imported 1 (created)" "target import"

cat >"$DEMO_DIR/tmpl-risky.yaml" <<'EOF'
name: orders-purge
version: "1.0"
description: 全表清空订单（演示高危拦截）
target:
  type: host
  hosts: ["db-01.prod"]
approval:
  level: high
  approvers: [alice, bob]
steps:
  - name: purge-orders
    action: mysql.query
    args:
      sql: "DELETE FROM orders;"
EOF

cat >"$DEMO_DIR/tmpl-warn.yaml" <<'EOF'
name: orders-schema
version: "1.0"
description: 订单遗留表结构变更（演示强制审批）
target:
  type: host
  hosts: ["db-01.prod"]
approval:
  level: high
  approvers: [alice, bob]
steps:
  - name: drop-legacy-table
    action: mysql.query
    args:
      sql: "DROP TABLE orders_legacy;"
EOF

"$LEEVE" template create --name orders-purge \
  --content "$(cat "$DEMO_DIR/tmpl-risky.yaml")" >"$DEMO_DIR/01-tmpl-risky.txt" 2>&1
"$LEEVE" template create --name orders-schema \
  --content "$(cat "$DEMO_DIR/tmpl-warn.yaml")" >"$DEMO_DIR/02-tmpl-warn.txt" 2>&1
pass "inventory imported, two templates registered"

# --- act 0: the rule engine on its own ---------------------------------------
say "act 0 — the rule engine, standalone"
rc=0
"$LEEVE" sqlreview --sql "DELETE FROM orders;" >"$DEMO_DIR/act0-sqlreview.txt" 2>&1 || rc=$?
assert_exit 1 "$rc" "act 0: sqlreview must exit 1 on a fail verdict"
assert_contains "$DEMO_DIR/act0-sqlreview.txt" "SR-02" "act 0: rule id in output"
assert_contains "$DEMO_DIR/act0-sqlreview.txt" "verdict: fail (1 fail, 0 warn)" "act 0: verdict line"
pass "DELETE without WHERE is scored fail by SR-02 before any workflow exists"

# --- act 1: high-risk SQL never reaches an approver ---------------------------
say "act 1 — 高危拦截: plan time refuses the fail verdict"
RUN_RISKY=$("$LEEVE" new orders-purge 2>/dev/null | awk '/^run_id:/{print $2}')
[ -n "$RUN_RISKY" ] || fail "act 1: levee new returned no run_id"
rc=0
"$LEEVE" plan "$RUN_RISKY" --targets db-01.prod >"$DEMO_DIR/act1-plan.txt" 2>&1 || rc=$?
assert_exit 1 "$rc" "act 1: plan must be refused (exit 1)"
assert_contains "$DEMO_DIR/act1-plan.txt" "SR-02" "act 1: refusal names the rule"
assert_contains "$DEMO_DIR/act1-plan.txt" "DELETE without WHERE" "act 1: refusal names the message"
pass "plan refused with SR-02 named — the run stops before any approval exists"

# --- act 2: warn SQL passes, but two people must sign -------------------------
say "act 2 — 强制审批: warn passes, plan_hash binds it, high level needs two"
RUN_WARN=$("$LEEVE" new orders-schema 2>/dev/null | awk '/^run_id:/{print $2}')
[ -n "$RUN_WARN" ] || fail "act 2: levee new returned no run_id"
rc=0
"$LEEVE" plan "$RUN_WARN" --targets db-01.prod >"$DEMO_DIR/act2-plan.txt" 2>&1 || rc=$?
assert_exit 0 "$rc" "act 2: warn-level plan must pass"
assert_contains "$DEMO_DIR/act2-plan.txt" "plan persisted and hash-bound" "act 2: plan persisted"

rc=0
"$LEEVE" apply "$RUN_WARN" >"$DEMO_DIR/act2-apply-early.txt" 2>&1 || rc=$?
assert_exit 4 "$rc" "act 2: apply before approval must be refused (exit 4)"

rc=0
"$LEEVE" approve "$RUN_WARN" --as alice >"$DEMO_DIR/act2-approve-alice.txt" 2>&1 || rc=$?
assert_exit 0 "$rc" "act 2: alice's vote must record"
assert_contains "$DEMO_DIR/act2-approve-alice.txt" "still pending" "act 2: one of two votes is not enough"

rc=0
"$LEEVE" apply "$RUN_WARN" >"$DEMO_DIR/act2-apply-mid.txt" 2>&1 || rc=$?
assert_exit 4 "$rc" "act 2: apply while quorum pending must stay refused (exit 4)"

rc=0
"$LEEVE" approve "$RUN_WARN" --as bob --comment "第二批准，窗口内执行" >"$DEMO_DIR/act2-approve-bob.txt" 2>&1 || rc=$?
assert_exit 0 "$rc" "act 2: bob's vote must record"
assert_contains "$DEMO_DIR/act2-approve-bob.txt" "approved by bob" "act 2: quorum reached"
# The initiator cannot be one of the two approvers; alice+bob are, demo-operator is not.
rc=0
"$LEEVE" apply "$RUN_WARN" >"$DEMO_DIR/act2-apply.txt" 2>&1 || rc=$?
assert_exit 0 "$rc" "act 2: apply after quorum"
assert_contains "$DEMO_DIR/act2-apply.txt" "status-only" "act 2: status-only apply marker"
pass "warn verdict stamped, two approvers signed, apply executed status-only"

# --- act 3: afterwards, the trail answers for itself --------------------------
say "act 3 — 事后审计追溯: the report shows verdict + plan hash + chain"
"$LEEVE" audit report --since "$(date -u +%Y-%m-%d)" \
  --output "$DEMO_DIR/report.html" >"$DEMO_DIR/act3-report.txt" 2>&1
assert_contains "$DEMO_DIR/report.html" "SR-07" "act 3: report carries the warn finding"
assert_contains "$DEMO_DIR/report.html" "v2:" "act 3: report carries the plan hash"
assert_contains "$DEMO_DIR/report.html" "alice" "act 3: report carries approver alice"
assert_contains "$DEMO_DIR/report.html" "bob" "act 3: report carries approver bob"
assert_contains "$DEMO_DIR/report.html" "exclude_initiator" "act 3: report shows initiator exclusion"
pass "HTML report binds warn verdict, plan hash and both approvals"

# The trace chain is empty by design for a status-only run: no batches ran,
# so no step evidence exists. The commands say so instead of claiming intact.
rc=0
"$LEEVE" audit verify "$RUN_WARN" >"$DEMO_DIR/act3-verify.txt" 2>&1 || rc=$?
assert_exit 1 "$rc" "act 3: verify on a trace-less run must not claim success"
assert_contains "$DEMO_DIR/act3-verify.txt" "no traces found" "act 3: truthful empty state"
grep -q "no trace records" "$DEMO_DIR/report.html" || fail "act 3: report must state the empty chain truthfully"
pass "empty trace chain reported as empty, not papered over"

say "done"
printf 'all acts passed; artifacts in %s\n' "$DEMO_DIR"
printf '  act0 rule engine:  %s\n' "$DEMO_DIR/act0-sqlreview.txt"
printf '  act1 blocked plan: %s\n' "$DEMO_DIR/act1-plan.txt"
printf '  act2 approvals:    %s / %s\n' "$DEMO_DIR/act2-approve-alice.txt" "$DEMO_DIR/act2-approve-bob.txt"
printf '  act3 report:       %s\n' "$DEMO_DIR/report.html"
