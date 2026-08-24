package autoplanner

// post_report_test.go exercises the PostReportGenerator and PostReport
// implemented in post_report.go. The tests use table-driven style with
// testify/require + assert and aim for 85%+ coverage.
//
// Audit-chain verification is exercised both via the skip path (nil verifier
// or empty run id) and via the real path (SQLiteStore + HashChainBuilder +
// ChainVerifier) so that the valid / failed / error branches are all
// covered.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/recommend"
	"github.com/nexus/levee/internal/state"
)

// --- Test fixtures ---------------------------------------------------------

// newWorkflow builds a minimal Workflow with the given id and risk level.
// Fields not relevant to the report are left zero.
func newWorkflow(id string, risk recommend.RiskLevel) *Workflow {
	return &Workflow{
		ID:        id,
		Name:      "Restart order-service with larger heap",
		Target:    "order-service.prod",
		RiskLevel: risk,
		CreatedAt: time.Now().UTC(),
	}
}

// newReportGenerator returns a PostReportGenerator with default configuration
// (nil verifier -> chain verification skipped).
func newReportGenerator() *PostReportGenerator {
	return NewPostReportGenerator(PostReportGeneratorConfig{})
}

// baseReportRequest builds a PostReportRequest with sensible defaults so
// individual tests can override only the fields they care about.
func baseReportRequest() PostReportRequest {
	return PostReportRequest{
		Workflow:   newWorkflow("wf-001", recommend.RiskLow),
		AlertID:    "alert-001",
		Success:    true,
		Duration:   42 * time.Second,
		StartedAt:  time.Date(2026, 8, 18, 4, 30, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 8, 18, 4, 30, 42, 0, time.UTC),
		RunID:      "run-001",
	}
}

// --- Audit-chain test helpers ----------------------------------------------

// newTestStore opens a fresh SQLiteStore backed by a temp file. Each test
// gets its own file so tests can run in parallel without colliding.
func newTestStore(t *testing.T) *state.SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "post_report_test.db")
	store, err := state.NewSQLiteStore(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// createRunForReport inserts a minimal run row so that trace records can
// satisfy the trace.run_id -> runs.id foreign-key constraint.
func createRunForReport(t *testing.T, store state.Store, runID string) {
	t.Helper()
	now := time.Now().UTC()
	err := store.CreateRun(context.Background(), &state.Run{
		ID:           runID,
		WorkflowName: "wf-test",
		TemplateName: "tpl-test",
		Status:       "running",
		CreatedAt:    now,
		UpdatedAt:    now,
		Creator:      "tester",
	})
	require.NoError(t, err)
}

// recordTracesForReport records n trace entries for runID via the
// TraceRecorder so that the hash-chain builder has records to chain.
func recordTracesForReport(t *testing.T, store state.Store, runID string, n int) {
	t.Helper()
	rec, err := audit.NewTraceRecorder(store)
	require.NoError(t, err)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, err := rec.RecordStep(ctx, runID, "order-service", "restart", "run",
			map[string]any{"step": i},
			map[string]any{"ok": true},
			time.Second, nil)
		require.NoError(t, err)
	}
}

// buildChainForReport records n traces for runID, builds the hash chain and
// returns the tail hash. It fails the test on any error.
func buildChainForReport(t *testing.T, store state.Store, runID string, n int) string {
	t.Helper()
	ctx := context.Background()
	createRunForReport(t, store, runID)
	recordTracesForReport(t, store, runID, n)
	b, err := audit.NewHashChainBuilder(store)
	require.NoError(t, err)
	_, tail, err := b.Build(ctx, runID)
	require.NoError(t, err)
	return tail
}

// newVerifierWithStore builds a ChainVerifier on top of a fresh temp-file
// store and returns both so the test can insert runs/traces.
func newVerifierWithStore(t *testing.T) (*audit.ChainVerifier, *state.SQLiteStore) {
	t.Helper()
	store := newTestStore(t)
	v, err := audit.NewChainVerifier(store)
	require.NoError(t, err)
	return v, store
}

// --- NewPostReportGenerator ------------------------------------------------

func TestNewPostReportGenerator_Defaults(t *testing.T) {
	g := NewPostReportGenerator(PostReportGeneratorConfig{})

	require.NotNil(t, g)
	assert.Nil(t, g.verifier, "nil Verifier should be preserved (chain verification disabled)")
	assert.NotNil(t, g.log, "log should default to a non-nil logger")
}

