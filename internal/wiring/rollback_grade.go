// rollback_grade.go builds the Grader that post-rollback verification uses
// to classify a rollback and dispatch its consequences.
//
// Why this file exists: T038 shipped a Grader, T037 shipped a
// PostRollbackVerifier with a WithGrader option, and neither was ever
// attached to anything — PostVerifyResult.Grade was structurally always
// empty and no notify / escalate / audit could ever fire. The engine now
// calls VerifyAndGrade (see closure.go), so a nil Grader would mean the
// grade is computed and thrown away. This file is the missing half.
//
// The grading logic itself is NOT here. rollback.Grader is deliberately
// transport-free: it classifies, and this file decides what "notify" and
// "escalate" concretely mean for a deployment.
//
// Transport status, stated plainly:
//
//   - Audit: written to the structured log. The audit package has a
//     hash-chained TraceRecorder, but the engine path records no traces
//     today and nothing in state carries the identity a rollback trace
//     would need, so claiming an audit record here would be a fiction. The
//     callback is the seam; hooking it to TraceRecorder is a separate
//     change with its own ownership question.
//   - Notify: routed through rollbackNotifySink. No sink is constructed
//     yet, and the reason is a missing fact rather than missing code —
//     state records no initiator for a change, and notify.RollbackNotifier
//     (which is implemented, and also had zero callers) refuses to send
//     without one. Inventing a recipient would be worse than not sending.
//     notifyRollbackSink below is the ready-made adapter for whoever wires
//     a NotificationManager once initiators are recorded.
//   - Escalate: logged at error level. A failed rollback that left targets
//     in an unknown state is the one event that must not pass silently;
//     until a paging transport is chosen, a loud structured log is the
//     honest floor, and the message says what an operator should do.

package wiring

import (
	"context"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/notify"
	"github.com/nexus/levee/internal/rollback"
)

// rollbackNotifySink is the transport seam for rollback notifications. It is
// deliberately narrower than notify.RollbackNotifier: recipients are the
// sink's business, not the grader's, and the grader must stay testable
// without a message bus.
type rollbackNotifySink interface {
	// NotifyGrade delivers one grade. Implementations decide the transport
	// and the recipients, and may ignore grades they have nothing to say
	// about (success has no notification by design).
	NotifyGrade(ctx context.Context, grade rollback.RollbackGrade, runID, summary string) error
}

// notifyRollbackSink adapts a *notify.RollbackNotifier to the sink
// interface. It is currently unused by the Engine (see the file comment),
// and exists so that wiring one is a single line rather than a rewrite.
type notifyRollbackSink struct {
	notifier  *notify.RollbackNotifier
	initiator string
	approver  string
	oncall    string
}

// NotifyGrade maps a grade onto the notifier's entry points. Success has
// no entry point and returns nil: rollback.Grader never dispatches actions
// for it anyway (GetAction returns an all-nil action), so reaching here
// with success would be a bug, not a silent drop worth reporting.
func (s *notifyRollbackSink) NotifyGrade(ctx context.Context, grade rollback.RollbackGrade, runID, summary string) error {
	switch grade {
	case rollback.GradeFailure:
		return s.notifier.NotifyFailed(ctx, runID, s.initiator, s.approver, s.oncall, summary)
	case rollback.GradePartial:
		return s.notifier.NotifyPartial(ctx, runID, s.initiator, s.approver, s.oncall, summary)
	default:
		return nil
	}
}

// newRollbackGrader returns the Grader attached to the per-run
// PostRollbackVerifier. A nil sink is valid and means "log-only": the grade
// is still classified and recorded, nothing is lost but the delivery.
//
// actors carries the resolved recipients. It is a value, not a config knob
// on the Engine, because the recipients are per-run facts (who created this
// run, who approved it) rather than deployment settings — resolving them
// once per run is also what lets the notify path work at all today, since
// notify.RollbackNotifier refuses to send without an initiator.
func newRollbackGrader(sink rollbackNotifySink) *rollback.Grader {
	return rollback.NewGrader(
		rollback.WithPartialNotify(gradeNotifier(sink)),
		rollback.WithPartialAudit(gradeAuditor),
		rollback.WithFailureNotify(gradeNotifier(sink)),
		rollback.WithFailureEscalate(gradeEscalator),
		rollback.WithFailureAudit(gradeAuditor),
	)
}

