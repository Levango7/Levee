// sqlreview_gate_test.go pins the plan-time SQL review gate: a fail verdict
// blocks the plan at the same funnel position as the change-window gate (no
// artifact, no approval, no audit-side hash to bless), a warn verdict passes
// but is stamped onto the artifact where plan_hash binds it, and a workflow
// with no reviewable SQL surface carries no verdict at all (nil, never a
// synthetic pass).
package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/sqlreview"
	"github.com/nexus/levee/internal/state"
)

// sqlWorkflow renders a workflow whose single step runs the given SQL through
// mysql.query — the only action that carries raw SQL text into the plan.
func sqlWorkflow(name, sql string) string {
	return fmt.Sprintf(`name: %s
target:
  type: host
  hosts: ["web-1"]
steps:
  - name: audit-data
    action: mysql.query
    args:
      sql: %q
`, name, sql)
}

func seedSQLRun(t *testing.T, src string) (*Engine, state.Store, string) {
	t.Helper()
	eng, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-sql", src)
	return eng, store, "run-sql"
}

// TestGeneratePlan_RefusedWhenSQLReviewFails is the gate's core refusal: an
// unbounded DELETE must not reach approval, and the refusal must leave no
// artifact behind (a persisted plan would be invisible drift).
func TestGeneratePlan_RefusedWhenSQLReviewFails(t *testing.T) {
	eng, store, runID := seedSQLRun(t, sqlWorkflow("del", "DELETE FROM users"))

	p, stored, err := eng.GeneratePlan(context.Background(), runID, []string{"web-1"})
	require.Error(t, err, "a fail review verdict must block the plan")
	assert.Nil(t, p)
	assert.Nil(t, stored)
	assert.True(t, errors.Is(err, sqlreview.ErrReviewFailed),
		"the refusal must be matchable as a review refusal, got: %v", err)
	assert.Contains(t, err.Error(), "SR-02")
	assert.Contains(t, err.Error(), "audit-data", "the operator needs the step anchor")

	run, err := store.GetRun(context.Background(), runID)
	require.NoError(t, err)
	assert.Empty(t, run.PlanJSON, "a refused plan leaves no artifact on the run")
	assert.Empty(t, run.PlanHash)
}

// TestGeneratePlan_SQLReviewWarnPassesAndIsStamped pins the warn half: DROP
// TABLE scores a warn in the review engine's own calibration (SR-07), the
// plan is produced, and the verdict rides into the artifact the hash covers.
func TestGeneratePlan_SQLReviewWarnPassesAndIsStamped(t *testing.T) {
	eng, _, runID := seedSQLRun(t, sqlWorkflow("drop", "DROP TABLE users"))

	_, stored, err := eng.GeneratePlan(context.Background(), runID, []string{"web-1"})
	require.NoError(t, err, "a warn verdict passes the gate")

	var p plan.Plan
	require.NoError(t, json.Unmarshal([]byte(stored.JSON), &p))
	require.NotNil(t, p.SQLReview)
	assert.Equal(t, string(sqlreview.VerdictWarn), p.SQLReview.Verdict)
	assert.NotZero(t, p.SQLReview.WarnCount)

	var anchored bool
	for _, f := range p.SQLReview.Findings {
		if f.Rule == "SR-07" {
			anchored = true
			assert.Equal(t, "audit-data", f.Step, "findings carry the step anchor")
		}
	}
	assert.True(t, anchored, "the SR-07 finding must be on the artifact")

	assert.Contains(t, stored.JSON, "sql_review", "the verdict must be inside the persisted artifact")
	var back plan.Plan
	require.NoError(t, json.Unmarshal([]byte(stored.JSON), &back))
	assert.Equal(t, plan.ComputeHash(&back), stored.Hash,
		"the artifact round-trips to exactly the hash the approval will sign")
}

