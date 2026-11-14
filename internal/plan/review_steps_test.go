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
