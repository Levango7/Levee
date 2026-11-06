package rollback

// manager_dryrun_test.go — 预览模式必须真的什么都不做。
//
// 这是回滚管理器自己的那道闸：预览把补偿步骤记成「本来会跑」，但绝不能真的
// 跑；strategy-"snapshot" 的步骤尤其要命——恢复快照是把采集到的前状态写回目标
// 机，这是回滚能做的最破坏性的动作，而回调明明是接好的。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
)

// undoPlanWith builds a one-batch plan whose single step declares an
// undo-action compensation (the ordinary machinery, not the snapshot path).
func undoPlanWith() *plan.Plan {
	return &plan.Plan{
		ID:           "plan-dry-rb",
		WorkflowName: "wf-dry-rb",
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
}

// TestDryRunRecordsTheStepAsWouldRun: a nil execute function means "preview",
// and the step must be marked as SELECTED — that is the difference between
// "a real rollback would compensate this" and "this was skipped because the
// forward step never ran". A console that cannot tell them apart cannot
// answer the operator's question.
func TestDryRunRecordsTheStepAsWouldRun(t *testing.T) {
	m := NewManager(WithWhitelistAll())
	res := m.Rollback(context.Background(), undoPlanWith(), nil)

	sr := res.BatchResults[0].TargetResults[0].StepResults[0]
	require.True(t, sr.Skipped)
	assert.True(t, sr.DryRun, "the step was selected for compensation; only the preview stopped it")
	assert.Equal(t, "no execute function", sr.SkipReason)
}

// TestDryRunDoesNotRestoreTheSnapshot pins the sharpest edge of the
// preview: with a restore callback wired, the captured pre-apply state must
// NOT be written back over the target.
func TestDryRunDoesNotRestoreTheSnapshot(t *testing.T) {
	restored := false
	restore := func(ctx context.Context, runID, target string, step plan.PlanStep) error {
		restored = true
		return nil
	}

	m := NewManager(
		WithWhitelistAll(),
		WithSnapshotRestore(restore),
		WithRunID("run-42"),
		WithDryRun(true),
	)
	res := m.Rollback(context.Background(), snapPlanWith(), nil)

	assert.False(t, restored, "a preview must not write the captured state back")

	sr := res.BatchResults[0].TargetResults[0].StepResults[0]
	require.True(t, sr.Skipped)
	assert.True(t, sr.DryRun)
	assert.Equal(t, "dry-run: snapshot restore not applied", sr.SkipReason)
}

// TestDryRunWithoutRestoreStillReportsTheWiringGap: the preview must not
// paper over a missing restore callback either — the reason stays the
// honest one.
func TestDryRunWithoutRestoreStillReportsTheWiringGap(t *testing.T) {
	m := NewManager(WithWhitelistAll(), WithDryRun(true))
	res := m.Rollback(context.Background(), snapPlanWith(), nil)

	sr := res.BatchResults[0].TargetResults[0].StepResults[0]
	require.True(t, sr.Skipped)
	assert.False(t, sr.DryRun, "without a restore wired there is nothing a preview could have applied")
	assert.Equal(t, "snapshot restore not wired", sr.SkipReason)
}
