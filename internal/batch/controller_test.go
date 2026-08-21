package batch

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

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/plan"
)

// --- test helpers ----------------------------------------------------------

// makePlan builds a Plan with the given batches. Each batch is a
// (targets, maxConcurrency) pair; every batch shares a single step
// named "s1".
func makePlan(batches ...struct {
	Targets        []string
	MaxConcurrency int
}) *plan.Plan {
	steps := []plan.PlanStep{{Name: "s1", Module: "shell", Action: "exec"}}
	out := &plan.Plan{ID: "plan-test", WorkflowName: "wf-test"}
	total := 0
	for i, b := range batches {
		out.Batches = append(out.Batches, plan.Batch{
			Index:          i,
			Targets:        b.Targets,
			Steps:          steps,
			MaxConcurrency: b.MaxConcurrency,
		})
		total += len(b.Targets)
	}
	out.TotalTargets = total
	out.CreatedAt = time.Now().UTC()
	return out
}

// makePlanSteps builds a Plan whose every batch has the given step names
// and the given targets. Used by step-level tests.
func makePlanSteps(stepNames []string, targets []string, maxConc int) *plan.Plan {
	steps := make([]plan.PlanStep, len(stepNames))
	for i, n := range stepNames {
		steps[i] = plan.PlanStep{Name: n, Module: "shell", Action: "exec"}
	}
	return &plan.Plan{
		ID:           "plan-test",
		WorkflowName: "wf-test",
		TotalTargets: len(targets),
		CreatedAt:    time.Now().UTC(),
		Batches:      []plan.Batch{{Index: 0, Targets: targets, Steps: steps, MaxConcurrency: maxConc}},
	}
}

// noopExec is an ExecuteFunc that always succeeds and records nothing.
func noopExec(_ context.Context, _ plan.Batch, _ string, _ plan.PlanStep) error {
	return nil
}

// countingExec returns an ExecuteFunc that increments a counter on every
// call and sleeps for the given duration to make concurrency observable.
func countingExec(counter *int32, d time.Duration) ExecuteFunc {
	return func(_ context.Context, _ plan.Batch, _ string, _ plan.PlanStep) error {
		atomic.AddInt32(counter, 1)
		if d > 0 {
			time.Sleep(d)
		}
		return nil
	}
}

// errorOnExec returns an ExecuteFunc that fails for targets in the
// bad set and succeeds otherwise.
func errorOnExec(bad map[string]bool, msg string) ExecuteFunc {
	return func(_ context.Context, _ plan.Batch, target string, _ plan.PlanStep) error {
		if bad[target] {
			return fmt.Errorf("%s: %s", msg, target)
		}
		return nil
	}
}

// errorOnStep returns an ExecuteFunc that fails for the given step name
// and succeeds for every other step.
func errorOnStep(badStep string, msg string) ExecuteFunc {
	return func(_ context.Context, _ plan.Batch, _ string, step plan.PlanStep) error {
		if step.Name == badStep {
			return fmt.Errorf("%s: %s", msg, badStep)
		}
		return nil
	}
}

// --- nil-input handling ----------------------------------------------------

func TestExecuteNilPlan(t *testing.T) {
	c := NewController()
	results := c.Execute(context.Background(), nil, noopExec)
	require.Len(t, results, 1)
	assert.Equal(t, -1, results[0].BatchIndex)
	require.Error(t, results[0].Error)
	assert.Contains(t, results[0].Error.Error(), "plan is nil")
}

func TestExecuteNilExecFn(t *testing.T) {
	c := NewController()
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: []string{"h1"}, MaxConcurrency: 1})
	results := c.Execute(context.Background(), p, nil)
	require.Len(t, results, 1)
	assert.Equal(t, -1, results[0].BatchIndex)
	require.Error(t, results[0].Error)
	assert.Contains(t, results[0].Error.Error(), "execFn is nil")
}

// --- single batch, single target ------------------------------------------

