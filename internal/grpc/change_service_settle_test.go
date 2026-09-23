// change_service_settle_test.go pins the approval settlement contract:
// the quorum gate (a partial 1/N approve must NOT approve the run), the
// one-vote veto mirroring, terminal-run protection (a late settlement
// never resurrects or demotes a settled run), and the five decision
// surfaces all converging on SettleApproval.

package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// timeNowUTC / timeNowPlusHour keep the run seeding below self-contained.
func timeNowUTC() time.Time      { return time.Now().UTC() }
func timeNowPlusHour() time.Time { return time.Now().UTC().Add(time.Hour) }

func TestApproveChange_PartialQuorumLeavesRunDraft(t *testing.T) {
	store := newTestStore(t)
	svc := NewChangeService(store, nil, approval.NewService(&kickoffStoreAdapter{store: store}), nil)

	// Draft run with a persisted plan (the approve gate requires one
	// only when an engine is wired; here engine=nil so the legacy path
	// applies — but settlement must still work).
	run := &state.Run{ID: "run-q", WorkflowName: "wf", Status: "draft", Creator: "alice", CreatedAt: timeNowUTC()}
	require.NoError(t, store.CreateRun(context.Background(), run))

	// Approval with quorum 2 over alice+bob.
	ap, err := approval.NewService(&kickoffStoreAdapter{store: store}).Create(context.Background(),
		approval.CreateRequest{
			RunID: "run-q", Level: approval.LevelStandard,
			Approvers: []string{"alice", "bob"}, MinApprovers: 2,
			ExpiresAt: timeNowPlusHour(),
		})
	require.NoError(t, err)
	require.NotEmpty(t, ap.ID)

	// First vote: the decision is recorded, the run stays draft.
	_, err = svc.ApproveChange(context.Background(), &pb.ApproveRequest{ChangeId: "run-q", Approver: "alice"})
	require.NoError(t, err)
	got, err := store.GetRun(context.Background(), "run-q")
	require.NoError(t, err)
	assert.Equal(t, "draft", got.Status, "1/2 votes must not approve the run")
	assert.NotEqual(t, "approved", got.ApprovalStatus)

	// Second vote: the quorum completes, the run settles.
	_, err = svc.ApproveChange(context.Background(), &pb.ApproveRequest{ChangeId: "run-q", Approver: "bob"})
	require.NoError(t, err)
	got, err = store.GetRun(context.Background(), "run-q")
	require.NoError(t, err)
	assert.Equal(t, "approved", got.Status, "2/2 votes must approve the run")
	assert.Equal(t, "approved", got.ApprovalStatus)
}

func TestApproveChange_RejectMirrorsImmediately(t *testing.T) {
	store := newTestStore(t)
	svc := NewChangeService(store, nil, approval.NewService(&kickoffStoreAdapter{store: store}), nil)
	run := &state.Run{ID: "run-v", WorkflowName: "wf", Status: "draft", Creator: "alice", CreatedAt: timeNowUTC()}
	require.NoError(t, store.CreateRun(context.Background(), run))

	ap, err := approval.NewService(&kickoffStoreAdapter{store: store}).Create(context.Background(),
		approval.CreateRequest{
			RunID: "run-v", Level: approval.LevelStandard,
			Approvers: []string{"alice", "bob"}, MinApprovers: 2,
			ExpiresAt: timeNowPlusHour(),
		})
	require.NoError(t, err)
	require.NotEmpty(t, ap.ID)

	// One-vote veto: even mid-quorum a single reject settles the run.
	_, err = svc.RejectChange(context.Background(), &pb.RejectRequest{ChangeId: "run-v", Rejecter: "bob", Reason: "bad timing"})
	require.NoError(t, err)
	got, err := store.GetRun(context.Background(), "run-v")
	require.NoError(t, err)
	assert.Equal(t, "rejected", got.Status)
	assert.Equal(t, "rejected", got.ApprovalStatus)
}

func TestSettleApproval_TerminalRunNotDemoted(t *testing.T) {
	// A run that already completed must not be resurrected or demoted
	// by a late-arriving approval settlement.
	store := newTestStore(t)
	svc := NewChangeService(store, nil, approval.NewService(&kickoffStoreAdapter{store: store}), nil)
	run := &state.Run{ID: "run-t", WorkflowName: "wf", Status: "completed", Creator: "alice", CreatedAt: timeNowUTC()}
	require.NoError(t, store.CreateRun(context.Background(), run))

	ap, err := approval.NewService(&kickoffStoreAdapter{store: store}).Create(context.Background(),
		approval.CreateRequest{
			RunID: "run-t", Level: approval.LevelStandard,
			Approvers: []string{"alice"}, MinApprovers: 1,
			ExpiresAt: timeNowPlusHour(),
		})
	require.NoError(t, err)
	require.NotEmpty(t, ap.ID)
	require.NoError(t, approval.NewService(&kickoffStoreAdapter{store: store}).Approve(context.Background(), ap.ID, "alice"))

	_, err = svc.SettleApproval(context.Background(), "run-t")
	require.NoError(t, err)
	got, err := store.GetRun(context.Background(), "run-t")
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status, "terminal status must survive late settlement")
}

