package rollback

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
)

// --- helpers ---------------------------------------------------------------

// mkPlan builds a plan with the given batches. Each batch is a list of steps
// (shared across all targets in that batch). Targets are auto-named
// "host-<batch>-<n>".
func mkPlan(batches ...[]plan.PlanStep) *plan.Plan {
	p := &plan.Plan{ID: "test-plan", WorkflowName: "test"}
	for i, steps := range batches {
		targets := []string{fmt.Sprintf("host-%d-0", i), fmt.Sprintf("host-%d-1", i)}
		p.Batches = append(p.Batches, plan.Batch{
			Index:          i,
			Targets:        targets,
			Steps:          steps,
			MaxConcurrency: 1,
		})
		p.TotalTargets += len(targets)
	}
	return p
}

// rbStep builds a dsl.Step for use inside a RollbackSpec.
func rbStep(name, module, action string) dsl.Step {
	return dsl.Step{Name: name, Module: module, Action: action}
}

// withRollback returns a copy of ps with the given rollback steps attached.
func withRollback(ps plan.PlanStep, steps ...dsl.Step) plan.PlanStep {
	ps.Rollback = &dsl.RollbackSpec{Steps: steps, Strategy: "undo-action"}
	return ps
}

// execRecorder is an ExecuteFunc stub that records every invocation and can
// be configured to fail for specific module.action pairs.
type execRecorder struct {
	mu       sync.Mutex
	calls    []execCall
	failOn   map[string]error // "module.action" -> error
	delayFor map[string]time.Duration
}

type execCall struct {
	target string
	step   dsl.Step
}

func newExecRecorder() *execRecorder {
	return &execRecorder{
		failOn:   make(map[string]error),
		delayFor: make(map[string]time.Duration),
	}
}

func (r *execRecorder) fn() ExecuteFunc {
	return func(_ context.Context, target string, step dsl.Step) error {
		r.mu.Lock()
		r.calls = append(r.calls, execCall{target: target, step: step})
		delay := r.delayFor[step.Module+"."+step.Action]
		err := r.failOn[step.Module+"."+step.Action]
		r.mu.Unlock()

		if delay > 0 {
			time.Sleep(delay)
		}
		return err
	}
}

func (r *execRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *execRecorder) setFail(module, action string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOn[module+"."+action] = err
}

func (r *execRecorder) setDelay(module, action string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delayFor[module+"."+action] = d
}

// --- construction ----------------------------------------------------------

func TestNewManagerDefaults(t *testing.T) {
	m := NewManager()
	assert.Empty(t, m.Whitelist())
	assert.Equal(t, 1, m.concurrency)
	assert.True(t, m.stopOnError)
}

func TestWithWhitelist(t *testing.T) {
	m := NewManager(WithWhitelist([]string{"pkg.install", "file.copy"}))
	assert.Equal(t, []string{"file.copy", "pkg.install"}, m.Whitelist())
}

func TestWithConcurrency(t *testing.T) {
	m := NewManager(WithConcurrency(4))
	assert.Equal(t, 4, m.concurrency)
}

func TestWithConcurrencyNonPositiveIgnored(t *testing.T) {
	m := NewManager(WithConcurrency(0))
	assert.Equal(t, 1, m.concurrency, "non-positive concurrency should keep default")
	m = NewManager(WithConcurrency(-1))
	assert.Equal(t, 1, m.concurrency)
}

func TestWithStopOnError(t *testing.T) {
	m := NewManager(WithStopOnError(false))
	assert.False(t, m.stopOnError)
	m = NewManager(WithStopOnError(true))
	assert.True(t, m.stopOnError)
}

// --- nil / empty cases -----------------------------------------------------

func TestRollbackNilPlan(t *testing.T) {
	m := NewManager()
	res := m.Rollback(context.Background(), nil, nil)
	require.NotNil(t, res)
	assert.False(t, res.Success)
	assert.Error(t, res.Error)
	assert.Contains(t, res.Error.Error(), "plan is nil")
}

func TestRollbackEmptyPlan(t *testing.T) {
	m := NewManager()
	p := &plan.Plan{ID: "empty"}
	res := m.Rollback(context.Background(), p, newExecRecorder().fn())
	require.NotNil(t, res)
	assert.True(t, res.Success, "empty plan should roll back trivially")
	assert.Empty(t, res.BatchResults)
	assert.NoError(t, res.Error)
}