func TestSingleBatchSingleTarget(t *testing.T) {
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: []string{"h1"}, MaxConcurrency: 1})

	var calls int32
	c := NewController()
	results := c.Execute(context.Background(), p, countingExec(&calls, 0))

	require.Len(t, results, 1)
	br := results[0]
	assert.Equal(t, 0, br.BatchIndex)
	assert.NoError(t, br.Error)
	require.Len(t, br.TargetResults, 1)
	assert.Equal(t, "h1", br.TargetResults[0].Target)
	assert.NoError(t, br.TargetResults[0].Error)
	require.Len(t, br.TargetResults[0].StepResults, 1)
	assert.Equal(t, "s1", br.TargetResults[0].StepResults[0].StepName)
	assert.NoError(t, br.TargetResults[0].StepResults[0].Error)
	assert.Equal(t, int32(1), calls)
	assert.GreaterOrEqual(t, br.Duration, time.Duration(0))
}

func TestSingleBatchEmptyTargets(t *testing.T) {
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: nil, MaxConcurrency: 1})

	c := NewController()
	results := c.Execute(context.Background(), p, noopExec)
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Error)
	assert.Empty(t, results[0].TargetResults)
}

func TestEmptyPlanBatches(t *testing.T) {
	p := &plan.Plan{ID: "p", WorkflowName: "wf"}
	c := NewController()
	results := c.Execute(context.Background(), p, noopExec)
	assert.Empty(t, results)
}

// --- single batch, multiple targets (concurrency) -------------------------

func TestSingleBatchMultipleTargetsConcurrent(t *testing.T) {
	targets := []string{"h1", "h2", "h3", "h4"}
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: targets, MaxConcurrency: 4})

	// Each call sleeps 50ms; with concurrency 4 the whole batch should
	// finish in ~50ms, well under the 150ms a serial run would take.
	var calls int32
	c := NewController()
	start := time.Now()
	results := c.Execute(context.Background(), p, countingExec(&calls, 50*time.Millisecond))
	elapsed := time.Since(start)

	require.Len(t, results, 1)
	br := results[0]
	assert.NoError(t, br.Error)
	require.Len(t, br.TargetResults, len(targets))
	assert.Equal(t, int32(len(targets)), calls)

	// Targets ran concurrently: total time is closer to one sleep than
	// to four sleeps.
	assert.Less(t, elapsed, 150*time.Millisecond,
		"targets should run concurrently, elapsed=%v", elapsed)

	// TargetResults preserve plan order regardless of completion order.
	for i, want := range targets {
		assert.Equal(t, want, br.TargetResults[i].Target)
	}
}

func TestSingleBatchMultipleTargetsResultsInPlanOrder(t *testing.T) {
	// Stagger the per-target sleep so completion order differs from plan
	// order; the results must still come back in plan order.
	targets := []string{"h1", "h2", "h3"}
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: targets, MaxConcurrency: 3})

	execFn := func(_ context.Context, _ plan.Batch, target string, _ plan.PlanStep) error {
		// h1 sleeps longest, h3 shortest, so completion order is h3,h2,h1.
		switch target {
		case "h1":
			time.Sleep(60 * time.Millisecond)
		case "h2":
			time.Sleep(30 * time.Millisecond)
		case "h3":
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}

	c := NewController()
	results := c.Execute(context.Background(), p, execFn)
	require.Len(t, results, 1)
	br := results[0]
	require.Len(t, br.TargetResults, 3)
	assert.Equal(t, "h1", br.TargetResults[0].Target)
	assert.Equal(t, "h2", br.TargetResults[1].Target)
	assert.Equal(t, "h3", br.TargetResults[2].Target)
}

// --- concurrency cap (MaxConcurrency) -------------------------------------

