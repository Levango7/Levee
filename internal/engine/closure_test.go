package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/batch"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/lock"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/verify"
)

// --- Test helpers ----------------------------------------------------------

// newTestStore creates a fresh SQLite store backed by a per-test temporary
// file. The store is registered for cleanup via t.Cleanup so each test
// gets an isolated database.
func newTestStore(t *testing.T) state.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "closure-test.db")
	store, err := state.NewSQLiteStore(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestClosureRunner builds a fully-wired ClosureRunner for tests. The
// given gates are registered with the verify.GateManager; pass nil/empty
// for a runner with no gates (all phases pass).
func newTestClosureRunner(t *testing.T, store state.Store, gates ...verify.Gate) *ClosureRunner {
	t.Helper()
	lockStore := lock.NewLockStore(store)
	lm := lock.NewLockManager(lockStore, store)
	gm := verify.NewGateManager()
	for _, g := range gates {
		gm.Register(g)
	}
	// WithWhitelistAll: these tests exercise the full rollback flow with
	// arbitrary module.action pairs, so the manager must not deny them.
	rm := rollback.NewManager(rollback.WithWhitelistAll())
	bc := batch.NewController()
	return NewClosureRunner(store, lm, gm, rm, bc, nil)
}

// newTestLockManager builds a standalone LockManager on the same store,
// for tests that need to pre-acquire or verify locks outside the runner.
func newTestLockManager(t *testing.T, store state.Store) *lock.LockManager {
	t.Helper()
	return lock.NewLockManager(lock.NewLockStore(store), store)
}

// newTestPlan builds a plan with the given batch target distribution.
// Each batch has a single "apply-change" step (pkg.upgrade) with a
// "undo-change" rollback step (pkg.downgrade), so rollback has work
// to do. MaxConcurrency is 1 (serial within each batch) for
// deterministic ordering.
func newTestPlan(batchTargets [][]string) *plan.Plan {
	batches := make([]plan.Batch, len(batchTargets))
	total := 0
	for i, targets := range batchTargets {
		batches[i] = plan.Batch{
			Index:   i,
			Targets: targets,
			Steps: []plan.PlanStep{{
				Name:   "apply-change",
				Module: "pkg",
				Action: "upgrade",
				Rollback: &dsl.RollbackSpec{
					Steps: []dsl.Step{{
						Name:   "undo-change",
						Module: "pkg",
						Action: "downgrade",
					}},
				},
			}},
			MaxConcurrency: 1,
		}
		total += len(targets)
	}
	return &plan.Plan{
		ID:           "plan-test",
		WorkflowName: "test-workflow",
		Batches:      batches,
		TotalTargets: total,
		CreatedAt:    time.Now().UTC(),
	}
}

// mockExecutor is a test stub for rollback.ExecuteFunc. It records every
// call and optionally returns an error based on the configured failOn
// predicate. It respects context cancellation by returning ctx.Err()
// before invoking failOn, unless ignoreCtx is true.
type mockExecutor struct {
	mu        sync.Mutex
	calls     []mockCall
	failOn    func(target, action string) error
	ignoreCtx bool
}

type mockCall struct {
	target string
	action string
	step   string
}

func (m *mockExecutor) exec(ctx context.Context, target string, step dsl.Step) error {
	if !m.ignoreCtx {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.calls = append(m.calls, mockCall{target: target, action: step.Action, step: step.Name})
	failOn := m.failOn
	m.mu.Unlock()
	if failOn != nil {
		return failOn(target, step.Action)
	}
	return nil
}

func (m *mockExecutor) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *mockExecutor) callsFor(action string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if c.action == action {
			n++
		}
	}
	return n
}

// assertLocksReleased tries to re-acquire locks for the given targets.
// It succeeds only when the previous owner has released them.
func assertLocksReleased(t *testing.T, store state.Store, targets ...string) {
	t.Helper()
	lm := newTestLockManager(t, store)
	for _, target := range targets {
		_, err := lm.Acquire(context.Background(), target, "verify-run")
		assert.NoErrorf(t, err, "lock for %s should have been released", target)
	}
}

// --- Tests -----------------------------------------------------------------

