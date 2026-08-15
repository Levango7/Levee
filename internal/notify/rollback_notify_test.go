package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Helpers ---------------------------------------------------------------

// newRollbackSetup builds a NotificationManager with a single mock notifier
// and returns the RollbackNotifier plus the mock so tests can inspect captured
// messages.
func newRollbackSetup(t *testing.T) (*RollbackNotifier, *mockNotifier) {
	t.Helper()
	mgr := NewNotificationManager()
	mock := newMock("rollback-mock")
	require.NoError(t, mgr.Register(mock))
	return NewRollbackNotifier(mgr), mock
}

// rollbackArgs holds the common inputs for a rollback notification.
type rollbackArgs struct {
	runID     string
	initiator string
	approver  string
	oncall    string
	detail    string
}

func defaultRollbackArgs() rollbackArgs {
	return rollbackArgs{
		runID:     "run-rollback-1",
		initiator: "alice",
		approver:  "bob",
		oncall:    "carol",
		detail:    "manual rollback requested",
	}
}

// assertRollbackRecipients verifies that the message was sent to exactly the
// initiator, approver and oncall, in that order.
func assertRollbackRecipients(t *testing.T, msg Message, args rollbackArgs) {
	t.Helper()
	require.Len(t, msg.Recipients, 3, "expected initiator+approver+oncall")
	assert.Equal(t, RecipientInitiator, msg.Recipients[0].Type)
	assert.Equal(t, args.initiator, msg.Recipients[0].ID)
	assert.Equal(t, RecipientApprover, msg.Recipients[1].Type)
	assert.Equal(t, args.approver, msg.Recipients[1].ID)
	assert.Equal(t, RecipientOncall, msg.Recipients[2].Type)
	assert.Equal(t, args.oncall, msg.Recipients[2].ID)
}

// --- NotifyTriggered ------------------------------------------------------

// TestRollbackNotifyTriggered verifies that NotifyTriggered sends a message
// with the correct Event, Level, Title, Body and Recipients.
func TestRollbackNotifyTriggered(t *testing.T) {
	rn, mock := newRollbackSetup(t)
	args := defaultRollbackArgs()

	err := rn.NotifyTriggered(context.Background(), args.runID, args.initiator, args.approver, args.oncall, args.detail)
	require.NoError(t, err)

	msgs := mock.messages()
	require.Len(t, msgs, 1, "expected exactly one message")
	msg := msgs[0]

	assert.Equal(t, string(TriggerRollbackTriggered), msg.Event)
	assert.Equal(t, LevelWarning, msg.Level)
	assert.Equal(t, "Rollback triggered for run run-rollback-1", msg.Title)
	assert.Equal(t, args.detail, msg.Body)
	assert.Equal(t, args.runID, msg.RunID)
	assertRollbackRecipients(t, msg, args)
}

// --- NotifySucceeded ------------------------------------------------------

// TestRollbackNotifySucceeded verifies that NotifySucceeded sends a message
// with the correct Event, Level and Recipients.
func TestRollbackNotifySucceeded(t *testing.T) {
	rn, mock := newRollbackSetup(t)
	args := defaultRollbackArgs()

	err := rn.NotifySucceeded(context.Background(), args.runID, args.initiator, args.approver, args.oncall, "rollback ok")
	require.NoError(t, err)

	msgs := mock.messages()
	require.Len(t, msgs, 1)
	msg := msgs[0]

	assert.Equal(t, string(TriggerRollbackCompleted), msg.Event)
	assert.Equal(t, LevelInfo, msg.Level)
	assert.Equal(t, "Rollback completed for run run-rollback-1", msg.Title)
	assert.Equal(t, "rollback ok", msg.Body)
	assertRollbackRecipients(t, msg, args)
}

// --- NotifyFailed ---------------------------------------------------------

// TestRollbackNotifyFailed verifies that NotifyFailed sends a message with
// Level=Critical and the correct Recipients.
func TestRollbackNotifyFailed(t *testing.T) {
	rn, mock := newRollbackSetup(t)
	args := defaultRollbackArgs()

	err := rn.NotifyFailed(context.Background(), args.runID, args.initiator, args.approver, args.oncall, "snapshots missing")
	require.NoError(t, err)

	msgs := mock.messages()
	require.Len(t, msgs, 1)
	msg := msgs[0]

	assert.Equal(t, string(TriggerRollbackFailed), msg.Event)
	assert.Equal(t, LevelCritical, msg.Level)
	assert.Equal(t, "Rollback FAILED for run run-rollback-1", msg.Title)
	assert.Equal(t, "snapshots missing", msg.Body)
	assertRollbackRecipients(t, msg, args)
}

// --- NotifyPartial --------------------------------------------------------

