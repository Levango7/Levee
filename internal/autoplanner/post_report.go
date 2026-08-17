package autoplanner

// post_report.go implements the PostReportGenerator that produces an
// after-action audit report for a completed workflow run. The report
// captures the operational outcome (success, duration, metrics delta), the
// integrity status of the audit hash chain, and the lessons learned during
// the run so that operators and downstream consumers (dashboards, incident
// reviews, knowledge bases) receive a single self-contained document.
//
// The generator is intentionally side-effect free: it never writes to the
// store. It only reads the hash chain via the audit.ChainVerifier when one is
// configured and a RunID is supplied. When either is absent the audit-chain
// section is marked as skipped rather than failing the report generation.
//
// PostReportGenerator is safe for concurrent use: all fields are read-only
// after construction. It never panics; errors are propagated through error
// returns.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrPostReportNilWorkflow is returned when Generate is called with a
	// nil Workflow in the request.
	ErrPostReportNilWorkflow = errors.New("autoplanner: nil workflow")
	// ErrPostReportEmptyWorkflowID is returned when the workflow carries an
	// empty ID, which would make the report impossible to correlate.
	ErrPostReportEmptyWorkflowID = errors.New("autoplanner: empty workflow id")
)

// --- PostReport -------------------------------------------------------------

// PostReport is the after-action audit report for a completed workflow run.
// It bundles the operational outcome, the metrics delta, the audit-chain
// integrity status and the lessons learned into a single value that
// serialises cleanly to JSON and renders to a human-readable text form via
// ToText.
type PostReport struct {
	// ReportID is a UUID identifying this report instance.
	ReportID string `json:"report_id"`
	// WorkflowID is the ID of the workflow this report covers.
	WorkflowID string `json:"workflow_id"`
	// AlertID is the originating alert id, if any.
	AlertID string `json:"alert_id"`
	// Target is the target host / service the workflow ran against.
	Target string `json:"target"`
	// Summary is a short human-readable summary of the run.
	Summary string `json:"summary"`
	// Success reports whether the workflow completed successfully.
	Success bool `json:"success"`
	// Duration is the wall-clock duration of the run.
	Duration time.Duration `json:"duration"`
	// StartedAt is the run start time (UTC).
	StartedAt time.Time `json:"started_at"`
	// FinishedAt is the run finish time (UTC).
	FinishedAt time.Time `json:"finished_at"`
	// MetricsBefore is the snapshot of metrics captured before the run.
	MetricsBefore map[string]float64 `json:"metrics_before"`
	// MetricsAfter is the snapshot of metrics captured after the run.
	MetricsAfter map[string]float64 `json:"metrics_after"`
	// MetricsDelta is the per-key delta (after - before) computed by
	// Generate. Keys present in only one snapshot are included with the
	// other side treated as zero.
	MetricsDelta map[string]float64 `json:"metrics_delta"`
	// AuditChainValid reports whether the audit hash chain was verified
	// intact. It is false when verification was skipped.
	AuditChainValid bool `json:"audit_chain_valid"`
	// AuditChainDetail carries a human-readable note describing the
	// verification outcome (valid / skipped / failed / error). It is
	// omitted from JSON when empty.
	AuditChainDetail string `json:"audit_chain_detail,omitempty"`
	// TraceCount is the number of trace records inspected during chain
	// verification. It is zero when verification was skipped.
	TraceCount int `json:"trace_count"`
	// LessonsLearned is the list of lessons captured during the run.
	LessonsLearned []string `json:"lessons_learned"`
	// RiskLevel is the risk tier of the workflow, copied from the
	// Workflow as a string.
	RiskLevel string `json:"risk_level"`
	// RollbackUsed reports whether the rollback plan was invoked.
	RollbackUsed bool `json:"rollback_used"`
	// GeneratedAt is the report generation timestamp (UTC).
	GeneratedAt time.Time `json:"generated_at"`
}

// --- PostReportGenerator ----------------------------------------------------

// PostReportGenerator produces PostReport values for completed workflow runs.
// It is safe for concurrent use: all fields are read-only after construction.
type PostReportGenerator struct {
	verifier *audit.ChainVerifier
	log      *slog.Logger
}

// PostReportGeneratorConfig configures a PostReportGenerator. Nil fields are
// replaced with sensible defaults by NewPostReportGenerator so that a
// zero-value config produces a fully wired generator. A nil Verifier is
// valid and disables audit-chain verification (the report will mark the
// chain section as skipped).
type PostReportGeneratorConfig struct {
	// Verifier is the hash-chain verifier. Nil -> chain verification
	// skipped.
	Verifier *audit.ChainVerifier
	// Logger is the structured logger. Nil -> log.With("component", ...).
	Logger *slog.Logger
}

