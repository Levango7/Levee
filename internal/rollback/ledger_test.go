package rollback

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/batch"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
)

// --- helpers ----------------------------------------------------------------

// newD2Plan builds a single-batch plan over the given targets with one
// forward step ("apply-change") whose declared compensation is
// "undo-change" — or, with withRollback false, a forward step with NO
// rollback spec (the compensation-gap scenario).
func newD2Plan(targets []string, withRollback bool) *plan.Plan {
	ps := plan.PlanStep{
		Name:   "apply-change",
		Module: "pkg",
		Action: "upgrade",
	}
	if withRollback {
		ps.Rollback = &dsl.RollbackSpec{
			Steps: []dsl.Step{{
				Name:   "undo-change",
				Module: "pkg",
				Action: "downgrade",
			}},
		}
	}
	return &plan.Plan{
		ID:           "plan-d2",
		WorkflowName: "d2",
		Batches: []plan.Batch{{
			Index:   0,
			Targets: targets,
			Steps:   []plan.PlanStep{ps},
		}},
		TotalTargets: len(targets),
	}
}

// d2Recorder records every dispatch as "target/action" and can fail chosen
// combinations via failOn.
type d2Recorder struct {
	calls  []string
	failOn func(target, action string) error
}

func (r *d2Recorder) exec(_ context.Context, target string, step dsl.Step) error {
	r.calls = append(r.calls, target+"/"+step.Action)
	if r.failOn != nil {
		return r.failOn(target, step.Action)
	}
	return nil
}

func (r *d2Recorder) count(targetAction string) int {
	n := 0
	for _, c := range r.calls {
		if c == targetAction {
			n++
		}
	}
	return n
}

// --- LedgerFromBatchResults --------------------------------------------------

// The ledger must mirror dispatch evidence: dispatched+ok → ran,
// dispatched+failed → ran & unknown, never-dispatched → absent. An empty
// input yields a non-nil "nothing ran" ledger.
func TestLedgerFromBatchResults_DerivesDispatchEvidence(t *testing.T) {
	applyErr := errors.New("forward failed")
	in := []*batch.BatchResult{{
		BatchIndex: 0,
		TargetResults: []batch.TargetResult{
			{
				Target: "h1",
				StepResults: []batch.StepResult{
					{StepName: "s-clean"},
					{StepName: "s-fail", Error: applyErr},
				},
			},
			{Target: "h2"}, // skipped before dispatch: no step rows
		},
	}}

	l := LedgerFromBatchResults(in)
	require.NotNil(t, l)
	assert.True(t, l.Ran("h1", "s-clean"))
	assert.False(t, l.SideEffectsUnknown("h1", "s-clean"))
	assert.True(t, l.Ran("h1", "s-fail"), "a failed forward step still ran")
	assert.True(t, l.SideEffectsUnknown("h1", "s-fail"))
	assert.False(t, l.Ran("h2", "s-clean"), "never-dispatched target must not be marked")
	assert.Equal(t, 2, l.RanCount())

	empty := LedgerFromBatchResults(nil)
	require.NotNil(t, empty, "nil input must still yield a usable ledger")
	assert.Equal(t, 0, empty.RanCount())
}

// --- RollbackWithLedger verdicts (D-2 v2 design item 3) -----------------------

// P0-3 core: a step the ledger does not mark as run is never compensated —
// the undo of a never-started target must not be dispatched — while the
// dispatched target is fully compensated and the verdict is clean.
func TestRollbackWithLedger_SkipsNeverDispatchedSteps(t *testing.T) {
	p := newD2Plan([]string{"h1", "h2"}, true)
	ledger := NewExecutionLedger()
	ledger.MarkRan("h1", "apply-change")

	rec := &d2Recorder{}
	m := NewManager(WithWhitelistAll())
	res := m.RollbackWithLedger(context.Background(), p, rec.exec, ledger)
	require.NotNil(t, res)

	assert.Equal(t, 1, rec.count("h1/downgrade"), "dispatched step must be compensated")
	assert.Equal(t, 0, rec.count("h2/downgrade"), "never-started step must NOT be compensated")

	assert.Equal(t, 1, res.RequiredCompensations)
	assert.Equal(t, 1, res.CompletedCompensations)
	assert.NoError(t, res.Error)
	assert.True(t, res.Success)
	assert.False(t, res.PartialRollback)

	// The benign skip is recorded with NotExecuted for auditability.
	require.Len(t, res.BatchResults, 1)
	var h2 TargetRollbackResult
	for _, tr := range res.BatchResults[0].TargetResults {
		if tr.Target == "h2" {
			h2 = tr
		}
	}
	require.Len(t, h2.StepResults, 1)
	assert.True(t, h2.StepResults[0].Skipped)
	assert.True(t, h2.StepResults[0].NotExecuted)
}

