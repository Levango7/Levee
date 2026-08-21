package llm_diag

// reasoning_test.go exercises the ReasoningEngine and ReasoningContext
// implemented in reasoning.go and context.go. The tests use the
// recommend.MockLLMClient so no network access is required.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/recommend"
)

// --- Test helpers ----------------------------------------------------------

// newTestReport returns a diagnostic report that looks like a Java OOM on
// order-service. It is the seed for the reasoning loop in the tests below.
func newTestReport() *diagnosis.DiagnosticReport {
	return &diagnosis.DiagnosticReport{
		ID:         "diag-oom-001",
		Target:     "order-service.prod",
		Trigger:    diagnosis.TriggerAlert,
		AlertID:    "alert-001",
		Status:     diagnosis.DiagUnhealthy,
		RootCause:  "Java heap exhaustion due to memory leak",
		Confidence: 0.7,
		Summary:    "order-service is unhealthy; OOM errors in logs",
		Findings: []diagnosis.Finding{
			{ID: "F-1", Category: "service", Severity: "critical",
				Title:       "java.lang.OutOfMemoryError",
				Description: "heap space exhausted",
				Suggestion:  "restart with larger -Xmx"},
		},
		StartedAt: time.Now().UTC(),
	}
}

// newTestEngine returns a ReasoningEngine with the given LLM client and max
// turns. It fails the test when construction errors.
func newTestEngine(t *testing.T, llm recommend.LLMClient, maxTurns int) *ReasoningEngine {
	t.Helper()
	e, err := NewReasoningEngine(ReasoningEngineConfig{
		LLM:      llm,
		MaxTurns: maxTurns,
	})
	if err != nil {
		t.Fatalf("NewReasoningEngine: %v", err)
	}
	return e
}

// errorLLMClient is an LLMClient that always returns the configured error. It
// is used to exercise the error path of Diagnose.
type errorLLMClient struct {
	err error
}

func (c *errorLLMClient) Chat(_ context.Context, _ []recommend.LLMMessage) (string, error) {
	return "", c.err
}

func (c *errorLLMClient) ChatWithSystem(_ context.Context, _ string, _ []recommend.LLMMessage) (string, error) {
	return "", c.err
}

func (c *errorLLMClient) Name() string { return "error" }

// jsonResp builds a JSON reasoning response string from the given fields.
func jsonResp(hypothesis string, confidence float64, converged bool, suggestions ...string) string {
	sugs := ""
	for i, s := range suggestions {
		if i > 0 {
			sugs += ", "
		}
		sugs += `"` + s + `"`
	}
	conv := "false"
	if converged {
		conv = "true"
	}
	return `{"hypothesis": "` + hypothesis + `", "confidence": ` +
		formatFloat(confidence) + `, "converged": ` + conv +
		`, "suggestions": [` + sugs + `]}`
}

// formatFloat renders a float as a string suitable for JSON. It keeps the
// tests free of strconv imports.
func formatFloat(v float64) string {
	switch v {
	case 0.5:
		return "0.5"
	case 0.3:
		return "0.3"
	case 0.7:
		return "0.7"
	case 0.9:
		return "0.9"
	case 0.95:
		return "0.95"
	default:
		return "0.0"
	}
}

// --- NewReasoningEngine ----------------------------------------------------

// TestNewReasoningEngine_Defaults verifies that the engine applies sensible
// defaults when optional config fields are omitted.
func TestNewReasoningEngine_Defaults(t *testing.T) {
	mock := recommend.NewMockLLMClient()
	e, err := NewReasoningEngine(ReasoningEngineConfig{LLM: mock})
	if err != nil {
		t.Fatalf("NewReasoningEngine: %v", err)
	}
	if e.maxTurns != DefaultMaxTurns {
		t.Errorf("maxTurns = %d, want %d", e.maxTurns, DefaultMaxTurns)
	}
	if e.convergenceThreshold != DefaultConvergenceThreshold {
		t.Errorf("convergenceThreshold = %v, want %v", e.convergenceThreshold, DefaultConvergenceThreshold)
	}
	if e.sanitizer == nil {
		t.Error("sanitizer is nil, want default sanitizer")
	}
	if e.llm == nil {
		t.Error("llm is nil")
	}
	if e.log == nil {
		t.Error("log is nil")
	}
}