// newRunNotifySink builds the per-run notify transport, or nil when there is
// nothing to deliver through.
//
// It returns nil (rather than a sink that fails) when no NotificationManager
// is configured or the initiator is unknown, and gradeNotifier then logs the
// grade instead. That is the honest outcome: a notification with an invented
// recipient is worse than a logged grade, because it looks delivered.
//
// The sink is per-run, not shared, so binding the recipients at construction
// is race-free — the alternative (mutating the recipients inside the notify
// callback) would be a data race waiting for two concurrent rollbacks.
func (e *Engine) newRunNotifySink(actors RollbackActors) rollbackNotifySink {
	if e == nil || e.notifier == nil {
		return nil
	}
	if actors.Initiator == "" {
		log.Warn("rollback notification transport present but initiator unknown; " +
			"notify suppressed rather than addressed to an invented recipient")
		return nil
	}
	return &notifyRollbackSink{
		notifier:  notify.NewRollbackNotifier(e.notifier),
		initiator: actors.Initiator,
		approver:  actors.Approver,
		oncall:    actors.Oncall,
	}
}

// RollbackActors are the humans a rollback notification must reach, resolved
// per run from the run record.
type RollbackActors struct {
	// Initiator created the run. Required by notify.RollbackNotifier.
	Initiator string
	// Approver signed off on it, when one did.
	Approver string
	// Oncall is whoever is on duty; not recorded in state today, so it is
	// usually empty and the notification simply goes to fewer people.
	Oncall string
}

// resolveRollbackActors reads the run record to find who to notify.
//
// A missing run is not an error here: grading runs on the rollback path,
// where a run that cannot be read is exactly the situation an operator needs
// to hear about, and returning an error would only replace the notification
// with another log line. Empty actors degrade the notification, they do not
// skip it — notifyRollbackSink still fires, and the message reaches whoever
// the manager has channels for.
func (e *Engine) resolveRollbackActors(ctx context.Context, changeID string) RollbackActors {
	if e == nil || e.store == nil || changeID == "" {
		return RollbackActors{}
	}
	run, err := e.store.GetRun(ctx, changeID)
	if err != nil || run == nil {
		log.Warn("rollback actors unresolved: run record unreadable; notification recipients may be incomplete",
			"change_id", changeID,
			"error", err)
		return RollbackActors{}
	}
	return RollbackActors{Initiator: run.Creator}
}

// gradeNotifier builds the Notify callback for both partial and failure
// grades. Delivery failure is reported to the caller (VerifyAndGrade wraps
// and returns it) but is never fatal to the run: the rollback already
// happened, and a refused webhook does not undo that.
func gradeNotifier(sink rollbackNotifySink) rollback.NotifyFunc {
	return func(ctx context.Context, grade rollback.RollbackGrade, result *rollback.RollbackResult) error {
		summary := rollback.GradeSummary(grade, result)
		if sink == nil {
			log.Warn("rollback graded; no notification transport is wired (notify suppressed)",
				"grade", string(grade),
				"run_id", runIDOf(result),
				"summary", summary)
			return nil
		}
		if err := sink.NotifyGrade(ctx, grade, runIDOf(result), summary); err != nil {
			log.Error("rollback notification delivery failed",
				"grade", string(grade),
				"run_id", runIDOf(result),
				"error", err)
			return err
		}
		return nil
	}
}

// gradeEscalator is the failure-grade escalation. It is a log line today;
// the wording is written for whoever is on call reading it at 3am.
func gradeEscalator(_ context.Context, grade rollback.RollbackGrade, result *rollback.RollbackResult) error {
	log.Error("ROLLBACK FAILED - operator action required: targets may be in an unknown state",
		"grade", string(grade),
		"run_id", runIDOf(result),
		"required_compensations", compensationsOf(result, true),
		"completed_compensations", compensationsOf(result, false),
		"unknown_side_effects", unknownOf(result),
		"summary", rollback.GradeSummary(grade, result))
	return nil
}

// gradeAuditor records the grade and the rollback verdict in the structured
// log. See the file comment for why this is not the audit package yet.
func gradeAuditor(_ context.Context, grade rollback.RollbackGrade, result *rollback.RollbackResult) error {
	log.Info("rollback graded",
		"grade", string(grade),
		"run_id", runIDOf(result),
		"summary", rollback.GradeSummary(grade, result))
	return nil
}

// runIDOf extracts the run id defensively: the grader callbacks are
// reachable with a nil result (Grader.Grade(nil) is legal and yields
// failure), and a nil dereference in a notification path is exactly the kind
// of crash that turns "rollback failed" into "rollback failed AND the
// process died".
func runIDOf(result *rollback.RollbackResult) string {
	if result == nil {
		return ""
	}
	return result.RunID
}

func compensationsOf(result *rollback.RollbackResult, required bool) int {
	if result == nil {
		return 0
	}
	if required {
		return result.RequiredCompensations
	}
	return result.CompletedCompensations
}

func unknownOf(result *rollback.RollbackResult) int {
	if result == nil {
		return 0
	}
	return result.UnknownSideEffects
}