func TestNewPostReportGenerator_CustomConfig(t *testing.T) {
	store := newTestStore(t)
	v, err := audit.NewChainVerifier(store)
	require.NoError(t, err)

	g := NewPostReportGenerator(PostReportGeneratorConfig{Verifier: v})

	require.NotNil(t, g)
	assert.Same(t, v, g.verifier, "custom Verifier should be preserved")
}

// --- Generate: error paths -------------------------------------------------

func TestGenerate_NilWorkflow(t *testing.T) {
	g := newReportGenerator()
	req := baseReportRequest()
	req.Workflow = nil

	report, err := g.Generate(context.Background(), req)

	require.ErrorIs(t, err, ErrPostReportNilWorkflow)
	assert.Nil(t, report)
}

func TestGenerate_EmptyWorkflowID(t *testing.T) {
	g := newReportGenerator()
	req := baseReportRequest()
	req.Workflow = newWorkflow("", recommend.RiskLow)

	report, err := g.Generate(context.Background(), req)

	require.ErrorIs(t, err, ErrPostReportEmptyWorkflowID)
	assert.Nil(t, report)
}

// --- Generate: success path -----------------------------------------------

func TestGenerate_Success(t *testing.T) {
	g := newReportGenerator()
	req := baseReportRequest()

	report, err := g.Generate(context.Background(), req)

	require.NoError(t, err)
	require.NotNil(t, report)
	assert.NotEmpty(t, report.ReportID, "ReportID should be a non-empty UUID")
	assert.Equal(t, "wf-001", report.WorkflowID)
	assert.Equal(t, "alert-001", report.AlertID)
	assert.Equal(t, "order-service.prod", report.Target)
	assert.Equal(t, "Restart order-service with larger heap", report.Summary)
	assert.True(t, report.Success)
	assert.Equal(t, 42*time.Second, report.Duration)
	assert.Equal(t, "low", report.RiskLevel)
	assert.False(t, report.GeneratedAt.IsZero(), "GeneratedAt should be set")
}

func TestGenerate_NilContext(t *testing.T) {
	g := newReportGenerator()
	req := baseReportRequest()

	// Generate should tolerate a nil context by substituting background.
	report, err := g.Generate(nil, req)

	require.NoError(t, err)
	require.NotNil(t, report)
}

// --- Generate: metrics delta ----------------------------------------------

func TestGenerate_MetricsDelta(t *testing.T) {
	g := newReportGenerator()
	req := baseReportRequest()
	req.MetricsBefore = map[string]float64{
		"cpu":         50.0,
		"mem":         80.0,
		"only_before": 10.0,
	}
	req.MetricsAfter = map[string]float64{
		"cpu":        30.0,
		"mem":        85.0,
		"only_after": 20.0,
	}

	report, err := g.Generate(context.Background(), req)

	require.NoError(t, err)
	require.NotNil(t, report)
	require.NotNil(t, report.MetricsDelta)

	// delta = after - before
	assert.InDelta(t, -20.0, report.MetricsDelta["cpu"], 1e-9)
	assert.InDelta(t, 5.0, report.MetricsDelta["mem"], 1e-9)
	assert.InDelta(t, -10.0, report.MetricsDelta["only_before"], 1e-9)
	assert.InDelta(t, 20.0, report.MetricsDelta["only_after"], 1e-9)
}

func TestGenerate_MetricsDelta_EmptySnapshots(t *testing.T) {
	g := newReportGenerator()
	req := baseReportRequest()
	// Both snapshots nil -> delta should be nil.

	report, err := g.Generate(context.Background(), req)

	require.NoError(t, err)
	require.NotNil(t, report)
	assert.Nil(t, report.MetricsDelta)
}

// --- Generate: audit chain ------------------------------------------------

func TestGenerate_AuditChainSkipped(t *testing.T) {
	cases := []struct {
		name   string
		verif  *audit.ChainVerifier
		runID  string
		detail string
	}{
		{
			name:   "nil_verifier",
			verif:  nil,
			runID:  "run-001",
			detail: "skipped: no verifier configured",
		},
		{
			name:   "empty_run_id",
			verif:  newReportGenerator().verifier, // still nil, but tests the run-id branch
			runID:  "",
			detail: "skipped: no verifier configured",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewPostReportGenerator(PostReportGeneratorConfig{Verifier: tc.verif})
			req := baseReportRequest()
			req.RunID = tc.runID

			report, err := g.Generate(context.Background(), req)

			require.NoError(t, err)
			require.NotNil(t, report)
			assert.False(t, report.AuditChainValid, "chain should be skipped -> not valid")
			assert.Equal(t, tc.detail, report.AuditChainDetail)
			assert.Zero(t, report.TraceCount)
		})
	}
}