// TestNewReasoningEngine_NilLLM verifies that a nil LLM client is rejected.
func TestNewReasoningEngine_NilLLM(t *testing.T) {
	_, err := NewReasoningEngine(ReasoningEngineConfig{})
	if !errors.Is(err, ErrNilLLM) {
		t.Errorf("err = %v, want ErrNilLLM", err)
	}
}

// --- Diagnose --------------------------------------------------------------

// TestDiagnose_Success exercises a normal two-turn reasoning loop: the first
// turn refines the hypothesis without converging, the second turn converges.
func TestDiagnose_Success(t *testing.T) {
	report := newTestReport()
	mock := recommend.NewMockLLMClient()

	// Turn 1: refine without converging.
	prompt1 := buildTurnPrompt(1, 5, report.RootCause, report.Confidence)
	mock.SetResponse(prompt1, jsonResp("memory leak in cache layer", 0.5, false, "increase heap size"))

	// Turn 2: converge with high confidence.
	prompt2 := buildTurnPrompt(2, 5, "memory leak in cache layer", 0.5)
	mock.SetResponse(prompt2, jsonResp("unbounded cache in OrderService", 0.9, true, "add cache eviction", "restart service"))

	e := newTestEngine(t, mock, 5)
	result, err := e.Diagnose(context.Background(), report.Target, report)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if result.Turns != 2 {
		t.Errorf("Turns = %d, want 2", result.Turns)
	}
	if result.Status != StatusConverged {
		t.Errorf("Status = %s, want converged", result.Status)
	}
	if result.Hypothesis != "unbounded cache in OrderService" {
		t.Errorf("Hypothesis = %q, want %q", result.Hypothesis, "unbounded cache in OrderService")
	}
	if result.Confidence != 0.9 {
		t.Errorf("Confidence = %v, want 0.9", result.Confidence)
	}
	if result.RootCause != result.Hypothesis {
		t.Errorf("RootCause = %q, want %q", result.RootCause, result.Hypothesis)
	}
	if len(result.Suggestions) != 2 {
		t.Errorf("len(Suggestions) = %d, want 2", len(result.Suggestions))
	}
	if result.Duration <= 0 {
		t.Error("Duration should be positive")
	}
	// The context should carry the full transcript: 2 user + 2 assistant turns.
	if len(result.Context.Messages) != 4 {
		t.Errorf("len(Context.Messages) = %d, want 4", len(result.Context.Messages))
	}
	if result.Context.Target != report.Target {
		t.Errorf("Context.Target = %q, want %q", result.Context.Target, report.Target)
	}
	if result.Context.AlertID != report.AlertID {
		t.Errorf("Context.AlertID = %q, want %q", result.Context.AlertID, report.AlertID)
	}
}

// TestDiagnose_Converge verifies that the engine stops after the first turn
// when the LLM reports convergence.
func TestDiagnose_Converge(t *testing.T) {
	report := newTestReport()
	mock := recommend.NewMockLLMClient()

	prompt1 := buildTurnPrompt(1, 5, report.RootCause, report.Confidence)
	mock.SetResponse(prompt1, jsonResp("memory leak confirmed", 0.95, true, "restart with larger heap"))

	e := newTestEngine(t, mock, 5)
	result, err := e.Diagnose(context.Background(), report.Target, report)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if result.Turns != 1 {
		t.Errorf("Turns = %d, want 1", result.Turns)
	}
	if result.Status != StatusConverged {
		t.Errorf("Status = %s, want converged", result.Status)
	}
	if result.Hypothesis != "memory leak confirmed" {
		t.Errorf("Hypothesis = %q, want %q", result.Hypothesis, "memory leak confirmed")
	}
	if result.Confidence != 0.95 {
		t.Errorf("Confidence = %v, want 0.95", result.Confidence)
	}
}

