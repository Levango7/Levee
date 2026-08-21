package diagnosis

// engine.go implements Phase A6 of LEVEE's diagnostic subsystem: the
// diagnostic engine entry point. The engine orchestrates the three Phase
// A3-A5 primitives — LogCollector, LogAnalyzer and HealthProber — into a
// single DiagnosticReport that the operator can act on.
//
// The engine runs the log pipeline (collect + analyse) and the health probe
// concurrently: the two are independent and running them in parallel halves
// the wall-clock latency of a diagnosis run on a typical target. The results
// are then synthesised into findings, a root-cause hypothesis, a confidence
// score and a list of remediation recommendations.
//
// All public types are safe for concurrent use. The engine never panics;
// failures are recorded on the returned DiagnosticReport.Errors slice rather
// than propagated through a panic or a nil report.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nexus/levee/internal/alert"
	"github.com/nexus/levee/internal/log"
)

// --- Defaults ---------------------------------------------------------------

// DefaultDiagTimeout is the wall-clock budget applied to a Diagnose call when
// DiagEngineConfig.Timeout is zero. It is generous because log collection on
// a busy target can take several seconds per source.
const DefaultDiagTimeout = 30 * time.Second

// DefaultLogWindow is the look-back window used when Diagnose is called
// without an explicit window. The engine collects logs from
// now-DefaultLogWindow to now.
const DefaultLogWindow = 15 * time.Minute

// DefaultRuntime is the target OS family assumed when the engine has no
// explicit information. It selects the default log source set.
const DefaultRuntime = RuntimeLinux

// --- DiagEngine -------------------------------------------------------------

// DiagEngine is the diagnostic engine. It orchestrates log collection, log
// analysis and health probing into a single DiagnosticReport.
//
// A DiagEngine is immutable after construction and therefore safe for
// concurrent use by any number of goroutines. The zero value is not usable;
// callers must use NewDiagEngine.
type DiagEngine struct {
	collector *LogCollector
	analyzer  *LogAnalyzer
	prober    *HealthProber
	log       *slog.Logger
	timeout   time.Duration
	window    time.Duration
	runtime   Runtime
}

// DiagEngineConfig configures a DiagEngine. All fields are optional; zero
// values are replaced with sensible defaults by NewDiagEngine. At least one
// of Collector/Analyzer or Prober should be non-nil, otherwise Diagnose will
// return a report with DiagUnknown and an explanatory error.
type DiagEngineConfig struct {
	// Collector pulls logs from the target. When nil, the log pipeline is
	// skipped and LogAnalysis is left empty.
	Collector *LogCollector

	// Analyzer matches collected logs against the pattern library. When
	// nil, the log pipeline is skipped even if Collector is set, because
	// there is no way to turn a LogBatch into an AnalysisResult without
	// an analyzer.
	Analyzer *LogAnalyzer

	// Prober runs health probes against the target. When nil, the health
	// probe is skipped and Health is left as the zero value.
	Prober *HealthProber

	// Timeout is the wall-clock budget for a single Diagnose call. Zero
	// defaults to DefaultDiagTimeout. The engine derives a child context
	// with this deadline from the caller's context.
	Timeout time.Duration

	// LogWindow is the look-back window for log collection. Zero defaults
	// to DefaultLogWindow. Ignored when Collector is nil.
	LogWindow time.Duration

	// Runtime is the target OS family. Zero defaults to DefaultRuntime.
	// It selects the default log source set used by the collector.
	Runtime Runtime

	// Logger is the structured logger used by the engine. When nil the
	// package-level singleton from internal/log is used.
	Logger *slog.Logger
}

