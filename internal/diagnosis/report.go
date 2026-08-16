package diagnosis

// report.go implements Phase A6 of LEVEE's diagnostic subsystem: the
// DiagnosticReport type that aggregates the outputs of LogAnalyzer and
// HealthProber into a single document the operator can act on.
//
// A DiagnosticReport is produced by DiagEngine.Diagnose (see engine.go).
// It carries the health probe result, the log analysis result, a list of
// synthesised Findings, a root-cause hypothesis with confidence, a list of
// remediation Recommendations and a human-readable Summary. All fields are
// plain value types so the report serialises cleanly to JSON and can be
// stored, forwarded or rendered without further transformation.
//
// The types in this file are immutable value objects; they are safe for
// concurrent use by any number of goroutines once constructed.

import (
	"fmt"
	"strings"
	"time"
)

// --- TriggerType ------------------------------------------------------------

// TriggerType identifies what initiated the diagnosis run. It is stored on
// the report so that operators can distinguish manual, alert-triggered and
// scheduled runs in the audit trail.
type TriggerType string

const (
	// TriggerManual means the diagnosis was started by an operator running
	// `levee diagnose <target>` (or the equivalent API call).
	TriggerManual TriggerType = "manual"
	// TriggerAlert means the diagnosis was started automatically in response
	// to an incoming alert. The originating alert id is stored in
	// DiagnosticReport.AlertID.
	TriggerAlert TriggerType = "alert"
	// TriggerScheduled means the diagnosis was started by a periodic
	// scheduler (e.g. a cron entry or a watchdog timer).
	TriggerScheduled TriggerType = "scheduled"
)

// --- DiagStatus -------------------------------------------------------------

// DiagStatus is the overall outcome of a diagnosis run. It is derived from
// the health report status and the log analysis confidence: an unhealthy
// probe or a high-confidence root cause yields DiagUnhealthy; a degraded
// probe or a medium-confidence root cause yields DiagDegraded; otherwise
// the run is DiagHealthy. DiagUnknown is used when the engine could not
// gather enough evidence to decide.
type DiagStatus string

const (
	// DiagHealthy means the target is operating within nominal parameters.
	DiagHealthy DiagStatus = "healthy"
	// DiagDegraded means the target is usable but shows signs of stress.
	DiagDegraded DiagStatus = "degraded"
	// DiagUnhealthy means the target is failing and needs operator action.
	DiagUnhealthy DiagStatus = "unhealthy"
	// DiagUnknown means the engine could not determine the target status.
	DiagUnknown DiagStatus = "unknown"
)

// --- Finding ----------------------------------------------------------------

// Finding represents a single diagnostic finding extracted from the health
// report or the log analysis. Findings are the human-actionable unit of the
// report: each one names a problem, points at the evidence and suggests a
// remediation step. The engine synthesises findings from HealthReport and
// AnalysisResult in buildFindings (see engine.go).
type Finding struct {
	// ID is a short stable identifier (e.g. "FINDING-001") used to
	// reference the finding from the recommendations list and from
	// external tooling.
	ID string `json:"id"`

	// Category classifies the finding. The well-known values are
	// "network", "node", "service", "data" and "log". Callers may use
	// custom values for vendor-specific findings.
	Category string `json:"category"`

	// Severity is the importance of the finding. The well-known values
	// are "critical", "warning" and "info", matching the alert severity
	// vocabulary so that findings can be rendered uniformly.
	Severity string `json:"severity"`

	// Title is a short human-readable summary of the finding.
	Title string `json:"title"`

	// Description is a longer explanation of what was observed.
	Description string `json:"description"`

	// Evidence is the supporting data for the finding (e.g. a sample log
	// line, a metric value). It is optional and may be empty.
	Evidence string `json:"evidence,omitempty"`

	// Suggestion is a remediation hint shown to the operator. It is
	// optional; findings without a suggestion are informational only.
	Suggestion string `json:"suggestion,omitempty"`
}

// --- DiagnosticReport -------------------------------------------------------