// 1. Happy path: plan → apply all succeed → verify all pass → completed.
func TestClosureRunner_HappyPath(t *testing.T) {
	store := newTestStore(t)
	// No gates registered → all phases pass (empty results).
	cr := newTestClosureRunner(t, store)

	p := newTestPlan([][]string{{"host-a", "host-b"}})
	exec := &mockExecutor{}

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err)
	assert.Equal(t, PhaseCompleted, result.Phase)
	assert.Equal(t, "plan-test", result.PlanID)
	assert.NotEmpty(t, result.RunID)
	require.Len(t, result.BatchResults, 1)
	assert.Nil(t, result.BatchResults[0].Error)
	assert.Nil(t, result.RollbackResult)
	// Two targets, one step each → 2 apply calls, 0 rollback calls.
	assert.Equal(t, 2, exec.callsFor("upgrade"))
	assert.Equal(t, 0, exec.callsFor("downgrade"))
}

// 2. Pre-apply failure: pre_apply gate fails → abort, no apply, no locks.
func TestClosureRunner_PreApplyFailure(t *testing.T) {
	store := newTestStore(t)
	preGate := verify.NewNoopGate("pre-fail", verify.PhasePreApply, false)
	cr := newTestClosureRunner(t, store, preGate)

	p := newTestPlan([][]string{{"host-a"}})
	exec := &mockExecutor{}

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.Error(t, err)
	assert.Equal(t, PhaseFailed, result.Phase)
	assert.Empty(t, result.BatchResults) // no batches executed
	assert.Equal(t, 0, exec.callCount()) // no steps executed
	assert.Nil(t, result.RollbackResult) // no rollback needed
	// No locks should have been acquired.
	assertLocksReleased(t, store, "host-a")
}

//  3. Post-batch failure: first batch succeeds, post_batch gate fails →
//     rollback → rolled_back.
func TestClosureRunner_PostBatchFailure(t *testing.T) {
	store := newTestStore(t)
	postBatchGate := verify.NewNoopGate("post-batch-fail", verify.PhasePostBatch, false)
	cr := newTestClosureRunner(t, store, postBatchGate)

	p := newTestPlan([][]string{{"host-a"}, {"host-b"}})
	exec := &mockExecutor{}

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err) // rollback succeeded → no error return
	assert.Equal(t, PhaseRolledBack, result.Phase)
	require.NotNil(t, result.RollbackResult)
	assert.True(t, result.RollbackResult.Success)
	// First batch executed (1 apply call), then post-batch gate failed.
	assert.Equal(t, 1, exec.callsFor("upgrade"))
	// Rollback reversed batch 0 (1 downgrade call).
	assert.Equal(t, 1, exec.callsFor("downgrade"))
	// Locks released despite rollback.
	assertLocksReleased(t, store, "host-a", "host-b")
}

//  4. Post-apply failure: all batches succeed, post_apply gate fails →
//     full rollback → rolled_back.
func TestClosureRunner_PostApplyFailure(t *testing.T) {
	store := newTestStore(t)
	postApplyGate := verify.NewNoopGate("post-apply-fail", verify.PhasePostApply, false)
	cr := newTestClosureRunner(t, store, postApplyGate)

	p := newTestPlan([][]string{{"host-a", "host-b"}})
	exec := &mockExecutor{}

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err)
	assert.Equal(t, PhaseRolledBack, result.Phase)
	require.NotNil(t, result.RollbackResult)
	assert.True(t, result.RollbackResult.Success)
	// Both targets executed (2 apply calls), then 2 rollback calls.
	assert.Equal(t, 2, exec.callsFor("upgrade"))
	assert.Equal(t, 2, exec.callsFor("downgrade"))
	assertLocksReleased(t, store, "host-a", "host-b")
}

