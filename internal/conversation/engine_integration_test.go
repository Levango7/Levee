// engine_integration_test.go is an end-to-end integration test of the AI
// pipeline: a real ConversationEngine drives a real RecommendEngine (backed by
// a MockLLMClient and the default knowledge base) and a real DiagEngine. No
// network, no real LLM, no PG — the whole pipeline runs in-process and the
// assertions pin the observable behaviour every layer agrees on.
package conversation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/recommend"
)

// integrationEngine builds a ConversationEngine wired with a mock LLM and a
// bare DiagEngine — the full AI pipeline in-process. The DiagEngine has no
// collector/analyzer/prober (the log pipeline and health probe are skipped),
// but it still produces a DiagnosticReport with Target set, which is enough
// for the recommend engine to generate a recommendation.
func integrationEngine(t *testing.T) *ConversationEngine {
	t.Helper()

	// Mock LLM returns a realistic JSON fix proposal so the recommend engine
	// exercises the LLM path (not the knowledge-base fallback). The empty
	// key matches any prompt the recommend engine sends (the actual prompt
	// depends on the diagnosis report content).
	mockLLM := recommend.NewMockLLMClient()
	mockLLM.SetResponse("", `{
		"summary": "Restart the overloaded service and scale replicas",
		"approach": "Gracefully restart the service, then increase replica count",
		"risk_level": "medium",
		"confidence": 0.85,
		"steps": [{"action": "restart", "target": "order-svc"}],
		"pre_conditions": ["healthcheck passes"],
		"rollback_plan": "Scale replicas back to original and restart again",
		"alternatives": [{"summary": "Vertical scale CPU/memory", "approach": "Increase resource limits"}]
	}`)

	rec := recommend.NewRecommendEngine(recommend.RecommendEngineConfig{
		LLMClient: mockLLM,
	})

	diag := diagnosis.NewDiagEngine(diagnosis.DiagEngineConfig{})

	return NewConversationEngine(ConversationEngineConfig{
		Recommend: rec,
		Diagnose:  diag,
	})
}

// TestIntegration_DiagnoseFlow exercises the headline pipeline: /diagnose runs
// the diagnosis engine, drives the recommend engine (which calls the mock
// LLM), and lands the session in StateReviewing with a populated
// Recommendation.
func TestIntegration_DiagnoseFlow(t *testing.T) {
	e := integrationEngine(t)
	ctx := context.Background()

	sess, err := e.NewSession("operator-1")
	require.NoError(t, err)
	require.Equal(t, StateIdle, sess.GetState())

	// /diagnose triggers diagnosis + recommendation.
	reply, err := e.HandleMessage(ctx, sess.ID, "operator-1", "/diagnose order-svc")
	require.NoError(t, err)
	require.NotNil(t, reply)

	// The session should have transitioned through diagnosing → recommending
	// → reviewing, driven by the synchronous pipeline.
	assert.Equal(t, StateReviewing, sess.GetState(),
		"after /diagnose the session should be awaiting operator review")

	// The recommendation should be populated from the mock LLM response.
	rec := sess.GetRecommendation()
	require.NotNil(t, rec, "a recommendation must be available for review")
	assert.Equal(t, "Restart the overloaded service and scale replicas", rec.Summary)
	assert.Equal(t, "medium", string(rec.RiskLevel))
	assert.InDelta(t, 0.85, rec.Confidence, 0.001)
	assert.NotEmpty(t, rec.Approach)

	// The reply text should summarise the recommendation.
	assert.Contains(t, reply.Text, "建议摘要")
}

// TestIntegration_ApproveFlow verifies the review → executing transition when
// the operator approves the recommendation.
func TestIntegration_ApproveFlow(t *testing.T) {
	e := integrationEngine(t)
	ctx := context.Background()

	sess, err := e.NewSession("operator-1")
	require.NoError(t, err)

	_, err = e.HandleMessage(ctx, sess.ID, "operator-1", "/diagnose order-svc")
	require.NoError(t, err)
	require.Equal(t, StateReviewing, sess.GetState())

	// Operator approves.
	reply, err := e.HandleMessage(ctx, sess.ID, "operator-1", "执行")
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Equal(t, StateExecuting, sess.GetState(),
		"approving the recommendation should transition to executing")
	assert.Contains(t, reply.Text, "开始执行")
}

// TestIntegration_RejectFlow verifies the review → failed transition when the
// operator rejects the recommendation.
func TestIntegration_RejectFlow(t *testing.T) {
	e := integrationEngine(t)
	ctx := context.Background()

	sess, err := e.NewSession("operator-1")
	require.NoError(t, err)

	_, err = e.HandleMessage(ctx, sess.ID, "operator-1", "/diagnose order-svc")
	require.NoError(t, err)
	require.Equal(t, StateReviewing, sess.GetState())

	reply, err := e.HandleMessage(ctx, sess.ID, "operator-1", "拒绝")
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Equal(t, StateFailed, sess.GetState(),
		"rejecting the recommendation should transition to failed")
	assert.Contains(t, reply.Text, "已拒绝")
}

// TestIntegration_RecommendFromHistory exercises the /recommend command
// (without a prior /diagnose). The recommend engine synthesises a report from
// the session context and still produces a knowledge-base-backed suggestion.
func TestIntegration_RecommendFromHistory(t *testing.T) {
	e := integrationEngine(t)
	ctx := context.Background()

	sess, err := e.NewSession("operator-1")
	require.NoError(t, err)

	reply, err := e.HandleMessage(ctx, sess.ID, "operator-1", "/recommend")
	require.NoError(t, err)
	require.NotNil(t, reply)

	// The session should be reviewing a recommendation.
	assert.Equal(t, StateReviewing, sess.GetState())
	rec := sess.GetRecommendation()
	require.NotNil(t, rec)
	assert.NotEmpty(t, rec.Summary)
}

// TestIntegration_HelpCommand verifies the /help command returns the command
// reference without changing state.
func TestIntegration_HelpCommand(t *testing.T) {
	e := integrationEngine(t)
	ctx := context.Background()

	sess, err := e.NewSession("operator-1")
	require.NoError(t, err)

	reply, err := e.HandleMessage(ctx, sess.ID, "operator-1", "/help")
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Contains(t, reply.Text, "/diagnose")
	assert.Contains(t, reply.Text, "/recommend")
	assert.Equal(t, StateIdle, sess.GetState(),
		"/help must not change the session state")
}

// TestIntegration_KnowledgeBaseFallback verifies that when the mock LLM is not
// configured (pure knowledge-base mode), the recommend engine still produces
// a usable recommendation — the LLM path is an enhancement, not a dependency.
func TestIntegration_KnowledgeBaseFallback(t *testing.T) {
	rec := recommend.NewRecommendEngine(recommend.RecommendEngineConfig{})
	diag := diagnosis.NewDiagEngine(diagnosis.DiagEngineConfig{})
	e := NewConversationEngine(ConversationEngineConfig{
		Recommend: rec,
		Diagnose:  diag,
	})
	ctx := context.Background()

	sess, err := e.NewSession("operator-1")
	require.NoError(t, err)

	reply, err := e.HandleMessage(ctx, sess.ID, "operator-1", "/diagnose order-svc")
	require.NoError(t, err)
	require.NotNil(t, reply)

	// Even without an LLM, the knowledge base should yield a recommendation.
	assert.Equal(t, StateReviewing, sess.GetState())
	rec2 := sess.GetRecommendation()
	require.NotNil(t, rec2, "knowledge-base mode must still produce a recommendation")
	assert.NotEmpty(t, rec2.Summary)
}