func TestGeneratePlan_SQLReviewCleanVerdictStamped(t *testing.T) {
	eng, _, runID := seedSQLRun(t, sqlWorkflow("clean",
		"CREATE TABLE IF NOT EXISTS t1 (id INT PRIMARY KEY) engine=InnoDB"))

	_, stored, err := eng.GeneratePlan(context.Background(), runID, []string{"web-1"})
	require.NoError(t, err)
	var p plan.Plan
	require.NoError(t, json.Unmarshal([]byte(stored.JSON), &p))
	require.NotNil(t, p.SQLReview)
	assert.Equal(t, string(sqlreview.VerdictPass), p.SQLReview.Verdict)
	assert.Zero(t, p.SQLReview.FailCount)
	assert.Zero(t, p.SQLReview.WarnCount)
}

// TestGeneratePlan_NoSQLSurfaceLeavesVerdictNil guards the nil-vs-clean
// distinction: a workflow without a mysql.query step must not grow a
// synthetic pass verdict, which would rewrite every existing plan's hash.
func TestGeneratePlan_NoSQLSurfaceLeavesVerdictNil(t *testing.T) {
	eng, _, runID := planForWindow(t, testWorkflowYAML)

	_, stored, err := eng.GeneratePlan(context.Background(), runID, []string{"web-1"})
	require.NoError(t, err)
	assert.NotContains(t, stored.JSON, "sql_review")

	var p plan.Plan
	require.NoError(t, json.Unmarshal([]byte(stored.JSON), &p))
	assert.Nil(t, p.SQLReview, "no reviewable surface means no verdict, not a pass")
}

// TestGeneratePlan_SQLReviewSeesResolvedInputs pins that the gate scores the
// RESOLVED text (params substituted), not the template: a statement that is
// only dangerous once the parameter lands must be caught with the substituted
// text in hand. Asserting SR-02 (not SR-01 "unparseable") is what proves the
// substitution happened before the review ran.
func TestGeneratePlan_SQLReviewSeesResolvedInputs(t *testing.T) {
	const src = `name: sql-input-gate
input:
  - name: tbl
    type: string
target:
  type: host
  hosts: ["web-1"]
steps:
  - name: audit-data
    action: mysql.query
    args:
      sql: "DELETE FROM {{input.tbl}}"
`
	eng, store := newTestEngine(t, "web-1")
	seedRunWithParams(t, store, "run-sql-in", src, `{"tbl":"users"}`)

	_, _, err := eng.GeneratePlan(context.Background(), "run-sql-in", []string{"web-1"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, sqlreview.ErrReviewFailed))
	assert.Contains(t, err.Error(), "SR-02",
		"the verdict must come from the resolved text, not the template")
}

// TestPlanChange_SQLReviewRefusalIsFailedPreconditionAndAudited pins the RPC
// shape: a failed review is a refusal the caller can act on (not an Internal),
// and the refusal itself is hash-chained evidence.
func TestPlanChange_SQLReviewRefusalIsFailedPreconditionAndAudited(t *testing.T) {
	eng, store, runID := seedSQLRun(t, sqlWorkflow("del", "DELETE FROM users"))
	svc := grpc.NewChangeService(store, eng.Adapter(), nil, nil)

	_, err := svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId:    runID,
		TargetHosts: []string{"web-1"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Convert(err).Code(),
		"a review refusal must not surface as Internal, got: %v", err)
	assert.Contains(t, err.Error(), "SR-02")

	rows := sqlReviewAuditRows(t, store, runID)
	require.Len(t, rows, 1, "the refusal itself must be audited")
	assert.Equal(t, state.AuditActionSQLReview, rows[0].Action)
	assert.Equal(t, state.AuditResultFailed, rows[0].Result)
	assert.NotEmpty(t, rows[0].Actor)

	run, err := store.GetRun(context.Background(), runID)
	require.NoError(t, err)
	assert.Empty(t, run.PlanHash, "a refused review leaves nothing to approve")
}

