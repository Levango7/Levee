// errors_test.go pins the refusal vocabulary of the wiring layer.
//
// Each refusal must satisfy BOTH readers at once:
//
//   - errors.Is finds the sentinel, so a caller that imports wiring (the CLI,
//     the tests) can branch on the REASON;
//   - status.FromError finds the gRPC CODE, so the gateway's writeGRPCError
//     answers 400 (malformed request) or 412 (state refusal) instead of 500.
//
// A sentinel without a code is a 500 with a nicer sentence; a code without a
// sentinel is a status nobody can explain. Both are pinned here.
package wiring

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
)

// codeOf reads the gRPC code an error carries. It fails the test when the
// error carries none — an unclassified refusal is exactly the bug this file
// exists to prevent.
func codeOf(t *testing.T, err error) codes.Code {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "the refusal must carry a gRPC status: %v", err)
	return st.Code()
}

func TestPlanRefusalsCarrySentinelAndCode(t *testing.T) {
	ctx := context.Background()
	e, store := newTestEngine(t, "web-1")
	require.NoError(t, store.UpsertTarget(ctx, &state.Target{
		ID: "tgt-retired", Hostname: "gone-1", ChannelType: "ssh",
		Status: "retired", CreatedAt: time.Now().UTC(),
	}))
	seedRun(t, store, "run-err-1", testWorkflowYAML)

	// No targets at all: the request is malformed. 400, not 500.
	_, _, err := e.GeneratePlan(ctx, "run-err-1", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTargetHostsRequired)
	assert.Equal(t, codes.InvalidArgument, codeOf(t, err))

	// A host that is not in the inventory: the request names something that
	// cannot exist. 400.
	_, _, err = e.GeneratePlan(ctx, "run-err-1", []string{"nope-1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTargetsNotFound)
	assert.Equal(t, codes.InvalidArgument, codeOf(t, err))

	// A retired host: the request is well-formed but the STATE forbids it.
	// 412 — the same request against an active host would plan.
	_, _, err = e.GeneratePlan(ctx, "run-err-1", []string{"gone-1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTargetsRetired)
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
}

// TestPlanRefusalMessagesStayReadable pins the user-facing text: the sentinel
// text IS what the operator reads, so a reworded sentinel must not silently
// change what the console shows.
func TestPlanRefusalMessagesStayReadable(t *testing.T) {
	ctx := context.Background()
	e, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-err-msg", testWorkflowYAML)

	_, _, err := e.GeneratePlan(ctx, "run-err-msg", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target_hosts is required")

	_, _, err = e.GeneratePlan(ctx, "run-err-msg", []string{"nope-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in inventory")
	assert.Contains(t, err.Error(), "nope-1")
}

func TestLoadStoredPlanRefusalsCarrySentinelAndCode(t *testing.T) {
	ctx := context.Background()
	e, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-err-2", testWorkflowYAML)

	// No plan artifact: the operator can fix it by planning (and
	// re-approving). 412.
	_, err := e.loadStoredPlan(ctx, "run-err-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoPersistedPlan)
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))

	// Drift between the artifact and the hash on the run: also a state the
	// operator can fix (re-plan). 412.
	p := &plan.Plan{
		ID: "drift-id", WorkflowName: "wf", TotalTargets: 1,
		Batches: []plan.Batch{{
			Index: 0, Targets: []string{"web-1"},
			Steps: []plan.PlanStep{{Name: "noop", Module: "shell", Action: "exec"}},
		}},
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	run, err := store.GetRun(ctx, "run-err-2")
	require.NoError(t, err)
	require.NotNil(t, run)
	require.NoError(t, store.UpdateRunPlan(ctx, run.ID, string(raw), "hash-that-does-not-match", time.Now().UTC()))

	_, err = e.loadStoredPlan(ctx, run.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPlanDrift)
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	assert.Contains(t, err.Error(), "does not match its plan hash")
}