func TestConcurrencyCapRespected(t *testing.T) {
	targets := []string{"h1", "h2", "h3", "h4", "h5", "h6"}
	// MaxConcurrency = 2: at most 2 targets run at the same time.
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: targets, MaxConcurrency: 2})

	var current int32
	var maxObserved int32
	execFn := func(_ context.Context, _ plan.Batch, _ string, _ plan.PlanStep) error {
		cur := atomic.AddInt32(&current, 1)
		// Track the high-water mark.
		for {
			m := atomic.LoadInt32(&maxObserved)
			if cur <= m || atomic.CompareAndSwapInt32(&maxObserved, m, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		return nil
	}

	c := NewController()
	results := c.Execute(context.Background(), p, execFn)
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Error)

	assert.LessOrEqual(t, atomic.LoadInt32(&maxObserved), int32(2),
		"MaxConcurrency=2 must cap in-flight targets, observed=%d",
		atomic.LoadInt32(&maxObserved))
	assert.Equal(t, int32(2), atomic.LoadInt32(&maxObserved),
		"with 6 targets and cap 2 the high-water mark should reach 2")
}

func TestConcurrencyCapZeroMeansUnlimited(t *testing.T) {
	targets := []string{"h1", "h2", "h3", "h4", "h5"}
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: targets, MaxConcurrency: 0})

	var current int32
	var maxObserved int32
	execFn := func(_ context.Context, _ plan.Batch, _ string, _ plan.PlanStep) error {
		cur := atomic.AddInt32(&current, 1)
		for {
			m := atomic.LoadInt32(&maxObserved)
			if cur <= m || atomic.CompareAndSwapInt32(&maxObserved, m, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		return nil
	}

	c := NewController()
	results := c.Execute(context.Background(), p, execFn)
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Error)
	// MaxConcurrency=0 means unlimited: all 5 targets run at once.
	assert.Equal(t, int32(5), atomic.LoadInt32(&maxObserved))
}

// --- multiple batches, serial execution ------------------------------------

func TestMultipleBatchesSerial(t *testing.T) {
	p := makePlan(
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b1h1", "b1h2"}, MaxConcurrency: 2},
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b2h1", "b2h2"}, MaxConcurrency: 2},
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b3h1"}, MaxConcurrency: 1},
	)

	// Record execution order across batches. Each call appends its batch
	// index to a shared log guarded by a mutex; batch 1 entries must all
	// precede batch 2 entries, which must all precede batch 3 entries.
	var mu sync.Mutex
	log := []int{}
	execFn := func(_ context.Context, b plan.Batch, _ string, _ plan.PlanStep) error {
		mu.Lock()
		log = append(log, b.Index)
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		return nil
	}

	c := NewController()
	results := c.Execute(context.Background(), p, execFn)
	require.Len(t, results, 3)
	for i, br := range results {
		assert.Equal(t, i, br.BatchIndex)
		assert.NoError(t, br.Error)
	}

	// Verify serial ordering: all batch-0 entries first, then batch-1,
	// then batch-2.
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, log, 5)
	for _, idx := range log[:2] {
		assert.Equal(t, 0, idx, "batch 0 must run first, log=%v", log)
	}
	for _, idx := range log[2:4] {
		assert.Equal(t, 1, idx, "batch 1 must run second, log=%v", log)
	}
	assert.Equal(t, 2, log[4], "batch 2 must run last, log=%v", log)
}