func TestGenerate_AuditChainSkipped_EmptyRunIDWithVerifier(t *testing.T) {
	v, _ := newVerifierWithStore(t)
	g := NewPostReportGenerator(PostReportGeneratorConfig{Verifier: v})

	req := baseReportRequest()
	req.RunID = ""

	report, err := g.Generate(context.Background(), req)

	require.NoError(t, err)
	require.NotNil(t, report)
	assert.False(t, report.AuditChainValid)
	assert.Equal(t, "skipped: no run id provided", report.AuditChainDetail)
	assert.Zero(t, report.TraceCount)
}

func TestGenerate_AuditChainValid(t *testing.T) {
	v, store := newVerifierWithStore(t)
	buildChainForReport(t, store, "run-valid", 3)

	g := NewPostReportGenerator(PostReportGeneratorConfig{Verifier: v})
	req := baseReportRequest()
	req.RunID = "run-valid"

	report, err := g.Generate(context.Background(), req)

	require.NoError(t, err)
	require.NotNil(t, report)
	assert.True(t, report.AuditChainValid, "chain should be valid")
	assert.Equal(t, "valid", report.AuditChainDetail)
	assert.Equal(t, 3, report.TraceCount)
}

func TestGenerate_AuditChainError(t *testing.T) {
	v, store := newVerifierWithStore(t)
	// Create a run with no traces -> Verify returns ErrNoTraces.
	createRunForReport(t, store, "run-empty")

	g := NewPostReportGenerator(PostReportGeneratorConfig{Verifier: v})
	req := baseReportRequest()
	req.RunID = "run-empty"

	report, err := g.Generate(context.Background(), req)

	// Verification error should NOT fail the report; it should be
	// captured in AuditChainDetail.
	require.NoError(t, err)
	require.NotNil(t, report)
	assert.False(t, report.AuditChainValid)
	assert.Contains(t, report.AuditChainDetail, "error:")
	assert.Zero(t, report.TraceCount)
}

func TestGenerate_AuditChainTampered(t *testing.T) {
	v, store := newVerifierWithStore(t)
	buildChainForReport(t, store, "run-tampered", 3)

	// Tamper with the last trace's CurrHash directly so the chain
	// verification completes (no error) but reports Valid=false.
	traces, err := store.ListTraces(context.Background(), state.TraceFilter{RunID: "run-tampered"})
	require.NoError(t, err)
	require.Len(t, traces, 3)
	last := traces[len(traces)-1]
	// state.SQLiteStore.ExecRaw was removed as an escape hatch; tests that
	// need raw SQL use the exposed *sql.DB handle directly.
	_, err = store.DB().ExecContext(context.Background(),
		"UPDATE trace SET curr_hash = ? WHERE id = ?",
		"tampered-curr-hash-value", last.ID)
	require.NoError(t, err)

	g := NewPostReportGenerator(PostReportGeneratorConfig{Verifier: v})
	req := baseReportRequest()
	req.RunID = "run-tampered"

	report, err := g.Generate(context.Background(), req)

	// Tampering should NOT fail the report; it should be captured in
	// AuditChainDetail with the "failed:" prefix.
	require.NoError(t, err)
	require.NotNil(t, report)
	assert.False(t, report.AuditChainValid)
	assert.Contains(t, report.AuditChainDetail, "failed:")
	assert.Contains(t, report.AuditChainDetail, "tampered")
	assert.Equal(t, 3, report.TraceCount)
}

// --- Generate: rollback used ----------------------------------------------

func TestGenerate_RollbackUsed(t *testing.T) {
	g := newReportGenerator()
	req := baseReportRequest()
	req.RollbackUsed = true

	report, err := g.Generate(context.Background(), req)

	require.NoError(t, err)
	require.NotNil(t, report)
	assert.True(t, report.RollbackUsed)
}

func TestGenerate_RollbackNotUsed(t *testing.T) {
	g := newReportGenerator()
	req := baseReportRequest()
	req.RollbackUsed = false

	report, err := g.Generate(context.Background(), req)

	require.NoError(t, err)
	require.NotNil(t, report)
	assert.False(t, report.RollbackUsed)
}