func TestSettleApproval_NoApprovalRowsIsNone(t *testing.T) {
	// Pre-kickoff compatibility: runs without approval rows settle to
	// SettleNone and the run is untouched (legacy direct-write callers
	// keep working).
	store := newTestStore(t)
	svc := NewChangeService(store, nil, nil, nil)
	run := &state.Run{ID: "run-none", WorkflowName: "wf", Status: "draft", Creator: "alice", CreatedAt: timeNowUTC()}
	require.NoError(t, store.CreateRun(context.Background(), run))

	settled, err := svc.SettleApproval(context.Background(), "run-none")
	require.NoError(t, err)
	assert.Equal(t, SettleNone, settled)
	got, err := store.GetRun(context.Background(), "run-none")
	require.NoError(t, err)
	assert.Equal(t, "draft", got.Status)
}

func TestSettleApproval_Idempotent(t *testing.T) {
	// Settling twice is safe: the second call sees the already-settled
	// state and does nothing.
	store := newTestStore(t)
	svc := NewChangeService(store, nil, approval.NewService(&kickoffStoreAdapter{store: store}), nil)
	run := &state.Run{ID: "run-idem", WorkflowName: "wf", Status: "draft", Creator: "alice", CreatedAt: timeNowUTC()}
	require.NoError(t, store.CreateRun(context.Background(), run))
	ap, err := approval.NewService(&kickoffStoreAdapter{store: store}).Create(context.Background(),
		approval.CreateRequest{
			RunID: "run-idem", Level: approval.LevelStandard,
			Approvers: []string{"alice"}, MinApprovers: 1,
			ExpiresAt: timeNowPlusHour(),
		})
	require.NoError(t, err)
	require.NoError(t, approval.NewService(&kickoffStoreAdapter{store: store}).Approve(context.Background(), ap.ID, "alice"))

	first, err := svc.SettleApproval(context.Background(), "run-idem")
	require.NoError(t, err)
	assert.Equal(t, SettleApproved, first)
	second, err := svc.SettleApproval(context.Background(), "run-idem")
	require.NoError(t, err)
	assert.Equal(t, SettleApproved, second, "second settlement sees the approved row and is idempotent")
	got, _ := store.GetRun(context.Background(), "run-idem")
	assert.Equal(t, "approved", got.Status)
}

func TestSettleApproval_MismatchedPlanHashIgnored(t *testing.T) {
	// D-1 v2: an approval attests to ONE plan revision. An approved row for
	// an OLD plan must not settle a run whose plan has since changed.
	store := newTestStore(t)
	svc := NewChangeService(store, nil, approval.NewService(&kickoffStoreAdapter{store: store}), nil)

	run := &state.Run{ID: "run-h1", WorkflowName: "wf", Status: "draft",
		Creator: "alice", CreatedAt: timeNowUTC(), PlanHash: "hash-v2"}
	require.NoError(t, store.CreateRun(context.Background(), run))

	// An approved approval row for the OLD plan (hash-v1), plus a pending row
	// for the current plan (hash-v2).
	require.NoError(t, store.CreateApproval(context.Background(), &state.Approval{
		ID: "ap-h1", RunID: "run-h1", Level: "high", Status: "approved", PlanHash: "hash-v1",
	}))
	require.NoError(t, store.CreateApproval(context.Background(), &state.Approval{
		ID: "ap-h2", RunID: "run-h1", Level: "high", Status: "pending", PlanHash: "hash-v2",
	}))

	// The old approved row is not consulted; the run stays draft.
	settled, err := svc.SettleApproval(context.Background(), "run-h1")
	require.NoError(t, err)
	assert.Equal(t, SettlePending, settled, "current-plan row is pending, so the run must not move")
	got, err := store.GetRun(context.Background(), "run-h1")
	require.NoError(t, err)
	assert.Equal(t, "draft", got.Status, "a stale-plan approval must not approve the run")
}

