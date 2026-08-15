package rollback

// grading.go implements rollback failure grading (MVP task T038). After a
// rollback run completes (and after the post-rollback verification, if any),
// the result is classified into one of three grades:
//
//   - GradeSuccess — every executed rollback step succeeded.
//   - GradePartial — some rollback steps succeeded and some failed.
//   - GradeFailure — every executed rollback step failed (or the rollback
//     could not even start, e.g. nil plan / nil result).
//
// Each grade maps to a GradeAction that prescribes whether to notify
// operations, escalate to human intervention and record an audit entry.
// The Grader is deliberately side-effect free at the Grade / GetAction
// level: it returns a GradeAction describing what should happen, and the
// caller (CLI / orchestrator) is responsible for dispatching the notify /
// escalate / audit calls. This keeps the grading logic trivially testable
// and free of transport concerns. The convenience method GradeAndAct
// bundles grading + action dispatch for callers that want both in one
// call.

import (
	"context"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- RollbackGrade ---------------------------------------------------------

// RollbackGrade is the three-valued classification of a rollback outcome.
// The string values are stable identifiers used in logs, the audit trail
// and notification payloads.
type RollbackGrade string

const (
	// GradeSuccess indicates a fully successful rollback: every executed
	// step completed without error. Skipped steps (no RollbackSpec, not
	// in whitelist) do not affect the grade.
	GradeSuccess RollbackGrade = "success"

	// GradePartial indicates a partially successful rollback: at least one
	// executed step succeeded and at least one failed. Human inspection is
	// typically required.
	GradePartial RollbackGrade = "partial"

	// GradeFailure indicates a fully failed rollback: every executed step
	// failed, or the rollback could not start (nil plan, nil result).
	GradeFailure RollbackGrade = "failure"
)

// --- Action callbacks ------------------------------------------------------

// NotifyFunc is the action invoked to notify operations staff of a rollback
// outcome. Implementations typically forward to the notify package; the
// grading package itself only defines the signature so that it stays free of
// transport concerns. The callback receives the grade and the rollback
// result so that it can build a meaningful message.
type NotifyFunc func(ctx context.Context, grade RollbackGrade, result *RollbackResult) error

// EscalateFunc is the action invoked to escalate a rollback failure to human
// intervention. Implementations typically create a ticket, page on-call, or
// mark the run as requiring manual cleanup.
type EscalateFunc func(ctx context.Context, grade RollbackGrade, result *RollbackResult) error

// AuditFunc is the action invoked to record an audit entry for the rollback
// outcome. Implementations typically forward to the audit package.
type AuditFunc func(ctx context.Context, grade RollbackGrade, result *RollbackResult) error

// --- GradeAction -----------------------------------------------------------

// GradeAction describes the side effects that should be dispatched for a
// given RollbackGrade. The Grader fills the fields with the configured
// actions; a nil field means "no action for this grade".
//
// Mapping (per task T038):
//
//   - GradeSuccess: no notify, no escalate, no audit (only a log entry).
//   - GradePartial: notify operations + record audit.
//   - GradeFailure: notify operations + escalate to human + record audit.
type GradeAction struct {
	// Grade is the grade this action corresponds to.
	Grade RollbackGrade

	// Notify is invoked to notify operations staff. nil means no
	// notification is required for this grade.
	Notify NotifyFunc

	// Escalate is invoked to escalate to human intervention. nil means no
	// escalation is required for this grade.
	Escalate EscalateFunc

	// Audit is invoked to record an audit entry. nil means no audit entry
	// is required for this grade.
	Audit AuditFunc
}

// --- Grader ----------------------------------------------------------------

// Grader classifies a RollbackResult into a RollbackGrade and returns the
// corresponding GradeAction. It is configured at construction time with the
// notify / escalate / audit actions to dispatch for each grade; the Grader
// itself does not invoke them in Grade / GetAction, it only returns the
// action descriptor. The caller is responsible for invoking Notify /
// Escalate / Audit when non-nil. The convenience method GradeAndAct bundles
// both steps.
type Grader struct {
	// onPartialNotify is the notification action for GradePartial.
	onPartialNotify NotifyFunc
	// onPartialAudit is the audit action for GradePartial.
	onPartialAudit AuditFunc
	// onFailureNotify is the notification action for GradeFailure.
	onFailureNotify NotifyFunc
	// onFailureEscalate is the escalation action for GradeFailure.
	onFailureEscalate EscalateFunc
	// onFailureAudit is the audit action for GradeFailure.
	onFailureAudit AuditFunc
}

// GraderOption configures a Grader at construction time.
type GraderOption func(*Grader)

// WithPartialNotify sets the notification action dispatched for GradePartial.
func WithPartialNotify(f NotifyFunc) GraderOption {
	return func(g *Grader) { g.onPartialNotify = f }
}

// WithPartialAudit sets the audit action dispatched for GradePartial.
func WithPartialAudit(f AuditFunc) GraderOption {
	return func(g *Grader) { g.onPartialAudit = f }
}

// WithFailureNotify sets the notification action dispatched for GradeFailure.
func WithFailureNotify(f NotifyFunc) GraderOption {
	return func(g *Grader) { g.onFailureNotify = f }
}

// WithFailureEscalate sets the escalation action dispatched for GradeFailure.
func WithFailureEscalate(f EscalateFunc) GraderOption {
	return func(g *Grader) { g.onFailureEscalate = f }
}

// WithFailureAudit sets the audit action dispatched for GradeFailure.
func WithFailureAudit(f AuditFunc) GraderOption {
	return func(g *Grader) { g.onFailureAudit = f }
}

// NewGrader returns a Grader configured by opts. By default no actions are
// configured; callers wire notify / escalate / audit via the With* options.
func NewGrader(opts ...GraderOption) *Grader {
	g := &Grader{}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Grade classifies a RollbackResult into a RollbackGrade.
//
// Grading rules:
//
//   - nil result                          → GradeFailure
//   - result.Success == true              → GradeSuccess
//   - result.PartialRollback == true      → GradePartial
//   - otherwise (all steps failed)        → GradeFailure
//
// The method is side-effect free; it logs the grade at info level for
// observability but does not invoke any notify / escalate / audit action.
func (g *Grader) Grade(result *RollbackResult) RollbackGrade {
	if result == nil {
		log.Info("rollback grade: failure (nil result)")
		return GradeFailure
	}

	if result.Success {
		log.Info("rollback grade: success",
			"duration_ms", result.Duration.Milliseconds())
		return GradeSuccess
	}

	if result.PartialRollback {
		log.Info("rollback grade: partial",
			"duration_ms", result.Duration.Milliseconds())
		return GradePartial
	}

	log.Info("rollback grade: failure",
		"duration_ms", result.Duration.Milliseconds())
	return GradeFailure
}

// GetAction returns the GradeAction corresponding to grade. The returned
// action's Notify / Escalate / Audit fields are populated from the Grader's
// configured actions; nil fields mean "no action for this grade".
//
// Mapping:
//
//   - GradeSuccess: all fields nil (success is logged only).
//   - GradePartial: Notify + Audit populated.
//   - GradeFailure: Notify + Escalate + Audit populated.
//   - unknown grade: empty GradeAction with the given grade.
func (g *Grader) GetAction(grade RollbackGrade) GradeAction {
	switch grade {
	case GradeSuccess:
		return GradeAction{Grade: GradeSuccess}
	case GradePartial:
		return GradeAction{
			Grade:  GradePartial,
			Notify: g.onPartialNotify,
			Audit:  g.onPartialAudit,
		}
	case GradeFailure:
		return GradeAction{
			Grade:    GradeFailure,
			Notify:   g.onFailureNotify,
			Escalate: g.onFailureEscalate,
			Audit:    g.onFailureAudit,
		}
	default:
		return GradeAction{Grade: grade}
	}
}

// GradeAndAct is a convenience method that grades the result and immediately
// dispatches the corresponding action's Notify / Escalate / Audit callbacks
// (when non-nil). It returns the grade and the first error encountered while
// dispatching actions; nil means all actions (if any) succeeded.
//
// Dispatch order is Notify → Escalate → Audit. The first non-nil error
// short-circuits and is returned wrapped with the action that failed; later
// actions are not dispatched. This keeps the failure cause unambiguous.
//
// This is the one-stop entry point for callers that want grading + action
// dispatch in a single call. Callers that need to inspect the GradeAction
// before dispatching should use Grade + GetAction instead.
func (g *Grader) GradeAndAct(ctx context.Context, result *RollbackResult) (RollbackGrade, error) {
	grade := g.Grade(result)
	action := g.GetAction(grade)

	if action.Notify != nil {
		if err := action.Notify(ctx, grade, result); err != nil {
			return grade, fmt.Errorf("rollback grade notify: %w", err)
		}
	}
	if action.Escalate != nil {
		if err := action.Escalate(ctx, grade, result); err != nil {
			return grade, fmt.Errorf("rollback grade escalate: %w", err)
		}
	}
	if action.Audit != nil {
		if err := action.Audit(ctx, grade, result); err != nil {
			return grade, fmt.Errorf("rollback grade audit: %w", err)
		}
	}
	return grade, nil
}

// --- helpers ---------------------------------------------------------------

// GradeSummary returns a human-readable one-line summary of the grade and
// result, suitable for logging or inclusion in a notification body. It
// gracefully handles a nil result.
func GradeSummary(grade RollbackGrade, result *RollbackResult) string {
	if result == nil {
		return fmt.Sprintf("rollback grade=%s (nil result)", grade)
	}
	return fmt.Sprintf("rollback grade=%s success=%t partial=%t duration=%s",
		grade, result.Success, result.PartialRollback,
		result.Duration.Round(time.Millisecond))
}