//  5. Rollback failure: apply fails → rollback also fails → failed with
//     PartialRollback marker.
func TestClosureRunner_RollbackFailure(t *testing.T) {
	store := newTestStore(t)
	cr := newTestClosureRunner(t, store)

	// Two targets in one batch. Apply fails on both (serial, so host-a
	// fails first and host-b is skipped under PolicyAbort). Rollback
	// succeeds on host-a but fails on host-b → PartialRollback.
	p := newTestPlan([][]string{{"host-a", "host-b"}})
	exec := &mockExecutor{}
	exec.failOn = func(target, action string) error {
		if action == "upgrade" {
			return fmt.Errorf("apply failed on %s", target)
		}
		if action == "downgrade" && target == "host-b" {
			return fmt.Errorf("rollback failed on %s", target)
		}
		return nil
	}

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err) // Run itself does not return error for rollback failure
	assert.Equal(t, PhaseFailed, result.Phase)
	require.NotNil(t, result.RollbackResult)
	assert.False(t, result.RollbackResult.Success)
	assert.True(t, result.RollbackResult.PartialRollback,
		"expected PartialRollback when some rollback steps succeed and some fail")
	assertLocksReleased(t, store, "host-a", "host-b")
}

//  6. Lock conflict: target already locked by another owner → abort with
//     lock error, no apply.
func TestClosureRunner_LockConflict(t *testing.T) {
	store := newTestStore(t)
	cr := newTestClosureRunner(t, store)

	// Pre-acquire a lock on host-a with a different owner.
	lm := newTestLockManager(t, store)
	_, err := lm.Acquire(context.Background(), "host-a", "other-run")
	require.NoError(t, err)

	p := newTestPlan([][]string{{"host-a"}})
	exec := &mockExecutor{}

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.Error(t, err)
	assert.Equal(t, PhaseFailed, result.Phase)
	assert.Empty(t, result.BatchResults)
	assert.Equal(t, 0, exec.callCount())
	assert.Nil(t, result.RollbackResult)
	// The pre-existing lock is still held by other-run.
	ls := lock.NewLockStore(store)
	existing, getErr := ls.Get(context.Background(), "host-a")
	require.NoError(t, getErr)
	require.NotNil(t, existing)
	assert.Equal(t, "other-run", existing.Owner)
}

//  7. Multi-batch partial failure: 3 batches, first 2 succeed, 3rd fails →
//     rollback all executed batches.
func TestClosureRunner_MultiBatchPartialFailure(t *testing.T) {
	store := newTestStore(t)
	cr := newTestClosureRunner(t, store)

	p := newTestPlan([][]string{{"host-a"}, {"host-b"}, {"host-c"}})
	exec := &mockExecutor{}
	// Apply fails on host-c (batch 2, the 3rd batch).
	exec.failOn = func(target, action string) error {
		if action == "upgrade" && target == "host-c" {
			return fmt.Errorf("apply failed on %s", target)
		}
		return nil
	}

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err) // rollback succeeds
	assert.Equal(t, PhaseRolledBack, result.Phase)
	require.NotNil(t, result.RollbackResult)
	assert.True(t, result.RollbackResult.Success)
	// Batches 0, 1, 2 were all attempted (batch 2 failed).
	assert.Len(t, result.BatchResults, 3)
	// 3 apply calls (host-a, host-b, host-c).
	assert.Equal(t, 3, exec.callsFor("upgrade"))
	// Rollback reverses all 3 batches → 3 downgrade calls.
	assert.Equal(t, 3, exec.callsFor("downgrade"))
	assertLocksReleased(t, store, "host-a", "host-b", "host-c")
}

//  8. Context cancellation: ctx cancelled mid-run → graceful abort,
//     locks released.
func TestClosureRunner_ContextCancel(t *testing.T) {
	store := newTestStore(t)
	cr := newTestClosureRunner(t, store)

	p := newTestPlan([][]string{{"host-a"}, {"host-b"}})

	ctx, cancel := context.WithCancel(context.Background())
	var applyCount int32
	// Custom execFn: cancel after the first successful apply, but always
	// return nil so rollback can succeed even with a cancelled ctx.
	execFn := func(ctx context.Context, target string, step dsl.Step) error {
		if step.Action == "upgrade" {
			if atomic.AddInt32(&applyCount, 1) == 1 {
				cancel()
			}
		}
		return nil
	}

	result, _ := cr.Run(ctx, p, execFn)
	// Batch 0 succeeds, ctx cancelled, batch 1 sees ctx.Err() → batch
	// error → rollback triggered. Rollback succeeds (execFn returns nil).
	assert.Equal(t, PhaseRolledBack, result.Phase)
	require.NotNil(t, result.RollbackResult)
	assert.True(t, result.RollbackResult.Success)
	// Locks must have been released despite the cancellation.
	assertLocksReleased(t, store, "host-a", "host-b")
}