// --- Generate: lessons learned --------------------------------------------

func TestGenerate_LessonsLearned(t *testing.T) {
	g := newReportGenerator()
	req := baseReportRequest()
	req.LessonsLearned = []string{
		"increase heap before restart",
		"verify health endpoint after restart",
	}

	report, err := g.Generate(context.Background(), req)

	require.NoError(t, err)
	require.NotNil(t, report)
	require.Len(t, report.LessonsLearned, 2)
	assert.Equal(t, "increase heap before restart", report.LessonsLearned[0])
	assert.Equal(t, "verify health endpoint after restart", report.LessonsLearned[1])
}

func TestGenerate_LessonsLearned_Nil(t *testing.T) {
	g := newReportGenerator()
	req := baseReportRequest()
	req.LessonsLearned = nil

	report, err := g.Generate(context.Background(), req)

	require.NoError(t, err)
	require.NotNil(t, report)
	assert.Nil(t, report.LessonsLearned)
}

// --- Generate: risk level propagation -------------------------------------

func TestGenerate_RiskLevel(t *testing.T) {
	cases := []struct {
		name string
		risk recommend.RiskLevel
		want string
	}{
		{"low", recommend.RiskLow, "low"},
		{"medium", recommend.RiskMedium, "medium"},
		{"high", recommend.RiskHigh, "high"},
		{"critical", recommend.RiskCritical, "critical"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newReportGenerator()
			req := baseReportRequest()
			req.Workflow = newWorkflow("wf-risk", tc.risk)

			report, err := g.Generate(context.Background(), req)

			require.NoError(t, err)
			require.NotNil(t, report)
			assert.Equal(t, tc.want, report.RiskLevel)
		})
	}
}

// --- PostReport.ToText ----------------------------------------------------

func TestPostReport_ToText(t *testing.T) {
	report := &PostReport{
		ReportID:         "rep-001",
		WorkflowID:       "wf-001",
		AlertID:          "alert-001",
		Target:           "order-service.prod",
		Summary:          "Restart order-service",
		Success:          true,
		Duration:         42 * time.Second,
		StartedAt:        time.Date(2026, 8, 18, 4, 30, 0, 0, time.UTC),
		FinishedAt:       time.Date(2026, 8, 18, 4, 30, 42, 0, time.UTC),
		MetricsBefore:    map[string]float64{"cpu": 50.0},
		MetricsAfter:     map[string]float64{"cpu": 30.0},
		MetricsDelta:     map[string]float64{"cpu": -20.0},
		AuditChainValid:  true,
		AuditChainDetail: "valid",
		TraceCount:       3,
		LessonsLearned:   []string{"lesson one", "lesson two"},
		RiskLevel:        "low",
		RollbackUsed:     false,
		GeneratedAt:      time.Date(2026, 8, 18, 4, 31, 0, 0, time.UTC),
	}

	text := report.ToText()

	assert.Contains(t, text, "Post-Action Audit Report rep-001")
	assert.Contains(t, text, "Workflow ID : wf-001")
	assert.Contains(t, text, "Alert ID    : alert-001")
	assert.Contains(t, text, "Target      : order-service.prod")
	assert.Contains(t, text, "Summary     : Restart order-service")
	assert.Contains(t, text, "Risk Level  : low")
	assert.Contains(t, text, "Success     : true")
	assert.Contains(t, text, "Rollback    : false")
	assert.Contains(t, text, "Duration    : 42s")
	assert.Contains(t, text, "-- Metrics Delta --")
	assert.Contains(t, text, "cpu: 50 -> 30 (delta=-20)")
	assert.Contains(t, text, "-- Audit Chain --")
	assert.Contains(t, text, "Valid       : true")
	assert.Contains(t, text, "Trace Count : 3")
	assert.Contains(t, text, "Detail      : valid")
	assert.Contains(t, text, "-- Lessons Learned --")
	assert.Contains(t, text, "1. lesson one")
	assert.Contains(t, text, "2. lesson two")
}

