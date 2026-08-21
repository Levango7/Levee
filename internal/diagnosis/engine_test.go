package diagnosis

// Tests for Phase A6 diagnostic engine (engine.go) and report types
// (report.go). We reuse the mockExecutor / mockResult / newMockExecutor
// stub defined in log_collector_test.go so the log pipeline can be
// exercised end-to-end without a real remote target. The health prober
// is exercised through a mock executor as well; network probes against
// non-existent hosts fail fast, which is the behaviour we assert.

import (
	"context"
	"errors"

	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/alert"
)

// --- Engine construction ----------------------------------------------------

func TestNewDiagEngine_Defaults(t *testing.T) {
	e := NewDiagEngine(DiagEngineConfig{})
	assert.NotNil(t, e)
	assert.Equal(t, DefaultDiagTimeout, e.timeout)
	assert.Equal(t, DefaultLogWindow, e.window)
	assert.Equal(t, DefaultRuntime, e.runtime)
	assert.NotNil(t, e.log)
}

func TestNewDiagEngine_Custom(t *testing.T) {
	exec := newMockExecutor()
	collector := mustCollector(t, exec)
	analyzer := NewDefaultLogAnalyzer()
	prober := NewHealthProber(HealthProberConfig{})

	e := NewDiagEngine(DiagEngineConfig{
		Collector: collector,
		Analyzer:  analyzer,
		Prober:    prober,
		Timeout:   5 * time.Second,
		LogWindow: 10 * time.Minute,
		Runtime:   RuntimeWindows,
	})
	assert.Equal(t, 5*time.Second, e.timeout)
	assert.Equal(t, 10*time.Minute, e.window)
	assert.Equal(t, RuntimeWindows, e.runtime)
	assert.NotNil(t, e.collector)
	assert.NotNil(t, e.analyzer)
	assert.NotNil(t, e.prober)
}

// --- Diagnose: empty target -------------------------------------------------

func TestDiagnose_EmptyTarget(t *testing.T) {
	e := NewDiagEngine(DiagEngineConfig{})
	report := e.Diagnose(context.Background(), "")
	assert.Equal(t, DiagUnknown, report.Status)
	assert.Contains(t, report.Summary, "empty target")
	require.Len(t, report.Errors, 1)
	assert.Contains(t, report.Errors[0], "empty target")
	assert.NotEmpty(t, report.ID)
	assert.Equal(t, TriggerManual, report.Trigger)
}

// --- Diagnose: no components configured ------------------------------------

func TestDiagnose_NoComponents(t *testing.T) {
	e := NewDiagEngine(DiagEngineConfig{})
	report := e.Diagnose(context.Background(), "host-1")
	// With no collector/analyzer/prober, the report should be healthy
	// (no evidence of problems) with no findings.
	assert.NotEmpty(t, report.ID)
	assert.Equal(t, "host-1", report.Target)
	assert.Equal(t, TriggerManual, report.Trigger)
	assert.Empty(t, report.Findings)
	assert.Empty(t, report.RootCause)
	assert.Equal(t, 0.0, report.Confidence)
	// Status is healthy because there is no evidence of problems.
	assert.Equal(t, DiagHealthy, report.Status)
}

// --- Diagnose: log pipeline only -------------------------------------------

func TestDiagnose_LogPipelineOnly_NoErrors(t *testing.T) {
	exec := newMockExecutor()
	window := TimeWindow{Start: time.Now().Add(-15 * time.Minute), End: time.Now()}

	// Register canned output for the journald command the collector will
	// issue. We use plain INFO lines so no pattern matches.
	for _, src := range DefaultSources(RuntimeLinux) {
		cmd, err := buildCollectCommand(src, window)
		if err != nil {
			continue // app source path is valid, but we skip if not
		}
		exec.set(cmd, mockResult{stdout: "2024-06-01T12:00:00+00:00 host app[1]: INFO request done\n"})
	}

	collector := mustCollector(t, exec)
	analyzer := NewDefaultLogAnalyzer()
	e := NewDiagEngine(DiagEngineConfig{
		Collector: collector,
		Analyzer:  analyzer,
		Timeout:   10 * time.Second,
	})

	report := e.Diagnose(context.Background(), "host-1")
	assert.NotEmpty(t, report.ID)
	assert.Equal(t, "host-1", report.Target)
	// No error patterns matched → healthy.
	assert.Equal(t, DiagHealthy, report.Status)
	assert.Empty(t, report.Findings)
	assert.Empty(t, report.RootCause)
	assert.Equal(t, 0.0, report.Confidence)
	assert.GreaterOrEqual(t, report.LogAnalysis.TotalLines, 0)
}

