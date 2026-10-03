// diagnosis_reasoner_test.go — the optional LLM refinement of Diagnose.
// The reasoner is driven by a scripted LLM (fixed reply per call), so these
// tests exercise the real ReasoningEngine: the stability gate, the
// convergence status and the enrich/degrade branches of DiagnosisService.
package grpc

import (
	"context"
	"errors"
	"testing"

	"github.com/nexus/levee/internal/diagnosis/llm_diag"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/recommend"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedLLM is a minimal recommend.LLMClient: it returns the same reply
// for every call (or a fixed error), which is exactly what the reasoning
// loop needs for deterministic convergence tests. A repeated identical
// hypothesis passes the stability gate on the second turn.
type scriptedLLM struct {
	reply string
	err   error
}

func (s *scriptedLLM) Chat(_ context.Context, _ []recommend.LLMMessage) (string, error) {
	return s.reply, s.err
}

func (s *scriptedLLM) ChatWithSystem(_ context.Context, _ string, _ []recommend.LLMMessage) (string, error) {
	return s.reply, s.err
}

func (s *scriptedLLM) Name() string { return "scripted" }

const scriptedConvergedReply = `{"hypothesis":"connection pool exhaustion in checkout-api", "confidence":0.9, "converged":true, "suggestions":["raise pool size","restart checkout-api"]}`

func newReasonerForTest(t *testing.T, llm recommend.LLMClient) *llm_diag.ReasoningEngine {
	t.Helper()
	r, err := llm_diag.NewReasoningEngine(llm_diag.ReasoningEngineConfig{LLM: llm})
	require.NoError(t, err)
	return r
}

func TestDiagnoseService_ReasonerRefinesOnlyWhenConverged(t *testing.T) {
	svc := NewDiagnosisService(newTestDiagEngine(), nil).
		WithReasoner(newReasonerForTest(t, &scriptedLLM{reply: scriptedConvergedReply}))

	resp, err := svc.Diagnose(context.Background(), &pb.DiagnoseRequest{Target: "host-01"})
	require.NoError(t, err)

	assert.Contains(t, resp.GetRootCause(), "connection pool exhaustion",
		"a converged reasoning result must refine the rule-based root cause")
	assert.InDelta(t, 0.9, resp.GetConfidence(), 0.001,
		"the model's (corroborated) confidence is written back")
	assert.Contains(t, resp.GetRecommendations(), "raise pool size",
		"model suggestions become the report's recommendations")

	// The enriched report is what the cache serves, not the raw rule-based one.
	got, err := svc.GetDiagnosis(context.Background(), &pb.GetDiagnosisRequest{Id: resp.GetId()})
	require.NoError(t, err)
	assert.Contains(t, got.GetRootCause(), "connection pool exhaustion")
}

func TestDiagnoseService_ReasonerNonConvergenceKeepsRuleBased(t *testing.T) {
	// Baseline: the same engine without any reasoner.
	baseSvc := NewDiagnosisService(newTestDiagEngine(), nil)
	base, err := baseSvc.Diagnose(context.Background(), &pb.DiagnoseRequest{Target: "host-02"})
	require.NoError(t, err)

	// The scripted model never claims convergence and stays below the
	// threshold: the reasoner exhausts its turns and must NOT overwrite the
	// rule-based report.
	nonConverging := `{"hypothesis":"wild guess", "confidence":0.3, "converged":false}`
	svc := NewDiagnosisService(newTestDiagEngine(), nil).
		WithReasoner(newReasonerForTest(t, &scriptedLLM{reply: nonConverging}))

	resp, err := svc.Diagnose(context.Background(), &pb.DiagnoseRequest{Target: "host-02"})
	require.NoError(t, err)

	assert.Equal(t, base.GetRootCause(), resp.GetRootCause(),
		"a non-converged reasoner must leave the rule-based root cause untouched")
	assert.InDelta(t, base.GetConfidence(), resp.GetConfidence(), 0.001)
}

func TestDiagnoseService_ReasonerErrorKeepsRuleBasedAndDoesNotFail(t *testing.T) {
	baseSvc := NewDiagnosisService(newTestDiagEngine(), nil)
	base, err := baseSvc.Diagnose(context.Background(), &pb.DiagnoseRequest{Target: "host-03"})
	require.NoError(t, err)

	svc := NewDiagnosisService(newTestDiagEngine(), nil).
		WithReasoner(newReasonerForTest(t, &scriptedLLM{err: errors.New("llm unavailable")}))

	resp, err := svc.Diagnose(context.Background(), &pb.DiagnoseRequest{Target: "host-03"})
	require.NoError(t, err, "an LLM outage must not fail the Diagnose RPC")
	assert.Equal(t, base.GetRootCause(), resp.GetRootCause(),
		"an errored reasoner must leave the rule-based root cause untouched")
}
