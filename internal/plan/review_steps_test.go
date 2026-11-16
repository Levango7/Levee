package plan

// review_steps_test.go — 共享审核核心的行为必须与计划期门禁一致。
//
// 这层存在的理由：复算路径（`levee sqlreview --change`）与计划期门禁
// （internal/wiring/sqlreview_gate.go）必须对同一段 SQL 给出同一个结论。
// 一旦两边各写一份聚合逻辑，「规则变了」与「实现漂移了」就再也分不开——
// 而这两件事对操作员意味着完全不同的处置。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/sqlreview"
)

func TestReviewStepsAggregatesTheWorstVerdict(t *testing.T) {
	summary, err := ReviewSteps([]SQLStepInput{
		{Step: "safe", SQL: "SELECT id FROM accounts WHERE id = 1"},
		{Step: "risky", SQL: "UPDATE accounts SET tier='gold' WHERE id = 42"},
	})
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, string(sqlreview.VerdictWarn), summary.Verdict,
		"one warn step makes the aggregate warn")
	assert.Equal(t, 0, summary.FailCount)
	assert.GreaterOrEqual(t, summary.WarnCount, 1)
	for _, f := range summary.Findings {
		assert.NotEmpty(t, f.Step, "every finding must name the step it came from")
		assert.NotEmpty(t, f.Rule)
	}
}

func TestReviewStepsReturnsNilWhenThereIsNothingToReview(t *testing.T) {
	summary, err := ReviewSteps(nil)
	require.NoError(t, err)
	assert.Nil(t, summary, "no reviewable SQL is nil — never 'reviewed and clean'")

	summary, err = ReviewSteps([]SQLStepInput{{Step: "empty", SQL: "   "}})
	require.NoError(t, err)
	assert.Nil(t, summary)
}

func TestReviewStepsReportsAFailVerdictWithoutErroring(t *testing.T) {
	// The core does not decide what a fail means: the plan gate refuses to
	// mint an artifact over it, the re-check reports it. What the core must
	// NOT do is swallow it.
	summary, err := ReviewSteps([]SQLStepInput{
		{Step: "danger", SQL: "DELETE FROM accounts"},
	})
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, string(sqlreview.VerdictFail), summary.Verdict)
	assert.Greater(t, summary.FailCount, 0)
}

// The predicate that decides which steps are SQL at all. Three callers share
// it (the plan gate, the ticket, the re-check), and pg.query is the case that
// proved sharing matters: every copy of the rule said "mysql" and PostgreSQL
// steps went unreviewed.
func TestReviewableSQL(t *testing.T) {
	cases := []struct {
		name           string
		module, action string
		args           map[string]any
		want           string
		ok             bool
	}{
		{"mysql query", "mysql", "query", map[string]any{"sql": "DELETE FROM t"}, "DELETE FROM t", true},
		{"pg query", "pg", "query", map[string]any{"sql": "DELETE FROM t"}, "DELETE FROM t", true},
		{"empty sql", "mysql", "query", map[string]any{"sql": "   "}, "", false},
		{"missing sql", "mysql", "query", map[string]any{"database": "d"}, "", false},
		{"sql not a string", "mysql", "query", map[string]any{"sql": 42}, "", false},
		{"pt_osc carries a fragment, not a statement", "mysql", "pt_osc",
			map[string]any{"table": "t", "alter": "ADD COLUMN c INT"}, "", false},
		{"replica switch", "mysql", "replica_switch", map[string]any{"new_primary": "db-2"}, "", false},
		{"other module", "shell", "query", map[string]any{"sql": "rm -rf /"}, "", false},
		{"query action elsewhere", "k8s", "query", map[string]any{"sql": "SELECT 1"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, ok := ReviewableSQL(c.module, c.action, c.args)
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.want, sql)
		})
	}
}

// The summary carries ONE impact surface, unioned across steps: a table
// touched by any step is touched by the change. And it exists only when there
// is a surface at all — an empty impact on a change with no SQL would read as
// "reviewed, and it touches nothing".
func TestReviewStepsMergesTheImpactSurface(t *testing.T) {
	summary, err := ReviewSteps([]SQLStepInput{
		{Step: "a", SQL: "DELETE FROM orders"},
		{Step: "b", SQL: "UPDATE orders SET note='x' WHERE id=1; SELECT * FROM users"},
	})
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.NotNil(t, summary.Impact)
	assert.False(t, summary.Impact.Incomplete)

	byName := map[string]sqlreview.TableImpact{}
	for _, te := range summary.Impact.Tables {
		byName[te.Table] = te
	}
	require.Len(t, byName, 2)
	assert.Equal(t, []string{"delete", "update"}, byName["orders"].Operations,
		"the union keeps both verbs against the table both steps touch")
	assert.True(t, byName["orders"].WholeTable)
	assert.Equal(t, []string{"select"}, byName["users"].Operations)

	// An unread statement makes the whole surface a lower bound.
	summary, err = ReviewSteps([]SQLStepInput{
		{Step: "a", SQL: "DELETE FROM orders"},
		{Step: "b", SQL: "SELECT TOP (10) * FROM users"},
	})
	require.NoError(t, err)
	assert.True(t, summary.Impact.Incomplete,
		"one unread statement makes the union a lower bound, and the surface must say so")

	// No SQL surface at all => no impact surface at all.
	summary, err = ReviewSteps([]SQLStepInput{{Step: "x", SQL: " "}})
	require.NoError(t, err)
	assert.Nil(t, summary)
}

func TestReviewStepsFailsAnUnparsableStatement(t *testing.T) {
	// The engine's own rule: a statement the grammar cannot parse is a FAIL
	// (SR-01), never a pass and never a silent skip. The core must carry that
	// verdict through unchanged — a re-check that "passed" an unparsable
	// statement would be a gate with a hole in it.
	summary, err := ReviewSteps([]SQLStepInput{{Step: "broken", SQL: "SELECT FROM WHERE"}})
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, string(sqlreview.VerdictFail), summary.Verdict)
	assert.Equal(t, 1, summary.FailCount)
}