// PostReportRequest is the input to PostReportGenerator.Generate. It carries
// the workflow being reported on together with the run-level measurements
// (success, duration, metrics snapshots) and the optional RunID used for
// hash-chain verification.
type PostReportRequest struct {
	// Workflow is the workflow being reported on. Must be non-nil and
	// carry a non-empty ID.
	Workflow *Workflow
	// AlertID is the originating alert id, if any. May be empty.
	AlertID string
	// Success reports whether the workflow completed successfully.
	Success bool
	// Duration is the wall-clock duration of the run.
	Duration time.Duration
	// StartedAt is the run start time.
	StartedAt time.Time
	// FinishedAt is the run finish time.
	FinishedAt time.Time
	// MetricsBefore is the snapshot of metrics captured before the run.
	// May be nil.
	MetricsBefore map[string]float64
	// MetricsAfter is the snapshot of metrics captured after the run.
	// May be nil.
	MetricsAfter map[string]float64
	// RunID is the run id used for hash-chain verification. When empty
	// (or when no verifier is configured) chain verification is skipped.
	RunID string
	// RollbackUsed reports whether the rollback plan was invoked.
	RollbackUsed bool
	// LessonsLearned is the list of lessons captured during the run.
	// May be nil.
	LessonsLearned []string
}

// NewPostReportGenerator returns a PostReportGenerator with the given config.
// Nil fields in cfg are replaced with sensible defaults. A nil Verifier is
// preserved (it disables chain verification) so that callers can build a
// report-only generator without a backing store.
func NewPostReportGenerator(cfg PostReportGeneratorConfig) *PostReportGenerator {
	lg := cfg.Logger
	if lg == nil {
		lg = log.With("component", "post_report")
	}
	return &PostReportGenerator{
		verifier: cfg.Verifier,
		log:      lg,
	}
}

// Generate builds a PostReport for the given request. The report is
// assembled as follows:
//
//  1. Validate the request (non-nil workflow, non-empty workflow id).
//  2. Compute the metrics delta (after - before) for every key present in
//     either snapshot.
//  3. Verify the audit hash chain when both a verifier and a RunID are
//     supplied; otherwise mark the chain section as skipped.
//  4. Stamp the report with a fresh UUID and the generation timestamp.
//
// Generate returns ErrPostReportNilWorkflow for a nil workflow and
// ErrPostReportEmptyWorkflowID for an empty workflow id. A failure during
// hash-chain verification is logged but does not fail the report: the
// AuditChainValid field is set to false and the error is captured in
// AuditChainDetail so that the operator is informed without losing the
// rest of the report.
func (g *PostReportGenerator) Generate(ctx context.Context, req PostReportRequest) (*PostReport, error) {
	if req.Workflow == nil {
		return nil, ErrPostReportNilWorkflow
	}
	if req.Workflow.ID == "" {
		return nil, ErrPostReportEmptyWorkflowID
	}
	if ctx == nil {
		ctx = context.Background()
	}

	delta := computeMetricsDelta(req.MetricsBefore, req.MetricsAfter)
	chainValid, chainDetail, traceCount := g.verifyChain(ctx, req.RunID)

	report := &PostReport{
		ReportID:         uuid.NewString(),
		WorkflowID:       req.Workflow.ID,
		AlertID:          req.AlertID,
		Target:           req.Workflow.Target,
		Summary:          req.Workflow.Name,
		Success:          req.Success,
		Duration:         req.Duration,
		StartedAt:        req.StartedAt,
		FinishedAt:       req.FinishedAt,
		MetricsBefore:    req.MetricsBefore,
		MetricsAfter:     req.MetricsAfter,
		MetricsDelta:     delta,
		AuditChainValid:  chainValid,
		AuditChainDetail: chainDetail,
		TraceCount:       traceCount,
		LessonsLearned:   req.LessonsLearned,
		RiskLevel:        string(req.Workflow.RiskLevel),
		RollbackUsed:     req.RollbackUsed,
		GeneratedAt:      time.Now().UTC(),
	}

	g.log.InfoContext(ctx, "post_report: report generated",
		"report_id", report.ReportID,
		"workflow_id", report.WorkflowID,
		"alert_id", report.AlertID,
		"success", report.Success,
		"audit_chain_valid", report.AuditChainValid,
		"trace_count", report.TraceCount,
		"rollback_used", report.RollbackUsed,
	)

	return report, nil
}