// TestRollbackNotifyPartial verifies that NotifyPartial sends a message with
// Level=Warning and the correct Recipients.
func TestRollbackNotifyPartial(t *testing.T) {
	rn, mock := newRollbackSetup(t)
	args := defaultRollbackArgs()

	err := rn.NotifyPartial(context.Background(), args.runID, args.initiator, args.approver, args.oncall, "3 of 5 steps reverted")
	require.NoError(t, err)

	msgs := mock.messages()
	require.Len(t, msgs, 1)
	msg := msgs[0]

	assert.Equal(t, string(TriggerRollbackCompleted), msg.Event)
	assert.Equal(t, LevelWarning, msg.Level)
	assert.Equal(t, "Rollback partially completed for run run-rollback-1", msg.Title)
	assert.Equal(t, "3 of 5 steps reverted", msg.Body)
	assertRollbackRecipients(t, msg, args)
}

// --- Independence ---------------------------------------------------------

// TestRollbackNotificationsIndependent verifies that consecutive rollback
// notifications produce independent messages with distinct IDs, even for the
// same run. This is the core independence guarantee: each call yields a fresh
// Message, not a merge with previous ones.
func TestRollbackNotificationsIndependent(t *testing.T) {
	rn, mock := newRollbackSetup(t)
	args := defaultRollbackArgs()

	require.NoError(t, rn.NotifyTriggered(context.Background(), args.runID, args.initiator, args.approver, args.oncall, "trigger"))
	require.NoError(t, rn.NotifySucceeded(context.Background(), args.runID, args.initiator, args.approver, args.oncall, "done"))

	msgs := mock.messages()
	require.Len(t, msgs, 2, "expected two independent messages")

	// Distinct IDs and timestamps prove they are separate Message values.
	assert.NotEqual(t, msgs[0].ID, msgs[1].ID, "messages must have distinct ids")
	assert.NotEqual(t, msgs[0].Event, msgs[1].Event, "triggered vs completed must differ in event")
	assert.NotEqual(t, msgs[0].Level, msgs[1].Level, "warning vs info must differ in level")
	assert.NotEqual(t, msgs[0].Title, msgs[1].Title, "titles must differ")
}

// TestRollbackIndependentFromApply verifies that a rollback notification is
// independent from an apply notification sent earlier for the same run. The
// two messages must coexist with distinct Events and Titles, proving that the
// rollback notifier does not merge with or suppress the apply notification.
func TestRollbackIndependentFromApply(t *testing.T) {
	mgr := NewNotificationManager()
	mock := newMock("indep-mock")
	require.NoError(t, mgr.Register(mock))

	// Send an apply-style notification directly through the manager.
	applyMsg := NewMessage(string(TriggerRunCompleted), "run-x", LevelInfo, "Run completed", "apply finished")
	applyMsg.Recipients = []Recipient{Initiator("alice", "alice")}
	require.NoError(t, mgr.Notify(context.Background(), applyMsg))

	// Now send a rollback notification for the same run.
	rn := NewRollbackNotifier(mgr)
	require.NoError(t, rn.NotifyTriggered(context.Background(), "run-x", "alice", "bob", "carol", "rollback needed"))

	msgs := mock.messages()
	require.Len(t, msgs, 2, "apply and rollback must both be delivered")

	apply, rollback := msgs[0], msgs[1]
	assert.Equal(t, string(TriggerRunCompleted), apply.Event)
	assert.Equal(t, string(TriggerRollbackTriggered), rollback.Event)
	assert.NotEqual(t, apply.Event, rollback.Event, "apply and rollback events must differ")
	assert.NotEqual(t, apply.Title, rollback.Title, "apply and rollback titles must differ")
	assert.NotEqual(t, apply.ID, rollback.ID, "apply and rollback must have distinct ids")
}

// --- Recipient completeness -----------------------------------------------

// TestRollbackRecipientsComplete verifies that every rollback notification
// method delivers to initiator + approver + oncall.
func TestRollbackRecipientsComplete(t *testing.T) {
	rn, mock := newRollbackSetup(t)
	args := defaultRollbackArgs()
	ctx := context.Background()

	require.NoError(t, rn.NotifyTriggered(ctx, args.runID, args.initiator, args.approver, args.oncall, "r"))
	require.NoError(t, rn.NotifySucceeded(ctx, args.runID, args.initiator, args.approver, args.oncall, "r"))
	require.NoError(t, rn.NotifyFailed(ctx, args.runID, args.initiator, args.approver, args.oncall, "r"))
	require.NoError(t, rn.NotifyPartial(ctx, args.runID, args.initiator, args.approver, args.oncall, "r"))

	msgs := mock.messages()
	require.Len(t, msgs, 4)
	for i, msg := range msgs {
		assertRollbackRecipients(t, msg, args)
		// Sanity: each message has a unique id.
		for j := i + 1; j < len(msgs); j++ {
			assert.NotEqual(t, msg.ID, msgs[j].ID, "message ids must be unique")
		}
	}
}

// --- Validation -----------------------------------------------------------