// TestDiagnose_ConvergeByThreshold verifies that the engine stops when the
// confidence crosses the convergence threshold even if the LLM does not set
// "converged": true.
func TestDiagnose_ConvergeByThreshold(t *testing.T) {
	report := newTestReport()
	mock := recommend.NewMockLLMClient()

	prompt1 := buildTurnPrompt(1, 5, report.RootCause, report.Confidence)
	// converged=false but confidence=0.9 >= threshold 0.8.
	mock.SetResponse(prompt1, jsonResp("disk full", 0.9, false, "clean up logs"))

	e := newTestEngine(t, mock, 5)
	result, err := e.Diagnose(context.Background(), report.Target, report)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if result.Turns != 1 {
		t.Errorf("Turns = %d, want 1", result.Turns)
	}
	if result.Status != StatusConverged {
		t.Errorf("Status = %s, want converged", result.Status)
	}
}

// TestDiagnose_MaxTurns verifies that the engine runs to the turn cap and
// reports StatusMaxTurnsReached when the LLM never converges.
func TestDiagnose_MaxTurns(t *testing.T) {
	report := newTestReport()
	mock := recommend.NewMockLLMClient()

	// Turn 1: refine without converging.
	prompt1 := buildTurnPrompt(1, 2, report.RootCause, report.Confidence)
	mock.SetResponse(prompt1, jsonResp("hypothesis 1", 0.3, false))

	// Turn 2: still not converging.
	prompt2 := buildTurnPrompt(2, 2, "hypothesis 1", 0.3)
	mock.SetResponse(prompt2, jsonResp("hypothesis 2", 0.3, false))

	e := newTestEngine(t, mock, 2)
	result, err := e.Diagnose(context.Background(), report.Target, report)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if result.Turns != 2 {
		t.Errorf("Turns = %d, want 2", result.Turns)
	}
	if result.Status != StatusMaxTurnsReached {
		t.Errorf("Status = %s, want max_turns_reached", result.Status)
	}
	if result.Hypothesis != "hypothesis 2" {
		t.Errorf("Hypothesis = %q, want %q", result.Hypothesis, "hypothesis 2")
	}
}

// TestDiagnose_LLMError verifies that an LLM error is propagated and the
// result carries StatusError.
func TestDiagnose_LLMError(t *testing.T) {
	report := newTestReport()
	llmErr := errors.New("llm unavailable")
	e := newTestEngine(t, &errorLLMClient{err: llmErr}, 5)

	result, err := e.Diagnose(context.Background(), report.Target, report)
	if err == nil {
		t.Fatal("Diagnose: expected error, got nil")
	}
	if result == nil {
		t.Fatal("result is nil, want non-nil result with StatusError")
	}
	if result.Status != StatusError {
		t.Errorf("Status = %s, want error", result.Status)
	}
	if result.Turns != 0 {
		t.Errorf("Turns = %d, want 0", result.Turns)
	}
}

// TestDiagnose_NilReport verifies that a nil report is rejected.
func TestDiagnose_NilReport(t *testing.T) {
	mock := recommend.NewMockLLMClient()
	e := newTestEngine(t, mock, 5)

	result, err := e.Diagnose(context.Background(), "target", nil)
	if !errors.Is(err, ErrNilReport) {
		t.Errorf("err = %v, want ErrNilReport", err)
	}
	if result != nil {
		t.Error("result is non-nil, want nil")
	}
}

// TestDiagnose_NilContext verifies that Diagnose tolerates a nil context by
// substituting context.Background().
func TestDiagnose_NilContext(t *testing.T) {
	report := newTestReport()
	mock := recommend.NewMockLLMClient()

	prompt1 := buildTurnPrompt(1, 5, report.RootCause, report.Confidence)
	mock.SetResponse(prompt1, jsonResp("converged hypothesis", 0.95, true))

	e := newTestEngine(t, mock, 5)
	//nolint:staticcheck // intentionally passing nil context to test tolerance
	result, err := e.Diagnose(nil, report.Target, report)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if result.Status != StatusConverged {
		t.Errorf("Status = %s, want converged", result.Status)
	}
}

// --- ReasoningContext ------------------------------------------------------