func TestMultipleBatchesDoNotOverlap(t *testing.T) {
	// Stronger guarantee than ordering: batches must not overlap in
	// time. We track the in-flight count and assert it never exceeds
	// the batch's own MaxConcurrency, which would only be possible if
	// two batches ran concurrently.
	p := makePlan(
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"a", "b"}, MaxConcurrency: 2},
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"c", "d"}, MaxConcurrency: 2},
	)

	var current int32
	var maxObserved int32
	execFn := func(_ context.Context, _ plan.Batch, _ string, _ plan.PlanStep) error {
		cur := atomic.AddInt32(&current, 1)
		for {
			m := atomic.LoadInt32(&maxObserved)
			if cur <= m || atomic.CompareAndSwapInt32(&maxObserved, m, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		return nil
	}

	c := NewController()
	results := c.Execute(context.Background(), p, execFn)
	require.Len(t, results, 2)
	// Each batch has MaxConcurrency=2, so if batches never overlap the
	// high-water mark is exactly 2. If they overlapped it would be 4.
	assert.Equal(t, int32(2), atomic.LoadInt32(&maxObserved),
		"batches must not overlap, maxObserved=%d", atomic.LoadInt32(&maxObserved))
}

// --- inter-batch delay -----------------------------------------------------

func TestInterBatchDelay(t *testing.T) {
	p := makePlan(
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"h1"}, MaxConcurrency: 1},
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"h2"}, MaxConcurrency: 1},
	)

	delay := 80 * time.Millisecond
	c := NewController(WithInterBatchDelay(delay))
	start := time.Now()
	results := c.Execute(context.Background(), p, noopExec)
	elapsed := time.Since(start)

	require.Len(t, results, 2)
	assert.NoError(t, results[0].Error)
	assert.NoError(t, results[1].Error)
	// Two trivial batches plus an 80ms delay: total >= 80ms.
	assert.GreaterOrEqual(t, elapsed, delay,
		"inter-batch delay should be observed, elapsed=%v", elapsed)
	// And not wildly more (no extra delays between steps/targets).
	assert.Less(t, elapsed, delay+100*time.Millisecond,
		"only one delay expected, elapsed=%v", elapsed)
}

func TestInterBatchDelayNotAppliedAfterLastBatch(t *testing.T) {
	p := makePlan(
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"h1"}, MaxConcurrency: 1},
	)
	c := NewController(WithInterBatchDelay(200 * time.Millisecond))
	start := time.Now()
	_ = c.Execute(context.Background(), p, noopExec)
	elapsed := time.Since(start)
	// Single batch: no delay at all.
	assert.Less(t, elapsed, 100*time.Millisecond,
		"no delay after the last batch, elapsed=%v", elapsed)
}

func TestInterBatchDelayZeroMeansNoDelay(t *testing.T) {
	p := makePlan(
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"h1"}, MaxConcurrency: 1},
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"h2"}, MaxConcurrency: 1},
	)
	c := NewController(WithInterBatchDelay(0))
	start := time.Now()
	_ = c.Execute(context.Background(), p, noopExec)
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 50*time.Millisecond,
		"zero delay should not slow execution, elapsed=%v", elapsed)
}

// --- error policy: in-batch (target) ---------------------------------------

func TestInBatchErrorPolicyAbortCancelsRemainingTargets(t *testing.T) {
	// Under PolicyAbort (default), a failing target cancels the rest of
	// its batch. We make h2 fail fast; h1 and h3 sleep long enough that
	// they would still be running when h2 fails. With cancellation they
	// should return early with a context error, so their sleep must NOT
	// complete.
	targets := []string{"h1", "h2", "h3"}
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: targets, MaxConcurrency: 3})

	execFn := func(ctx context.Context, _ plan.Batch, target string, _ plan.PlanStep) error {
		if target == "h2" {
			return errors.New("boom")
		}
		// Long sleep; if not cancelled this dominates the test runtime.
		select {
		case <-time.After(500 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	c := NewController() // default PolicyAbort
	start := time.Now()
	results := c.Execute(context.Background(), p, execFn)
	elapsed := time.Since(start)

	require.Len(t, results, 1)
	br := results[0]
	require.Error(t, br.Error)

	// h2 failed; h1/h3 were cancelled well before their 500ms sleep.
	assert.Less(t, elapsed, 300*time.Millisecond,
		"abort policy should cancel in-flight targets, elapsed=%v", elapsed)

	// At least one target reported an error (h2 for sure; h1/h3 may
	// have either the ctx error or nil depending on timing).
	failedCount := 0
	for _, tr := range br.TargetResults {
		if tr.Error != nil {
			failedCount++
		}
	}
	assert.GreaterOrEqual(t, failedCount, 1)
}

func TestInBatchErrorPolicyContinueLetsTargetsFinish(t *testing.T) {
	targets := []string{"h1", "h2", "h3"}
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: targets, MaxConcurrency: 3})

	var calls int32
	execFn := func(_ context.Context, _ plan.Batch, target string, _ plan.PlanStep) error {
		atomic.AddInt32(&calls, 1)
		if target == "h2" {
			return errors.New("boom")
		}
		time.Sleep(20 * time.Millisecond)
		return nil
	}

	c := NewController(WithTargetErrorPolicy(PolicyContinue))
	results := c.Execute(context.Background(), p, execFn)
	require.Len(t, results, 1)
	br := results[0]

	// All three targets ran despite h2 failing.
	assert.Equal(t, int32(3), calls)
	require.Error(t, br.Error) // batch error is h2's error

	// h1 and h3 succeeded; only h2 failed.
	for _, tr := range br.TargetResults {
		switch tr.Target {
		case "h2":
			assert.Error(t, tr.Error)
		default:
			assert.NoError(t, tr.Error)
		}
	}
}

