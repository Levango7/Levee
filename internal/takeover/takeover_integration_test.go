// takeover_integration_test.go pins the cross-component invariant between the
// dispatch assignment table and the takeover loop: when the leader settles an
// expired run to "interrupted", the corresponding run_assignment row must
// transition from an active state (pending/executing) to "interrupted" so the
// cluster status view reports the true state. These tests are PG-gated and run
// in CI's integration&postgres job.
//
// Background: run_assignment is owned by the dispatch loop, run_execution by
// the execution guard, and run.status by the takeover loop. Before this fix,
// takeover updated run.status and deleted run_execution but left
// run_assignment in "executing" — so the cluster status panel displayed
// interrupted runs as still executing and inflated TotalActive.
package takeover

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// assignmentState reads the current state of a run's assignment row directly.
func assignmentState(t *testing.T, env *takeoverEnv, runID string) string {
	t.Helper()
	a, err := env.store.GetAssignment(context.Background(), runID)
	require.NoError(t, err)
	if a == nil {
		return ""
	}
	return a.State
}

// TestIntegration_TakeoverReflectsInClusterStatus is the headline cross-system
// check: an executing assignment whose worker dies is settled to interrupted
// by the takeover loop, and that interruption is mirrored onto the assignment
// row — so the cluster status view counts it under "interrupted" (not
// "executing") and excludes it from TotalActive.
func TestIntegration_TakeoverReflectsInClusterStatus(t *testing.T) {
	env := newTakeoverEnv(t)
	ctx := context.Background()
	const runID = "run-int-1"

	// Dispatch hands the run to node-b: pending → executing.
	env.seedRunningRun(t, runID)
	require.NoError(t, env.store.CreateAssignment(ctx, &state.Assignment{
		RunID: runID, OwnerNode: "node-b", Epoch: 1, State: state.AssignStatePending,
		AssignedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}))
	ok, err := env.store.UpdateAssignmentStateIf(ctx, runID, 1, state.AssignStatePending, state.AssignmentStateExecuting)
	require.NoError(t, err)
	require.True(t, ok, "the test fixture must own the pending→executing claim")

	// node-b registers its execution lease, then dies.
	_, err = env.guard.Register(ctx, runID, "node-b", 10*time.Second)
	require.NoError(t, err)
	env.expireLease(t, runID)

	// Sanity: before takeover the assignment is still executing.
	assert.Equal(t, state.AssignmentStateExecuting, assignmentState(t, env, runID))

	// Leader node-a sweeps.
	res, err := env.loopy.TakeoverOnce(ctx)
	require.NoError(t, err)
	assert.Contains(t, res.Settled, runID)

	// The run is interrupted.
	run, err := env.store.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, "interrupted", run.Status)

	// KEY INVARIANT: the assignment row mirrors the interruption.
	assert.Equal(t, state.AssignmentStateInterrupted, assignmentState(t, env, runID),
		"the cluster status view reads run_assignment — an interrupted run must not appear as executing")

	// The cluster status summary counts it as interrupted and excludes it
	// from active load.
	summary, err := env.store.AssignmentSummary(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Counts[state.AssignmentStateInterrupted],
		"interrupted count must reflect the takeover settlement")
	assert.NotZero(t, summary.Counts[state.AssignmentStateInterrupted])
	// The interrupted row must not inflate TotalActive.
	totalActive := 0
	for asgnState, count := range summary.Counts {
		if asgnState == state.AssignStatePending || asgnState == state.AssignmentStateExecuting {
			totalActive += count
		}
	}
	assert.Equal(t, 0, totalActive, "no active assignments should remain after the takeover")
}

// TestIntegration_TakeoverPendingAssignment covers the edge case where the
// worker dies before acquiring the assignment (CAS→Begin critical section).
// The assignment is still in "pending" when the lease expires — the takeover
// must reflect it to interrupted just the same.
func TestIntegration_TakeoverPendingAssignment(t *testing.T) {
	env := newTakeoverEnv(t)
	ctx := context.Background()
	const runID = "run-int-2"

	env.seedRunningRun(t, runID)
	require.NoError(t, env.store.CreateAssignment(ctx, &state.Assignment{
		RunID: runID, OwnerNode: "node-b", Epoch: 1, State: state.AssignStatePending,
		AssignedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}))
	// The lease is registered but the worker never promoted the assignment to
	// executing — it died first.
	_, err := env.guard.Register(ctx, runID, "node-b", 10*time.Second)
	require.NoError(t, err)
	env.expireLease(t, runID)

	res, err := env.loopy.TakeoverOnce(ctx)
	require.NoError(t, err)
	assert.Contains(t, res.Settled, runID)

	assert.Equal(t, state.AssignmentStateInterrupted, assignmentState(t, env, runID),
		"a pending assignment must also be reflected to interrupted")
}

// TestIntegration_TakeoverLeavesDoneUntouched verifies the takeover does NOT
// regress a terminal assignment. If dispatch finished the run (executing → done)
// before the takeover sweep, the assignment is "done" and must stay "done" even
// though the run itself is interrupted.
func TestIntegration_TakeoverLeavesDoneUntouched(t *testing.T) {
	env := newTakeoverEnv(t)
	ctx := context.Background()
	const runID = "run-int-3"

	env.seedRunningRun(t, runID)
	require.NoError(t, env.store.CreateAssignment(ctx, &state.Assignment{
		RunID: runID, OwnerNode: "node-b", Epoch: 1, State: state.AssignStatePending,
		AssignedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}))
	// Dispatch settles the assignment to done first.
	_, err := env.store.UpdateAssignmentStateIf(ctx, runID, 1, state.AssignStatePending, state.AssignmentStateExecuting)
	require.NoError(t, err)
	ok, err := env.store.UpdateAssignmentStateIf(ctx, runID, 1, state.AssignmentStateExecuting, state.AssignmentStateDone)
	require.NoError(t, err)
	require.True(t, ok)

	// A leftover lease from the dead worker still expires.
	_, err = env.guard.Register(ctx, runID, "node-b", 10*time.Second)
	require.NoError(t, err)
	env.expireLease(t, runID)

	res, err := env.loopy.TakeoverOnce(ctx)
	require.NoError(t, err)
	assert.Contains(t, res.Settled, runID)

	// The run is interrupted, but the assignment remains done.
	assert.Equal(t, state.AssignmentStateDone, assignmentState(t, env, runID),
		"a done assignment must never be regressed to interrupted")
}