func TestRollbackNilExecFnDryRun(t *testing.T) {
	m := NewManager()
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "pkg", Action: "remove"},
			rbStep("undo-s1", "pkg", "install")),
	})
	res := m.Rollback(context.Background(), p, nil)
	require.NotNil(t, res)
	assert.True(t, res.Success, "dry-run with nil execFn should be successful (all skipped)")
	assert.NotEmpty(t, res.BatchResults)
	// Every step result should be skipped.
	for _, br := range res.BatchResults {
		for _, tr := range br.TargetResults {
			for _, sr := range tr.StepResults {
				assert.True(t, sr.Skipped)
				assert.Contains(t, sr.SkipReason, "no execute function")
			}
		}
	}
}

// --- single batch rollback -------------------------------------------------

func TestSingleBatchRollback(t *testing.T) {
	m := NewManager()
	rec := newExecRecorder()
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "remove-nginx", Module: "pkg", Action: "remove"},
			rbStep("install-nginx", "pkg", "install")),
	})

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.NoError(t, res.Error)
	assert.Len(t, res.BatchResults, 1)

	// 2 targets × 1 rollback step = 2 exec calls.
	assert.Equal(t, 2, rec.count())
}

func TestSingleBatchMultipleStepsReversed(t *testing.T) {
	m := NewManager()
	rec := newExecRecorder()
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "file", Action: "copy"},
			rbStep("undo-s1", "file", "delete")),
		withRollback(plan.PlanStep{Name: "s2", Module: "pkg", Action: "install"},
			rbStep("undo-s2", "pkg", "remove")),
		withRollback(plan.PlanStep{Name: "s3", Module: "svc", Action: "start"},
			rbStep("undo-s3", "svc", "stop")),
	})

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.True(t, res.Success)

	// 2 targets × 3 steps = 6 calls. Verify the per-target order is
	// reversed: s3 undone before s2 undone before s1 undone.
	br := res.BatchResults[0]
	require.Len(t, br.TargetResults, 2)
	for _, tr := range br.TargetResults {
		require.Len(t, tr.StepResults, 3)
		assert.Equal(t, "s3", tr.StepResults[0].OrigStepName)
		assert.Equal(t, "s2", tr.StepResults[1].OrigStepName)
		assert.Equal(t, "s1", tr.StepResults[2].OrigStepName)
	}
}

// --- multi-batch reverse order ---------------------------------------------

func TestMultiBatchReverseOrder(t *testing.T) {
	m := NewManager()
	rec := newExecRecorder()
	p := mkPlan(
		[]plan.PlanStep{
			withRollback(plan.PlanStep{Name: "b0-s1", Module: "file", Action: "copy"},
				rbStep("undo-b0-s1", "file", "delete")),
		},
		[]plan.PlanStep{
			withRollback(plan.PlanStep{Name: "b1-s1", Module: "pkg", Action: "install"},
				rbStep("undo-b1-s1", "pkg", "remove")),
		},
		[]plan.PlanStep{
			withRollback(plan.PlanStep{Name: "b2-s1", Module: "svc", Action: "start"},
				rbStep("undo-b2-s1", "svc", "stop")),
		},
	)

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.True(t, res.Success)
	require.Len(t, res.BatchResults, 3)

	// Batch results should be in reverse order: batch 2, 1, 0.
	assert.Equal(t, 2, res.BatchResults[0].BatchIndex)
	assert.Equal(t, 1, res.BatchResults[1].BatchIndex)
	assert.Equal(t, 0, res.BatchResults[2].BatchIndex)
}

func TestMultiBatchReverseOrderExecutionSequence(t *testing.T) {
	m := NewManager()
	rec := newExecRecorder()
	p := mkPlan(
		[]plan.PlanStep{
			withRollback(plan.PlanStep{Name: "b0", Module: "a", Action: "x"},
				rbStep("undo-b0", "a", "undo-x")),
		},
		[]plan.PlanStep{
			withRollback(plan.PlanStep{Name: "b1", Module: "b", Action: "y"},
				rbStep("undo-b1", "b", "undo-y")),
		},
	)

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.True(t, res.Success)

	// Verify execution order: b1 undone before b0 undone. With 2 targets
	// per batch and serial execution, the call sequence is:
	//   b1/host-1-0, b1/host-1-1, b0/host-0-0, b0/host-0-1
	calls := rec.calls
	require.Len(t, calls, 4)
	assert.Equal(t, "b", calls[0].step.Module)
	assert.Equal(t, "b", calls[1].step.Module)
	assert.Equal(t, "a", calls[2].step.Module)
	assert.Equal(t, "a", calls[3].step.Module)
}