// computeMetricsDelta returns the per-key delta (after - before) for every
// key present in either snapshot. Keys present in only one snapshot are
// included with the missing side treated as zero. The result is nil when
// both snapshots are empty.
func computeMetricsDelta(before, after map[string]float64) map[string]float64 {
	if len(before) == 0 && len(after) == 0 {
		return nil
	}
	delta := make(map[string]float64, len(before)+len(after))
	for k, v := range before {
		delta[k] -= v
	}
	for k, v := range after {
		delta[k] += v
	}
	return delta
}

// verifyChain runs the hash-chain verification when both a verifier and a
// run id are available. It returns (valid, detail, traceCount). When
// verification is skipped (no verifier or empty run id) it returns
// (false, "skipped: ...", 0). When verification fails with an error the
// error is logged and returned as (false, "error: ...", 0) so that the
// report still surfaces the failure to the operator.
func (g *PostReportGenerator) verifyChain(ctx context.Context, runID string) (valid bool, detail string, traceCount int) {
	if g.verifier == nil {
		return false, "skipped: no verifier configured", 0
	}
	if runID == "" {
		return false, "skipped: no run id provided", 0
	}

	result, err := g.verifier.Verify(ctx, runID)
	if err != nil {
		g.log.WarnContext(ctx, "post_report: chain verification error",
			"run_id", runID,
			"err", err.Error(),
		)
		return false, fmt.Sprintf("error: %s", err.Error()), 0
	}
	if !result.Valid {
		return false, fmt.Sprintf("failed: %d of %d records tampered", len(result.Failures), result.Count), result.Count
	}
	return true, "valid", result.Count
}

// --- Rendering --------------------------------------------------------------

// ToText renders the report as a human-readable multi-line text document. The
// document is section-oriented so that it remains readable when surfaced in
// a terminal, an incident ticket or a log buffer. The layout is stable so
// that downstream tools can grep for section headers.
func (r *PostReport) ToText() string {
	var b strings.Builder

	fmt.Fprintf(&b, "Post-Action Audit Report %s\n", r.ReportID)
	fmt.Fprintf(&b, "========================================\n")
	fmt.Fprintf(&b, "Workflow ID : %s\n", r.WorkflowID)
	if r.AlertID != "" {
		fmt.Fprintf(&b, "Alert ID    : %s\n", r.AlertID)
	}
	fmt.Fprintf(&b, "Target      : %s\n", r.Target)
	fmt.Fprintf(&b, "Summary     : %s\n", r.Summary)
	fmt.Fprintf(&b, "Risk Level  : %s\n", r.RiskLevel)
	fmt.Fprintf(&b, "Success     : %t\n", r.Success)
	fmt.Fprintf(&b, "Rollback    : %t\n", r.RollbackUsed)
	fmt.Fprintf(&b, "Duration    : %s\n", r.Duration.String())
	fmt.Fprintf(&b, "Started At  : %s\n", r.StartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Finished At : %s\n", r.FinishedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Generated At: %s\n", r.GeneratedAt.UTC().Format(time.RFC3339))

	fmt.Fprintf(&b, "\n-- Metrics Delta --\n")
	if len(r.MetricsDelta) == 0 {
		fmt.Fprintf(&b, "(none)\n")
	} else {
		for _, k := range sortedKeys(r.MetricsDelta) {
			fmt.Fprintf(&b, "  %s: %.6g -> %.6g (delta=%.6g)\n",
				k, r.MetricsBefore[k], r.MetricsAfter[k], r.MetricsDelta[k])
		}
	}

	fmt.Fprintf(&b, "\n-- Audit Chain --\n")
	fmt.Fprintf(&b, "Valid       : %t\n", r.AuditChainValid)
	fmt.Fprintf(&b, "Trace Count : %d\n", r.TraceCount)
	if r.AuditChainDetail != "" {
		fmt.Fprintf(&b, "Detail      : %s\n", r.AuditChainDetail)
	}

	fmt.Fprintf(&b, "\n-- Lessons Learned --\n")
	if len(r.LessonsLearned) == 0 {
		fmt.Fprintf(&b, "(none)\n")
	} else {
		for i, l := range r.LessonsLearned {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, l)
		}
	}

	return b.String()
}

// ToJSON renders the report as a compact JSON document using the standard
// encoding/json marshaler. The struct field tags control the schema.
func (r *PostReport) ToJSON() ([]byte, error) {
	return json.Marshal(r)
}

// sortedKeys returns the keys of m sorted lexicographically. It is a helper
// for ToText so that the metrics delta section has a stable, deterministic
// order across runs.
func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