func TestPostReport_ToText_EmptySections(t *testing.T) {
	report := &PostReport{
		ReportID:   "rep-002",
		WorkflowID: "wf-002",
		Summary:    "Empty report",
	}

	text := report.ToText()

	assert.Contains(t, text, "Post-Action Audit Report rep-002")
	// No AlertID -> the "Alert ID" line should be absent.
	assert.NotContains(t, text, "Alert ID")
	// Empty metrics delta -> "(none)".
	assert.Contains(t, text, "-- Metrics Delta --")
	assert.Contains(t, text, "(none)")
	// Empty lessons -> "(none)".
	assert.Contains(t, text, "-- Lessons Learned --")
	assert.Contains(t, text, "(none)")
	// No chain detail -> "Detail" line absent.
	assert.NotContains(t, text, "Detail")
}

// --- PostReport.ToJSON ----------------------------------------------------

func TestPostReport_ToJSON(t *testing.T) {
	report := &PostReport{
		ReportID:         "rep-001",
		WorkflowID:       "wf-001",
		AlertID:          "alert-001",
		Target:           "order-service.prod",
		Summary:          "Restart order-service",
		Success:          true,
		Duration:         42 * time.Second,
		StartedAt:        time.Date(2026, 8, 18, 4, 30, 0, 0, time.UTC),
		FinishedAt:       time.Date(2026, 8, 18, 4, 30, 42, 0, time.UTC),
		MetricsBefore:    map[string]float64{"cpu": 50.0},
		MetricsAfter:     map[string]float64{"cpu": 30.0},
		MetricsDelta:     map[string]float64{"cpu": -20.0},
		AuditChainValid:  true,
		AuditChainDetail: "valid",
		TraceCount:       3,
		LessonsLearned:   []string{"lesson one"},
		RiskLevel:        "low",
		RollbackUsed:     false,
		GeneratedAt:      time.Date(2026, 8, 18, 4, 31, 0, 0, time.UTC),
	}

	raw, err := report.ToJSON()

	require.NoError(t, err)
	require.NotEmpty(t, raw)

	// Unmarshal back and verify a few fields round-trip.
	var decoded PostReport
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, report.ReportID, decoded.ReportID)
	assert.Equal(t, report.WorkflowID, decoded.WorkflowID)
	assert.Equal(t, report.AlertID, decoded.AlertID)
	assert.Equal(t, report.Summary, decoded.Summary)
	assert.True(t, decoded.Success)
	assert.Equal(t, report.Duration, decoded.Duration)
	assert.Equal(t, report.MetricsDelta["cpu"], decoded.MetricsDelta["cpu"])
	assert.True(t, decoded.AuditChainValid)
	assert.Equal(t, report.AuditChainDetail, decoded.AuditChainDetail)
	assert.Equal(t, report.TraceCount, decoded.TraceCount)
	require.Len(t, decoded.LessonsLearned, 1)
	assert.Equal(t, "lesson one", decoded.LessonsLearned[0])
}

func TestPostReport_ToJSON_OmitsEmptyDetail(t *testing.T) {
	report := &PostReport{
		ReportID:         "rep-003",
		WorkflowID:       "wf-003",
		AuditChainDetail: "", // empty -> should be omitted
	}

	raw, err := report.ToJSON()

	require.NoError(t, err)
	// The omitempty tag should drop the audit_chain_detail field.
	assert.NotContains(t, string(raw), "audit_chain_detail")
}

// --- Generate: end-to-end with real verifier ------------------------------

func TestGenerate_EndToEnd_WithRealVerifier(t *testing.T) {
	v, store := newVerifierWithStore(t)
	buildChainForReport(t, store, "run-e2e", 5)

	g := NewPostReportGenerator(PostReportGeneratorConfig{Verifier: v})
	req := baseReportRequest()
	req.RunID = "run-e2e"
	req.MetricsBefore = map[string]float64{"error_rate": 0.05}
	req.MetricsAfter = map[string]float64{"error_rate": 0.01}
	req.LessonsLearned = []string{"reducing thread pool size lowered error rate"}
	req.RollbackUsed = false

	report, err := g.Generate(context.Background(), req)

	require.NoError(t, err)
	require.NotNil(t, report)
	assert.True(t, report.AuditChainValid)
	assert.Equal(t, 5, report.TraceCount)
	assert.InDelta(t, -0.04, report.MetricsDelta["error_rate"], 1e-9)

	// The report should render to both text and JSON without error.
	text := report.ToText()
	assert.NotEmpty(t, text)

	raw, err := report.ToJSON()
	require.NoError(t, err)
	assert.NotEmpty(t, raw)
}
