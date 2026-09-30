// snapshot restore branch tests: strategy-"snapshot" steps route to the
// wired restore callback instead of undo steps, fail closed when the
// callback is missing or errors, and are recorded visibly for audit.

package rollback

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
)

// snapPlanWith builds a one-batch plan whose single step declares
// strategy "snapshot" plus (optionally) undo steps that must NOT run.
func snapPlanWith(undoSteps ...dsl.Step) *plan.Plan {
	return &plan.Plan{
		ID:           "plan-snap-rb",
		WorkflowName: "wf-snap-rb",
		Batches: []plan.Batch{{
			Index:          0,
			Targets:        []string{"web1"},
			MaxConcurrency: 1,
			Steps: []plan.PlanStep{{
				Name:   "push-conf",
				Module: "file",
				Action: "copy",
				Rollback: &dsl.RollbackSpec{
					Strategy:      "snapshot",
					SnapshotPaths: []string{"/etc/app.conf"},
					Steps:         undoSteps,
				},
			}},
		}},
		TotalTargets: 1,
	}
}

func TestSnapshotStepRestoresViaCallback(t *testing.T) {
	var restored []string
	restore := func(ctx context.Context, runID, target string, step plan.PlanStep) error {
		restored = append(restored, runID+"/"+target+"/"+step.Name)
		return nil
	}

	// The snapshot step ALSO declares an undo step: restore wins and the
	// undo step never runs (double-undo is forbidden).
	undoRan := false
	exec := func(ctx context.Context, target string, step dsl.Step) error {
		undoRan = true
		return nil
	}

	m := NewManager(
		WithWhitelistAll(),
		WithSnapshotRestore(restore),
		WithRunID("run-42"),
	)
	res := m.Rollback(context.Background(), snapPlanWith(dsl.Step{Name: "undo-copy", Module: "file", Action: "delete"}), exec)

	require.True(t, res.Success, "err: %v", res.Error)
	require.Len(t, restored, 1)
	assert.Equal(t, "run-42/web1/push-conf", restored[0])
	assert.False(t, undoRan, "undo steps of a snapshot step must not execute")

	// The result records the snapshot restore for audit visibility.
	sr := res.BatchResults[0].TargetResults[0].StepResults[0]
	assert.False(t, sr.Skipped)
	assert.Equal(t, "snapshot:push-conf", sr.RollbackStepName)
	assert.Equal(t, "file", sr.Module)
	assert.Equal(t, "copy", sr.Action)
}

func TestSnapshotStepWithoutCallbackSkipsVisibly(t *testing.T) {
	// No restore wired (pre-wiring behaviour): skip with an explicit
	// reason, NEVER an undo-action execution.
	undoRan := false
	exec := func(ctx context.Context, target string, step dsl.Step) error {
		undoRan = true
		return nil
	}

	m := NewManager(WithWhitelistAll())
	res := m.Rollback(context.Background(), snapPlanWith(dsl.Step{Name: "undo-copy", Module: "file", Action: "delete"}), exec)

	// Skipped steps are not errors (the run-level verdict stays success).
	require.True(t, res.Success)
	assert.False(t, undoRan)
	sr := res.BatchResults[0].TargetResults[0].StepResults[0]
	require.True(t, sr.Skipped)
	assert.Equal(t, "snapshot restore not wired", sr.SkipReason)
}

func TestSnapshotStepWithoutRunIDSkipsVisibly(t *testing.T) {
	restore := func(ctx context.Context, runID, target string, step plan.PlanStep) error {
		return errors.New("must not be called")
	}
	m := NewManager(WithWhitelistAll(), WithSnapshotRestore(restore)) // no WithRunID
	res := m.Rollback(context.Background(), snapPlanWith(), nil)

	sr := res.BatchResults[0].TargetResults[0].StepResults[0]
	require.True(t, sr.Skipped)
	assert.Equal(t, "snapshot restore without run id", sr.SkipReason)
}

func TestSnapshotStepRestoreErrorFailsTheStep(t *testing.T) {
	restore := func(ctx context.Context, runID, target string, step plan.PlanStep) error {
		return errors.New("restore boom")
	}
	m := NewManager(WithWhitelistAll(), WithSnapshotRestore(restore), WithRunID("run-1"))
	res := m.Rollback(context.Background(), snapPlanWith(), nil)

	require.False(t, res.Success)
	sr := res.BatchResults[0].TargetResults[0].StepResults[0]
	require.False(t, sr.Skipped)
	require.Error(t, sr.Error)
	assert.Contains(t, sr.Error.Error(), "restore boom")
}

func TestSetRunIDBeforeRollback(t *testing.T) {
	// The closure calls SetRunID right before the rollback flow (the
	// Manager is constructed before the run id exists).
	var got string
	restore := func(ctx context.Context, runID, target string, step plan.PlanStep) error {
		got = runID
		return nil
	}
	m := NewManager(WithWhitelistAll(), WithSnapshotRestore(restore))
	m.SetRunID("run-late")
	res := m.Rollback(context.Background(), snapPlanWith(), nil)
	require.True(t, res.Success)
	assert.Equal(t, "run-late", got)
}

func TestUndoActionStepsStillRun(t *testing.T) {
	// Regression pin: undo-action steps (the pre-existing machinery) are
	// unaffected by the snapshot branch.
	var undoRan []string
	exec := func(ctx context.Context, target string, step dsl.Step) error {
		undoRan = append(undoRan, step.Name)
		return nil
	}
	p := &plan.Plan{
		ID:           "plan-undo",
		WorkflowName: "wf-undo",
		Batches: []plan.Batch{{
			Index:          0,
			Targets:        []string{"web1"},
			MaxConcurrency: 1,
			Steps: []plan.PlanStep{{
				Name:   "install-pkg",
				Module: "pkg",
				Action: "install",
				Rollback: &dsl.RollbackSpec{
					Strategy: "undo-action",
					Steps:    []dsl.Step{{Name: "undo-pkg", Module: "pkg", Action: "remove"}},
				},
			}},
		}},
		TotalTargets: 1,
	}
	m := NewManager(WithWhitelistAll())
	res := m.Rollback(context.Background(), p, exec)
	require.True(t, res.Success)
	assert.Equal(t, []string{"undo-pkg"}, undoRan)
}
