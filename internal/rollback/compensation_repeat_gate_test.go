package rollback

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
)

// idempotencyPlan builds a one-batch plan whose forward step "apply-change"
// declares the undo "undo-change", marked idempotent when idempotent is true.
func idempotencyPlan(targets []string, idempotent bool) *plan.Plan {
	ps := plan.PlanStep{
		Name:   "apply-change",
		Module: "pkg",
		Action: "upgrade",
		Rollback: &dsl.RollbackSpec{
			Steps: []dsl.Step{{
				Name:       "undo-change",
				Module:     "pkg",
				Action:     "downgrade",
				Idempotent: idempotent,
			}},
		},
	}
	return &plan.Plan{
		ID:           "plan-idem",
		WorkflowName: "idem",
		Batches: []plan.Batch{{
			Index: 0, Targets: targets, Steps: []plan.PlanStep{ps},
		}},
		TotalTargets: len(targets),
	}
}

// TestCompensationRepeatGate_UndeclaredIdempotencyRefuses is the policy gate
// for the residual case the evidence ordering cannot settle: a prior successful
// compensation is on record, but it cannot be ordered against the forward
// execution. Re-running an undo that never declared itself idempotent is
// refused and reported as a GAP (we do not know that the state is restored),
// never as a benign skip.
func TestCompensationRepeatGate_UndeclaredIdempotencyRefuses(t *testing.T) {
	mgr := NewManager(WithWhitelistAll(), WithConcurrency(1))
	rec := &d2Recorder{}
	l := NewExecutionLedger()
	l.MarkRan("h1", "apply-change")
	l.MarkCompensationUncertain("h1", "apply-change")

	res := mgr.RollbackWithLedger(context.Background(), idempotencyPlan([]string{"h1"}, false), rec.exec, l)

	assert.NotContains(t, rec.calls, "h1/downgrade",
		"an undeclared non-idempotent undo must not be re-run on a guess")
	assert.False(t, res.Success, "refusing to repeat is a gap, so the verdict must not read clean")
	assert.Equal(t, 1, res.RequiredCompensations)
	assert.Equal(t, 0, res.CompletedCompensations)

	var skip *StepRollbackResult
	for i, sr := range res.BatchResults[0].TargetResults[0].StepResults {
		if sr.Skipped {
			skip = &res.BatchResults[0].TargetResults[0].StepResults[i]
		}
	}
	require.NotNil(t, skip)
	assert.Contains(t, skip.SkipReason, "idempotent")
	assert.False(t, skip.NotExecuted, "an unproven restore is a gap, not a benign skip")
	assert.False(t, skip.AlreadyCompensated, "it is not known-compensated either")
}

// TestCompensationRepeatGate_DeclaredIdempotencyProceeds is the other side: the
// author declared the undo repeatable, so the unorderable case may re-run it.
func TestCompensationRepeatGate_DeclaredIdempotencyProceeds(t *testing.T) {
	mgr := NewManager(WithWhitelistAll(), WithConcurrency(1))
	rec := &d2Recorder{}
	l := NewExecutionLedger()
	l.MarkRan("h1", "apply-change")
	l.MarkCompensationUncertain("h1", "apply-change")

	res := mgr.RollbackWithLedger(context.Background(), idempotencyPlan([]string{"h1"}, true), rec.exec, l)

	assert.Contains(t, rec.calls, "h1/downgrade",
		"a declared-idempotent undo may be repeated")
	assert.True(t, res.Success)
}

// TestCompensationRepeatGate_NoUncertaintyRunsNormally guards the common case:
// without uncertainty there is no reason to touch the repeat gate at all, so
// the first compensation of a step proceeds even if the undo is undeclared.
func TestCompensationRepeatGate_NoUncertaintyRunsNormally(t *testing.T) {
	mgr := NewManager(WithWhitelistAll(), WithConcurrency(1))
	rec := &d2Recorder{}
	l := NewExecutionLedger()
	l.MarkRan("h1", "apply-change")

	res := mgr.RollbackWithLedger(context.Background(), idempotencyPlan([]string{"h1"}, false), rec.exec, l)

	assert.Contains(t, rec.calls, "h1/downgrade")
	assert.True(t, res.Success)
}

// TestCompensationRepeatGate_NotExecutedWinsOverUncertainty pins the gate
// ORDER: a step that never ran is a benign skip, and must be reported as
// such even if the ledger also flags it — an uncertain flag must never turn
// "never changed" into a scary gap.
func TestCompensationRepeatGate_NotExecutedWinsOverUncertainty(t *testing.T) {
	mgr := NewManager(WithWhitelistAll(), WithConcurrency(1))
	rec := &d2Recorder{}
	l := NewExecutionLedger()
	l.MarkCompensationUncertain("h1", "apply-change") // never MarkRan

	res := mgr.RollbackWithLedger(context.Background(), idempotencyPlan([]string{"h1"}, false), rec.exec, l)

	assert.Empty(t, rec.calls, "a step that never ran must not be compensated")
	assert.True(t, res.Success)
	sr := res.BatchResults[0].TargetResults[0].StepResults[0]
	assert.True(t, sr.NotExecuted)
	assert.Contains(t, sr.SkipReason, "not executed")
}

// TestCompensationRepeatable_SnapshotAndSpecless covers the two shapes the
// gate treats as repeatable by construction.
func TestCompensationRepeatable_SnapshotAndSpecless(t *testing.T) {
	assert.True(t, compensationRepeatable(plan.PlanStep{}),
		"a step with no rollback spec has no undo command to repeat")
	assert.True(t, compensationRepeatable(plan.PlanStep{
		Rollback: &dsl.RollbackSpec{Strategy: "snapshot", SnapshotPaths: []string{"/etc/a"}},
	}), "restoring the same capture again is repeatable by construction")
	assert.False(t, compensationRepeatable(plan.PlanStep{
		Rollback: &dsl.RollbackSpec{Steps: []dsl.Step{{Name: "a"}, {Name: "b", Idempotent: true}}},
	}), "one undeclared undo step makes the whole compensation non-repeatable")
}
