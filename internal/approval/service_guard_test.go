package approval

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingGuard is a DecisionGuard test double that records every call and
// answers from an injectable verdict (nil verdict = allow).
type recordingGuard struct {
	mu      sync.Mutex
	calls   []guardCall
	verdict func(call guardCall) error
}

type guardCall struct {
	subject string
	action  string
	runID   string
	level   string
}

func (g *recordingGuard) AuthorizeDecision(ctx context.Context, subject, action string, a *Approval) error {
	call := guardCall{subject: subject, action: action, runID: a.RunID, level: a.Level}
	g.mu.Lock()
	g.calls = append(g.calls, call)
	verdict := g.verdict
	g.mu.Unlock()
	if verdict == nil {
		return nil
	}
	return verdict(call)
}

func (g *recordingGuard) recorded() []guardCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]guardCall(nil), g.calls...)
}

// seedPending creates a pending approval with the given approvers and
// returns its id.
func seedPending(t *testing.T, svc *Service, runID string, approvers ...string) string {
	t.Helper()
	a, err := svc.Create(context.Background(), CreateRequest{
		RunID:     runID,
		Level:     LevelStandard,
		Approvers: approvers,
	})
	require.NoError(t, err)
	return a.ID
}

// A guard that refuses must veto the vote, leave the record untouched, and
// surface ErrDecisionDenied so callers can map it distinctly from the
// record-internal refusals.
func TestDecisionGuardRefusalVetoesVoteAndKeepsRecordPending(t *testing.T) {
	store := newMockStore()
	svc := NewService(store).WithDecisionGuard(&recordingGuard{
		verdict: func(call guardCall) error { return errors.New("no approve on prod for this team") },
	})
	id := seedPending(t, svc, "run-1", "alice")

	err := svc.Approve(context.Background(), id, "alice")

	require.ErrorIs(t, err, ErrDecisionDenied)
	assert.Contains(t, err.Error(), "run-1")
	assert.Contains(t, err.Error(), "alice")
	after, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, after.Status, "a refused vote must not touch the record")
	assert.Empty(t, after.Decisions)
}

// The guard runs BEFORE the record-internal checks: an operator the policy
// refused learns the policy reason, not a record-state reason (and record
// state is not revealed to them). Proof by ordering: an approver who is
// also off the Approvers list still gets the policy refusal.
func TestDecisionGuardRunsBeforeRecordInternalChecks(t *testing.T) {
	store := newMockStore()
	svc := NewService(store).WithDecisionGuard(&recordingGuard{
		verdict: func(call guardCall) error { return errors.New("policy says no") },
	})
	id := seedPending(t, svc, "run-1", "alice")

	// "mallory" is not on the Approvers list either — the guard refusal must
	// win the race, so the error classifies as policy, not as roster.
	err := svc.Approve(context.Background(), id, "mallory")

	require.ErrorIs(t, err, ErrDecisionDenied)
	assert.NotErrorIs(t, err, ErrUnauthorizedApprover)
}

// A nil guard is the historical posture: the vote is judged by the record's
// own checks alone, and no guard code participates.
func TestDecisionGuardNilKeepsHistoricalBehaviour(t *testing.T) {
	store := newMockStore()
	svc := NewService(store) // no guard installed
	id := seedPending(t, svc, "run-1", "alice")

	require.NoError(t, svc.Approve(context.Background(), id, "alice"))
	after, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, after.Status)
}

// An allowing guard lets the vote through, and the guard observed the vote
// it allowed: subject, action and the approval's own run id.
func TestDecisionGuardAllowedVoteRecordsWithGuardObservation(t *testing.T) {
	store := newMockStore()
	guard := &recordingGuard{}
	svc := NewService(store).WithDecisionGuard(guard)
	id := seedPending(t, svc, "run-42", "alice", "bob")

	require.NoError(t, svc.Approve(context.Background(), id, "alice"))

	calls := guard.recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, "alice", calls[0].subject)
	assert.Equal(t, ActionApprove, calls[0].action)
	assert.Equal(t, "run-42", calls[0].runID)
	after, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, after.Status)
}

// Reject passes through the same gate with its own action token, so a
// deployment can grant approve without granting reject (or vice versa).
func TestDecisionGuardCoversRejectWithItsOwnAction(t *testing.T) {
	store := newMockStore()
	guard := &recordingGuard{
		verdict: func(call guardCall) error {
			if call.action == ActionReject {
				return errors.New("veto right not granted to this team")
			}
			return nil
		},
	}
	svc := NewService(store).WithDecisionGuard(guard)
	id := seedPending(t, svc, "run-1", "alice")

	err := svc.Reject(context.Background(), id, "alice", "wrong window")
	require.ErrorIs(t, err, ErrDecisionDenied)
	after, getErr := store.Get(context.Background(), id)
	require.NoError(t, getErr)
	assert.Equal(t, StatusPending, after.Status, "a refused reject must not veto the run")

	// The same subject may still approve: the refusal was action-scoped.
	require.NoError(t, svc.Approve(context.Background(), id, "alice"))
	calls := guard.recorded()
	require.Len(t, calls, 2)
	assert.Equal(t, ActionReject, calls[0].action)
	assert.Equal(t, ActionApprove, calls[1].action)
}

// WithDecisionGuard(nil) removes a previously installed guard — the escape
// hatch callers need to build "guarded when a policy exists, unguarded
// otherwise" without two service constructors.
func TestWithDecisionGuardNilRemovesGuard(t *testing.T) {
	store := newMockStore()
	guard := &recordingGuard{
		verdict: func(call guardCall) error { return errors.New("always no") },
	}
	svc := NewService(store).WithDecisionGuard(guard)
	id := seedPending(t, svc, "run-1", "alice")

	require.ErrorIs(t, svc.Approve(context.Background(), id, "alice"), ErrDecisionDenied)

	svc.WithDecisionGuard(nil)
	require.NoError(t, svc.Approve(context.Background(), id, "alice"))
	assert.Len(t, guard.recorded(), 1, "the removed guard must not be consulted again")
}