// Design item 3 + 可持续性 extension point: a forward step that FAILED is
// compensated and its undetermined side effects are COUNTED — but the count
// does not flip the verdict. All required compensations completed → Success.
func TestRollbackWithLedger_UnknownSideEffectsAreInformational(t *testing.T) {
	p := newD2Plan([]string{"h1"}, true)
	ledger := NewExecutionLedger()
	ledger.MarkUnknown("h1", "apply-change")

	rec := &d2Recorder{}
	m := NewManager(WithWhitelistAll())
	res := m.RollbackWithLedger(context.Background(), p, rec.exec, ledger)
	require.NotNil(t, res)

	assert.Equal(t, 1, rec.count("h1/downgrade"), "failed forward step still gets its compensation")
	assert.Equal(t, 1, res.UnknownSideEffects, "undetermined side effects must be surfaced")
	assert.Equal(t, 1, res.RequiredCompensations)
	assert.Equal(t, 1, res.CompletedCompensations)
	assert.NoError(t, res.Error)
	assert.True(t, res.Success,
		"design item 3: Success = Error == nil && Required == Completed; the unknown count is informational")
}

// A step that RAN but declares no compensation is required_but_missing: a
// gap that keeps Success false even though no command errored (Error nil).
// Nothing completed → PartialRollback false (the closure maps this to
// rollback_incomplete).
func TestRollbackWithLedger_MissingCompensationIsGap(t *testing.T) {
	p := newD2Plan([]string{"h1"}, false) // no rollback spec
	ledger := NewExecutionLedger()
	ledger.MarkRan("h1", "apply-change")

	rec := &d2Recorder{}
	m := NewManager(WithWhitelistAll())
	res := m.RollbackWithLedger(context.Background(), p, rec.exec, ledger)
	require.NotNil(t, res)

	assert.Empty(t, rec.calls, "nothing to dispatch without a declared compensation")
	assert.Equal(t, 1, res.RequiredCompensations)
	assert.Equal(t, 0, res.CompletedCompensations)
	assert.NoError(t, res.Error, "a gap is not a command error")
	assert.False(t, res.Success, "required_but_missing must keep Success false")
	assert.False(t, res.PartialRollback)
}

// Some compensations complete, one fails → Success false with
// PartialRollback true (the closure maps this to rolled_back_partial).
func TestRollbackWithLedger_PartialWhenOneCompensationFails(t *testing.T) {
	p := newD2Plan([]string{"h1", "h2"}, true)
	ledger := NewExecutionLedger()
	ledger.MarkRan("h1", "apply-change")
	ledger.MarkRan("h2", "apply-change")

	rec := &d2Recorder{failOn: func(target, action string) error {
		if target == "h1" && action == "downgrade" {
			return errors.New("compensation failed")
		}
		return nil
	}}
	m := NewManager(WithWhitelistAll())
	res := m.RollbackWithLedger(context.Background(), p, rec.exec, ledger)
	require.NotNil(t, res)

	assert.Equal(t, 2, res.RequiredCompensations)
	assert.Equal(t, 1, res.CompletedCompensations)
	assert.Error(t, res.Error)
	assert.False(t, res.Success)
	assert.True(t, res.PartialRollback)
}

// nil ledger = legacy contract (pre-D-2, design item 2): every step of the
// plan is treated as run and compensated — direct/manual callers without
// evidence keep working unchanged.
func TestRollbackWithLedger_NilLedgerTreatsAllAsRun(t *testing.T) {
	p := newD2Plan([]string{"h1", "h2"}, true)
	rec := &d2Recorder{}
	m := NewManager(WithWhitelistAll())
	res := m.RollbackWithLedger(context.Background(), p, rec.exec, nil)
	require.NotNil(t, res)

	assert.Equal(t, 1, rec.count("h1/downgrade"))
	assert.Equal(t, 1, rec.count("h2/downgrade"))
	assert.Equal(t, 2, res.RequiredCompensations)
	assert.Equal(t, 2, res.CompletedCompensations)
	assert.True(t, res.Success)
}

// nil ledger + skipped compensation = pre-D-2 benign reading (design item
// 2: nil = 旧行为): without evidence the step is only ASSUMED run, so a
// no-spec/policy/dry-run skip does not fail the verdict — the 8 legacy
// manager/snapshot/post_verify tests pin this compatibility contract.
// Evidence-backed gap counting is covered by
// TestRollbackWithLedger_MissingCompensationIsGap above.
func TestRollbackWithLedger_NilLedgerSkipsAreBenign(t *testing.T) {
	// No rollback spec anywhere, nil ledger: nothing required, no errors.
	p := newD2Plan([]string{"h1"}, false)
	rec := &d2Recorder{}
	m := NewManager(WithWhitelistAll())
	res := m.RollbackWithLedger(context.Background(), p, rec.exec, nil)
	require.NotNil(t, res)

	assert.Empty(t, rec.calls)
	assert.Equal(t, 0, res.RequiredCompensations)
	assert.NoError(t, res.Error)
	assert.True(t, res.Success, "nil-ledger skips must keep the pre-D-2 benign verdict")
	assert.False(t, res.PartialRollback)
}