// TestReasoningContext_AddTurn verifies that turns are appended with the
// correct role and content and that UpdatedAt advances.
func TestReasoningContext_AddTurn(t *testing.T) {
	c := NewReasoningContext("target", nil)
	if len(c.Messages) != 0 {
		t.Errorf("initial len(Messages) = %d, want 0", len(c.Messages))
	}
	before := c.UpdatedAt

	c.AddTurn("user", "hello")
	if len(c.Messages) != 1 {
		t.Fatalf("len(Messages) = %d, want 1", len(c.Messages))
	}
	if c.Messages[0].Role != "user" {
		t.Errorf("Messages[0].Role = %q, want %q", c.Messages[0].Role, "user")
	}
	if c.Messages[0].Content != "hello" {
		t.Errorf("Messages[0].Content = %q, want %q", c.Messages[0].Content, "hello")
	}
	if c.Messages[0].Timestamp.IsZero() {
		t.Error("Messages[0].Timestamp is zero")
	}
	if !c.UpdatedAt.After(before) && !c.UpdatedAt.Equal(before) {
		t.Error("UpdatedAt did not advance")
	}

	c.AddTurn("assistant", "hi there")
	if len(c.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2", len(c.Messages))
	}
	if c.Messages[1].Role != "assistant" {
		t.Errorf("Messages[1].Role = %q, want %q", c.Messages[1].Role, "assistant")
	}
	if c.Messages[1].Content != "hi there" {
		t.Errorf("Messages[1].Content = %q, want %q", c.Messages[1].Content, "hi there")
	}
}

// TestReasoningContext_ToMessages verifies that the turn history is converted
// into the recommend.LLMMessage slice preserving order and dropping turns with
// an empty role.
func TestReasoningContext_ToMessages(t *testing.T) {
	c := NewReasoningContext("target", nil)
	c.AddTurn("system", "sys prompt")
	c.AddTurn("user", "first question")
	c.AddTurn("assistant", "first answer")
	c.AddTurn("user", "second question")

	msgs := c.ToMessages()
	if len(msgs) != 4 {
		t.Fatalf("len(msgs) = %d, want 4", len(msgs))
	}
	want := []struct{ role, content string }{
		{"system", "sys prompt"},
		{"user", "first question"},
		{"assistant", "first answer"},
		{"user", "second question"},
	}
	for i, w := range want {
		if msgs[i].Role != w.role {
			t.Errorf("msgs[%d].Role = %q, want %q", i, msgs[i].Role, w.role)
		}
		if msgs[i].Content != w.content {
			t.Errorf("msgs[%d].Content = %q, want %q", i, msgs[i].Content, w.content)
		}
	}

	// Empty context returns nil.
	empty := NewReasoningContext("target", nil)
	if got := empty.ToMessages(); got != nil {
		t.Errorf("empty ToMessages = %v, want nil", got)
	}
}

// TestReasoningContext_SetHypothesis verifies that the hypothesis and
// confidence are updated and the confidence is clamped to [0, 1].
func TestReasoningContext_SetHypothesis(t *testing.T) {
	c := NewReasoningContext("target", nil)

	// Normal value.
	c.SetHypothesis("test hypothesis", 0.7)
	if c.Hypothesis != "test hypothesis" {
		t.Errorf("Hypothesis = %q, want %q", c.Hypothesis, "test hypothesis")
	}
	if c.Confidence != 0.7 {
		t.Errorf("Confidence = %v, want 0.7", c.Confidence)
	}

	// Above 1 is clamped to 1.
	c.SetHypothesis("high", 1.5)
	if c.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0", c.Confidence)
	}

	// Below 0 is clamped to 0.
	c.SetHypothesis("low", -0.5)
	if c.Confidence != 0.0 {
		t.Errorf("Confidence = %v, want 0.0", c.Confidence)
	}

	// Empty hypothesis is allowed.
	c.SetHypothesis("", 0.5)
	if c.Hypothesis != "" {
		t.Errorf("Hypothesis = %q, want empty", c.Hypothesis)
	}
	if c.Confidence != 0.5 {
		t.Errorf("Confidence = %v, want 0.5", c.Confidence)
	}
}

// --- NewReasoningContext ---------------------------------------------------

