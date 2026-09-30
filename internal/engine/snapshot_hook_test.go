// snapshot_hook_test.go pins the closure-side snapshot semantics: the
// capture phase runs after lock acquisition and before batch execution,
// a capture failure aborts the run with no mutation, and a nil
// snapshotter keeps the pre-wiring no-op behaviour.

package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
)

// captureRecorder implements Snapshotter and records capture calls in the
// order they fire relative to batch execution (the mock executor logs its
// own calls into the same slice).
type captureRecorder struct {
	calls []string
	fail  bool
}

func (r *captureRecorder) CaptureForStep(_ context.Context, runID, target string, step plan.PlanStep) error {
	if r.fail {
		return errors.New("capture boom")
	}
	r.calls = append(r.calls, "capture:"+target+":"+step.Name)
	return nil
}

func (r *captureRecorder) RestoreForStep(_ context.Context, runID, target string, step plan.PlanStep) error {
	return nil
}

// snapPlan builds a one-batch plan over two steps: one declaring snapshot
// rollback (must be captured), one declaring undo-action (must NOT).
func snapPlan() *plan.Plan {
	return &plan.Plan{
		ID:           "plan-snap",
		WorkflowName: "wf-snap",
		Batches: []plan.Batch{{
			Index:          0,
			Targets:        []string{"web1"},
			MaxConcurrency: 1,
			Steps: []plan.PlanStep{
				{
					Name: "snap-step", Module: "file", Action: "copy",
					Rollback: &dsl.RollbackSpec{
						Strategy:      "snapshot",
						SnapshotPaths: []string{"/etc/app.conf"},
					},
				},
				{
					Name: "undo-step", Module: "pkg", Action: "install",
					Rollback: &dsl.RollbackSpec{Strategy: "undo-action"},
				},
			},
		}},
		TotalTargets: 1,
	}
}

// orderedExec wraps a mockExecutor so forward-step dispatches are logged
// into the recorder's shared call slice.
func orderedExec(rec *captureRecorder, base func(ctx context.Context, target string, step dsl.Step) error) func(ctx context.Context, target string, step dsl.Step) error {
	return func(ctx context.Context, target string, step dsl.Step) error {
		rec.calls = append(rec.calls, "exec:"+target+":"+step.Name)
		return base(ctx, target, step)
	}
}

func noopExec(_ context.Context, _ string, _ dsl.Step) error { return nil }

func TestCaptureRunsBeforeExecutionAndFiltersSnapshotSteps(t *testing.T) {
	rec := &captureRecorder{}
	exec := orderedExec(rec, noopExec)

	runner := newTestClosureRunner(t, newTestStore(t))
	runner.SetSnapshotter(rec)

	res, err := runner.Run(context.Background(), snapPlan(), exec)
	require.NoError(t, err)
	require.Equal(t, PhaseCompleted, res.Phase)

	// Only the snapshot step was captured, and capture preceded execution.
	require.Len(t, rec.calls, 3, "calls: %v", rec.calls)
	assert.Equal(t, "capture:web1:snap-step", rec.calls[0])
	assert.Equal(t, "exec:web1:snap-step", rec.calls[1])
	assert.Equal(t, "exec:web1:undo-step", rec.calls[2])
}

func TestCaptureFailureAbortsBeforeAnyMutation(t *testing.T) {
	rec := &captureRecorder{fail: true}
	var executed []string
	exec := func(ctx context.Context, target string, step dsl.Step) error {
		executed = append(executed, step.Name)
		return nil
	}

	runner := newTestClosureRunner(t, newTestStore(t))
	runner.SetSnapshotter(rec)

	res, err := runner.Run(context.Background(), snapPlan(), exec)
	require.Error(t, err)
	require.Equal(t, PhaseFailed, res.Phase)
	assert.Contains(t, err.Error(), "pre-apply snapshot")
	// No step executed and no batch result recorded.
	assert.Empty(t, executed)
	assert.Empty(t, res.BatchResults)
}

func TestNilSnapshotterIsNoOp(t *testing.T) {
	runner := newTestClosureRunner(t, newTestStore(t)) // no snapshotter
	res, err := runner.Run(context.Background(), snapPlan(), noopExec)
	require.NoError(t, err)
	require.Equal(t, PhaseCompleted, res.Phase)
	assert.Len(t, res.BatchResults, 1)
}

func TestSetSnapshotterAttachesPostConstruction(t *testing.T) {
	rec := &captureRecorder{}
	exec := orderedExec(rec, noopExec)

	runner := newTestClosureRunner(t, newTestStore(t))
	runner.SetSnapshotter(rec) // the wiring-layer attach path

	res, err := runner.Run(context.Background(), snapPlan(), exec)
	require.NoError(t, err)
	require.Equal(t, PhaseCompleted, res.Phase)
	require.Len(t, rec.calls, 3)
	assert.Equal(t, "capture:web1:snap-step", rec.calls[0])
}

func TestCaptureSkippedWhenRollbackHasNoStrategy(t *testing.T) {
	// A step with a RollbackSpec whose strategy is neither "snapshot" nor
	// undo-action-relevant is not captured (the filter checks strategy
	// "snapshot" exactly).
	rec := &captureRecorder{}
	exec := orderedExec(rec, noopExec)

	p := snapPlan()
	p.Batches[0].Steps[0].Rollback = &dsl.RollbackSpec{Strategy: "config-revert"}

	runner := newTestClosureRunner(t, newTestStore(t))
	runner.SetSnapshotter(rec)
	res, err := runner.Run(context.Background(), p, exec)
	require.NoError(t, err)
	require.Equal(t, PhaseCompleted, res.Phase)
	// No capture fired; only the two forward execs.
	require.Len(t, rec.calls, 2)
	assert.Equal(t, "exec:web1:snap-step", rec.calls[0])
}
