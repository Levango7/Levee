package grpc

// change_service_getchange_plan_test.go pins the include_plan behaviour that
// closed ledger #18's "annotation promised it but the response type couldn't
// carry it" gap: pb.Change gained a Plan field, and GetChange now fills it
// from the run's stored canonical plan (run.plan_json).
//
// Four properties worth pinning, because each is a live risk:
//   - include_plan=true actually returns the plan derived from the STORED
//     artifact (not a re-generation and not a shape the change_service
//     invented).
//   - include_plan=false must NOT leak the plan. The wire default is absent;
//     making the opt-in the ONLY way a plan appears is what lets us widen the
//     response type without silently widening every existing caller's payload
//     or the ZCode wire snapshot.
//   - a run with no stored plan under include_plan=true degrades to nil,
//     without erroring. The change itself is still readable — "no plan yet"
//     is a real state for a draft, not a failure.
//   - a corrupt stored plan (unparsable JSON) also degrades to nil, not a
//     gRPC error. GetChange is a read; inventing an error here would let a
//     single malformed artifact hide the entire change, which is a worse
//     operator answer than "the change is here, its plan artifact is unreadable".

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

func TestGetChange_IncludePlan(t *testing.T) {
	ctx := context.Background()

	// seed a run; return its id so each subtest starts identically.
	seed := func(t *testing.T) (*ChangeService, state.Store, string) {
		t.Helper()
		svc, store := newTestChangeService(t)
		id := "run-plan-test"
		require.NoError(t, store.CreateRun(ctx, &state.Run{
			ID: id, WorkflowName: "wf", Status: "draft", Creator: "test",
		}))
		return svc, store, id
	}

	t.Run("include_plan=true returns the stored plan", func(t *testing.T) {
		svc, store, id := seed(t)
		p := persistPlanOnRun(t, store, id)
		resp, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: id, IncludePlan: true})
		require.NoError(t, err)
		require.NotNil(t, resp.GetPlan(), "include_plan=true on a planned run must carry the plan")
		assert.Equal(t, id, resp.GetPlan().GetChangeId())
		assert.Len(t, resp.GetPlan().GetBatches(), len(p.Batches))
		// One host per plan target, deduped in plan order.
		assert.Equal(t, []string{"web-1"}, resp.GetPlan().GetTargetHosts())
	})

	t.Run("include_plan=false does not leak the plan", func(t *testing.T) {
		svc, store, id := seed(t)
		persistPlanOnRun(t, store, id)
		// A stored plan exists; without the flag it MUST NOT appear. This is
		// the shape-stability property the wire-snapshot guard relies on: every
		// existing caller keeps the same Change key set they had before the
		// field existed.
		resp, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: id})
		require.NoError(t, err)
		assert.Nil(t, resp.GetPlan(), "the plan must only appear when include_plan is explicitly true")
	})

	t.Run("include_plan=true with no stored plan degrades to nil", func(t *testing.T) {
		svc, _, id := seed(t)
		resp, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: id, IncludePlan: true})
		require.NoError(t, err, "a draft with no plan yet is a readable change")
		assert.Nil(t, resp.GetPlan(), "no stored artifact means no plan attached — not an empty-plan lie")
	})

	t.Run("include_plan=true with a corrupt artifact degrades to nil", func(t *testing.T) {
		svc, store, id := seed(t)
		run, err := store.GetRun(ctx, id)
		require.NoError(t, err)
		run.PlanJSON = "{ not valid json"
		require.NoError(t, store.UpdateRun(ctx, run))
		resp, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: id, IncludePlan: true})
		require.NoError(t, err, "an unreadable plan must not hide the change itself")
		assert.Nil(t, resp.GetPlan(), "unreadable artifact degrades to no-plan, never a fabricated shape")
	})
}