// --- error policy: step-level (within a target) ---------------------------

func TestStepErrorPolicyAbortStopsLaterSteps(t *testing.T) {
	// Under PolicyAbort (default), a failing step stops the target's
	// later steps. Step s2 fails, so s3 must never run.
	p := makePlanSteps([]string{"s1", "s2", "s3"}, []string{"h1"}, 1)

	var calls int32
	execFn := func(_ context.Context, _ plan.Batch, _ string, step plan.PlanStep) error {
		atomic.AddInt32(&calls, 1)
		if step.Name == "s2" {
			return errors.New("step failed")
		}
		return nil
	}

	c := NewController() // default PolicyAbort
	results := c.Execute(context.Background(), p, execFn)
	require.Len(t, results, 1)
	br := results[0]
	require.Error(t, br.Error)
	require.Len(t, br.TargetResults, 1)
	tr := br.TargetResults[0]
	require.Error(t, tr.Error)
	// s1 and s2 ran (2 calls); s3 was skipped.
	assert.Equal(t, int32(2), calls)
	require.Len(t, tr.StepResults, 2)
	assert.Equal(t, "s1", tr.StepResults[0].StepName)
	assert.NoError(t, tr.StepResults[0].Error)
	assert.Equal(t, "s2", tr.StepResults[1].StepName)
	assert.Error(t, tr.StepResults[1].Error)
}

func TestStepErrorPolicyContinueRunsAllSteps(t *testing.T) {
	p := makePlanSteps([]string{"s1", "s2", "s3"}, []string{"h1"}, 1)

	var calls int32
	execFn := func(_ context.Context, _ plan.Batch, _ string, step plan.PlanStep) error {
		atomic.AddInt32(&calls, 1)
		if step.Name == "s2" {
			return errors.New("step failed")
		}
		return nil
	}

	c := NewController(WithTargetErrorPolicy(PolicyContinue))
	results := c.Execute(context.Background(), p, execFn)
	require.Len(t, results, 1)
	br := results[0]
	require.Error(t, br.Error)
	require.Len(t, br.TargetResults, 1)
	tr := br.TargetResults[0]
	// All three steps ran despite s2 failing.
	assert.Equal(t, int32(3), calls)
	require.Len(t, tr.StepResults, 3)
	assert.NoError(t, tr.StepResults[0].Error) // s1
	assert.Error(t, tr.StepResults[1].Error)   // s2
	assert.NoError(t, tr.StepResults[2].Error) // s3
}

// --- error policy: inter-batch --------------------------------------------