// NewDiagEngine creates a DiagEngine from config, applying defaults for any
// zero-valued fields. It never returns nil and never panics.
func NewDiagEngine(cfg DiagEngineConfig) *DiagEngine {
	e := &DiagEngine{
		collector: cfg.Collector,
		analyzer:  cfg.Analyzer,
		prober:    cfg.Prober,
		timeout:   cfg.Timeout,
		window:    cfg.LogWindow,
		runtime:   cfg.Runtime,
		log:       cfg.Logger,
	}
	if e.timeout == 0 {
		e.timeout = DefaultDiagTimeout
	}
	if e.window == 0 {
		e.window = DefaultLogWindow
	}
	if e.runtime == "" {
		e.runtime = DefaultRuntime
	}
	if e.log == nil {
		e.log = log.Logger()
	}
	return e
}

// --- Diagnose ---------------------------------------------------------------

// Diagnose runs a full diagnosis on target. It concurrently collects logs,
// analyses them and probes health, then synthesises the results into a
// DiagnosticReport.
//
// The returned report is always non-nil. Failures are recorded in
// report.Errors rather than propagated through a separate error return so
// that callers always get a complete report even when one of the sub-steps
// fails. The report ID is a freshly generated UUID.
//
// Diagnose respects the caller's context deadline. When ctx has no deadline
// the engine applies cfg.Timeout.
func (e *DiagEngine) Diagnose(ctx context.Context, target string) DiagnosticReport {
	return e.diagnose(ctx, target, TriggerManual, "")
}

// DiagnoseFromAlert runs a diagnosis triggered by an alert. It extracts the
// target from alert.Labels (using the "instance" key, falling back to
// "host", "node" and "target"). When no target can be extracted the report
// is marked DiagUnknown with an explanatory error.
//
// The alert id is recorded on the returned report so operators can trace a
// diagnosis back to its triggering alert.
func (e *DiagEngine) DiagnoseFromAlert(ctx context.Context, a *alert.Alert) DiagnosticReport {
	if a == nil {
		report := newReport("", TriggerAlert, "")
		report.Status = DiagUnknown
		report.Errors = []string{"nil alert provided"}
		report.Summary = "diagnosis skipped: nil alert"
		return report
	}

	target := targetFromAlert(a)
	report := e.diagnose(ctx, target, TriggerAlert, a.ID)
	if target == "" {
		report.Errors = append(report.Errors, "could not extract target from alert labels")
		if report.Summary == "" {
			report.Summary = "diagnosis skipped: no target in alert labels"
		}
	}
	return report
}

// DiagnoseMulti runs diagnosis on multiple targets concurrently. The returned
// slice has the same length and order as targets; a slot is the zero value
// only when the corresponding target is empty.
//
// All sub-runs share the caller's context. The engine does not impose a
// global concurrency cap here; callers that need one should wrap DiagnoseMulti
// in a semaphore.
func (e *DiagEngine) DiagnoseMulti(ctx context.Context, targets []string) []DiagnosticReport {
	reports := make([]DiagnosticReport, len(targets))
	if len(targets) == 0 {
		return reports
	}

	var wg sync.WaitGroup
	wg.Add(len(targets))
	for i, t := range targets {
		go func(idx int, target string) {
			defer wg.Done()
			reports[idx] = e.Diagnose(ctx, target)
		}(i, t)
	}
	wg.Wait()
	return reports
}

// --- Internal orchestration -------------------------------------------------