// TestNewReasoningContext verifies that the context is initialised from the
// report when one is provided.
func TestNewReasoningContext(t *testing.T) {
	report := newTestReport()
	c := NewReasoningContext("fallback-target", report)

	if c.ID == "" {
		t.Error("ID is empty")
	}
	if c.Target != report.Target {
		t.Errorf("Target = %q, want %q", c.Target, report.Target)
	}
	if c.AlertID != report.AlertID {
		t.Errorf("AlertID = %q, want %q", c.AlertID, report.AlertID)
	}
	if c.InitialReport != report {
		t.Error("InitialReport not set")
	}
	if c.Hypothesis != report.RootCause {
		t.Errorf("Hypothesis = %q, want %q", c.Hypothesis, report.RootCause)
	}
	if c.Confidence != report.Confidence {
		t.Errorf("Confidence = %v, want %v", c.Confidence, report.Confidence)
	}
	if c.Status != StatusReasoning {
		t.Errorf("Status = %s, want reasoning", c.Status)
	}
	if c.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

// TestNewReasoningContext_NilReport verifies that the context falls back to
// the explicit target when the report is nil.
func TestNewReasoningContext_NilReport(t *testing.T) {
	c := NewReasoningContext("explicit-target", nil)
	if c.Target != "explicit-target" {
		t.Errorf("Target = %q, want %q", c.Target, "explicit-target")
	}
	if c.AlertID != "" {
		t.Errorf("AlertID = %q, want empty", c.AlertID)
	}
	if c.Hypothesis != "" {
		t.Errorf("Hypothesis = %q, want empty", c.Hypothesis)
	}
	if c.Confidence != 0 {
		t.Errorf("Confidence = %v, want 0", c.Confidence)
	}
}

// --- ReasoningStatus.String ------------------------------------------------

// TestReasoningStatus_String verifies the string rendering of every status.
func TestReasoningStatus_String(t *testing.T) {
	cases := []struct {
		status ReasoningStatus
		want   string
	}{
		{StatusReasoning, "reasoning"},
		{StatusConverged, "converged"},
		{StatusInconclusive, "inconclusive"},
		{StatusMaxTurnsReached, "max_turns_reached"},
		{StatusError, "error"},
		{ReasoningStatus(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.status.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// --- parseReasoningResponse ------------------------------------------------

// TestParseReasoningResponse verifies that bare JSON and fenced JSON are both
// accepted.
func TestParseReasoningResponse(t *testing.T) {
	// Bare JSON.
	r, err := parseReasoningResponse(`{"hypothesis": "h", "confidence": 0.5, "converged": false, "suggestions": ["a"]}`)
	if err != nil {
		t.Fatalf("parseReasoningResponse: %v", err)
	}
	if r.Hypothesis != "h" {
		t.Errorf("Hypothesis = %q, want %q", r.Hypothesis, "h")
	}
	if r.Confidence != 0.5 {
		t.Errorf("Confidence = %v, want 0.5", r.Confidence)
	}
	if r.Converged {
		t.Error("Converged = true, want false")
	}
	if len(r.Suggestions) != 1 || r.Suggestions[0] != "a" {
		t.Errorf("Suggestions = %v, want [a]", r.Suggestions)
	}

	// Fenced JSON.
	fenced := "```json\n" + `{"hypothesis": "h2", "confidence": 0.9, "converged": true, "suggestions": []}` + "\n```"
	r2, err := parseReasoningResponse(fenced)
	if err != nil {
		t.Fatalf("parseReasoningResponse fenced: %v", err)
	}
	if r2.Hypothesis != "h2" {
		t.Errorf("Hypothesis = %q, want %q", r2.Hypothesis, "h2")
	}
	if !r2.Converged {
		t.Error("Converged = false, want true")
	}

	// Empty response.
	if _, err := parseReasoningResponse(""); !errors.Is(err, ErrEmptyResponse) {
		t.Errorf("err = %v, want ErrEmptyResponse", err)
	}

	// Invalid JSON.
	if _, err := parseReasoningResponse("not json"); !errors.Is(err, ErrParseResponse) {
		t.Errorf("err = %v, want ErrParseResponse", err)
	}
}
