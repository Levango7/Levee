// rollback_notify.go implements RollbackNotifier, a dedicated notifier for
// rollback events. Rollback notifications are intentionally kept independent
// from apply notifications: each rollback event (triggered, succeeded, failed,
// partial) produces its own Message with its own Event, Level and Title, and
// is delivered through the NotificationManager without merging with any
// previously sent apply notification.
//
// The notifier ensures that the run initiator, the approver and the oncall
// user all receive every rollback notification, so that the human stakeholders
// who started or approved the run are never left in the dark when a rollback
// is triggered or completes.

package notify

import (
	"context"
	"errors"
	"fmt"

	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrEmptyInitiator is returned when a rollback notification is requested
	// without an initiator identifier.
	ErrEmptyInitiator = errors.New("notify: empty initiator")
)

// --- RollbackNotifier -------------------------------------------------------

// RollbackNotifier sends independent notifications for rollback events. It
// deliberately does not merge rollback notifications with apply notifications:
// every call produces a fresh Message with a rollback-specific Event, Level and
// Title, and the message is delivered through the NotificationManager as a
// standalone notification. This guarantees that even when an apply
// notification has already been sent for the same run, the rollback
// notification is still delivered independently and is distinguishable by
// downstream consumers via its Event field.
//
// All rollback notifications are addressed to the same three stakeholders:
// the run initiator, the approver and the oncall user. This makes sure the
// people who started or approved the run, plus the person currently
// responsible for the affected service, are all informed about the rollback.
type RollbackNotifier struct {
	manager *NotificationManager
}

// NewRollbackNotifier constructs a RollbackNotifier that delivers through the
// given NotificationManager. The manager must be non-nil; otherwise the
// returned notifier will panic on first use. Callers typically share a single
// manager across all notifier types (apply, approval, rollback, ...).
func NewRollbackNotifier(manager *NotificationManager) *RollbackNotifier {
	return &RollbackNotifier{manager: manager}
}

// rollbackRecipients builds the canonical recipient list for a rollback
// notification: initiator + approver + oncall. The order is stable so that
// tests and downstream consumers can rely on it.
func rollbackRecipients(initiator, approver, oncall string) []Recipient {
	return []Recipient{
		Initiator(initiator, initiator),
		Approver(approver, approver),
		Oncall(oncall, oncall),
	}
}

// validateRollbackInputs performs the common pre-send checks shared by all
// rollback notification methods. It returns a wrapped sentinel error on
// failure. The run id and initiator are required; approver and oncall are
// strongly recommended but not enforced here so that callers can still send
// notifications when one of them is genuinely unknown (e.g. no approver for
// an auto-approved run).
func validateRollbackInputs(runID, initiator string) error {
	if runID == "" {
		return fmt.Errorf("notify: rollback: %w", ErrEmptyRunID)
	}
	if initiator == "" {
		return fmt.Errorf("notify: rollback: %w", ErrEmptyInitiator)
	}
	return nil
}

// buildRollbackMessage constructs a standalone Message for a rollback event.
// The message carries the run id in its Metadata so that downstream consumers
// can correlate it with the run even if they only inspect the metadata. Each
// call produces a new Message with a fresh ID and timestamp, which is what
// makes rollback notifications independent from each other and from any
// apply notification that may have been sent earlier for the same run.
func buildRollbackMessage(event TriggerPoint, runID string, level MessageLevel, title, body, initiator, approver, oncall string) Message {
	msg := NewMessage(string(event), runID, level, title, body)
	msg.Recipients = rollbackRecipients(initiator, approver, oncall)
	msg.Metadata = map[string]string{
		"run_id":    runID,
		"initiator": initiator,
		"approver":  approver,
		"oncall":    oncall,
		"event":     string(event),
		"rollback":  "true",
		"scope":     "rollback",
	}
	return msg
}

// sendRollback is the shared delivery helper. It validates the inputs,
// constructs the message and delegates to the manager. Errors are logged and
// returned to the caller so that callers can decide whether to retry or
// surface the failure.
func (rn *RollbackNotifier) sendRollback(ctx context.Context, event TriggerPoint, runID string, level MessageLevel, title, body, initiator, approver, oncall string) error {
	if err := validateRollbackInputs(runID, initiator); err != nil {
		return err
	}
	if rn.manager == nil {
		return fmt.Errorf("notify: rollback: %w", ErrNotifierNotFound)
	}

	msg := buildRollbackMessage(event, runID, level, title, body, initiator, approver, oncall)
	if err := rn.manager.Notify(ctx, msg); err != nil {
		log.Warn("notify: rollback send failed",
			"event", string(event),
			"run_id", runID,
			"err", err)
		return fmt.Errorf("notify: rollback %s: %w", event, err)
	}
	return nil
}

// NotifyTriggered notifies that a rollback has been triggered for the given
// run. The notification is sent at LevelWarning because a rollback trigger
// usually indicates something went wrong with the original apply, but the
// rollback itself has not yet completed. Recipients: initiator + approver +
// oncall. The provided reason becomes the message body.
func (rn *RollbackNotifier) NotifyTriggered(ctx context.Context, runID, initiator, approver, oncall, reason string) error {
	title := fmt.Sprintf("Rollback triggered for run %s", runID)
	return rn.sendRollback(ctx, TriggerRollbackTriggered, runID, LevelWarning, title, reason, initiator, approver, oncall)
}

// NotifySucceeded notifies that a rollback has completed successfully for the
// given run. The notification is sent at LevelInfo because the rollback
// achieved its goal. Recipients: initiator + approver + oncall. The provided
// details become the message body.
func (rn *RollbackNotifier) NotifySucceeded(ctx context.Context, runID, initiator, approver, oncall, details string) error {
	title := fmt.Sprintf("Rollback completed for run %s", runID)
	return rn.sendRollback(ctx, TriggerRollbackCompleted, runID, LevelInfo, title, details, initiator, approver, oncall)
}

// NotifyFailed notifies that a rollback has failed for the given run. The
// notification is sent at LevelCritical because a failed rollback leaves the
// system in a potentially inconsistent state and requires immediate human
// intervention. Recipients: initiator + approver + oncall. The provided
// failureReason becomes the message body.
func (rn *RollbackNotifier) NotifyFailed(ctx context.Context, runID, initiator, approver, oncall, failureReason string) error {
	title := fmt.Sprintf("Rollback FAILED for run %s", runID)
	return rn.sendRollback(ctx, TriggerRollbackFailed, runID, LevelCritical, title, failureReason, initiator, approver, oncall)
}

// NotifyPartial notifies that a rollback has only partially completed for the
// given run. The notification is sent at LevelWarning because the rollback
// made progress but did not fully restore the desired state. Recipients:
// initiator + approver + oncall. The provided details become the message body.
func (rn *RollbackNotifier) NotifyPartial(ctx context.Context, runID, initiator, approver, oncall, details string) error {
	title := fmt.Sprintf("Rollback partially completed for run %s", runID)
	return rn.sendRollback(ctx, TriggerRollbackCompleted, runID, LevelWarning, title, details, initiator, approver, oncall)
}