// --- no rollback spec skipped ----------------------------------------------

func TestNoRollbackSpecSkipped(t *testing.T) {
	m := NewManager()
	rec := newExecRecorder()
	p := mkPlan([]plan.PlanStep{
		{Name: "no-rollback", Module: "shell", Action: "exec"}, // no Rollback
		withRollback(plan.PlanStep{Name: "with-rollback", Module: "pkg", Action: "remove"},
			rbStep("undo", "pkg", "install")),
	})

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.True(t, res.Success)

	// Only the step with a RollbackSpec should be executed.
	assert.Equal(t, 2, rec.count(), "1 target-pair × 1 rollback step = 2 calls")

	br := res.BatchResults[0]
	for _, tr := range br.TargetResults {
		require.Len(t, tr.StepResults, 2)
		// Reverse order: with-rollback undone first, then no-rollback skipped.
		assert.Equal(t, "with-rollback", tr.StepResults[0].OrigStepName)
		assert.False(t, tr.StepResults[0].Skipped)

		assert.Equal(t, "no-rollback", tr.StepResults[1].OrigStepName)
		assert.True(t, tr.StepResults[1].Skipped)
		assert.Contains(t, tr.StepResults[1].SkipReason, "no rollback spec")
	}
}

func TestAllStepsNoRollbackSpec(t *testing.T) {
	m := NewManager()
	rec := newExecRecorder()
	p := mkPlan([]plan.PlanStep{
		{Name: "s1", Module: "shell", Action: "exec"},
		{Name: "s2", Module: "file", Action: "copy"},
	})

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.True(t, res.Success, "all-skipped run should be successful")
	assert.Equal(t, 0, rec.count())
}

// --- whitelist validation --------------------------------------------------

func TestWhitelistAllowsListedSteps(t *testing.T) {
	m := NewManager(WithWhitelist([]string{"pkg.install", "file.copy"}))
	rec := newExecRecorder()
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "pkg", Action: "remove"},
			rbStep("undo-s1", "pkg", "install")),
		withRollback(plan.PlanStep{Name: "s2", Module: "file", Action: "delete"},
			rbStep("undo-s2", "file", "copy")),
	})

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.Equal(t, 4, rec.count(), "2 targets × 2 listed steps = 4 calls")
}

func TestWhitelistSkipsUnlistedSteps(t *testing.T) {
	m := NewManager(WithWhitelist([]string{"pkg.install"}))
	rec := newExecRecorder()
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "pkg", Action: "remove"},
			rbStep("undo-s1", "pkg", "install")), // allowed
		withRollback(plan.PlanStep{Name: "s2", Module: "file", Action: "delete"},
			rbStep("undo-s2", "file", "delete")), // NOT allowed
	})

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.True(t, res.Success)

	// Only pkg.install should run: 2 targets × 1 step = 2 calls.
	assert.Equal(t, 2, rec.count())

	br := res.BatchResults[0]
	for _, tr := range br.TargetResults {
		require.Len(t, tr.StepResults, 2)
		// Reverse order: s2 undone first (skipped), then s1 undone (executed).
		assert.Equal(t, "s2", tr.StepResults[0].OrigStepName)
		assert.True(t, tr.StepResults[0].Skipped)
		assert.Contains(t, tr.StepResults[0].SkipReason, "not in rollback whitelist")
		assert.Contains(t, tr.StepResults[0].SkipReason, "file.delete")

		assert.Equal(t, "s1", tr.StepResults[1].OrigStepName)
		assert.False(t, tr.StepResults[1].Skipped)
	}
}

func TestWhitelistEmptyAllowsAll(t *testing.T) {
	m := NewManager() // no whitelist
	rec := newExecRecorder()
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "anything", Action: "whatever"},
			rbStep("undo-s1", "any", "thing")),
	})

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.Equal(t, 2, rec.count(), "empty whitelist should allow all")
}

// --- error handling --------------------------------------------------------