// diagnose is the single entry point shared by Diagnose and DiagnoseFromAlert.
// It runs the log pipeline and the health probe concurrently, then merges
// the results into a DiagnosticReport.
func (e *DiagEngine) diagnose(ctx context.Context, target string, trigger TriggerType, alertID string) DiagnosticReport {
	report := newReport(target, trigger, alertID)
	report.StartedAt = time.Now()
	defer func() {
		report.Duration = time.Since(report.StartedAt)
	}()

	if target == "" {
		report.Status = DiagUnknown
		report.Summary = "diagnosis skipped: empty target"
		report.Errors = append(report.Errors, "empty target")
		return report
	}

	// Derive a child context with our timeout when the caller did not set
	// a deadline. We keep the cancel func around so the goroutines below
	// can signal cancellation on early return.
	ctx, cancel := e.withTimeout(ctx)
	defer cancel()

	// Run the log pipeline and the health probe concurrently. Each writes
	// into its own slot; the engine merges them after both complete.
	type logResult struct {
		analysis *AnalysisResult
		err      error
	}
	type healthResult struct {
		health HealthReport
	}

	var lr logResult
	var hr healthResult

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		lr.analysis, lr.err = e.runLogPipeline(ctx, target)
	}()

	go func() {
		defer wg.Done()
		hr.health = e.runHealthProbe(ctx, target)
	}()

	wg.Wait()

	// Merge log analysis.
	if lr.err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("log pipeline: %v", lr.err))
	}
	if lr.analysis != nil {
		report.LogAnalysis = *lr.analysis
	}

	// Merge health (the probe degrades gracefully and never errors).
	report.Health = hr.health

	// Synthesise findings, root cause, confidence, recommendations, status
	// and summary from the gathered evidence.
	report.Findings = buildFindings(&report.Health, &report.LogAnalysis)
	report.RootCause, report.Confidence = synthesiseRootCause(&report.LogAnalysis, &report.Health, report.Findings)
	report.Recommendations = buildRecommendations(report.Findings)
	report.Status = deriveStatus(&report.Health, &report.LogAnalysis, report.Findings)
	report.Summary = buildReportSummary(&report, &report.Health, &report.LogAnalysis)

	e.log.Info("diagnosis: run complete",
		"target", target,
		"status", report.Status,
		"findings", len(report.Findings),
		"confidence", report.Confidence,
		"duration_ms", report.Duration.Milliseconds())

	return report
}

// withTimeout returns ctx unchanged when it already has a deadline; otherwise
// it derives a child context with the engine's timeout. The returned cancel
// function must be called by the caller.
func (e *DiagEngine) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, e.timeout)
}

// runLogPipeline collects logs from target and analyses them. It returns the
// analysis result and any error. When the engine has no collector or no
// analyzer the pipeline is skipped and a nil result with no error is returned.
func (e *DiagEngine) runLogPipeline(ctx context.Context, target string) (*AnalysisResult, error) {
	if e.collector == nil || e.analyzer == nil {
		return nil, nil
	}

	sources := DefaultSources(e.runtime)
	if len(sources) == 0 {
		sources = DefaultSources(DefaultRuntime)
	}

	window := TimeWindow{
		Start: time.Now().Add(-e.window),
		End:   time.Now(),
	}

	batch, err := e.collector.Collect(ctx, target, sources, window)
	if err != nil {
		return nil, fmt.Errorf("collect: %w", err)
	}

	analysis := e.analyzer.Analyze(batch)
	return analysis, nil
}

// runHealthProbe runs the health probe against target. When the engine has
// no prober the probe is skipped and a zero-value report is returned.
func (e *DiagEngine) runHealthProbe(ctx context.Context, target string) HealthReport {
	if e.prober == nil {
		return HealthReport{}
	}
	return e.prober.ProbeAll(ctx, target)
}

// --- Findings synthesis -----------------------------------------------------

// buildFindings extracts actionable findings from the health report and the
// log analysis. The findings are sorted by severity (critical first) then by
// category so that the most important problems appear at the top of the
// report.
func buildFindings(health *HealthReport, analysis *AnalysisResult) []Finding {
	if health == nil && analysis == nil {
		return nil
	}

	var findings []Finding
	counter := 0
	nextID := func() string {
		counter++
		return fmt.Sprintf("FINDING-%03d", counter)
	}

	if health != nil {
		findings = append(findings, healthFindings(health, nextID)...)
	}
	if analysis != nil {
		findings = append(findings, logFindings(analysis, nextID)...)
	}

	sortFindings(findings)
	return findings
}

