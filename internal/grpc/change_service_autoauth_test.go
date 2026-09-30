// change_service_autoauth_test.go pins the spec contract that closes the
// client-controlled auto-approve bypass.
//
// leveelang-spec.md §8.1 writes auto_approve as "强制 false" for the high
// and emergency tiers — the approval chain is meant to be unavoidable on
// the most dangerous changes. But autoApprove reaches ApplyChange and
// RollbackChange as a plain request field (proto
// ApplyChangeRequest.auto_approve), and before risk.AutoApproveForbidden
// was wired in, setting it true skipped BOTH the status guard and the
// plan/approval binding gate: a complete bypass of the approval chain,
// reachable by any caller holding a valid token.
//
// These cases pin both halves of the contract — the bypass is refused
// where the spec forbids it, and preserved where the spec allows it (the
// documented CI / bootstrapping path that
// TestApplyChange_AutoApproveBypassesPlanApprovalGate protects).

package grpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/risk"
	"github.com/nexus/levee/internal/state"
)

// TestApplyChange_HighRiskRejectsAutoApprove covers the spec's "强制
// false" tiers. The engine must never be reached: refusing early is the
// whole point (a status-only refusal that still dispatched would be worse
// than useless on a destructive change).
func TestApplyChange_HighRiskRejectsAutoApprove(t *testing.T) {
	for _, level := range []string{risk.LevelHigh, risk.LevelEmergency} {
		t.Run(level, func(t *testing.T) {
			store := newTestStore(t)
			rec := &recordingEngine{runID: "exec-x", runSuccess: true, runPhase: "completed"}
			svc := NewChangeService(store, rec.adapter(), nil, nil)

			run := &state.Run{
				ID: "run-" + level, WorkflowName: "wf", Status: "approved",
				Creator: "alice", CreatedAt: timeNowUTC(),
				PlanHash: "hash-v2", ApprovalLevel: level,
			}
			require.NoError(t, store.CreateRun(context.Background(), run))
			persistPlanOnRun(t, store, run.ID)

			_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
				ChangeId: run.ID, AutoApprove: true,
			})
			require.Error(t, err, "%s change must not be auto-approvable", level)
			assert.Equal(t, codes.PermissionDenied, status.Code(err))
			assert.Equal(t, int32(0), rec.runCalled, "the engine must never be reached")
		})
	}
}

// TestApplyChange_StandardRiskKeepsAutoApprove guards against
// over-correction: standard-risk auto-approval is the documented
// CI / bootstrapping path and must keep working.
func TestApplyChange_StandardRiskKeepsAutoApprove(t *testing.T) {
	store := newTestStore(t)
	rec := &recordingEngine{runID: "exec-std", runSuccess: true, runPhase: "completed"}
	svc := NewChangeService(store, rec.adapter(), nil, nil)

	run := &state.Run{
		ID: "run-std", WorkflowName: "wf", Status: "draft",
		Creator: "alice", CreatedAt: timeNowUTC(),
		PlanHash: "hash-v2", ApprovalLevel: risk.LevelStandard,
	}
	require.NoError(t, store.CreateRun(context.Background(), run))
	persistPlanOnRun(t, store, "run-std")

	_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId: "run-std", AutoApprove: true,
	})
	require.NoError(t, err, "standard-risk autoApprove must keep working")
	assert.Equal(t, int32(1), rec.runCalled)
}

// TestApplyChange_UnscoredRunKeepsAutoApprove pins the empty-level
// decision. The spec only forces auto_approve=false for high/emergency;
// a run that predates risk scoring carries no level, and blocking it
// would break the CI path without buying any safety (reaching this point
// still requires a persisted plan).
func TestApplyChange_UnscoredRunKeepsAutoApprove(t *testing.T) {
	store := newTestStore(t)
	rec := &recordingEngine{runID: "exec-unscored", runSuccess: true, runPhase: "completed"}
	svc := NewChangeService(store, rec.adapter(), nil, nil)

	run := &state.Run{
		ID: "run-unscored", WorkflowName: "wf", Status: "draft",
		Creator: "alice", CreatedAt: timeNowUTC(), PlanHash: "hash-v2",
	}
	require.NoError(t, store.CreateRun(context.Background(), run))
	persistPlanOnRun(t, store, "run-unscored")

	_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId: "run-unscored", AutoApprove: true,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), rec.runCalled)
}