// TestPlanChange_SQLReviewPassIsAuditedAgainstThePlanHash pins the success
// shape: one row, result passed, Target naming the artifact the verdict
// scored — the join key the compliance report uses to connect verdict →
// plan_hash → approval chain.
func TestPlanChange_SQLReviewPassIsAuditedAgainstThePlanHash(t *testing.T) {
	eng, store, runID := seedSQLRun(t, sqlWorkflow("clean",
		"CREATE TABLE IF NOT EXISTS t1 (id INT PRIMARY KEY) engine=InnoDB"))
	svc := grpc.NewChangeService(store, eng.Adapter(), nil, nil)

	_, err := svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId:    runID,
		TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)

	rows := sqlReviewAuditRows(t, store, runID)
	require.Len(t, rows, 1)
	assert.Equal(t, state.AuditActionSQLReview, rows[0].Action)
	assert.Equal(t, state.AuditResultPassed, rows[0].Result)

	run, err := store.GetRun(context.Background(), runID)
	require.NoError(t, err)
	assert.Equal(t, run.PlanHash, rows[0].Target,
		"the audit row must name the artifact whose hash the approval signs")
}

// TestPlanChange_SQLReviewWarnStillRecordsPassed pins the row semantics: a
// warn verdict passes the gate, so the row records passed; the warn detail
// lives in the plan artifact, not in a vocabulary column.
func TestPlanChange_SQLReviewWarnStillRecordsPassed(t *testing.T) {
	eng, store, runID := seedSQLRun(t, sqlWorkflow("drop", "DROP TABLE users"))
	svc := grpc.NewChangeService(store, eng.Adapter(), nil, nil)

	_, err := svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId:    runID,
		TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)

	rows := sqlReviewAuditRows(t, store, runID)
	require.Len(t, rows, 1)
	assert.Equal(t, state.AuditResultPassed, rows[0].Result)

	run, err := store.GetRun(context.Background(), runID)
	require.NoError(t, err)
	var p plan.Plan
	require.NoError(t, json.Unmarshal([]byte(run.PlanJSON), &p))
	require.NotNil(t, p.SQLReview)
	assert.Equal(t, string(sqlreview.VerdictWarn), p.SQLReview.Verdict,
		"the warn detail survives in the artifact the row's Target names")
}

// TestPlanChange_SQLReviewDryRunWritesNoEvidence pins the preview contract:
// a dry run plans (and therefore reviews) but persists nothing and audits
// nothing — a preview is not a governance event.
func TestPlanChange_SQLReviewDryRunWritesNoEvidence(t *testing.T) {
	eng, store, runID := seedSQLRun(t, sqlWorkflow("clean",
		"CREATE TABLE IF NOT EXISTS t1 (id INT PRIMARY KEY) engine=InnoDB"))
	svc := grpc.NewChangeService(store, eng.Adapter(), nil, nil)

	p, err := svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId:    runID,
		TargetHosts: []string{"web-1"},
		DryRun:      true,
	})
	require.NoError(t, err)
	require.NotNil(t, p)

	assert.Empty(t, sqlReviewAuditRows(t, store, runID), "a preview must not audit itself")
	run, err := store.GetRun(context.Background(), runID)
	require.NoError(t, err)
	assert.Empty(t, run.PlanJSON)
	assert.Empty(t, run.PlanHash)
}

// TestSQLReviewGateHasExactlyOneEnforcementPoint is the structural guard the
// window gate keeps for its own funnel claim: the review is consulted by the
// plan funnel alone, and the rollback path never regenerates plans.
func TestSQLReviewGateHasExactlyOneEnforcementPoint(t *testing.T) {
	hits := map[string]int{}
	rollbackBody := ""

	for _, root := range []string{"../../internal", "../../cmd"} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel := relativePOSIX(path)
			if n := strings.Count(string(src), "reviewWorkflowSQL(wf)"); n > 0 {
				hits[rel] = n
			}
			if strings.HasSuffix(rel, "internal/wiring/run.go") {
				rollbackBody = functionBody(string(src), "func (e *Engine) rollbackChange")
			}
			return nil
		})
	}

	assert.Equal(t, map[string]int{"internal/wiring/plan.go": 1}, hits,
		"the review gate must be consulted by exactly one production call site: the plan funnel")
	require.NotEmpty(t, rollbackBody, "rollbackChange must be found to prove it stays outside the gate")
	assert.NotContains(t, rollbackBody, "GeneratePlan",
		"rollback loads the stored plan; regenerating it would put recovery behind a review gate")
	assert.NotContains(t, rollbackBody, "reviewWorkflowSQL")
}