// --- Additional edge-case tests -------------------------------------------

// Nil plan returns a failed result without panicking.
func TestClosureRunner_NilPlan(t *testing.T) {
	store := newTestStore(t)
	cr := newTestClosureRunner(t, store)
	exec := &mockExecutor{}

	result, err := cr.Run(context.Background(), nil, exec.exec)
	require.Error(t, err)
	assert.Equal(t, PhaseFailed, result.Phase)
	assert.Empty(t, result.PlanID)
	assert.Nil(t, result.RollbackResult)
}

// Nil execFn is treated as a dry-run: batch execution is a no-op and
// rollback records every step as skipped.
func TestClosureRunner_NilExecFunc(t *testing.T) {
	store := newTestStore(t)
	cr := newTestClosureRunner(t, store)
	p := newTestPlan([][]string{{"host-a"}})

	result, err := cr.Run(context.Background(), p, nil)
	require.NoError(t, err)
	assert.Equal(t, PhaseCompleted, result.Phase)
	// Batch executed with nil execFn → no error (adaptExecFunc returns nil).
	require.Len(t, result.BatchResults, 1)
	assert.Nil(t, result.BatchResults[0].Error)
}

// Empty batches: a plan with no batches completes trivially.
func TestClosureRunner_EmptyBatches(t *testing.T) {
	store := newTestStore(t)
	cr := newTestClosureRunner(t, store)
	p := &plan.Plan{
		ID:           "plan-empty",
		WorkflowName: "empty",
		Batches:      nil,
		TotalTargets: 0,
		CreatedAt:    time.Now().UTC(),
	}
	exec := &mockExecutor{}

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err)
	assert.Equal(t, PhaseCompleted, result.Phase)
	assert.Empty(t, result.BatchResults)
}

// Pre-apply gate passes, post-apply gate passes → completed. This
// verifies that gates are actually consulted in the happy path.
func TestClosureRunner_GatesPass(t *testing.T) {
	store := newTestStore(t)
	preGate := verify.NewNoopGate("pre-ok", verify.PhasePreApply, true)
	postBatchGate := verify.NewNoopGate("post-batch-ok", verify.PhasePostBatch, true)
	postApplyGate := verify.NewNoopGate("post-apply-ok", verify.PhasePostApply, true)
	cr := newTestClosureRunner(t, store, preGate, postBatchGate, postApplyGate)

	p := newTestPlan([][]string{{"host-a"}, {"host-b"}})
	exec := &mockExecutor{}

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err)
	assert.Equal(t, PhaseCompleted, result.Phase)
	// pre-apply (1) + post-batch (2) + post-apply (1) = 4 gate results.
	assert.Len(t, result.VerifyResults, 4)
	for _, gr := range result.VerifyResults {
		assert.True(t, gr.Passed, "gate %s should have passed", gr.Message)
	}
}

// Multiple targets in a single batch all fail apply → rollback attempted
// on all → all rollback steps fail → PhaseFailed, no PartialRollback
// (nothing succeeded).
func TestClosureRunner_AllRollbackStepsFail(t *testing.T) {
	store := newTestStore(t)
	cr := newTestClosureRunner(t, store)

	p := newTestPlan([][]string{{"host-a", "host-b"}})
	exec := &mockExecutor{}
	// Everything fails.
	exec.failOn = func(target, action string) error {
		return errors.New("everything fails")
	}

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err)
	assert.Equal(t, PhaseFailed, result.Phase)
	require.NotNil(t, result.RollbackResult)
	assert.False(t, result.RollbackResult.Success)
	// When all rollback steps fail, PartialRollback is false (nothing
	// succeeded).
	assert.False(t, result.RollbackResult.PartialRollback)
}