// healthFindings extracts findings from a HealthReport.
func healthFindings(h *HealthReport, nextID func() string) []Finding {
	var findings []Finding

	// Network.
	switch h.Network.Status {
	case StatusUnhealthy:
		findings = append(findings, Finding{
			ID:          nextID(),
			Category:    "network",
			Severity:    "critical",
			Title:       "Network unreachable",
			Description: "The target's network is down or unreachable.",
			Evidence:    subReportEvidence("network", h.Network.Status, h.Network.Err),
			Suggestion:  "Check interface status, routing and firewall rules.",
		})
	case StatusDegraded:
		findings = append(findings, Finding{
			ID:          nextID(),
			Category:    "network",
			Severity:    "warning",
			Title:       "Network degraded",
			Description: "One or more network probes reported degraded behaviour.",
			Evidence:    subReportEvidence("network", h.Network.Status, h.Network.Err),
			Suggestion:  "Investigate ping/DNS/TCP latency and packet loss.",
		})
	case StatusUnknown:
		if h.Network.Err != "" {
			findings = append(findings, Finding{
				ID:          nextID(),
				Category:    "network",
				Severity:    "info",
				Title:       "Network probe inconclusive",
				Description: "The network probe could not determine the target status.",
				Evidence:    h.Network.Err,
			})
		}
	}

	// Node.
	switch h.Node.Status {
	case StatusUnhealthy:
		findings = append(findings, Finding{
			ID:          nextID(),
			Category:    "node",
			Severity:    "critical",
			Title:       "Node resources exhausted",
			Description: "CPU, memory or disk usage crossed the critical threshold.",
			Evidence:    subReportEvidence("node", h.Node.Status, h.Node.Err),
			Suggestion:  "Scale out, free resources or restart the offending process.",
		})
	case StatusDegraded:
		findings = append(findings, Finding{
			ID:          nextID(),
			Category:    "node",
			Severity:    "warning",
			Title:       "Node resources under pressure",
			Description: "Resource usage is above the warning threshold.",
			Evidence:    subReportEvidence("node", h.Node.Status, h.Node.Err),
			Suggestion:  "Monitor the trend and pre-emptively scale if needed.",
		})
	}

	// Service.
	switch h.Service.Status {
	case StatusUnhealthy:
		findings = append(findings, Finding{
			ID:          nextID(),
			Category:    "service",
			Severity:    "critical",
			Title:       "Service down",
			Description: "One or more required services are not running.",
			Evidence:    subReportEvidence("service", h.Service.Status, h.Service.Err),
			Suggestion:  "Restart the failed service and check its logs.",
		})
	case StatusDegraded:
		findings = append(findings, Finding{
			ID:          nextID(),
			Category:    "service",
			Severity:    "warning",
			Title:       "Service degraded",
			Description: "At least one service is running but not healthy.",
			Evidence:    subReportEvidence("service", h.Service.Status, h.Service.Err),
			Suggestion:  "Inspect service healthz endpoint and recent logs.",
		})
	}

	// Data.
	switch h.Data.Status {
	case StatusUnhealthy:
		findings = append(findings, Finding{
			ID:          nextID(),
			Category:    "data",
			Severity:    "critical",
			Title:       "Data layer unhealthy",
			Description: "Database connectivity, replication or disk failed.",
			Evidence:    subReportEvidence("data", h.Data.Status, h.Data.Err),
			Suggestion:  "Check DB connectivity, replication lag and disk space.",
		})
	case StatusDegraded:
		findings = append(findings, Finding{
			ID:          nextID(),
			Category:    "data",
			Severity:    "warning",
			Title:       "Data layer degraded",
			Description: "Replication lag or disk usage is above the warning threshold.",
			Evidence:    subReportEvidence("data", h.Data.Status, h.Data.Err),
			Suggestion:  "Monitor replication lag and disk usage trends.",
		})
	}

	return findings
}