func TestInterBatchErrorPolicyAbortStopsLaterBatches(t *testing.T) {
	p := makePlan(
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b1h1"}, MaxConcurrency: 1},
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b2h1"}, MaxConcurrency: 1},
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b3h1"}, MaxConcurrency: 1},
	)

	var calls int32
	execFn := func(_ context.Context, b plan.Batch, target string, _ plan.PlanStep) error {
		atomic.AddInt32(&calls, 1)
		if b.Index == 0 {
			return errors.New("batch 0 failed")
		}
		return nil
	}

	c := NewController() // default PolicyAbort
	results := c.Execute(context.Background(), p, execFn)

	// Only batch 0 ran; batches 1 and 2 were skipped.
	require.Len(t, results, 1)
	assert.Equal(t, 0, results[0].BatchIndex)
	require.Error(t, results[0].Error)
	assert.Equal(t, int32(1), calls)
}

func TestInterBatchErrorPolicyContinueRunsAllBatches(t *testing.T) {
	p := makePlan(
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b1h1"}, MaxConcurrency: 1},
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b2h1"}, MaxConcurrency: 1},
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b3h1"}, MaxConcurrency: 1},
	)

	var calls int32
	execFn := func(_ context.Context, b plan.Batch, _ string, _ plan.PlanStep) error {
		atomic.AddInt32(&calls, 1)
		if b.Index == 0 {
			return errors.New("batch 0 failed")
		}
		return nil
	}

	c := NewController(WithBatchErrorPolicy(PolicyContinue))
	results := c.Execute(context.Background(), p, execFn)

	// All three batches ran despite batch 0 failing.
	require.Len(t, results, 3)
	assert.Equal(t, int32(3), calls)
	require.Error(t, results[0].Error) // batch 0 failed
	assert.NoError(t, results[1].Error)
	assert.NoError(t, results[2].Error)
}

// --- context cancellation --------------------------------------------------

func TestContextCancelledBeforeStart(t *testing.T) {
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: []string{"h1"}, MaxConcurrency: 1})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Execute

	c := NewController()
	results := c.Execute(ctx, p, noopExec)
	require.Len(t, results, 1)
	require.Error(t, results[0].Error)
	assert.Contains(t, results[0].Error.Error(), "skipped")
}

func TestContextCancelledMidBatch(t *testing.T) {
	// One target cancels the context from inside its execFn; the
	// remaining targets must observe the cancellation and return
	// promptly instead of hanging.
	targets := []string{"h1", "h2", "h3", "h4"}
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: targets, MaxConcurrency: 4})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	execFn := func(execCtx context.Context, _ plan.Batch, target string, _ plan.PlanStep) error {
		if target == "h1" {
			cancel() // cancel from within the first target
			return nil
		}
		// Other targets wait on the per-call context (which is a child
		// of ctx and therefore cancelled when h1 calls cancel).
		<-execCtx.Done()
		return execCtx.Err()
	}

	c := NewController(WithTargetErrorPolicy(PolicyContinue))
	done := make(chan []*BatchResult, 1)
	go func() {
		done <- c.Execute(ctx, p, execFn)
	}()
	select {
	case results := <-done:
		require.Len(t, results, 1)
		// Some targets were cancelled; the batch reports an error.
		require.Error(t, results[0].Error)
	case <-time.After(3 * time.Second):
		t.Fatal("Execute hung after mid-batch context cancellation")
	}
}

// --- concurrency limiter integration ---------------------------------------