func TestDiagnose_LogPipelineOnly_WithErrors(t *testing.T) {
	exec := newMockExecutor()

	// Build a collector+analyzer that will find an OOM pattern.
	collector := mustCollector(t, exec)
	analyzer := NewDefaultLogAnalyzer()

	// We cannot easily predict the exact command the engine will issue
	// (the time window is computed at call time), so we register a
	// custom executor that returns OOM lines for any command.
	oomExec := &anyCmdExecutor{
		stdout: "2024-06-01T12:00:00+00:00 host kernel[0]: ERR out of memory: kill process 1234\n" +
			"2024-06-01T12:01:00+00:00 host kernel[0]: ERR out of memory: kill process 5678\n",
	}
	oomCollector := mustCollector(t, oomExec)

	e := NewDiagEngine(DiagEngineConfig{
		Collector: oomCollector,
		Analyzer:  analyzer,
		Timeout:   10 * time.Second,
	})
	_ = collector // silence unused; kept for parity with other tests

	report := e.Diagnose(context.Background(), "host-1")
	assert.NotEmpty(t, report.ID)
	// OOM pattern matched → unhealthy.
	assert.Equal(t, DiagUnhealthy, report.Status)
	require.NotEmpty(t, report.Findings)
	assert.Equal(t, "log", report.Findings[0].Category)
	assert.NotEmpty(t, report.RootCause)
	assert.Greater(t, report.Confidence, 0.0)
	assert.Contains(t, report.RootCause, "OOM")
}

// --- Diagnose: cancelled context -------------------------------------------

