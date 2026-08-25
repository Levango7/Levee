// plan_freeze_test.go — pins the first frozen-target enforcement point:
// PlanChange rejects plans targeting hosts that are frozen in the
// inventory, before any engine delegation happens.
package grpc

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanChangeRejectsFrozenHosts(t *testing.T) {
	store := newTestStore(t)
	svc := NewChangeService(store, nil, nil, nil)
	ctx := context.Background()

	run := &state.Run{ID: "run-frz", WorkflowName: "wf", Status: "approved"}
	require.NoError(t, store.CreateRun(ctx, run))

	require.NoError(t, store.UpsertTarget(ctx, &state.Target{
		ID: "tgt-f", Hostname: "frozen-host", Port: 22, Status: state.StatusFrozen,
	}))
	require.NoError(t, store.UpsertTarget(ctx, &state.Target{
		ID: "tgt-a", Hostname: "active-host", Port: 22, Status: state.StatusActive,
	}))

	// A plan naming a frozen host is rejected with FailedPrecondition.
	_, err := svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId:    "run-frz",
		TargetHosts: []string{"active-host", "frozen-host"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "frozen targets cannot receive changes")
	assert.Contains(t, err.Error(), "frozen-host")

	// Plans touching only active/unknown hosts pass.
	ok, err := svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId:    "run-frz",
		TargetHosts: []string{"active-host", "not-in-inventory"},
	})
	require.NoError(t, err)
	require.NotNil(t, ok)

	// Empty target list passes (nothing to guard).
	empty, err := svc.PlanChange(ctx, &pb.PlanChangeRequest{ChangeId: "run-frz"})
	require.NoError(t, err)
	require.NotNil(t, empty)
}