func TestStepErrorStopOnErrorTrue(t *testing.T) {
	m := NewManager(WithStopOnError(true))
	wantErr := errors.New("channel broken")
	rec := newExecRecorder()
	rec.setFail("pkg", "install", wantErr)
	p := mkPlan(
		[]plan.PlanStep{
			withRollback(plan.PlanStep{Name: "b0", Module: "pkg", Action: "remove"},
				rbStep("undo-b0", "pkg", "install")),
		},
		[]plan.PlanStep{
			withRollback(plan.PlanStep{Name: "b1", Module: "pkg", Action: "remove"},
				rbStep("undo-b1", "pkg", "install")),
		},
	)

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.False(t, res.Success)
	assert.Error(t, res.Error)
	assert.ErrorIs(t, res.Error, wantErr)

	// stopOnError=true: only batch 1 (the first rolled back) should have
	// results; batch 0 should not be attempted.
	require.Len(t, res.BatchResults, 1)
	assert.Equal(t, 1, res.BatchResults[0].BatchIndex)
}

func TestStepErrorStopOnErrorFalse(t *testing.T) {
	m := NewManager(WithStopOnError(false))
	wantErr := errors.New("channel broken")
	rec := newExecRecorder()
	rec.setFail("pkg", "install", wantErr)
	p := mkPlan(
		[]plan.PlanStep{
			withRollback(plan.PlanStep{Name: "b0", Module: "pkg", Action: "remove"},
				rbStep("undo-b0", "pkg", "install")),
		},
		[]plan.PlanStep{
			withRollback(plan.PlanStep{Name: "b1", Module: "pkg", Action: "remove"},
				rbStep("undo-b1", "pkg", "install")),
		},
	)

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.False(t, res.Success)
	assert.Error(t, res.Error)

	// stopOnError=false: both batches should be rolled back.
	require.Len(t, res.BatchResults, 2)
	// All 4 targets × 1 step = 4 calls (all fail).
	assert.Equal(t, 4, rec.count())
}

func TestPartialRollback(t *testing.T) {
	m := NewManager(WithStopOnError(false))
	rec := newExecRecorder()
	// Make pkg.install fail but file.copy succeed.
	rec.setFail("pkg", "install", errors.New("pkg fail"))
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "pkg", Action: "remove"},
			rbStep("undo-s1", "pkg", "install")), // will fail
		withRollback(plan.PlanStep{Name: "s2", Module: "file", Action: "delete"},
			rbStep("undo-s2", "file", "copy")), // will succeed
	})

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.False(t, res.Success)
	assert.True(t, res.PartialRollback, "some steps succeeded and some failed")
}

func TestPartialRollbackAllFail(t *testing.T) {
	m := NewManager(WithStopOnError(false))
	rec := newExecRecorder()
	rec.setFail("pkg", "install", errors.New("fail"))
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "pkg", Action: "remove"},
			rbStep("undo-s1", "pkg", "install")),
	})

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.False(t, res.Success)
	assert.False(t, res.PartialRollback, "no step succeeded -> not partial")
}

// --- concurrency -----------------------------------------------------------

func TestConcurrencySerial(t *testing.T) {
	m := NewManager(WithConcurrency(1))
	var atomicCount atomic.Int32
	rec := newExecRecorder()
	rec.setDelay("svc", "stop", 10*time.Millisecond)
	fn := func(ctx context.Context, target string, step dsl.Step) error {
		// Track concurrent invocations; with concurrency=1 the high
		// water mark should never exceed 1.
		now := atomicCount.Add(1)
		if now > 1 {
			t.Errorf("concurrency exceeded: %d simultaneous calls", now)
		}
		time.Sleep(10 * time.Millisecond)
		atomicCount.Add(-1)
		return nil
	}
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "svc", Action: "start"},
			rbStep("undo-s1", "svc", "stop")),
	})
	_ = rec
	res := m.Rollback(context.Background(), p, fn)
	require.NotNil(t, res)
	assert.True(t, res.Success)
}

