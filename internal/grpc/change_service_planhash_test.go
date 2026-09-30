// change_service_planhash_test.go pins D-1 v2: an approval must be bound to the
// exact plan version it ratified. Settlement filters by the run's current
// plan_hash (empty-hash rows are legacy and still settle), apply refuses a
// run whose approved approval matches a DIFFERENT plan, and re-planning a new
// plan resets a prior approval back to draft.
package grpc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
)

func TestSettleApproval_IgnoresApprovalForDifferentPlan(t *testing.T) {
	store := newTestStore(t)
	svc := NewChangeService(store, nil, approval.NewService(&kickoffStoreAdapter{store: store}), nil)

	run := &state.Run{ID: "ph-mismatch", WorkflowName: "wf", Status: "draft",
		Creator: "alice", CreatedAt: timeNowUTC(), PlanHash: "hash-v2"}
	require.NoError(t, store.CreateRun(context.Background(), run))

	// An approved row signed for the PREVIOUS plan version (hash-v1).
	require.NoError(t, store.CreateApproval(context.Background(), &state.Approval{
		ID: "ap-old", RunID: "ph-mismatch", Level: "high", Status: "approved",
		PlanHash: "hash-v1",
	}))

	settled, err := svc.SettleApproval(context.Background(), "ph-mismatch")
	require.NoError(t, err)
	assert.Equal(t, SettleNone, settled, "an approval for a different plan must not settle the run")
	got, err := store.GetRun(context.Background(), "ph-mismatch")
	require.NoError(t, err)
	assert.NotEqual(t, "approved", got.Status, "stale-plan approval must not approve the run")
}

func TestSettleApproval_LegacyEmptyHashRowStillSettles(t *testing.T) {
	store := newTestStore(t)
	svc := NewChangeService(store, nil, approval.NewService(&kickoffStoreAdapter{store: store}), nil)
	run := &state.Run{ID: "ph-legacy", WorkflowName: "wf", Status: "draft",
		Creator: "alice", CreatedAt: timeNowUTC(), PlanHash: "hash-v2"}
	require.NoError(t, store.CreateRun(context.Background(), run))

	// Upgrades must not break in-flight approval chains: rows written before
	// the plan_hash column are treated as legacy and still settle.
	require.NoError(t, store.CreateApproval(context.Background(), &state.Approval{
		ID: "ap-legacy", RunID: "ph-legacy", Level: "high", Status: "approved",
	}))

	settled, err := svc.SettleApproval(context.Background(), "ph-legacy")
	require.NoError(t, err)
	assert.Equal(t, SettleApproved, settled, "empty plan_hash is legacy and must still settle")
}

func TestApplyChange_RefusesApprovalForDifferentPlan(t *testing.T) {
	store := newTestStore(t)
	engine := &EngineAdapter{Run: func(context.Context, string, bool, int32) (string, bool, string, error) {
		return "exec-1", true, "completed", nil
	}}
	svc := NewChangeService(store, engine, approval.NewService(&kickoffStoreAdapter{store: store}), nil)

	run := &state.Run{ID: "ph-apply", WorkflowName: "wf", Status: "approved",
		Creator: "alice", CreatedAt: timeNowUTC()}
	require.NoError(t, store.CreateRun(context.Background(), run))

	// Persist plan version B (matching the run.PlanHash).
	p := testPlan()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	hashB := plan.ComputeHash(p)
	run.PlanJSON = string(raw)
	run.PlanHash = hashB
	require.NoError(t, store.UpdateRun(context.Background(), run))

	// The approved approval signed a DIFFERENT plan (A).
	require.NoError(t, store.CreateApproval(context.Background(), &state.Approval{
		ID: "ap-a", RunID: "ph-apply", Level: "high", Status: "approved",
		PlanHash: "hash-a",
	}))

	_, err = svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{ChangeId: "ph-apply"})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "approval", "apply must cite the plan-approval binding as the cause")
}

func TestPlanChange_ReplanResetsPriorApproval(t *testing.T) {
	store := newTestStore(t)
	engine := &EngineAdapter{
		Plan: func(_ context.Context, _ string, _ []string) (*pb.Plan, *StoredPlan, error) {
			// The replan produces a DIFFERENT (newer) plan version B.
			p2 := testPlan()
			p2.ID = "plan-test-2"
			p2.Batches[0].Steps[0].Name = "reconfigure"
			raw, err := json.Marshal(p2)
			if err != nil {
				return nil, nil, err
			}
			return &pb.Plan{ChangeId: "ph-replan"}, &StoredPlan{JSON: string(raw), Hash: plan.ComputeHash(p2)}, nil
		},
	}
	svc := NewChangeService(store, engine, approval.NewService(&kickoffStoreAdapter{store: store}), nil)

	// Run was approved under an earlier plan.
	run := &state.Run{ID: "ph-replan", WorkflowName: "wf", Status: "approved",
		ApprovalStatus: "approved", ApprovalLevel: "high",
		Creator: "alice", CreatedAt: timeNowUTC(), PlanHash: "old-hash"}
	require.NoError(t, store.CreateRun(context.Background(), run))

	_, err := svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId:    "ph-replan",
		TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)

	got, err := store.GetRun(context.Background(), "ph-replan")
	require.NoError(t, err)
	assert.Equal(t, "draft", got.Status, "a new plan invalidates the prior approval")
	assert.Equal(t, "pending", got.ApprovalStatus)
	assert.NotEqual(t, "old-hash", got.PlanHash)
}