// subReportEvidence builds a short evidence string for a health sub-report.
// It includes the category, status and (when non-empty) the error message.
func subReportEvidence(category string, status HealthStatus, errMsg string) string {
	if errMsg != "" {
		return fmt.Sprintf("%s=%s err=%s", category, status, errMsg)
	}
	return fmt.Sprintf("%s=%s", category, status)
}

// logFindings extracts findings from an AnalysisResult. It promotes the top
// matched patterns to findings, capped at a small number so the report stays
// readable.
func logFindings(a *AnalysisResult, nextID func() string) []Finding {
	if a == nil || len(a.ErrorPatterns) == 0 {
		return nil
	}

	const maxLogFindings = 5
	findings := make([]Finding, 0, maxLogFindings)

	for i, mp := range a.ErrorPatterns {
		if i >= maxLogFindings {
			break
		}
		sev := severityFromPattern(mp.Pattern.Severity)
		findings = append(findings, Finding{
			ID:          nextID(),
			Category:    "log",
			Severity:    sev,
			Title:       mp.Pattern.Name,
			Description: mp.Pattern.Description,
			Evidence:    fmt.Sprintf("matched %d times", mp.Count),
			Suggestion:  mp.Pattern.Suggestion,
		})
	}
	return findings
}

// severityFromPattern maps a diagnosis.Severity to the finding severity
// vocabulary ("critical" / "warning" / "info").
func severityFromPattern(s Severity) string {
	switch s {
	case SeverityFatal:
		return "critical"
	case SeverityError:
		return "critical"
	case SeverityWarn:
		return "warning"
	default:
		return "info"
	}
}

// sortFindings sorts findings by severity rank descending then by category
// ascending so that the most severe problems appear first and findings of
// equal severity are grouped by category.
func sortFindings(findings []Finding) {
	// Simple insertion sort: the finding list is small (< 20 entries).
	for i := 1; i < len(findings); i++ {
		for j := i; j > 0; j-- {
			ri := severityRank(findings[j].Severity)
			rj := severityRank(findings[j-1].Severity)
			if ri > rj || (ri == rj && findings[j].Category < findings[j-1].Category) {
				findings[j-1], findings[j] = findings[j], findings[j-1]
			} else {
				break
			}
		}
	}
}

// --- Root cause / confidence / status / summary ----------------------------

// synthesiseRootCause picks the root cause and confidence from the log
// analysis and the health report. When the log analysis has high confidence
// its root cause is used directly. When the log analysis is inconclusive but
// a critical health finding exists, the health finding title is used as the
// root cause with a low confidence. Otherwise the root cause is empty and
// confidence is zero.
func synthesiseRootCause(analysis *AnalysisResult, health *HealthReport, findings []Finding) (string, float64) {
	// Prefer the log analysis root cause when it has any confidence.
	if analysis != nil && analysis.Confidence > 0 && analysis.RootCause.ID != "" {
		rc := fmt.Sprintf("%s: %s", analysis.RootCause.ID, analysis.RootCause.Name)
		conf := analysis.Confidence
		// Boost confidence when the health report corroborates the log
		// root cause (i.e. health is not healthy).
		if health != nil && health.Status != StatusHealthy && health.Status != StatusUnknown {
			conf = min64(conf+0.1, 1.0)
		}
		return rc, conf
	}

	// Fall back to the worst health finding.
	worst := worstFindingIn(findings)
	if worst != nil && (worst.Severity == "critical" || worst.Severity == "warning") {
		sev := 0.5
		if worst.Severity == "critical" {
			sev = 0.7
		}
		return worst.Title, sev
	}

	return "", 0
}

// worstFindingIn returns a pointer to the worst finding in the slice, or nil
// when the slice is empty. It is the package-private counterpart of
// DiagnosticReport.WorstFinding.
func worstFindingIn(findings []Finding) *Finding {
	if len(findings) == 0 {
		return nil
	}
	worst := &findings[0]
	worstRank := severityRank(worst.Severity)
	for i := 1; i < len(findings); i++ {
		rank := severityRank(findings[i].Severity)
		if rank > worstRank {
			worst = &findings[i]
			worstRank = rank
		}
	}
	return worst
}