// TestRollbackNotifyEmptyRunID verifies that an empty run id is rejected with
// ErrEmptyRunID for every method.
func TestRollbackNotifyEmptyRunID(t *testing.T) {
	rn, _ := newRollbackSetup(t)
	args := defaultRollbackArgs()
	args.runID = ""

	cases := []struct {
		name string
		fn   func() error
	}{
		{"triggered", func() error {
			return rn.NotifyTriggered(context.Background(), args.runID, args.initiator, args.approver, args.oncall, "r")
		}},
		{"succeeded", func() error {
			return rn.NotifySucceeded(context.Background(), args.runID, args.initiator, args.approver, args.oncall, "r")
		}},
		{"failed", func() error {
			return rn.NotifyFailed(context.Background(), args.runID, args.initiator, args.approver, args.oncall, "r")
		}},
		{"partial", func() error {
			return rn.NotifyPartial(context.Background(), args.runID, args.initiator, args.approver, args.oncall, "r")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.fn()
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrEmptyRunID), "want ErrEmptyRunID, got %v", err)
		})
	}
}

// TestRollbackNotifyEmptyInitiator verifies that an empty initiator is
// rejected with ErrEmptyInitiator.
func TestRollbackNotifyEmptyInitiator(t *testing.T) {
	rn, _ := newRollbackSetup(t)
	args := defaultRollbackArgs()
	args.initiator = ""

	err := rn.NotifyTriggered(context.Background(), args.runID, args.initiator, args.approver, args.oncall, "r")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmptyInitiator), "want ErrEmptyInitiator, got %v", err)
}

// --- Metadata -------------------------------------------------------------

// TestRollbackMetadataContainsRunID verifies that every rollback message
// carries the run id in its Metadata so downstream consumers can correlate
// the notification with the run.
func TestRollbackMetadataContainsRunID(t *testing.T) {
	rn, mock := newRollbackSetup(t)
	args := defaultRollbackArgs()
	ctx := context.Background()

	require.NoError(t, rn.NotifyTriggered(ctx, args.runID, args.initiator, args.approver, args.oncall, "r"))
	require.NoError(t, rn.NotifySucceeded(ctx, args.runID, args.initiator, args.approver, args.oncall, "r"))
	require.NoError(t, rn.NotifyFailed(ctx, args.runID, args.initiator, args.approver, args.oncall, "r"))
	require.NoError(t, rn.NotifyPartial(ctx, args.runID, args.initiator, args.approver, args.oncall, "r"))

	msgs := mock.messages()
	require.Len(t, msgs, 4)
	for _, msg := range msgs {
		assert.Equal(t, args.runID, msg.Metadata["run_id"], "metadata must carry run_id")
		assert.Equal(t, args.initiator, msg.Metadata["initiator"])
		assert.Equal(t, args.approver, msg.Metadata["approver"])
		assert.Equal(t, args.oncall, msg.Metadata["oncall"])
		assert.Equal(t, "true", msg.Metadata["rollback"], "metadata must mark the message as a rollback")
		assert.Equal(t, "rollback", msg.Metadata["scope"], "metadata must scope the message to rollback")
	}
}

// --- Partial vs Full ------------------------------------------------------

// TestRollbackPartialVsFullLevel verifies that a partial rollback uses
// LevelWarning while a full successful rollback uses LevelInfo. The two must
// not be conflated, because operators triage them differently.
func TestRollbackPartialVsFullLevel(t *testing.T) {
	rn, mock := newRollbackSetup(t)
	args := defaultRollbackArgs()
	ctx := context.Background()

	require.NoError(t, rn.NotifySucceeded(ctx, args.runID, args.initiator, args.approver, args.oncall, "fully reverted"))
	require.NoError(t, rn.NotifyPartial(ctx, args.runID, args.initiator, args.approver, args.oncall, "partially reverted"))

	msgs := mock.messages()
	require.Len(t, msgs, 2)
	full, partial := msgs[0], msgs[1]

	assert.Equal(t, LevelInfo, full.Level, "full rollback must be info")
	assert.Equal(t, LevelWarning, partial.Level, "partial rollback must be warning")
	assert.NotEqual(t, full.Level, partial.Level, "full and partial must differ in level")
	// Both reuse the same Event but differ in Level, which is the documented
	// behaviour for partial vs full rollback.
	assert.Equal(t, full.Event, partial.Event, "both use rollback_completed event")
}

// --- Nil manager ----------------------------------------------------------

// TestRollbackNilManager verifies that a nil manager produces a wrapped
// ErrNotifierNotFound rather than a panic.
func TestRollbackNilManager(t *testing.T) {
	rn := NewRollbackNotifier(nil)
	args := defaultRollbackArgs()

	err := rn.NotifyTriggered(context.Background(), args.runID, args.initiator, args.approver, args.oncall, "r")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotifierNotFound), "want ErrNotifierNotFound, got %v", err)
}