// DiagnosticReport is the complete output of a single diagnosis run. It is
// always non-nil when returned by DiagEngine.Diagnose; failures are recorded
// in the Errors slice rather than propagated through a separate error return.
//
// The report is a value object: once constructed it is not mutated. Callers
// may serialise it to JSON, store it or forward it without copying.
type DiagnosticReport struct {
	// ID is a unique identifier for the report, generated as a UUID at
	// construction time. It is stable for the lifetime of the report.
	ID string `json:"id"`

	// Target is the host the diagnosis was run against.
	Target string `json:"target"`

	// Trigger records what initiated the diagnosis run.
	Trigger TriggerType `json:"trigger"`

	// AlertID is the id of the alert that triggered the diagnosis when
	// Trigger == TriggerAlert. It is empty for other trigger types.
	AlertID string `json:"alert_id,omitempty"`

	// Status is the overall outcome of the diagnosis. It is derived from
	// Health.Status and LogAnalysis.Confidence by deriveStatus.
	Status DiagStatus `json:"status"`

	// Health is the result of the health probe. It is always populated;
	// a probe that could not run has Status == StatusUnknown.
	Health HealthReport `json:"health"`

	// LogAnalysis is the result of the log analysis. It is always
	// populated; a run that collected no logs has TotalLines == 0.
	LogAnalysis AnalysisResult `json:"log_analysis"`

	// Findings is the list of synthesised findings extracted from Health
	// and LogAnalysis. It is sorted by severity (critical first) then by
	// category. Empty when no problems were found.
	Findings []Finding `json:"findings"`

	// RootCause is the engine's hypothesis about the underlying cause of
	// the observed problems. It is taken from LogAnalysis.RootCause when
	// the log analysis has high confidence, otherwise it is synthesised
	// from the worst health finding. Empty when no root cause could be
	// determined.
	RootCause string `json:"root_cause"`

	// Confidence is the engine's confidence in RootCause, in the range
	// [0, 1]. It is a blend of LogAnalysis.Confidence and the health
	// status. Zero means no root cause could be determined.
	Confidence float64 `json:"confidence"`

	// Recommendations is a list of remediation steps the operator should
	// take. Each entry references one or more Findings by id. Empty when
	// no recommendations could be made.
	Recommendations []string `json:"recommendations"`

	// Summary is a short human-readable description of the diagnosis
	// outcome, suitable for display at the top of an incident report.
	Summary string `json:"summary"`

	// StartedAt is the wall-clock time at which Diagnose was called.
	StartedAt time.Time `json:"started_at"`

	// Duration is how long the diagnosis took end-to-end.
	Duration time.Duration `json:"duration_ms"`

	// Errors is the list of non-fatal errors encountered during the run
	// (e.g. a log source that could not be collected). Empty when the run
	// was clean. Fatal errors are recorded here as well; the engine never
	// returns a nil report.
	Errors []string `json:"errors,omitempty"`
}

// --- Helper methods ---------------------------------------------------------

// HasFindings reports whether the report contains any findings. It is a
// convenience accessor so callers do not have to check len(r.Findings) == 0.
func (r *DiagnosticReport) HasFindings() bool {
	return r != nil && len(r.Findings) > 0
}

// WorstFinding returns a pointer to the most severe finding in the report, or
// nil when the report has no findings. "critical" beats "warning" beats "info";
// findings of equal severity are ordered by their position in the slice.
//
// The returned pointer points into the report's Findings slice; callers must
// not mutate it.
func (r *DiagnosticReport) WorstFinding() *Finding {
	if r == nil || len(r.Findings) == 0 {
		return nil
	}
	worst := &r.Findings[0]
	worstRank := severityRank(worst.Severity)
	for i := 1; i < len(r.Findings); i++ {
		rank := severityRank(r.Findings[i].Severity)
		if rank > worstRank {
			worst = &r.Findings[i]
			worstRank = rank
		}
	}
	return worst
}

// severityRank maps a finding severity string to a numeric rank so that
// "critical" > "warning" > "info" > anything else. Higher numbers mean more
// severe.
func severityRank(sev string) int {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "critical", "fatal":
		return 3
	case "warning", "warn":
		return 2
	case "info", "informational":
		return 1
	default:
		return 0
	}
}

// String returns a multi-line human-readable rendering of the report. It is
// intended for CLI output and log entries; for structured transport use JSON
// marshalling instead. The output is deterministic and does not include
// timestamps in locale-sensitive form.
func (r *DiagnosticReport) String() string {
	if r == nil {
		return "<nil diagnostic report>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Diagnostic Report %s\n", r.ID)
	fmt.Fprintf(&b, "  Target:     %s\n", r.Target)
	fmt.Fprintf(&b, "  Trigger:    %s\n", r.Trigger)
	if r.AlertID != "" {
		fmt.Fprintf(&b, "  AlertID:    %s\n", r.AlertID)
	}
	fmt.Fprintf(&b, "  Status:     %s\n", r.Status)
	fmt.Fprintf(&b, "  StartedAt:  %s\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "  Duration:   %s\n", r.Duration)
	fmt.Fprintf(&b, "  RootCause:  %s (confidence %.2f)\n", r.RootCause, r.Confidence)
	fmt.Fprintf(&b, "  Summary:    %s\n", r.Summary)

	if r.HasFindings() {
		fmt.Fprintf(&b, "  Findings (%d):\n", len(r.Findings))
		for _, f := range r.Findings {
			fmt.Fprintf(&b, "    [%s] %s: %s\n", f.Severity, f.Category, f.Title)
			if f.Description != "" {
				fmt.Fprintf(&b, "      %s\n", f.Description)
			}
			if f.Suggestion != "" {
				fmt.Fprintf(&b, "      -> %s\n", f.Suggestion)
			}
		}
	}

	if len(r.Recommendations) > 0 {
		fmt.Fprintf(&b, "  Recommendations:\n")
		for i, rec := range r.Recommendations {
			fmt.Fprintf(&b, "    %d. %s\n", i+1, rec)
		}
	}

	if len(r.Errors) > 0 {
		fmt.Fprintf(&b, "  Errors (%d):\n", len(r.Errors))
		for _, e := range r.Errors {
			fmt.Fprintf(&b, "    - %s\n", e)
		}
	}

	return b.String()
}
