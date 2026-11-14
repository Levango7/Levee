package main

// cmd_sqlreview_change_test.go — `levee sqlreview --change <id>` 的行为。
//
// 三个必须成立的事实：
//
//  1. 复算用与计划期门禁**同一份**聚合核心（plan.ReviewSteps），所以两边
//     对同一段 SQL 必须给出同一个结论；
//  2. 复算结论与「计划时固化的结论」是**两个数**，报告必须并排给出，不能
//     只报一个（只报一个的话，读者不知道那是当时的决定还是现在的测量）；
//  3. 比不出来的时候（没有计划 / 计划不可读 / 没有 SQL 面）必须如实说
//     「比不了」，不能给一个看起来正常的结论。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
)

// sqlReviewChangeStore builds a store with one change whose plan artifact
// carries a frozen review verdict and one mysql.query step.
func sqlReviewChangeStore(t *testing.T, runID string, sqlText string, frozen *plan.SQLReviewSummary) state.Store {
	t.Helper()
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "sqlreview-change.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: runID, WorkflowName: "db-change.yaml", Status: "approved",
		Creator: "alice", CreatedAt: now, UpdatedAt: now,
	}))
	p := &plan.Plan{
		ID: runID,
		Batches: []plan.Batch{{Index: 0, Targets: []string{"db-1"}, Steps: []plan.PlanStep{{
			Name: "migrate", Module: "mysql", Action: "query",
			Args: map[string]any{"sql": sqlText},
		}}}},
		TotalTargets: 1,
		SQLReview:    frozen,
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.NoError(t, store.UpdateRunPlan(ctx, runID, string(raw), "v2:abc", now))
	return store
}

func TestCompareSQLReviewDetectsAddedAndRemovedFindings(t *testing.T) {
	frozen := &plan.SQLReviewSummary{
		Verdict: "warn", WarnCount: 1,
		Findings: []plan.SQLReviewFinding{{
			Step: "migrate", Rule: "SR-08", Severity: "warn", Statement: 1, Message: "old",
		}},
	}
	fresh := &plan.SQLReviewSummary{
		Verdict: "warn", WarnCount: 1,
		Findings: []plan.SQLReviewFinding{{
			Step: "migrate", Rule: "SR-12", Severity: "warn", Statement: 2, Message: "new",
		}},
	}

	diffs, aligned := compareSQLReview(frozen, fresh)
	assert.False(t, aligned)
	assert.Len(t, diffs, 2)
	assert.Contains(t, diffs[0]+diffs[1], "新增 SR-12")
	assert.Contains(t, diffs[0]+diffs[1], "消失 SR-08")
}

func TestCompareSQLReviewReportsAlignment(t *testing.T) {
	summary := &plan.SQLReviewSummary{
		Verdict: "warn", WarnCount: 1,
		Findings: []plan.SQLReviewFinding{{
			Step: "migrate", Rule: "SR-08", Severity: "warn", Statement: 1, Message: "same",
		}},
	}
	diffs, aligned := compareSQLReview(summary, summary)
	assert.True(t, aligned)
	assert.Empty(t, diffs)
}

func TestCompareSQLReviewTreatsAMissingVerdictAsNotAligned(t *testing.T) {
	// 「没有结论可比」不是「一致」——把它算成一致，读者会以为核对过了。
	diffs, aligned := compareSQLReview(nil, &plan.SQLReviewSummary{Verdict: "pass"})
	assert.False(t, aligned)
	assert.Empty(t, diffs)
}

func TestPlanSQLStepsOnlyTakesMySQLQuerySteps(t *testing.T) {
	p := &plan.Plan{Batches: []plan.Batch{{Steps: []plan.PlanStep{
		{Name: "svc", Module: "svc", Action: "restart"},
		{Name: "q", Module: "mysql", Action: "query", Args: map[string]any{"sql": "SELECT 1"}},
		{Name: "empty", Module: "mysql", Action: "query", Args: map[string]any{"sql": "  "}},
		{Name: "notsql", Module: "mysql", Action: "query", Args: map[string]any{"sql": 42}},
	}}}}
	steps := planSQLSteps(p)
	require.Len(t, steps, 1)
	assert.Equal(t, "q", steps[0].Step)
}

func TestSQLReviewChangeReportsTheFrozenAndFreshVerdictsSideBySide(t *testing.T) {
	// The frozen verdict in this fixture IS the engine's own verdict for that
	// SQL (the plan-time gate produced it), so the two must agree — and the
	// comparison must say so instead of inventing a difference.
	const sqlText = "UPDATE accounts SET tier='gold' WHERE id = 42"
	engineVerdict, err := plan.ReviewSteps([]plan.SQLStepInput{{Step: "migrate", SQL: sqlText}})
	require.NoError(t, err)
	require.NotNil(t, engineVerdict)

	store := sqlReviewChangeStore(t, "chg-sr-1", sqlText, engineVerdict)
	fresh, err := plan.ReviewSteps(planSQLSteps(mustPlan(t, store, "chg-sr-1")))
	require.NoError(t, err)

	diffs, aligned := compareSQLReview(engineVerdict, fresh)
	assert.True(t, aligned, "diffs=%v", diffs)
	assert.Empty(t, diffs)
}

func TestSQLReviewChangeDetectsDriftBetweenTheFrozenVerdictAndTheRules(t *testing.T) {
	// A plan approved under an older rule set carries a finding the current
	// rules do not produce (or the other way round). The comparison must
	// surface it: an operator looking at the ticket sees the OLD verdict, and
	// only this command says whether the rules still agree with it.
	store := sqlReviewChangeStore(t, "chg-sr-2",
		"UPDATE accounts SET tier='gold' WHERE id = 42",
		&plan.SQLReviewSummary{
			Verdict: "warn", WarnCount: 1,
			Findings: []plan.SQLReviewFinding{{
				Step: "migrate", Rule: "SR-08", Severity: "warn", Statement: 1, Message: "frozen under an older rule set",
			}},
		})
	fresh, err := plan.ReviewSteps(planSQLSteps(mustPlan(t, store, "chg-sr-2")))
	require.NoError(t, err)

	diffs, aligned := compareSQLReview(
		&plan.SQLReviewSummary{
			Verdict: "warn", WarnCount: 1,
			Findings: []plan.SQLReviewFinding{{
				Step: "migrate", Rule: "SR-08", Severity: "warn", Statement: 1, Message: "frozen under an older rule set",
			}},
		}, fresh)
	assert.False(t, aligned)
	assert.NotEmpty(t, diffs)
	assert.Contains(t, diffs[0]+diffs[1], "消失 SR-08")
}

// mustPlan reads a run's plan artifact out of a store.
func mustPlan(t *testing.T, store state.Store, runID string) *plan.Plan {
	t.Helper()
	run, err := store.GetRun(context.Background(), runID)
	require.NoError(t, err)
	require.NotNil(t, run)
	var p plan.Plan
	require.NoError(t, json.Unmarshal([]byte(run.PlanJSON), &p))
	return &p
}