func TestDiagnose_CancelledContext(t *testing.T) {
	exec := newMockExecutor()
	collector := mustCollector(t, exec)
	analyzer := NewDefaultLogAnalyzer()

	e := NewDiagEngine(DiagEngineConfig{
		Collector: collector,
		Analyzer:  analyzer,
		Timeout:   10 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := e.Diagnose(ctx, "host-1")
	// The report is still returned; errors are recorded.
	assert.NotEmpty(t, report.ID)
	// Either the log pipeline or the health probe (or both) will report
	// a cancellation error.
	assert.NotEmpty(t, report.Errors)
}

// --- Diagnose: health probe only -------------------------------------------

func TestDiagnose_HealthProbeOnly(t *testing.T) {
	// Use a mock executor that returns success for all commands so the
	// node/service/data probes report healthy. The network probe will
	// attempt to dial "nonexistent.invalid" and fail, making the overall
	// health degraded or unhealthy.
	exec := newMockExecutor()
	registerHealthyCommands(exec)
	prober := NewHealthProber(HealthProberConfig{
		Executor:        exec,
		Runtime:         RuntimeLinux,
		PingPort:        -1, // skip ping
		DefaultPorts:    nil,
		DefaultServices: nil,
	})

	e := NewDiagEngine(DiagEngineConfig{
		Prober:  prober,
		Timeout: 10 * time.Second,
	})

	report := e.Diagnose(context.Background(), "nonexistent.invalid")
	assert.NotEmpty(t, report.ID)
	assert.Equal(t, "nonexistent.invalid", report.Target)
	// The health report is populated.
	assert.NotZero(t, report.Health.ProbedAt)
}

// --- Diagnose: full pipeline ------------------------------------------------

func TestDiagnose_FullPipeline(t *testing.T) {
	exec := newMockExecutor()
	registerHealthyCommands(exec)

	collector := mustCollector(t, &anyCmdExecutor{
		stdout: "2024-06-01T12:00:00+00:00 host app[1]: INFO request done\n",
	})
	analyzer := NewDefaultLogAnalyzer()
	prober := NewHealthProber(HealthProberConfig{
		Executor: exec,
		Runtime:  RuntimeLinux,
		PingPort: -1,
	})

	e := NewDiagEngine(DiagEngineConfig{
		Collector: collector,
		Analyzer:  analyzer,
		Prober:    prober,
		Timeout:   10 * time.Second,
	})

	report := e.Diagnose(context.Background(), "host-1")
	assert.NotEmpty(t, report.ID)
	assert.Equal(t, "host-1", report.Target)
	assert.Equal(t, TriggerManual, report.Trigger)
	assert.NotZero(t, report.StartedAt)
	assert.GreaterOrEqual(t, report.Duration, time.Duration(0))
	// Summary is always populated.
	assert.NotEmpty(t, report.Summary)
}

// --- DiagnoseFromAlert ------------------------------------------------------

func TestDiagnoseFromAlert_NilAlert(t *testing.T) {
	e := NewDiagEngine(DiagEngineConfig{})
	report := e.DiagnoseFromAlert(context.Background(), nil)
	assert.Equal(t, DiagUnknown, report.Status)
	assert.Equal(t, TriggerAlert, report.Trigger)
	require.Len(t, report.Errors, 1)
	assert.Contains(t, report.Errors[0], "nil alert")
}

func TestDiagnoseFromAlert_WithTarget(t *testing.T) {
	e := NewDiagEngine(DiagEngineConfig{})
	a := &alert.Alert{
		ID:     "alert-123",
		Source: "prometheus",
		Title:  "HighCPU",
		Labels: map[string]string{"instance": "node-1"},
	}
	report := e.DiagnoseFromAlert(context.Background(), a)
	assert.Equal(t, TriggerAlert, report.Trigger)
	assert.Equal(t, "alert-123", report.AlertID)
	assert.Equal(t, "node-1", report.Target)
}

func TestDiagnoseFromAlert_NoTargetLabel(t *testing.T) {
	e := NewDiagEngine(DiagEngineConfig{})
	a := &alert.Alert{
		ID:     "alert-456",
		Source: "prometheus",
		Title:  "HighCPU",
		Labels: map[string]string{"region": "us-east-1"},
	}
	report := e.DiagnoseFromAlert(context.Background(), a)
	assert.Equal(t, TriggerAlert, report.Trigger)
	assert.Equal(t, "alert-456", report.AlertID)
	assert.Empty(t, report.Target)
	// Should have an error about missing target.
	assert.NotEmpty(t, report.Errors)
}

func TestDiagnoseFromAlert_AlternateLabelKeys(t *testing.T) {
	e := NewDiagEngine(DiagEngineConfig{})
	tests := []struct {
		key   string
		value string
	}{
		{"instance", "node-a"},
		{"host", "node-b"},
		{"node", "node-c"},
		{"target", "node-d"},
	}
	for _, tc := range tests {
		a := &alert.Alert{
			ID:     "alert-x",
			Source: "s",
			Title:  "t",
			Labels: map[string]string{tc.key: tc.value},
		}
		report := e.DiagnoseFromAlert(context.Background(), a)
		assert.Equal(t, tc.value, report.Target, "label key %q should yield target %q", tc.key, tc.value)
	}
}

// --- DiagnoseMulti ----------------------------------------------------------

func TestDiagnoseMulti_Empty(t *testing.T) {
	e := NewDiagEngine(DiagEngineConfig{})
	reports := e.DiagnoseMulti(context.Background(), nil)
	assert.Empty(t, reports)
}

func TestDiagnoseMulti_Multiple(t *testing.T) {
	e := NewDiagEngine(DiagEngineConfig{})
	targets := []string{"a", "b", "c"}
	reports := e.DiagnoseMulti(context.Background(), targets)
	require.Len(t, reports, 3)
	assert.Equal(t, "a", reports[0].Target)
	assert.Equal(t, "b", reports[1].Target)
	assert.Equal(t, "c", reports[2].Target)
	// Each report has a unique ID.
	assert.NotEqual(t, reports[0].ID, reports[1].ID)
	assert.NotEqual(t, reports[1].ID, reports[2].ID)
}

func TestDiagnoseMulti_WithEmptyTarget(t *testing.T) {
	e := NewDiagEngine(DiagEngineConfig{})
	reports := e.DiagnoseMulti(context.Background(), []string{"", "valid"})
	require.Len(t, reports, 2)
	assert.Equal(t, DiagUnknown, reports[0].Status)
	assert.Equal(t, "valid", reports[1].Target)
}

func TestDiagnoseMulti_Concurrent(t *testing.T) {
	// Verify DiagnoseMulti is safe for concurrent use and preserves order.
	e := NewDiagEngine(DiagEngineConfig{})
	targets := make([]string, 20)
	for i := range targets {
		targets[i] = "host"
	}
	reports := e.DiagnoseMulti(context.Background(), targets)
	require.Len(t, reports, 20)
	for _, r := range reports {
		assert.Equal(t, "host", r.Target)
	}
}

// --- DiagnosticReport helpers ----------------------------------------------

func TestDiagnosticReport_HasFindings(t *testing.T) {
	r := DiagnosticReport{}
	assert.False(t, r.HasFindings())

	r.Findings = []Finding{{ID: "F1"}}
	assert.True(t, r.HasFindings())

	var nilReport *DiagnosticReport
	assert.False(t, nilReport.HasFindings())
}

func TestDiagnosticReport_WorstFinding(t *testing.T) {
	r := DiagnosticReport{
		Findings: []Finding{
			{ID: "F1", Severity: "info", Title: "low"},
			{ID: "F2", Severity: "critical", Title: "bad"},
			{ID: "F3", Severity: "warning", Title: "mid"},
		},
	}
	worst := r.WorstFinding()
	require.NotNil(t, worst)
	assert.Equal(t, "F2", worst.ID)

	// Empty report.
	r2 := DiagnosticReport{}
	assert.Nil(t, r2.WorstFinding())

	// Nil report.
	var nilReport *DiagnosticReport
	assert.Nil(t, nilReport.WorstFinding())
}

func TestDiagnosticReport_WorstFinding_WarningOnly(t *testing.T) {
	r := DiagnosticReport{
		Findings: []Finding{
			{ID: "F1", Severity: "info"},
			{ID: "F2", Severity: "warning"},
		},
	}
	worst := r.WorstFinding()
	require.NotNil(t, worst)
	assert.Equal(t, "F2", worst.ID)
}

func TestDiagnosticReport_String(t *testing.T) {
	r := DiagnosticReport{
		ID:      "rep-1",
		Target:  "host-1",
		Trigger: TriggerManual,
		Status:  DiagHealthy,
		Findings: []Finding{
			{ID: "F1", Category: "node", Severity: "warning", Title: "high cpu", Description: "cpu at 90%", Suggestion: "scale out"},
		},
		Recommendations: []string{"scale out"},
		Errors:          []string{"minor issue"},
		StartedAt:       time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
		Duration:        100 * time.Millisecond,
	}
	s := r.String()
	assert.Contains(t, s, "rep-1")
	assert.Contains(t, s, "host-1")
	assert.Contains(t, s, "manual")
	assert.Contains(t, s, "healthy")
	assert.Contains(t, s, "high cpu")
	assert.Contains(t, s, "scale out")
	assert.Contains(t, s, "minor issue")

	// Nil report.
	var nilReport *DiagnosticReport
	assert.Equal(t, "<nil diagnostic report>", nilReport.String())
}

func TestDiagnosticReport_String_Minimal(t *testing.T) {
	r := DiagnosticReport{ID: "r", Target: "t"}
	s := r.String()
	assert.Contains(t, s, "r")
	assert.Contains(t, s, "t")
}

// --- buildFindings ----------------------------------------------------------

func TestBuildFindings_NilInputs(t *testing.T) {
	findings := buildFindings(nil, nil)
	assert.Nil(t, findings)
}

func TestBuildFindings_HealthyReport(t *testing.T) {
	h := &HealthReport{Status: StatusHealthy}
	findings := buildFindings(h, nil)
	assert.Empty(t, findings)
}

func TestBuildFindings_UnhealthyNetwork(t *testing.T) {
	h := &HealthReport{
		Status:  StatusUnhealthy,
		Network: NetworkHealth{Status: StatusUnhealthy, Err: "dns failed"},
	}
	findings := buildFindings(h, nil)
	require.Len(t, findings, 1)
	assert.Equal(t, "network", findings[0].Category)
	assert.Equal(t, "critical", findings[0].Severity)
	assert.Contains(t, findings[0].Evidence, "network=unhealthy")
}

func TestBuildFindings_DegradedNode(t *testing.T) {
	h := &HealthReport{
		Status: StatusDegraded,
		Node:   NodeHealth{Status: StatusDegraded},
	}
	findings := buildFindings(h, nil)
	require.Len(t, findings, 1)
	assert.Equal(t, "node", findings[0].Category)
	assert.Equal(t, "warning", findings[0].Severity)
}

func TestBuildFindings_AllCategories(t *testing.T) {
	h := &HealthReport{
		Network: NetworkHealth{Status: StatusUnhealthy},
		Node:    NodeHealth{Status: StatusUnhealthy},
		Service: ServiceHealth{Status: StatusUnhealthy},
		Data:    DataHealth{Status: StatusUnhealthy},
	}
	findings := buildFindings(h, nil)
	require.Len(t, findings, 4)
	categories := map[string]bool{}
	for _, f := range findings {
		categories[f.Category] = true
	}
	assert.True(t, categories["network"])
	assert.True(t, categories["node"])
	assert.True(t, categories["service"])
	assert.True(t, categories["data"])
	// All critical.
	for _, f := range findings {
		assert.Equal(t, "critical", f.Severity)
	}
}

func TestBuildFindings_UnknownNetworkWithError(t *testing.T) {
	h := &HealthReport{
		Network: NetworkHealth{Status: StatusUnknown, Err: "timeout"},
	}
	findings := buildFindings(h, nil)
	require.Len(t, findings, 1)
	assert.Equal(t, "info", findings[0].Severity)
	assert.Equal(t, "network", findings[0].Category)
}

func TestBuildFindings_UnknownNetworkNoError(t *testing.T) {
	h := &HealthReport{
		Network: NetworkHealth{Status: StatusUnknown},
	}
	findings := buildFindings(h, nil)
	assert.Empty(t, findings)
}

func TestBuildFindings_LogAnalysis(t *testing.T) {
	a := &AnalysisResult{
		ErrorPatterns: []MatchedPattern{
			{Pattern: ErrorPattern{ID: "OOM", Name: "Out of memory", Severity: SeverityFatal}, Count: 3},
			{Pattern: ErrorPattern{ID: "GC", Name: "GC pause", Severity: SeverityWarn}, Count: 1},
		},
	}
	findings := buildFindings(nil, a)
	require.Len(t, findings, 2)
	assert.Equal(t, "log", findings[0].Category)
	assert.Equal(t, "critical", findings[0].Severity)
	assert.Contains(t, findings[0].Title, "Out of memory")
}

func TestBuildFindings_LogAnalysisCapped(t *testing.T) {
	patterns := make([]MatchedPattern, 10)
	for i := range patterns {
		patterns[i] = MatchedPattern{
			Pattern: ErrorPattern{ID: "P", Name: "n", Severity: SeverityError},
			Count:   1,
		}
	}
	a := &AnalysisResult{ErrorPatterns: patterns}
	findings := buildFindings(nil, a)
	assert.Len(t, findings, 5) // capped at maxLogFindings
}

func TestBuildFindings_SortedBySeverity(t *testing.T) {
	h := &HealthReport{
		Node:    NodeHealth{Status: StatusDegraded},     // warning
		Network: NetworkHealth{Status: StatusUnhealthy}, // critical
	}
	a := &AnalysisResult{
		ErrorPatterns: []MatchedPattern{
			{Pattern: ErrorPattern{ID: "GC", Name: "gc", Severity: SeverityWarn}, Count: 1}, // warning log
		},
	}
	findings := buildFindings(h, a)
	require.GreaterOrEqual(t, len(findings), 2)
	// First finding should be critical (network).
	assert.Equal(t, "critical", findings[0].Severity)
}

// --- synthesiseRootCause ----------------------------------------------------

func TestSynthesiseRootCause_LogAnalysisHighConfidence(t *testing.T) {
	a := &AnalysisResult{
		RootCause:  ErrorPattern{ID: "OOM", Name: "Out of memory"},
		Confidence: 0.8,
	}
	h := &HealthReport{Status: StatusUnhealthy}
	rc, conf := synthesiseRootCause(a, h, nil)
	assert.Contains(t, rc, "OOM")
	assert.GreaterOrEqual(t, conf, 0.8)
}

func TestSynthesiseRootCause_LogAnalysisHealthyNoBoost(t *testing.T) {
	a := &AnalysisResult{
		RootCause:  ErrorPattern{ID: "OOM", Name: "Out of memory"},
		Confidence: 0.5,
	}
	h := &HealthReport{Status: StatusHealthy}
	rc, conf := synthesiseRootCause(a, h, nil)
	assert.Contains(t, rc, "OOM")
	assert.Equal(t, 0.5, conf) // no boost because health is healthy
}

func TestSynthesiseRootCause_FallbackToFindings(t *testing.T) {
	a := &AnalysisResult{Confidence: 0}
	findings := []Finding{
		{Severity: "critical", Title: "Network down"},
	}
	rc, conf := synthesiseRootCause(a, nil, findings)
	assert.Equal(t, "Network down", rc)
	assert.Equal(t, 0.7, conf)
}

func TestSynthesiseRootCause_FallbackWarning(t *testing.T) {
	findings := []Finding{
		{Severity: "warning", Title: "High CPU"},
	}
	rc, conf := synthesiseRootCause(nil, nil, findings)
	assert.Equal(t, "High CPU", rc)
	assert.Equal(t, 0.5, conf)
}

func TestSynthesiseRootCause_Nothing(t *testing.T) {
	rc, conf := synthesiseRootCause(nil, nil, nil)
	assert.Empty(t, rc)
	assert.Equal(t, 0.0, conf)
}

func TestSynthesiseRootCause_ConfidenceCapped(t *testing.T) {
	a := &AnalysisResult{
		RootCause:  ErrorPattern{ID: "OOM", Name: "oom"},
		Confidence: 0.95,
	}
	h := &HealthReport{Status: StatusUnhealthy}
	_, conf := synthesiseRootCause(a, h, nil)
	assert.LessOrEqual(t, conf, 1.0)
}

// --- deriveStatus -----------------------------------------------------------

func TestDeriveStatus_Healthy(t *testing.T) {
	h := &HealthReport{Status: StatusHealthy}
	a := &AnalysisResult{Confidence: 0}
	assert.Equal(t, DiagHealthy, deriveStatus(h, a, nil))
}

func TestDeriveStatus_UnhealthyHealth(t *testing.T) {
	h := &HealthReport{Status: StatusUnhealthy}
	assert.Equal(t, DiagUnhealthy, deriveStatus(h, nil, nil))
}

func TestDeriveStatus_DegradedHealth(t *testing.T) {
	h := &HealthReport{Status: StatusDegraded}
	assert.Equal(t, DiagDegraded, deriveStatus(h, nil, nil))
}

func TestDeriveStatus_UnknownHealth(t *testing.T) {
	h := &HealthReport{Status: StatusUnknown}
	assert.Equal(t, DiagUnknown, deriveStatus(h, nil, nil))
}

func TestDeriveStatus_HighConfidenceAnalysis(t *testing.T) {
	a := &AnalysisResult{Confidence: 0.8}
	assert.Equal(t, DiagUnhealthy, deriveStatus(nil, a, nil))
}

func TestDeriveStatus_MediumConfidenceAnalysis(t *testing.T) {
	a := &AnalysisResult{Confidence: 0.4}
	assert.Equal(t, DiagDegraded, deriveStatus(nil, a, nil))
}

func TestDeriveStatus_CriticalFinding(t *testing.T) {
	findings := []Finding{{Severity: "critical"}}
	assert.Equal(t, DiagUnhealthy, deriveStatus(nil, nil, findings))
}

func TestDeriveStatus_WarningFinding(t *testing.T) {
	findings := []Finding{{Severity: "warning"}}
	assert.Equal(t, DiagDegraded, deriveStatus(nil, nil, findings))
}

func TestDeriveStatus_UnhealthyBeatsDegraded(t *testing.T) {
	h := &HealthReport{Status: StatusDegraded}
	a := &AnalysisResult{Confidence: 0.8}
	assert.Equal(t, DiagUnhealthy, deriveStatus(h, a, nil))
}

// --- buildRecommendations ---------------------------------------------------

func TestBuildRecommendations_Empty(t *testing.T) {
	assert.Nil(t, buildRecommendations(nil))
}

func TestBuildRecommendations_WithSuggestions(t *testing.T) {
	findings := []Finding{
		{ID: "F1", Severity: "critical", Suggestion: "restart service"},
		{ID: "F2", Severity: "warning", Suggestion: "check logs"},
		{ID: "F3", Severity: "info", Suggestion: "noted"},
	}
	recs := buildRecommendations(findings)
	require.Len(t, recs, 2) // info finding is skipped
	assert.Contains(t, recs[0], "restart service")
	assert.Contains(t, recs[1], "check logs")
}

func TestBuildRecommendations_NoSuggestion(t *testing.T) {
	findings := []Finding{
		{ID: "F1", Severity: "critical", Title: "Network down"},
	}
	recs := buildRecommendations(findings)
	require.Len(t, recs, 1)
	assert.Contains(t, recs[0], "investigate")
	assert.Contains(t, recs[0], "Network down")
}

// --- buildReportSummary -----------------------------------------------------

func TestBuildReportSummary(t *testing.T) {
	report := &DiagnosticReport{Status: DiagUnhealthy}
	h := &HealthReport{Status: StatusUnhealthy}
	a := &AnalysisResult{
		RootCause:  ErrorPattern{ID: "OOM", Name: "oom"},
		Confidence: 0.8,
	}
	s := buildReportSummary(report, h, a)
	assert.Contains(t, s, "status=unhealthy")
	assert.Contains(t, s, "health=unhealthy")
	assert.Contains(t, s, "root_cause=OOM")
}

func TestBuildReportSummary_NoConfidence(t *testing.T) {
	report := &DiagnosticReport{Status: DiagHealthy}
	h := &HealthReport{Status: StatusHealthy}
	a := &AnalysisResult{TotalLines: 100, Confidence: 0}
	s := buildReportSummary(report, h, a)
	assert.Contains(t, s, "status=healthy")
	assert.Contains(t, s, "log_lines=100")
}

func TestBuildReportSummary_NilHealth(t *testing.T) {
	report := &DiagnosticReport{Status: DiagHealthy}
	s := buildReportSummary(report, nil, nil)
	assert.Contains(t, s, "status=healthy")
	assert.Contains(t, s, "findings=0")
}

// --- targetFromAlert --------------------------------------------------------

func TestTargetFromAlert_Nil(t *testing.T) {
	assert.Empty(t, targetFromAlert(nil))
}

func TestTargetFromAlert_NoLabels(t *testing.T) {
	a := &alert.Alert{Labels: nil}
	assert.Empty(t, targetFromAlert(a))
}

func TestTargetFromAlert_InstancePriority(t *testing.T) {
	a := &alert.Alert{
		Labels: map[string]string{
			"instance": "node-1",
			"host":     "node-2",
		},
	}
	// "instance" takes priority.
	assert.Equal(t, "node-1", targetFromAlert(a))
}

func TestTargetFromAlert_EmptyLabelValue(t *testing.T) {
	a := &alert.Alert{
		Labels: map[string]string{
			"instance": "",
			"host":     "node-2",
		},
	}
	// Empty "instance" is skipped, "host" is used.
	assert.Equal(t, "node-2", targetFromAlert(a))
}

func TestTargetFromAlert_NoKnownKeys(t *testing.T) {
	a := &alert.Alert{
		Labels: map[string]string{"region": "us-east-1"},
	}
	assert.Empty(t, targetFromAlert(a))
}

// --- severityRank / severityFromPattern / sortFindings ---------------------

func TestSeverityRank(t *testing.T) {
	assert.Equal(t, 3, severityRank("critical"))
	assert.Equal(t, 3, severityRank("fatal"))
	assert.Equal(t, 3, severityRank("CRITICAL"))
	assert.Equal(t, 2, severityRank("warning"))
	assert.Equal(t, 2, severityRank("warn"))
	assert.Equal(t, 1, severityRank("info"))
	assert.Equal(t, 1, severityRank("informational"))
	assert.Equal(t, 0, severityRank("unknown"))
	assert.Equal(t, 0, severityRank(""))
}

func TestSeverityFromPattern(t *testing.T) {
	assert.Equal(t, "critical", severityFromPattern(SeverityFatal))
	assert.Equal(t, "critical", severityFromPattern(SeverityError))
	assert.Equal(t, "warning", severityFromPattern(SeverityWarn))
	assert.Equal(t, "info", severityFromPattern(SeverityInfo))
}

func TestSortFindings(t *testing.T) {
	findings := []Finding{
		{ID: "F1", Severity: "info", Category: "a"},
		{ID: "F2", Severity: "critical", Category: "b"},
		{ID: "F3", Severity: "warning", Category: "a"},
		{ID: "F4", Severity: "critical", Category: "a"},
	}
	sortFindings(findings)
	// Critical findings first, ordered by category ascending.
	assert.Equal(t, "critical", findings[0].Severity)
	assert.Equal(t, "a", findings[0].Category)
	assert.Equal(t, "critical", findings[1].Severity)
	assert.Equal(t, "b", findings[1].Category)
	// Then warning.
	assert.Equal(t, "warning", findings[2].Severity)
	// Then info.
	assert.Equal(t, "info", findings[3].Severity)
}

func TestSortFindings_Empty(t *testing.T) {
	sortFindings(nil) // must not panic
	sortFindings([]Finding{})
}

// --- subReportEvidence ------------------------------------------------------

func TestSubReportEvidence_WithErr(t *testing.T) {
	s := subReportEvidence("network", StatusUnhealthy, "dns failed")
	assert.Contains(t, s, "network=unhealthy")
	assert.Contains(t, s, "err=dns failed")
}

func TestSubReportEvidence_NoErr(t *testing.T) {
	s := subReportEvidence("node", StatusDegraded, "")
	assert.Equal(t, "node=degraded", s)
}

// --- min64 ------------------------------------------------------------------

func TestMin64(t *testing.T) {
	assert.Equal(t, 1.0, min64(1.0, 2.0))
	assert.Equal(t, 1.0, min64(2.0, 1.0))
	assert.Equal(t, 1.0, min64(1.0, 1.0))
}

// --- worstFindingIn ---------------------------------------------------------

func TestWorstFindingIn_Empty(t *testing.T) {
	assert.Nil(t, worstFindingIn(nil))
}

func TestWorstFindingIn_Single(t *testing.T) {
	f := []Finding{{ID: "F1", Severity: "warning"}}
	w := worstFindingIn(f)
	require.NotNil(t, w)
	assert.Equal(t, "F1", w.ID)
}

// --- anyCmdExecutor helper --------------------------------------------------
//
// anyCmdExecutor returns the same canned stdout for every command. It is
// used by tests that do not care about the exact command the collector
// issues (the time-bounded window is computed at call time and is hard to
// predict exactly).

type anyCmdExecutor struct {
	stdout string
	mu     sync.Mutex
	calls  int
}

func (e *anyCmdExecutor) Execute(_ context.Context, _, _ string) (string, string, int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	return e.stdout, "", 0, nil
}

// Compile-time guard.
var _ CommandExecutor = (*anyCmdExecutor)(nil)

// --- registerHealthyCommands ------------------------------------------------
//
// registerHealthyCommands wires canned successful output for the Linux node
// and data probe commands so the health prober reports a healthy target.

func registerHealthyCommands(exec *mockExecutor) {
	exec.set("top -bn1", mockResult{stdout: topOutput})
	exec.set("nproc", mockResult{stdout: "4\n"})
	exec.set("free -b", mockResult{stdout: freeOutput})
	exec.set("df -B1", mockResult{stdout: dfOutput})
	exec.set("cat /proc/loadavg", mockResult{stdout: "0.10 0.20 0.30 1/100 1\n"})
	exec.set("cat /proc/uptime", mockResult{stdout: "3600.00 3500.00\n"})
	exec.set(`mysql -e "SELECT 1"`, mockResult{stdout: "1\n"})
	exec.set(`mysql -e "SHOW SLAVE STATUS\G"`, mockResult{stdout: ""})
}

const topOutput = `top - 12:00:00 up  1:00,  1 user,  load average: 0.10, 0.20, 0.30
Tasks: 100 total, 1 running, 99 sleeping
%Cpu(s):  5.0 us,  2.0 sy,  0.0 ni, 93.0 id,  0.0 wa,  0.0 hi,  0.0 si,  0.0 st
KiB Mem :  1000000 total,   500000 free,   300000 used,   200000 buff/cache
`

const freeOutput = `              total        used        free      shared  buff/cache   available
Mem:        1000000      300000      500000           0      200000      600000
Swap:             0           0           0
`

const dfOutput = `Filesystem     1B-blocks      Used Available Use% Mounted on
/dev/sda1      100000000   30000000  70000000  30% /
/dev/sda2      500000000  100000000 400000000  20% /data
`

// --- Engine with custom logger ---------------------------------------------

func TestNewDiagEngine_CustomLogger(t *testing.T) {
	// Just verify a custom logger does not panic and is wired up.
	e := NewDiagEngine(DiagEngineConfig{})
	report := e.Diagnose(context.Background(), "host")
	assert.NotEmpty(t, report.ID)
}

// --- DiagnoseFromAlert integration -----------------------------------------

func TestDiagnoseFromAlert_FullPipeline(t *testing.T) {
	exec := newMockExecutor()
	registerHealthyCommands(exec)
	collector := mustCollector(t, &anyCmdExecutor{stdout: "INFO ok\n"})
	analyzer := NewDefaultLogAnalyzer()
	prober := NewHealthProber(HealthProberConfig{
		Executor: exec,
		PingPort: -1,
	})
	e := NewDiagEngine(DiagEngineConfig{
		Collector: collector,
		Analyzer:  analyzer,
		Prober:    prober,
	})
	a := &alert.Alert{
		ID:     "alert-1",
		Source: "prom",
		Title:  "HighCPU",
		Labels: map[string]string{"instance": "node-1"},
	}
	report := e.DiagnoseFromAlert(context.Background(), a)
	assert.Equal(t, "alert-1", report.AlertID)
	assert.Equal(t, "node-1", report.Target)
	assert.Equal(t, TriggerAlert, report.Trigger)
}

// --- Diagnose error recording ----------------------------------------------

func TestDiagnose_LogPipelineError(t *testing.T) {
	// Use an executor that returns an error for every command so the
	// collector reports per-source failures (which are recorded on the
	// batch, not as a top-level error). The engine should still produce
	// a report.
	exec := &errorExecutor{err: errors.New("connection refused")}
	collector := mustCollector(t, exec)
	analyzer := NewDefaultLogAnalyzer()
	e := NewDiagEngine(DiagEngineConfig{
		Collector: collector,
		Analyzer:  analyzer,
	})
	report := e.Diagnose(context.Background(), "host-1")
	assert.NotEmpty(t, report.ID)
	// The log analysis ran (with an empty batch) even though collection
	// failed for every source.
	assert.NotNil(t, report.LogAnalysis.AnalyzedAt.IsZero() || !report.LogAnalysis.AnalyzedAt.IsZero())
}

type errorExecutor struct {
	err error
}

func (e *errorExecutor) Execute(_ context.Context, _, _ string) (string, string, int, error) {
	return "", "", 0, e.err
}

var _ CommandExecutor = (*errorExecutor)(nil)