func TestConcurrencyLimiterIntegration(t *testing.T) {
	targets := []string{"h1", "h2", "h3", "h4", "h5"}
	// Batch allows 5 concurrent, but the limiter caps global to 2.
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: targets, MaxConcurrency: 5})

	limiter := channel.NewLimiter(2, 0, 0, 0) // global cap 2, no timeout
	defer limiter.Close()

	var current int32
	var maxObserved int32
	execFn := func(_ context.Context, _ plan.Batch, _ string, _ plan.PlanStep) error {
		cur := atomic.AddInt32(&current, 1)
		for {
			m := atomic.LoadInt32(&maxObserved)
			if cur <= m || atomic.CompareAndSwapInt32(&maxObserved, m, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		return nil
	}

	c := NewController(WithConcurrencyLimiter(limiter))
	results := c.Execute(context.Background(), p, execFn)
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Error)
	// Limiter global cap is 2, so at most 2 targets ran at once even
	// though the batch allowed 5.
	assert.LessOrEqual(t, atomic.LoadInt32(&maxObserved), int32(2),
		"limiter should cap concurrency to 2, observed=%d",
		atomic.LoadInt32(&maxObserved))
	assert.Equal(t, int32(2), atomic.LoadInt32(&maxObserved))
}

func TestConcurrencyLimiterClosedReturnsError(t *testing.T) {
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: []string{"h1"}, MaxConcurrency: 1})

	limiter := channel.NewLimiter(1, 0, 0, 0)
	limiter.Close() // close before use

	c := NewController(WithConcurrencyLimiter(limiter))
	results := c.Execute(context.Background(), p, noopExec)
	require.Len(t, results, 1)
	require.Error(t, results[0].Error)
	assert.Contains(t, results[0].Error.Error(), "limiter")
}

// --- options ---------------------------------------------------------------

func TestErrorPolicyString(t *testing.T) {
	assert.Equal(t, "continue", PolicyContinue.String())
	assert.Equal(t, "abort", PolicyAbort.String())
	assert.Contains(t, ErrorPolicy(42).String(), "unknown")
}

func TestNewControllerDefaults(t *testing.T) {
	c := NewController()
	assert.Equal(t, PolicyAbort, c.batchErrorPolicy)
	assert.Equal(t, PolicyAbort, c.targetErrorPolicy)
	assert.Equal(t, time.Duration(0), c.interBatchDelay)
	assert.Nil(t, c.limiter)
}

func TestNewControllerWithOptions(t *testing.T) {
	limiter := channel.NewLimiter(1, 0, 0, 0)
	defer limiter.Close()
	c := NewController(
		WithInterBatchDelay(42*time.Millisecond),
		WithBatchErrorPolicy(PolicyContinue),
		WithTargetErrorPolicy(PolicyContinue),
		WithConcurrencyLimiter(limiter),
	)
	assert.Equal(t, 42*time.Millisecond, c.interBatchDelay)
	assert.Equal(t, PolicyContinue, c.batchErrorPolicy)
	assert.Equal(t, PolicyContinue, c.targetErrorPolicy)
	assert.Same(t, limiter, c.limiter)
}

// --- combined behaviour ----------------------------------------------------

func TestMultipleBatchesWithErrorAndDelay(t *testing.T) {
	// Batch 0 fails on one target; with PolicyContinue at both levels
	// and a 30ms inter-batch delay, all three batches run and the total
	// time reflects two delays.
	p := makePlan(
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b1h1", "b1h2"}, MaxConcurrency: 2},
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b2h1"}, MaxConcurrency: 1},
		struct {
			Targets        []string
			MaxConcurrency int
		}{Targets: []string{"b3h1"}, MaxConcurrency: 1},
	)

	execFn := errorOnExec(map[string]bool{"b1h1": true}, "fail")

	c := NewController(
		WithInterBatchDelay(30*time.Millisecond),
		WithBatchErrorPolicy(PolicyContinue),
		WithTargetErrorPolicy(PolicyContinue),
	)
	start := time.Now()
	results := c.Execute(context.Background(), p, execFn)
	elapsed := time.Since(start)

	require.Len(t, results, 3)
	require.Error(t, results[0].Error) // b1h1 failed
	assert.NoError(t, results[1].Error)
	assert.NoError(t, results[2].Error)
	// Two inter-batch delays between three batches.
	assert.GreaterOrEqual(t, elapsed, 60*time.Millisecond,
		"two 30ms delays expected, elapsed=%v", elapsed)
}