// min64 returns the smaller of two float64s. Go's standard library does not
// expose a float64 min before 1.21, and we keep this helper local to avoid a
// version constraint.
func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// deriveStatus computes the overall DiagStatus from the health report, the
// log analysis and the findings. Unhealthy wins, then degraded, then
// unknown, then healthy.
func deriveStatus(health *HealthReport, analysis *AnalysisResult, findings []Finding) DiagStatus {
	var hasUnhealthy, hasDegraded, hasUnknown bool

	if health != nil {
		switch health.Status {
		case StatusUnhealthy:
			hasUnhealthy = true
		case StatusDegraded:
			hasDegraded = true
		case StatusUnknown:
			// Unknown health only counts when we have no other signal.
			hasUnknown = true
		}
	}

	if analysis != nil && analysis.Confidence > 0 {
		switch {
		case analysis.Confidence >= 0.7:
			hasUnhealthy = true
		case analysis.Confidence >= 0.3:
			hasDegraded = true
		}
	}

	for _, f := range findings {
		switch f.Severity {
		case "critical":
			hasUnhealthy = true
		case "warning":
			hasDegraded = true
		}
	}

	switch {
	case hasUnhealthy:
		return DiagUnhealthy
	case hasDegraded:
		return DiagDegraded
	case hasUnknown:
		return DiagUnknown
	default:
		return DiagHealthy
	}
}

// buildRecommendations produces a list of remediation steps from the findings.
// Each critical or warning finding contributes one recommendation built from
// its Suggestion (when present) or its Title.
func buildRecommendations(findings []Finding) []string {
	if len(findings) == 0 {
		return nil
	}
	var recs []string
	for _, f := range findings {
		if f.Severity != "critical" && f.Severity != "warning" {
			continue
		}
		if f.Suggestion != "" {
			recs = append(recs, fmt.Sprintf("[%s] %s", f.ID, f.Suggestion))
		} else {
			recs = append(recs, fmt.Sprintf("[%s] investigate %s", f.ID, f.Title))
		}
	}
	return recs
}

// buildReportSummary produces a one-line human-readable summary of the
// diagnosis outcome.
func buildReportSummary(report *DiagnosticReport, health *HealthReport, analysis *AnalysisResult) string {
	var parts []string

	parts = append(parts, fmt.Sprintf("status=%s", report.Status))

	if health != nil {
		parts = append(parts, fmt.Sprintf("health=%s", health.Status))
	}
	if analysis != nil {
		if analysis.Confidence > 0 {
			parts = append(parts, fmt.Sprintf("root_cause=%s(%.2f)", analysis.RootCause.ID, analysis.Confidence))
		} else {
			parts = append(parts, fmt.Sprintf("log_lines=%d", analysis.TotalLines))
		}
	}
	parts = append(parts, fmt.Sprintf("findings=%d", len(report.Findings)))

	return strings.Join(parts, " ")
}

// --- Alert helpers ----------------------------------------------------------

// targetFromAlert extracts the target host from an alert's labels. It tries
// the conventional label keys in order: "instance", "host", "node", "target".
// Returns an empty string when none of the keys are present.
func targetFromAlert(a *alert.Alert) string {
	if a == nil || len(a.Labels) == 0 {
		return ""
	}
	for _, key := range []string{"instance", "host", "node", "target"} {
		if v, ok := a.Labels[key]; ok && v != "" {
			return v
		}
	}
	return ""
}

// --- Report construction ----------------------------------------------------

// newReport returns a DiagnosticReport with the ID and trigger fields set.
// It is the single allocation point for reports in this file.
func newReport(target string, trigger TriggerType, alertID string) DiagnosticReport {
	return DiagnosticReport{
		ID:      uuid.NewString(),
		Target:  target,
		Trigger: trigger,
		AlertID: alertID,
	}
}