// sqlReviewAuditRows returns the run's audit rows whose action is the SQL
// review action, in chain order.
func sqlReviewAuditRows(t *testing.T, store state.Store, runID string) []*state.Audit {
	t.Helper()
	audits, err := store.ListAudits(context.Background(), state.AuditFilter{RunID: runID})
	require.NoError(t, err)
	var rows []*state.Audit
	for _, a := range audits {
		if a.Action == state.AuditActionSQLReview {
			rows = append(rows, a)
		}
	}
	return rows
}

// TestReviewWorkflowSQLAggregatesWorstVerdict pins the aggregation the gate
// uses when a workflow carries several mysql.query steps: the worst verdict
// wins, and every step's findings are anchored to its own name. The dirty
// step here scores a warn (SR-07) so the aggregate itself is returned; the
// fail half of the aggregation is covered by the refusal tests above.
func TestReviewWorkflowSQLAggregatesWorstVerdict(t *testing.T) {
	src := `name: multi-sql
target:
  type: host
  hosts: ["web-1"]
steps:
  - name: clean-step
    action: mysql.query
    args:
      sql: "CREATE TABLE IF NOT EXISTS t1 (id INT PRIMARY KEY) engine=InnoDB"
  - name: dirty-step
    action: mysql.query
    args:
      sql: "DROP TABLE t1"
  - name: not-sql
    action: shell.exec
    args:
      cmd: "echo hi"
`
	wf, err := dsl.NewParser().ParseBytes([]byte(src))
	require.NoError(t, err)

	summary, err := reviewWorkflowSQL(wf)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, string(sqlreview.VerdictWarn), summary.Verdict,
		"the worst verdict across steps must win")
	assert.Equal(t, 1, summary.WarnCount)

	var dirtyAnchored, cleanAnchored bool
	for _, f := range summary.Findings {
		if f.Step == "dirty-step" && f.Rule == "SR-07" {
			dirtyAnchored = true
		}
		if f.Step == "clean-step" {
			cleanAnchored = true
		}
	}
	assert.True(t, dirtyAnchored, "the warn finding is anchored to its step")
	assert.False(t, cleanAnchored, "a clean statement produces no finding")
}

// TestReviewWorkflowSQLSkipsUnreviewableArgs pins the carrier boundary: a
// mysql.query step without a string sql argument is skipped (typing the
// argument is the executor's contract), and a nil workflow is a no-op.
func TestReviewWorkflowSQLSkipsUnreviewableArgs(t *testing.T) {
	src := `name: no-sql
target:
  type: host
  hosts: ["web-1"]
steps:
  - name: typed-wrong
    action: mysql.query
    args:
      sql: 42
  - name: missing
    action: mysql.query
`
	wf, err := dsl.NewParser().ParseBytes([]byte(src))
	require.NoError(t, err)

	summary, err := reviewWorkflowSQL(wf)
	require.NoError(t, err)
	assert.Nil(t, summary, "no reviewable surface means no summary")

	nilSummary, err := reviewWorkflowSQL(nil)
	require.NoError(t, err)
	assert.Nil(t, nilSummary)
}

// TestReviewWorkflowSQLEngineErrorFailsClosed pins the honesty posture: a
// batch the engine itself cannot split is "cannot judge", which the gate
// treats as a refusal, not a pass.
func TestReviewWorkflowSQLEngineErrorFailsClosed(t *testing.T) {
	src := `name: engine-error
target:
  type: host
  hosts: ["web-1"]
steps:
  - name: broken
    action: mysql.query
    args:
      sql: "SELECT (((("
`
	wf, err := dsl.NewParser().ParseBytes([]byte(src))
	require.NoError(t, err)

	_, err = reviewWorkflowSQL(wf)
	require.Error(t, err)
	assert.True(t, errors.Is(err, sqlreview.ErrReviewFailed),
		"an unparseable batch must refuse the plan, got: %v", err)
}
