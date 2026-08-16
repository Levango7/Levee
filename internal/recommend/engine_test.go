package recommend

// engine_test.go exercises the RecommendEngine and WorkflowGenerator
// implemented in engine.go and workflow_gen.go. The tests use the
// MockLLMClient so no network access is required.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/diagnosis"
)

// --- Test helpers ----------------------------------------------------------

// newTestEngine returns a RecommendEngine with a default knowledge base and
// the given LLM client (which may be nil).
func newTestEngine(t *testing.T, llm LLMClient) *RecommendEngine {
	t.Helper()
	return NewRecommendEngine(RecommendEngineConfig{
		KnowledgeBase: NewKnowledgeBaseWithDefaults(),
		LLMClient:     llm,
		Timeout:       5 * time.Second,
	})
}

// newOOMReport returns a diagnostic report that looks like a Java OOM on
// order-service. The knowledge base has a strong match for this scenario.
func newOOMReport() *diagnosis.DiagnosticReport {
	return &diagnosis.DiagnosticReport{
		ID:        "diag-oom-001",
		Target:    "order-service.prod",
		Trigger:   diagnosis.TriggerAlert,
		AlertID:   "alert-001",
		Status:    diagnosis.DiagUnhealthy,
		RootCause: "Java heap exhaustion due to memory leak",
		Confidence: 0.85,
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

// newDiskFullReport returns a report that matches the disk-full incident.
func newDiskFullReport() *diagnosis.DiagnosticReport {
	return &diagnosis.DiagnosticReport{
		ID:         "diag-disk-001",
		Target:     "web-01.prod",
		Trigger:    diagnosis.TriggerManual,
		Status:     diagnosis.DiagUnhealthy,
		RootCause:  "Log volume filled by unbounded application logs",
		Confidence: 0.7,
		Summary:    "disk full on /var/log",
		Findings: []diagnosis.Finding{
			{ID: "F-1", Category: "node", Severity: "critical",
				Title:       "no space left on device",
				Description: "disk usage above 90 percent"},
		},
		StartedAt: time.Now().UTC(),
	}
}

// newUnknownReport returns a report that does not match any built-in
// incident, runbook or pattern. The finding category is deliberately set to
// "misc" so it does not collide with any knowledge-base tag.
func newUnknownReport() *diagnosis.DiagnosticReport {
	return &diagnosis.DiagnosticReport{
		ID:         "diag-unknown-001",
		Target:     "misc-01.prod",
		Trigger:    diagnosis.TriggerManual,
		Status:     diagnosis.DiagDegraded,
		RootCause:  "transient gamma ray bit flip",
		Confidence: 0.1,
		Summary:    "intermittent errors of unknown origin",
		Findings: []diagnosis.Finding{
			{ID: "F-1", Category: "misc", Severity: "warning",
				Title: "intermittent 500s"},
		},
		StartedAt: time.Now().UTC(),
	}
}

// validProposalJSON is a well-formed LLM proposal JSON used by several tests.
const validProposalJSON = `{
  "summary": "Restart order-service with larger heap",
  "approach": "Increase -Xmx to 4g and restart the service",
  "risk_level": "high",
  "confidence": 0.9,
  "steps": [
    {"name": "restart-service", "module": "svc", "action": "restart", "target": "order-service", "args": {"service": "order-service"}, "description": "restart with -Xmx4g"}
  ],
  "pre_conditions": ["confirm SSH access", "snapshot heap"],
  "rollback_plan": "restart with previous -Xmx",
  "alternatives": [
    {"summary": "scale out", "approach": "add replicas", "risk_level": "medium", "confidence": 0.6}
  ]
}`

// --- NewRecommendEngine ----------------------------------------------------

func TestNewRecommendEngine_Defaults(t *testing.T) {
	e := NewRecommendEngine(RecommendEngineConfig{})
	if e == nil {
		t.Fatal("expected non-nil engine")
	}
	if e.kb == nil {
		t.Error("expected default knowledge base")
	}
	if e.sanitizer == nil {
		t.Error("expected non-nil sanitizer")
	}
	if e.wfGen == nil {
		t.Error("expected non-nil workflow generator")
	}
	if e.timeout != DefaultRecommendTimeout {
		t.Errorf("timeout = %v, want %v", e.timeout, DefaultRecommendTimeout)
	}
	if e.llm != nil {
		t.Error("expected nil LLM by default")
	}
}

func TestNewRecommendEngine_CustomConfig(t *testing.T) {
	kb := NewKnowledgeBase()
	mock := NewMockLLMClient()
	e := NewRecommendEngine(RecommendEngineConfig{
		KnowledgeBase: kb,
		LLMClient:     mock,
		Timeout:       7 * time.Second,
	})
	if e.kb != kb {
		t.Error("expected custom knowledge base")
	}
	if e.llm != mock {
		t.Error("expected custom LLM")
	}
	if e.timeout != 7*time.Second {
		t.Errorf("timeout = %v, want 7s", e.timeout)
	}
}

// --- Recommend error paths -------------------------------------------------

func TestRecommend_NilReport(t *testing.T) {
	e := newTestEngine(t, nil)
	_, err := e.Recommend(context.Background(), nil)
	if !errors.Is(err, ErrNilReport) {
		t.Errorf("err = %v, want ErrNilReport", err)
	}
}

func TestRecommend_EmptyReport(t *testing.T) {
	e := newTestEngine(t, nil)
	_, err := e.Recommend(context.Background(), &diagnosis.DiagnosticReport{})
	if !errors.Is(err, ErrEmptyReport) {
		t.Errorf("err = %v, want ErrEmptyReport", err)
	}
}

func TestRecommend_NilContext(t *testing.T) {
	e := newTestEngine(t, nil)
	rec, err := e.Recommend(nil, newOOMReport())
	if err != nil {
		t.Fatalf("nil ctx: %v", err)
	}
	if rec == nil {
		t.Fatal("expected non-nil recommendation")
	}
}

// --- Recommend pure knowledge-base mode ------------------------------------

func TestRecommend_KnowledgeBaseOnly_OOM(t *testing.T) {
	e := newTestEngine(t, nil)
	rec, err := e.Recommend(context.Background(), newOOMReport())
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Source != SourceKnowledgeBase {
		t.Errorf("Source = %v, want knowledge_base", rec.Source)
	}
	if rec.Target != "order-service.prod" {
		t.Errorf("Target = %q", rec.Target)
	}
	if rec.DiagnosisID != "diag-oom-001" {
		t.Errorf("DiagnosisID = %q", rec.DiagnosisID)
	}
	if rec.Summary == "" {
		t.Error("Summary should not be empty")
	}
	if rec.Approach == "" {
		t.Error("Approach should not be empty")
	}
	if rec.RiskLevel == "" {
		t.Error("RiskLevel should not be empty")
	}
	if rec.Confidence <= 0 || rec.Confidence > 1 {
		t.Errorf("Confidence = %v, want (0,1]", rec.Confidence)
	}
	if rec.WorkflowDraft == "" {
		t.Error("WorkflowDraft should not be empty")
	}
	if !strings.Contains(rec.WorkflowDraft, "name: auto-fix-") {
		t.Errorf("workflow draft missing name: %s", rec.WorkflowDraft)
	}
	if !strings.Contains(rec.WorkflowDraft, "order-service.prod") {
		t.Errorf("workflow draft missing target: %s", rec.WorkflowDraft)
	}
	if len(rec.PreConditions) == 0 {
		t.Error("PreConditions should not be empty")
	}
	if rec.RollbackPlan == "" {
		t.Error("RollbackPlan should not be empty")
	}
	if rec.ID == "" {
		t.Error("ID should not be empty")
	}
	if rec.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
	if len(rec.Matches) == 0 {
		t.Error("expected matches for OOM scenario")
	}
}

func TestRecommend_KnowledgeBaseOnly_DiskFull(t *testing.T) {
	e := newTestEngine(t, nil)
	rec, err := e.Recommend(context.Background(), newDiskFullReport())
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Source != SourceKnowledgeBase {
		t.Errorf("Source = %v, want knowledge_base", rec.Source)
	}
	if len(rec.Matches) == 0 {
		t.Fatal("expected matches for disk-full scenario")
	}
	if !strings.Contains(rec.WorkflowDraft, "web-01.prod") {
		t.Errorf("workflow draft missing target: %s", rec.WorkflowDraft)
	}
}

func TestRecommend_KnowledgeBaseOnly_NoMatch(t *testing.T) {
	e := newTestEngine(t, nil)
	rec, err := e.Recommend(context.Background(), newUnknownReport())
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Source != SourceKnowledgeBase {
		t.Errorf("Source = %v", rec.Source)
	}
	// Even with no matches we should get a usable recommendation.
	if rec.Summary == "" {
		t.Error("Summary should not be empty")
	}
	if rec.Approach == "" {
		t.Error("Approach should not be empty")
	}
	if rec.WorkflowDraft == "" {
		t.Error("WorkflowDraft should not be empty")
	}
}

func TestRecommend_AlternativesPopulated(t *testing.T) {
	e := newTestEngine(t, nil)
	rec, err := e.Recommend(context.Background(), newOOMReport())
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	// OOM should match incident + pattern + maybe runbook; we expect at
	// least one alternative when there are >= 2 strong matches.
	if len(rec.Matches) >= 2 && len(rec.Alternatives) == 0 {
		t.Errorf("expected alternatives with %d matches", len(rec.Matches))
	}
}

// --- Recommend with LLM (hybrid) -------------------------------------------

func TestRecommend_Hybrid_LLMAndKnowledgeBase(t *testing.T) {
	mock := NewMockLLMClient()
	mock.SetResponse("", validProposalJSON)
	e := newTestEngine(t, mock)

	rec, err := e.Recommend(context.Background(), newOOMReport())
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Source != SourceHybrid {
		t.Errorf("Source = %v, want hybrid", rec.Source)
	}
	if rec.Summary != "Restart order-service with larger heap" {
		t.Errorf("Summary = %q", rec.Summary)
	}
	if rec.Approach != "Increase -Xmx to 4g and restart the service" {
		t.Errorf("Approach = %q", rec.Approach)
	}
	if rec.RiskLevel != RiskHigh {
		t.Errorf("RiskLevel = %v, want high", rec.RiskLevel)
	}
	if rec.Confidence <= 0 || rec.Confidence > 1 {
		t.Errorf("Confidence = %v", rec.Confidence)
	}
	if len(rec.PreConditions) != 2 {
		t.Errorf("PreConditions = %v", rec.PreConditions)
	}
	if rec.RollbackPlan != "restart with previous -Xmx" {
		t.Errorf("RollbackPlan = %q", rec.RollbackPlan)
	}
	if len(rec.Alternatives) != 1 {
		t.Errorf("Alternatives = %v", rec.Alternatives)
	}
	if len(rec.Matches) == 0 {
		t.Error("expected matches in hybrid mode")
	}
	if !strings.Contains(rec.WorkflowDraft, "restart-service") {
		t.Errorf("workflow should use LLM step: %s", rec.WorkflowDraft)
	}
	if mock.CallsCount() != 1 {
		t.Errorf("LLM calls = %d, want 1", mock.CallsCount())
	}
}

func TestRecommend_LLMOnly_NoKnowledgeMatch(t *testing.T) {
	mock := NewMockLLMClient()
	mock.SetResponse("", validProposalJSON)
	e := newTestEngine(t, mock)

	rec, err := e.Recommend(context.Background(), newUnknownReport())
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Source != SourceLLM {
		t.Errorf("Source = %v, want llm", rec.Source)
	}
	if rec.Summary != "Restart order-service with larger heap" {
		t.Errorf("Summary = %q", rec.Summary)
	}
}

func TestRecommend_LLMFails_DegradesToKnowledgeBase(t *testing.T) {
	// Use a custom erroring client so the LLM call returns an error and the
	// engine degrades to pure knowledge-base mode.
	errClient := &errorLLMClient{}
	e := newTestEngine(t, errClient)

	rec, err := e.Recommend(context.Background(), newOOMReport())
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Source != SourceKnowledgeBase {
		t.Errorf("Source = %v, want knowledge_base after LLM failure", rec.Source)
	}
}

func TestRecommend_LLMReturnsInvalidJSON_DegradesToKnowledgeBase(t *testing.T) {
	mock := NewMockLLMClient()
	mock.SetResponse("", "this is not json {{{")
	e := newTestEngine(t, mock)

	rec, err := e.Recommend(context.Background(), newOOMReport())
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Source != SourceKnowledgeBase {
		t.Errorf("Source = %v, want knowledge_base after parse failure", rec.Source)
	}
}

func TestRecommend_LLMReturnsMarkdownFencedJSON(t *testing.T) {
	mock := NewMockLLMClient()
	fenced := "```json\n" + validProposalJSON + "\n```"
	mock.SetResponse("", fenced)
	e := newTestEngine(t, mock)

	rec, err := e.Recommend(context.Background(), newOOMReport())
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Source != SourceHybrid {
		t.Errorf("Source = %v, want hybrid", rec.Source)
	}
	if rec.Summary != "Restart order-service with larger heap" {
		t.Errorf("Summary = %q", rec.Summary)
	}
}

func TestRecommend_LLMResponseSanitised(t *testing.T) {
	mock := NewMockLLMClient()
	mock.SetResponse("", validProposalJSON)
	e := newTestEngine(t, mock)

	// Build a report whose root cause contains a fake secret.
	report := newOOMReport()
	report.RootCause = "leak detected: password=hunter2"

	if _, err := e.Recommend(context.Background(), report); err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if mock.CallsCount() != 1 {
		t.Fatalf("LLM calls = %d, want 1", mock.CallsCount())
	}
	calls := mock.Calls
	if len(calls[0]) == 0 {
		t.Fatal("expected recorded call messages")
	}
	for _, m := range calls[0] {
		if strings.Contains(m.Content, "hunter2") {
			t.Errorf("unsanitised secret in %s message: %s", m.Role, m.Content)
		}
	}
}

// --- Recommend context timeout ---------------------------------------------

func TestRecommend_RespectsContextDeadline(t *testing.T) {
	mock := NewMockLLMClient()
	mock.SetResponse("", validProposalJSON)
	e := newTestEngine(t, mock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already expired

	// Should still return a recommendation (KB path) — the LLM call may
	// fail but we degrade gracefully. The KB match does not consult ctx.
	rec, err := e.Recommend(ctx, newOOMReport())
	if err != nil {
		t.Fatalf("Recommend with cancelled ctx: %v", err)
	}
	if rec == nil {
		t.Fatal("expected non-nil recommendation")
	}
}

// --- extractDiagnosisSignals -----------------------------------------------

func TestExtractDiagnosisSignals(t *testing.T) {
	report := &diagnosis.DiagnosticReport{
		Target:    "h",
		RootCause: "disk full",
		Trigger:   diagnosis.TriggerAlert,
		Findings: []diagnosis.Finding{
			{Category: "node", Title: "no space left", Description: "disk 95%"},
		},
	}
	rc, sym, tags := extractDiagnosisSignals(report)
	if rc != "disk full" {
		t.Errorf("rootCause = %q", rc)
	}
	if len(sym) < 2 {
		t.Errorf("symptoms = %v", sym)
	}
	foundTag := false
	for _, tg := range tags {
		if tg == "node" || tg == "alert" {
			foundTag = true
		}
	}
	if !foundTag {
		t.Errorf("tags missing expected values: %v", tags)
	}
}

// --- filterStrongMatches ---------------------------------------------------

func TestFilterStrongMatches(t *testing.T) {
	matches := []*Match{
		{ID: "a", Score: 0.5},
		{ID: "b", Score: 0.05},
		{ID: "c", Score: 0.9},
	}
	out := filterStrongMatches(matches, 0.1)
	if len(out) != 2 {
		t.Errorf("len = %d, want 2", len(out))
	}
}

// --- parseFixProposal ------------------------------------------------------

func TestParseFixProposal_Valid(t *testing.T) {
	p, err := parseFixProposal(validProposalJSON)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Summary != "Restart order-service with larger heap" {
		t.Errorf("Summary = %q", p.Summary)
	}
	if len(p.Steps) != 1 {
		t.Errorf("Steps = %v", p.Steps)
	}
}

func TestParseFixProposal_Empty(t *testing.T) {
	_, err := parseFixProposal("")
	if err == nil {
		t.Error("expected error for empty response")
	}
}

func TestParseFixProposal_InvalidJSON(t *testing.T) {
	_, err := parseFixProposal("not json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseFixProposal_FencedJSON(t *testing.T) {
	fenced := "```json\n" + validProposalJSON + "\n```"
	p, err := parseFixProposal(fenced)
	if err != nil {
		t.Fatalf("parse fenced: %v", err)
	}
	if p.Summary == "" {
		t.Error("Summary should not be empty")
	}
}

// --- extractJSONBlock ------------------------------------------------------

func TestExtractJSONBlock(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", `{"a":1}`, `{"a":1}`},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"bare-fence", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractJSONBlock(c.in)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// --- risk helpers ----------------------------------------------------------

func TestRiskFromSeverity(t *testing.T) {
	cases := map[string]RiskLevel{
		"critical": RiskHigh,
		"warning":  RiskMedium,
		"info":     RiskLow,
		"unknown":  RiskMedium,
	}
	for in, want := range cases {
		if got := riskFromSeverity(in); got != want {
			t.Errorf("riskFromSeverity(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNormaliseRiskLevel(t *testing.T) {
	cases := map[RiskLevel]RiskLevel{
		RiskLow:      RiskLow,
		RiskMedium:   RiskMedium,
		RiskHigh:     RiskHigh,
		RiskCritical: RiskCritical,
		RiskLevel("HIGH"): RiskHigh,
		RiskLevel("garbage"): RiskMedium,
	}
	for in, want := range cases {
		if got := normaliseRiskLevel(in); got != want {
			t.Errorf("normaliseRiskLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRiskRank(t *testing.T) {
	if riskRank(RiskCritical) <= riskRank(RiskLow) {
		t.Error("critical should outrank low")
	}
}

func TestRiskFromRunbook(t *testing.T) {
	rb := Runbook{Steps: []RunbookStep{
		{RiskLevel: "low"},
		{RiskLevel: "high"},
	}}
	if got := riskFromRunbook(rb); got != RiskHigh {
		t.Errorf("got %v, want high", got)
	}
}

func TestClamp01(t *testing.T) {
	if clamp01(-1) != 0 {
		t.Error("clamp01(-1) should be 0")
	}
	if clamp01(2) != 1 {
		t.Error("clamp01(2) should be 1")
	}
	if clamp01(0.5) != 0.5 {
		t.Error("clamp01(0.5) should be 0.5")
	}
}

func TestDefaultPreConditions(t *testing.T) {
	low := defaultPreConditions(RiskLow)
	if len(low) != 2 {
		t.Errorf("low pre-conditions = %v", low)
	}
	high := defaultPreConditions(RiskHigh)
	if len(high) != 3 {
		t.Errorf("high pre-conditions = %v", high)
	}
}

func TestDefaultRollbackPlan(t *testing.T) {
	if defaultRollbackPlan(RiskCritical) == "" {
		t.Error("critical rollback should not be empty")
	}
	if defaultRollbackPlan(RiskLow) == "" {
		t.Error("low rollback should not be empty")
	}
}

// --- WorkflowGenerator.Generate --------------------------------------------

func TestWorkflowGenerator_Generate(t *testing.T) {
	g := NewWorkflowGenerator()
	steps := []FixStep{
		{Name: "restart", Module: "svc", Action: "restart", Target: "java",
			Args: map[string]string{"service": "java"}, Description: "restart java"},
	}
	out, err := g.Generate("host-01", steps)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "name: auto-fix-host-01-") {
		t.Errorf("missing name: %s", out)
	}
	if !strings.Contains(out, "host-01") {
		t.Errorf("missing target: %s", out)
	}
	if !strings.Contains(out, "module: svc") {
		t.Errorf("missing module: %s", out)
	}
	if !strings.Contains(out, "action: restart") {
		t.Errorf("missing action: %s", out)
	}
	if !strings.Contains(out, "service: java") {
		t.Errorf("missing arg: %s", out)
	}
	if !strings.Contains(out, "rollback:") {
		t.Errorf("missing rollback section: %s", out)
	}
	if !strings.Contains(out, "rollback-restart") {
		t.Errorf("missing rollback step: %s", out)
	}
}

func TestWorkflowGenerator_Generate_EmptyTarget(t *testing.T) {
	g := NewWorkflowGenerator()
	_, err := g.Generate("", []FixStep{{Name: "x"}})
	if !errors.Is(err, ErrEmptyTarget) {
		t.Errorf("err = %v, want ErrEmptyTarget", err)
	}
}

func TestWorkflowGenerator_Generate_NoSteps(t *testing.T) {
	g := NewWorkflowGenerator()
	_, err := g.Generate("h", nil)
	if !errors.Is(err, ErrNoSteps) {
		t.Errorf("err = %v, want ErrNoSteps", err)
	}
}

func TestWorkflowGenerator_Generate_DefaultsModuleAndAction(t *testing.T) {
	g := NewWorkflowGenerator()
	steps := []FixStep{{Name: "step1"}}
	out, err := g.Generate("h", steps)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "module: shell") {
		t.Errorf("expected default module shell: %s", out)
	}
	if !strings.Contains(out, "action: run") {
		t.Errorf("expected default action run: %s", out)
	}
}

func TestWorkflowGenerator_Generate_EmptyArgs(t *testing.T) {
	g := NewWorkflowGenerator()
	steps := []FixStep{{Name: "s", Module: "shell", Action: "run"}}
	out, err := g.Generate("h", steps)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "args: {}") {
		t.Errorf("expected empty args: %s", out)
	}
}

// --- WorkflowGenerator.GenerateFromMatch -----------------------------------

func TestWorkflowGenerator_GenerateFromMatch_Incident(t *testing.T) {
	g := NewWorkflowGenerator()
	inc := HistoricalIncident{
		ID:         "inc-1",
		Title:      "oom",
		Resolution: "restart java",
		Tags:       []string{"java", "oom"},
	}
	m := &Match{Type: MatchTypeIncident, ID: "inc-1", Title: "oom", Score: 0.9, Source: inc}
	out, err := g.GenerateFromMatch("host", m)
	if err != nil {
		t.Fatalf("GenerateFromMatch: %v", err)
	}
	if !strings.Contains(out, "restart-service") {
		t.Errorf("expected restart-service step: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_IncidentDisk(t *testing.T) {
	g := NewWorkflowGenerator()
	inc := HistoricalIncident{ID: "i", Tags: []string{"disk"}, Resolution: "clean"}
	m := &Match{Type: MatchTypeIncident, ID: "i", Source: inc}
	out, err := g.GenerateFromMatch("h", m)
	if err != nil {
		t.Fatalf("GenerateFromMatch: %v", err)
	}
	if !strings.Contains(out, "clean-disk") {
		t.Errorf("expected clean-disk: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_IncidentNetwork(t *testing.T) {
	g := NewWorkflowGenerator()
	inc := HistoricalIncident{ID: "i", Tags: []string{"network"}, Resolution: "restart net"}
	m := &Match{Type: MatchTypeIncident, ID: "i", Source: inc}
	out, err := g.GenerateFromMatch("h", m)
	if err != nil {
		t.Fatalf("GenerateFromMatch: %v", err)
	}
	if !strings.Contains(out, "restart-network") {
		t.Errorf("expected restart-network: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_IncidentConfig(t *testing.T) {
	g := NewWorkflowGenerator()
	inc := HistoricalIncident{ID: "i", Tags: []string{"config"}, Resolution: "rollback"}
	m := &Match{Type: MatchTypeIncident, ID: "i", Source: inc}
	out, err := g.GenerateFromMatch("h", m)
	if err != nil {
		t.Fatalf("GenerateFromMatch: %v", err)
	}
	if !strings.Contains(out, "rollback-config") {
		t.Errorf("expected rollback-config: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_IncidentGeneric(t *testing.T) {
	g := NewWorkflowGenerator()
	inc := HistoricalIncident{ID: "i", Tags: []string{"unknown-tag"}, Resolution: "do something"}
	m := &Match{Type: MatchTypeIncident, ID: "i", Source: inc}
	out, err := g.GenerateFromMatch("h", m)
	if err != nil {
		t.Fatalf("GenerateFromMatch: %v", err)
	}
	if !strings.Contains(out, "apply-resolution") {
		t.Errorf("expected apply-resolution: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_Runbook(t *testing.T) {
	g := NewWorkflowGenerator()
	rb := Runbook{
		ID:   "rb-1",
		Name: "restart",
		Steps: []RunbookStep{
			{Action: "capture", Command: "jstack", Description: "capture threads"},
			{Action: "restart", Command: "systemctl restart x", Description: "restart"},
		},
	}
	m := &Match{Type: MatchTypeRunbook, ID: "rb-1", Source: rb}
	out, err := g.GenerateFromMatch("h", m)
	if err != nil {
		t.Fatalf("GenerateFromMatch: %v", err)
	}
	if !strings.Contains(out, "capture") {
		t.Errorf("expected capture step: %s", out)
	}
	if !strings.Contains(out, "restart") {
		t.Errorf("expected restart step: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_Pattern(t *testing.T) {
	g := NewWorkflowGenerator()
	p := FixPattern{ID: "p", Name: "oom", Fix: "restart jvm", Tags: []string{"java"}}
	m := &Match{Type: MatchTypePattern, ID: "p", Source: p}
	out, err := g.GenerateFromMatch("h", m)
	if err != nil {
		t.Fatalf("GenerateFromMatch: %v", err)
	}
	if !strings.Contains(out, "restart-service") {
		t.Errorf("expected restart-service: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_PatternDisk(t *testing.T) {
	g := NewWorkflowGenerator()
	p := FixPattern{ID: "p", Fix: "clean", Tags: []string{"disk"}}
	m := &Match{Type: MatchTypePattern, ID: "p", Source: p}
	out, err := g.GenerateFromMatch("h", m)
	if err != nil {
		t.Fatalf("GenerateFromMatch: %v", err)
	}
	if !strings.Contains(out, "clean-disk") {
		t.Errorf("expected clean-disk: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_PatternDB(t *testing.T) {
	g := NewWorkflowGenerator()
	p := FixPattern{ID: "p", Fix: "tune", Tags: []string{"database"}}
	m := &Match{Type: MatchTypePattern, ID: "p", Source: p}
	out, err := g.GenerateFromMatch("h", m)
	if err != nil {
		t.Fatalf("GenerateFromMatch: %v", err)
	}
	if !strings.Contains(out, "tune-pool") {
		t.Errorf("expected tune-pool: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_PatternGeneric(t *testing.T) {
	g := NewWorkflowGenerator()
	p := FixPattern{ID: "p", Fix: "do thing", Tags: []string{"unknown"}}
	m := &Match{Type: MatchTypePattern, ID: "p", Source: p}
	out, err := g.GenerateFromMatch("h", m)
	if err != nil {
		t.Fatalf("GenerateFromMatch: %v", err)
	}
	if !strings.Contains(out, "apply-fix") {
		t.Errorf("expected apply-fix: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_EmptyTarget(t *testing.T) {
	g := NewWorkflowGenerator()
	m := &Match{Type: MatchTypePattern, Source: FixPattern{ID: "p"}}
	_, err := g.GenerateFromMatch("", m)
	if !errors.Is(err, ErrEmptyTarget) {
		t.Errorf("err = %v, want ErrEmptyTarget", err)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_NilMatch(t *testing.T) {
	g := NewWorkflowGenerator()
	_, err := g.GenerateFromMatch("h", nil)
	if !errors.Is(err, ErrNoSteps) {
		t.Errorf("err = %v, want ErrNoSteps", err)
	}
}

func TestWorkflowGenerator_GenerateFromMatch_BadSourceType(t *testing.T) {
	g := NewWorkflowGenerator()
	m := &Match{Type: MatchTypeIncident, Source: "not an incident"}
	_, err := g.GenerateFromMatch("h", m)
	if !errors.Is(err, ErrNoSteps) {
		t.Errorf("err = %v, want ErrNoSteps", err)
	}
}

// --- WorkflowGenerator.GenerateFromLLM -------------------------------------

func TestWorkflowGenerator_GenerateFromLLM_ValidJSON(t *testing.T) {
	g := NewWorkflowGenerator()
	resp := `{"steps":[{"name":"restart","module":"svc","action":"restart","args":{"service":"java"}}]}`
	out, err := g.GenerateFromLLM("h", resp)
	if err != nil {
		t.Fatalf("GenerateFromLLM: %v", err)
	}
	if !strings.Contains(out, "restart") {
		t.Errorf("expected restart step: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromLLM_InvalidJSON(t *testing.T) {
	g := NewWorkflowGenerator()
	out, err := g.GenerateFromLLM("h", "not json")
	if err != nil {
		t.Fatalf("GenerateFromLLM: %v", err)
	}
	// Should fall back to a review step.
	if !strings.Contains(out, "review-llm-output") {
		t.Errorf("expected review-llm-output fallback: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromLLM_EmptyResponse(t *testing.T) {
	g := NewWorkflowGenerator()
	out, err := g.GenerateFromLLM("h", "")
	if err != nil {
		t.Fatalf("GenerateFromLLM: %v", err)
	}
	if !strings.Contains(out, "review-llm-output") {
		t.Errorf("expected review-llm-output fallback: %s", out)
	}
}

func TestWorkflowGenerator_GenerateFromLLM_EmptyTarget(t *testing.T) {
	g := NewWorkflowGenerator()
	_, err := g.GenerateFromLLM("", "{}")
	if !errors.Is(err, ErrEmptyTarget) {
		t.Errorf("err = %v, want ErrEmptyTarget", err)
	}
}

func TestWorkflowGenerator_GenerateFromLLM_DefaultsModuleAction(t *testing.T) {
	g := NewWorkflowGenerator()
	resp := `{"steps":[{"name":"s"}]}`
	out, err := g.GenerateFromLLM("h", resp)
	if err != nil {
		t.Fatalf("GenerateFromLLM: %v", err)
	}
	if !strings.Contains(out, "module: shell") {
		t.Errorf("expected default module: %s", out)
	}
	if !strings.Contains(out, "action: run") {
		t.Errorf("expected default action: %s", out)
	}
}

// --- reverseAction ---------------------------------------------------------

func TestReverseAction(t *testing.T) {
	cases := map[string]string{
		"restart":   "restart",
		"stop":      "start",
		"start":     "stop",
		"remove":    "restore",
		"delete":    "restore",
		"copy":      "restore",
		"write":     "restore",
		"install":   "uninstall",
		"uninstall": "install",
		"unknown":   "noop",
	}
	for in, want := range cases {
		if got := reverseAction(in); got != want {
			t.Errorf("reverseAction(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- yaml helpers ----------------------------------------------------------

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"host-01.prod": "host-01.prod",
		"host 01":      "host-01",
		"":             "unknown",
		"a/b/c":        "a-b-c",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestYamlScalar(t *testing.T) {
	if yamlScalar("") != "\"\"" {
		t.Error("empty should be quoted")
	}
	if yamlScalar("plain") != "plain" {
		t.Error("plain should not be quoted")
	}
	out := yamlScalar("has: colon")
	if !strings.HasPrefix(out, "\"") {
		t.Errorf("colon should be quoted: %s", out)
	}
}

func TestOrDefault(t *testing.T) {
	if orDefault("", "x") != "x" {
		t.Error("empty should return default")
	}
	if orDefault("a", "x") != "a" {
		t.Error("non-empty should return itself")
	}
}

func TestLowerTags(t *testing.T) {
	out := lowerTags([]string{"Java", "OOM"})
	if out[0] != "java" || out[1] != "oom" {
		t.Errorf("lowerTags = %v", out)
	}
}

func TestContainsAny(t *testing.T) {
	if !containsAny([]string{"java", "oom"}, "oom") {
		t.Error("should contain oom")
	}
	if containsAny([]string{"java"}, "disk") {
		t.Error("should not contain disk")
	}
	if !containsAny([]string{"a", "b"}, "a", "c") {
		t.Error("should contain a")
	}
}

func TestSortedKeys(t *testing.T) {
	m := map[string]string{"b": "2", "a": "1", "c": "3"}
	k := sortedKeys(m)
	if k[0] != "a" || k[1] != "b" || k[2] != "c" {
		t.Errorf("sortedKeys = %v", k)
	}
}

// --- parseLLMSteps ---------------------------------------------------------

func TestParseLLMSteps_Valid(t *testing.T) {
	resp := `{"steps":[{"name":"a"},{"name":"b"}]}`
	steps := parseLLMSteps(resp)
	if len(steps) != 2 {
		t.Errorf("len = %d, want 2", len(steps))
	}
}

func TestParseLLMSteps_Empty(t *testing.T) {
	if parseLLMSteps("") != nil {
		t.Error("empty should return nil")
	}
}

func TestParseLLMSteps_NoSteps(t *testing.T) {
	if parseLLMSteps(`{"steps":[]}`) != nil {
		t.Error("empty steps should return nil")
	}
}

func TestParseLLMSteps_InvalidJSON(t *testing.T) {
	if parseLLMSteps("not json") != nil {
		t.Error("invalid JSON should return nil")
	}
}

// --- placeholderWorkflow ---------------------------------------------------

func TestPlaceholderWorkflow(t *testing.T) {
	out := placeholderWorkflow("h")
	if !strings.Contains(out, "manual review required") {
		t.Errorf("expected manual review: %s", out)
	}
	if !strings.Contains(out, "approval: manual") {
		t.Errorf("expected manual approval: %s", out)
	}
}

// --- errorLLMClient (test double) ------------------------------------------

// errorLLMClient is an LLMClient that always returns an error.
type errorLLMClient struct{}

func (c *errorLLMClient) Chat(_ context.Context, _ []LLMMessage) (string, error) {
	return "", errors.New("llm unavailable")
}

func (c *errorLLMClient) ChatWithSystem(_ context.Context, _ string, _ []LLMMessage) (string, error) {
	return "", errors.New("llm unavailable")
}

func (c *errorLLMClient) Name() string { return "error" }