func TestSettleApproval_LegacyEmptyHashStillSettles(t *testing.T) {
	// Pre-migration (empty plan_hash) approvals are honoured even on an
	// engine-born run: compatibility with databases created before D-1 v2.
	store := newTestStore(t)
	svc := NewChangeService(store, nil, approval.NewService(&kickoffStoreAdapter{store: store}), nil)

	run := &state.Run{ID: "run-lg", WorkflowName: "wf", Status: "draft",
		Creator: "alice", CreatedAt: timeNowUTC(), PlanHash: "hash-v2"}
	require.NoError(t, store.CreateRun(context.Background(), run))
	require.NoError(t, store.CreateApproval(context.Background(), &state.Approval{
		ID: "ap-lg", RunID: "run-lg", Level: "high", Status: "approved", PlanHash: "",
	}))

	settled, err := svc.SettleApproval(context.Background(), "run-lg")
	require.NoError(t, err)
	assert.Equal(t, SettleApproved, settled, "legacy (empty plan_hash) approval still settles")
	got, err := store.GetRun(context.Background(), "run-lg")
	require.NoError(t, err)
	assert.Equal(t, "approved", got.Status)
}

func TestApplyChange_ApprovalNotMatchingPlanRejected(t *testing.T) {
	// D-1 v2 apply gate: a run whose only approved approval attests to a
	// different plan revision must not be applied — even though status is
	// 'approved'. This closes the approve-v1 / re-plan-to-v2 / apply-v2 drift.
	store := newTestStore(t)
	rec := &recordingEngine{runID: "exec-1", runSuccess: true, runPhase: "completed"}
	svc := NewChangeService(store, rec.adapter(), approval.NewService(&kickoffStoreAdapter{store: store}), nil)

	run := &state.Run{ID: "run-ag", WorkflowName: "wf", Status: "draft",
		Creator: "alice", CreatedAt: timeNowUTC(), PlanHash: "hash-v2"}
	require.NoError(t, store.CreateRun(context.Background(), run))
	persistPlanOnRun(t, store, "run-ag")
	setRunStatus(t, store, "run-ag", "approved")

	// Approved row for the OLD plan only.
	require.NoError(t, store.CreateApproval(context.Background(), &state.Approval{
		ID: "ap-ag", RunID: "run-ag", Level: "high", Status: "approved", PlanHash: "hash-v1",
	}))

	_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{ChangeId: "run-ag"})
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition.String(), status.Code(err).String(), "apply must refuse a stale-plan approval")
	assert.Equal(t, int32(0), rec.runCalled, "engine must not run for a stale-plan approval")
}

func TestApplyChange_AutoApproveBypassesPlanApprovalGate(t *testing.T) {
	// The plan-approval binding gate is a review gate, not a hard invariant:
	// autoApprove is the documented explicit bypass (CI/bootstrapping paths).
	// Pin that it still works, so tightening the gate never breaks it.
	store := newTestStore(t)
	rec := &recordingEngine{runID: "exec-auto", runSuccess: true, runPhase: "completed"}
	svc := NewChangeService(store, rec.adapter(), approval.NewService(&kickoffStoreAdapter{store: store}), nil)

	run := &state.Run{ID: "run-auto", WorkflowName: "wf", Status: "draft",
		Creator: "alice", CreatedAt: timeNowUTC(), PlanHash: "hash-v2"}
	require.NoError(t, store.CreateRun(context.Background(), run))
	persistPlanOnRun(t, store, "run-auto")
	// No approval row at all: only autoApprove can let this through.
	require.NoError(t, store.CreateTrace(context.Background(), &state.Trace{
		ID: "trc-auto", RunID: "run-auto", Event: "seed", Actor: "alice", Timestamp: timeNowUTC(),
	}))

	_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId:    "run-auto",
		AutoApprove: true,
	})
	require.NoError(t, err, "autoApprove explicitly bypasses the approval gate")
	assert.Equal(t, int32(1), rec.runCalled)
}

func TestApplyChange_LegacyApprovalStillAuthorises(t *testing.T) {
	// Pre-migration (empty plan_hash) approved rows keep authorising apply.
	store := newTestStore(t)
	rec := &recordingEngine{runID: "exec-2", runSuccess: true, runPhase: "completed"}
	svc := NewChangeService(store, rec.adapter(), approval.NewService(&kickoffStoreAdapter{store: store}), nil)

	run := &state.Run{ID: "run-lex", WorkflowName: "wf", Status: "draft",
		Creator: "alice", CreatedAt: timeNowUTC(), PlanHash: "hash-v2"}
	require.NoError(t, store.CreateRun(context.Background(), run))
	persistPlanOnRun(t, store, "run-lex")
	setRunStatus(t, store, "run-lex", "approved")
	require.NoError(t, store.CreateApproval(context.Background(), &state.Approval{
		ID: "ap-lex", RunID: "run-lex", Level: "high", Status: "approved", PlanHash: "",
	}))

	_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{ChangeId: "run-lex"})
	require.NoError(t, err, "legacy approval authorises apply")
	assert.Equal(t, int32(1), rec.runCalled, "engine must run under a legacy approval")
}