func TestConcurrencyParallel(t *testing.T) {
	m := NewManager(WithConcurrency(4))
	var atomicCount atomic.Int32
	var maxConcurrent atomic.Int32
	fn := func(ctx context.Context, target string, step dsl.Step) error {
		now := atomicCount.Add(1)
		for {
			max := maxConcurrent.Load()
			if now <= max || maxConcurrent.CompareAndSwap(max, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomicCount.Add(-1)
		return nil
	}
	// Build a plan with 4 targets in a single batch.
	p := &plan.Plan{
		ID:           "test",
		WorkflowName: "test",
		Batches: []plan.Batch{{
			Index:   0,
			Targets: []string{"h0", "h1", "h2", "h3"},
			Steps: []plan.PlanStep{
				withRollback(plan.PlanStep{Name: "s1", Module: "svc", Action: "start"},
					rbStep("undo-s1", "svc", "stop")),
			},
		}},
	}

	res := m.Rollback(context.Background(), p, fn)
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.GreaterOrEqual(t, maxConcurrent.Load(), int32(2),
		"with concurrency=4 and 4 targets, at least 2 should run in parallel")
}

// --- context cancellation --------------------------------------------------

func TestContextCancellation(t *testing.T) {
	m := NewManager(WithStopOnError(false))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	rec := newExecRecorder()
	rec.setDelay("svc", "stop", 50*time.Millisecond)
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "svc", Action: "start"},
			rbStep("undo-s1", "svc", "stop")),
	})

	// The execFn here does not check ctx, so cancellation does not stop
	// execution; this test just verifies that a cancelled context does not
	// cause a panic or nil result.
	res := m.Rollback(ctx, p, rec.fn())
	require.NotNil(t, res)
}

// --- result helpers --------------------------------------------------------

func TestIsSkippedPredicate(t *testing.T) {
	assert.True(t, IsSkipped(StepRollbackResult{Skipped: true}))
	assert.False(t, IsSkipped(StepRollbackResult{Skipped: false}))
}

func TestHasErrorPredicate(t *testing.T) {
	// Executed with error.
	assert.True(t, HasError(StepRollbackResult{Error: errors.New("x")}))
	// Executed successfully.
	assert.False(t, HasError(StepRollbackResult{}))
	// Skipped (even with an error field set, which should not happen in
	// practice) is not an error.
	assert.False(t, HasError(StepRollbackResult{Skipped: true, Error: errors.New("x")}))
}

// --- duration --------------------------------------------------------------

func TestRollbackDurationRecorded(t *testing.T) {
	m := NewManager()
	rec := newExecRecorder()
	rec.setDelay("svc", "stop", 10*time.Millisecond)
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "svc", Action: "start"},
			rbStep("undo-s1", "svc", "stop")),
	})

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.Greater(t, res.Duration, time.Duration(0))

	// Per-step durations should also be recorded.
	for _, br := range res.BatchResults {
		for _, tr := range br.TargetResults {
			for _, sr := range tr.StepResults {
				if !sr.Skipped {
					assert.Greater(t, sr.Duration, time.Duration(0))
				}
			}
		}
	}
}

// --- multiple rollback steps per spec --------------------------------------

func TestMultipleRollbackStepsPerSpec(t *testing.T) {
	m := NewManager()
	rec := newExecRecorder()
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "deploy", Module: "svc", Action: "deploy"},
			rbStep("stop-svc", "svc", "stop"),
			rbStep("remove-binary", "file", "delete"),
			rbStep("restore-config", "file", "copy"),
		),
	})

	res := m.Rollback(context.Background(), p, rec.fn())
	require.NotNil(t, res)
	assert.True(t, res.Success)

	// 2 targets × 3 rollback steps = 6 calls.
	assert.Equal(t, 6, rec.count())

	br := res.BatchResults[0]
	for _, tr := range br.TargetResults {
		require.Len(t, tr.StepResults, 3)
		// The rollback steps inside the spec run in declared order.
		assert.Equal(t, "stop-svc", tr.StepResults[0].RollbackStepName)
		assert.Equal(t, "remove-binary", tr.StepResults[1].RollbackStepName)
		assert.Equal(t, "restore-config", tr.StepResults[2].RollbackStepName)
		// All share the same OrigStepName.
		for _, sr := range tr.StepResults {
			assert.Equal(t, "deploy", sr.OrigStepName)
		}
	}
}

// --- zero-value results ----------------------------------------------------

func TestRollbackResultZeroValue(t *testing.T) {
	var r RollbackResult
	assert.False(t, r.Success)
	assert.False(t, r.PartialRollback)
	assert.NoError(t, r.Error)
	assert.Equal(t, time.Duration(0), r.Duration)
}

func TestStepRollbackResultZeroValue(t *testing.T) {
	var sr StepRollbackResult
	assert.False(t, sr.Skipped)
	assert.Empty(t, sr.SkipReason)
	assert.NoError(t, sr.Error)
	assert.Equal(t, time.Duration(0), sr.Duration)
}
