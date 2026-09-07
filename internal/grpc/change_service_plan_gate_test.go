// change_service_plan_gate_test.go covers the A1 plan-persistence gates:
// PlanChange persists the canonical plan artifact on the run, and
// Approve/Apply refuse runs whose plan is missing, drifted or corrupt —
// so an executed change is always exactly the change that was planned.
package grpc

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func storedTestPlan(t *testing.T) *StoredPlan {
	t.Helper()
	p := testPlan()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	return &StoredPlan{JSON: string(raw), Hash: plan.ComputeHash(p)}
}

func TestPlanChange_PersistsPlanArtifact(t *testing.T) {
	sp := storedTestPlan(t)
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x", Batches: []*pb.Batch{}}, stored: sp}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "plan-persist"})
	require.NoError(t, err)

	got, err := svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId:    created.GetId(),
		TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)
	require.NotNil(t, got)

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, sp.JSON, run.PlanJSON, "canonical plan JSON must be persisted on the run")
	assert.Equal(t, sp.Hash, run.PlanHash, "plan hash must be persisted on the run")
}

func TestPlanChange_DryRunPersistsNothing(t *testing.T) {
	sp := storedTestPlan(t)
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "plan-dry"})
	require.NoError(t, err)

	_, err = svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId:    created.GetId(),
		TargetHosts: []string{"web-1"},
		DryRun:      true,
	})
	require.NoError(t, err)

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "", run.PlanJSON, "dry-run plans are previews and must not be persisted")
	assert.Equal(t, "", run.PlanHash)
}

func TestApplyChange_RefusesMissingPlan(t *testing.T) {
	engine := &recordingEngine{runID: "exec-x", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-noplan"})
	require.NoError(t, err)

	_, err = svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId:    created.GetId(),
		AutoApprove: true,
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "no persisted plan")

	assert.Zero(t, atomic.LoadInt32(&engine.runCalled), "engine must never be invoked for a planless run")
	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "draft", run.Status, "refusal must not consume the run")
}

func TestApplyChange_RefusesDriftedPlan(t *testing.T) {
	engine := &recordingEngine{runID: "exec-x", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-drift"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())

	// Simulate plan drift: the stored JSON changed after the hash was
	// stamped (re-plan without re-hash, or row corruption).
	ctx := context.Background()
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	var p plan.Plan
	require.NoError(t, json.Unmarshal([]byte(run.PlanJSON), &p))
	p.Batches[0].Targets = []string{"evil-1"}
	tampered, err := json.Marshal(&p)
	require.NoError(t, err)
	run.PlanJSON = string(tampered)
	require.NoError(t, store.UpdateRun(ctx, run))

	_, err = svc.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:    created.GetId(),
		AutoApprove: true,
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "does not match its plan hash")

	assert.Zero(t, atomic.LoadInt32(&engine.runCalled), "engine must never execute a drifted plan")
	run, err = store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "draft", run.Status, "drift refusal must not consume the run")
}

func TestApplyChange_RefusesCorruptPlanJSON(t *testing.T) {
	engine := &recordingEngine{runID: "exec-x", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-corrupt"})
	require.NoError(t, err)

	ctx := context.Background()
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	run.PlanJSON = "{not valid json"
	run.PlanHash = "deadbeef"
	require.NoError(t, store.UpdateRun(ctx, run))

	_, err = svc.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:    created.GetId(),
		AutoApprove: true,
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "corrupt")
}

func TestApproveChange_RefusesMissingPlanWithEngine(t *testing.T) {
	engine := &recordingEngine{runSuccess: true}
	svc, _ := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "approve-noplan"})
	require.NoError(t, err)

	_, err = svc.ApproveChange(context.Background(), &pb.ApproveRequest{
		ChangeId: created.GetId(),
		Approver: "alice",
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "no persisted plan to approve")
}

func TestApproveChange_LegacyFlowUnchangedWithoutEngine(t *testing.T) {
	// No engine wired (status-only deployment): approval keeps working
	// for plan-less runs exactly as before A1.
	svc, store := newTestChangeService(t)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "approve-legacy"})
	require.NoError(t, err)

	_, err = svc.ApproveChange(context.Background(), &pb.ApproveRequest{
		ChangeId: created.GetId(),
		Approver: "alice",
	})
	require.NoError(t, err)

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "approved", run.Status)
}