// TestRollbackChange_HighRiskRejectsAutoApprove mirrors the contract on
// the rollback path: rolling back is itself a destructive production
// action, so the same tier rule applies.
func TestRollbackChange_HighRiskRejectsAutoApprove(t *testing.T) {
	store := newTestStore(t)
	svc := NewChangeService(store, nil, nil, nil)

	run := &state.Run{
		ID: "run-rb", WorkflowName: "wf", Status: "rolled_back_partial",
		Creator: "alice", CreatedAt: timeNowUTC(), ApprovalLevel: risk.LevelHigh,
	}
	require.NoError(t, store.CreateRun(context.Background(), run))

	_, err := svc.RollbackChange(context.Background(), &pb.RollbackRequest{
		ChangeId: "run-rb", AutoApprove: true,
	})
	require.Error(t, err, "high-risk rollback must not be auto-approvable")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

// TestApplyChange_ForeignVocabularyRejectsAutoApprove pins the
// fail-closed reading of an unrecognised level.
//
// The cases below split into two groups, because "foreign vocabulary" is
// not one thing:
//
//   - "HIGH", "Emergency", "critical" — spellings of a tier that are
//     NOT the exact constant, plus a word from no vocabulary at all.
//     These must fail closed: a level we cannot classify is not a licence
//     to skip the approval chain.
//   - "urgent", "normal", "low" — real PRIORITY values, not tiers. These
//     must stay allowed: "normal" is the default CreateChange writes for
//     every ordinary change, so forbidding it would block routine
//     auto-approval outright (the regression this test used to lock in).
//
// Case variants are covered by the first group: the tiers are matched
// exactly, never case-insensitively.
func TestApplyChange_ForeignVocabularyRejectsAutoApprove(t *testing.T) {
	for _, level := range []string{"HIGH", "Emergency", "critical"} {
		t.Run(level, func(t *testing.T) {
			store := newTestStore(t)
			rec := &recordingEngine{runID: "exec-fv", runSuccess: true, runPhase: "completed"}
			svc := NewChangeService(store, rec.adapter(), nil, nil)

			run := &state.Run{
				ID: "run-fv", WorkflowName: "wf", Status: "approved",
				Creator: "alice", CreatedAt: timeNowUTC(),
				PlanHash: "hash-v2", ApprovalLevel: level,
			}
			require.NoError(t, store.CreateRun(context.Background(), run))
			persistPlanOnRun(t, store, "run-fv")

			_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
				ChangeId: "run-fv", AutoApprove: true,
			})
			require.Error(t, err, "unrecognised level %q must fail closed", level)
			assert.Equal(t, codes.PermissionDenied, status.Code(err))
			assert.Equal(t, int32(0), rec.runCalled, "the engine must never be reached")
		})
	}
}

// TestCreateChange_CreatorPrefersVerifiedSubject pins the fix for a real
// governance bypass: run.Creator feeds the approval chain's Initiator
// (kickoffApproval → approval.CreateRequest.Initiator), and Initiator
// drives exclude_initiator — the rule that stops an author from approving
// their own high-tier change.
//
// Sourcing Creator from the client-supplied actor label therefore let a
// caller whose identity IS in the approver set name someone else in the
// "x-actor" header, and then legitimately cast a vote that the
// independence gate should have refused.
//
// The contexts here are built with raw key injection rather than
// ContextWithActor, because that helper now sets BOTH keys (it delegates
// to ContextWithSubject) and so cannot express "actor label present,
// verified subject absent".
func TestCreateChange_CreatorPrefersVerifiedSubject(t *testing.T) {
	t.Run("verified subject wins over the actor label", func(t *testing.T) {
		store := newTestStore(t)
		svc := NewChangeService(store, nil, nil, nil)

		ctx := context.WithValue(context.Background(), actorKey{}, "charlie")
		ctx = context.WithValue(ctx, subjectKey{}, "alice")

		ch, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{WorkflowFile: "wf"})
		require.NoError(t, err)

		run, err := store.GetRun(context.Background(), ch.GetId())
		require.NoError(t, err)
		require.NotNil(t, run)
		assert.Equal(t, "alice", run.Creator,
			"Creator must be the verified subject: it becomes the approval Initiator")
	})

	t.Run("no subject falls back to the actor label", func(t *testing.T) {
		store := newTestStore(t)
		svc := NewChangeService(store, nil, nil, nil)

		ctx := context.WithValue(context.Background(), actorKey{}, "charlie")

		ch, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{WorkflowFile: "wf"})
		require.NoError(t, err)

		run, err := store.GetRun(context.Background(), ch.GetId())
		require.NoError(t, err)
		require.NotNil(t, run)
		assert.Equal(t, "charlie", run.Creator,
			"an unverifiable credential has no subject to prefer, so the label stands")
	})
}

// TestApplyChange_PriorityVocabularyKeepsAutoApprove is the counterpart to
// the foreign-vocabulary test: it pins that a run whose ApprovalLevel
// holds a PRIORITY value is still auto-approvable when its tier is not
// high/emergency.
//
// "normal" is not an edge case — it is what CreateChange writes for every
// change that does not name a priority, and kickoffApproval only replaces
// it once an approval service is wired AND a plan exists. Treating it as
// forbidden (a single "anything != standard is refused" rule) made every
// ordinary auto-approved change impossible to apply or roll back. The
// engine must therefore be REACHED for these levels.
func TestApplyChange_PriorityVocabularyKeepsAutoApprove(t *testing.T) {
	for _, level := range []string{"normal", "low"} {
		t.Run(level, func(t *testing.T) {
			store := newTestStore(t)
			rec := &recordingEngine{runID: "exec-pv", runSuccess: true, runPhase: "completed"}
			svc := NewChangeService(store, rec.adapter(), nil, nil)

			run := &state.Run{
				ID: "run-pv", WorkflowName: "wf", Status: "approved",
				Creator: "alice", CreatedAt: timeNowUTC(),
				PlanHash: "hash-v2", ApprovalLevel: level,
			}
			require.NoError(t, store.CreateRun(context.Background(), run))
			persistPlanOnRun(t, store, "run-pv")

			_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
				ChangeId: "run-pv", AutoApprove: true,
			})
			require.NoError(t, err, "priority level %q is not a risk tier and must remain auto-approvable", level)
			assert.Equal(t, int32(1), rec.runCalled, "the engine must be reached")
		})
	}
}
