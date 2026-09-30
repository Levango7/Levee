package engine

// run_snapshot_hook_test.go — run 级快照基线在闭包执行器上的契约。
//
// 三条性质，缺一条这个原语就不成立：
//
//	① **一次性**：一次 run 一次采集、一次恢复，哪怕计划有多个 target、
//	   多个 batch、多个 step。把「每个 step 一份前镜像」当成 run 级基线
//	   正是 roadmap 里方案 B 明确拒绝的那个语义错误。
//	② **位置**：采集在首个 batch 之前（此时零改动），恢复在补偿之后
//	   （否则 step 级恢复会覆盖 run 级基线，而基线才代表 run 的前状态）。
//	③ **fail-closed**：声明了基线却没有装采集器 → 在任何目标被碰之前
//	  拒绝这次 run。这与 step 级快照「没装就是 no-op」刻意相反。

import (
	"context"
	"sync"
	"testing"

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

// recordingRunSnapshotter records capture/restore calls and, crucially, what
// the executor had already done at the moment of the call. That is what turns
// "capture was called" into "capture happened before any mutation".
type recordingRunSnapshotter struct {
	mu sync.Mutex

	captures []runSnapshotCall
	restores []runSnapshotCall

	captureErr error
	restoreErr error

	// execCallsAtCapture / execCallsAtRestore snapshot the executor's call
	// count at hook time.
	execCallsAtCapture int
	execCallsAtRestore int
	exec               *mockExecutor
}

type runSnapshotCall struct {
	runID   string
	targets []string
	paths   []string
}

func (r *recordingRunSnapshotter) CaptureRun(_ context.Context, runID string, targets []string, spec *dsl.RunSnapshotSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.captures = append(r.captures, runSnapshotCall{runID: runID, targets: append([]string(nil), targets...), paths: spec.Paths})
	if r.exec != nil {
		r.execCallsAtCapture = r.exec.callCount()
	}
	return r.captureErr
}

func (r *recordingRunSnapshotter) RestoreRun(_ context.Context, runID string, targets []string, spec *dsl.RunSnapshotSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restores = append(r.restores, runSnapshotCall{runID: runID, targets: append([]string(nil), targets...), paths: spec.Paths})
	if r.exec != nil {
		r.execCallsAtRestore = r.exec.callCount()
	}
	return r.restoreErr
}

// newRunnerForRunSnapshot builds a runner with a run-level snapshotter
// attached. A nil rs installs none — the fail-closed scenario.
func newRunnerForRunSnapshot(t *testing.T, store state.Store, rs RunSnapshotter) *ClosureRunner {
	t.Helper()
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("post-apply-fail", verify.PhasePostApply, false))
	var opts []ClosureOption
	if rs != nil {
		opts = append(opts, WithRunSnapshotter(rs))
	}
	return NewClosureRunner(store,
		lock.NewLockManager(lock.NewLockStore(store), store), gm,
		rollback.NewManager(rollback.WithWhitelistAll()), batch.NewController(),
		nil, opts...)
}

func planWithRunSnapshot(batches [][]string) *plan.Plan {
	p := newTestPlan(batches)
	p.Rollback = &dsl.RollbackSpec{OnFailure: dsl.RollbackOnFailureAuto}
	p.RunSnapshot = &dsl.RunSnapshotSpec{Scope: "run", Paths: []string{"/etc/app.conf"}}
	return p
}

// ① 一次性 + ② 位置：多 target 多 batch 仍然只采一次，且此刻零步骤执行过。
func TestRunSnapshotCapturedOnceBeforeAnyMutation(t *testing.T) {
	store := newTestStore(t)
	exec := &mockExecutor{}
	rs := &recordingRunSnapshotter{exec: exec}
	cr := newRunnerForRunSnapshot(t, store, rs)

	p := planWithRunSnapshot([][]string{{"host-a"}, {"host-b"}})

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err)
	require.Equal(t, PhaseRolledBack, result.Phase,
		"this fixture's post-apply gate fails, so a rollback must have happened")

	require.Len(t, rs.captures, 1, "the baseline is captured once per run, not once per target/batch/step")
	assert.ElementsMatch(t, []string{"host-a", "host-b"}, rs.captures[0].targets,
		"one capture covers every target")
	assert.Equal(t, []string{"/etc/app.conf"}, rs.captures[0].paths)
	assert.Equal(t, 0, rs.execCallsAtCapture,
		"the baseline must be captured before the first mutation, not after it")

	require.Len(t, rs.restores, 1, "a rolled-back run restores its baseline exactly once")
	assert.ElementsMatch(t, []string{"host-a", "host-b"}, rs.restores[0].targets)
	assert.Equal(t, rs.captures[0].runID, rs.restores[0].runID,
		"capture and restore must address the same run")
}

// ② 位置（另一半）：恢复发生在补偿之后，因此 run 的前状态是最后写入的。
func TestRunSnapshotRestoredAfterCompensations(t *testing.T) {
	store := newTestStore(t)
	exec := &mockExecutor{}
	rs := &recordingRunSnapshotter{exec: exec}
	cr := newRunnerForRunSnapshot(t, store, rs)

	p := planWithRunSnapshot([][]string{{"host-a"}})

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err)
	require.Len(t, rs.restores, 1)
	require.NotNil(t, result.RollbackResult)

	downgrades := exec.callsFor("downgrade")
	upgrades := exec.callsFor("upgrade")
	require.Positive(t, downgrades, "this fixture must actually compensate something")
	require.Positive(t, upgrades)
	assert.Equal(t, upgrades+downgrades, rs.execCallsAtRestore,
		"the baseline is written back only after the forward steps AND their compensations "+
			"are done — restoring earlier would let a step-level restore win over the run's "+
			"own pre-state")
}

// ③ fail-closed：声明了基线却没装采集器 → 任何目标被碰之前就拒绝。
func TestRunSnapshotDeclaredButUnwiredRefusesTheRun(t *testing.T) {
	store := newTestStore(t)
	exec := &mockExecutor{}
	cr := newRunnerForRunSnapshot(t, store, nil) // nothing installed

	p := planWithRunSnapshot([][]string{{"host-a"}, {"host-b"}})

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.Error(t, err, "a declared baseline with no recorder must not run")
	assert.Equal(t, PhaseFailed, result.Phase)
	assert.Zero(t, exec.callCount(), "no target may be mutated when the baseline cannot be recorded")
	assert.Contains(t, err.Error(), "run snapshotter is installed")
	assertLocksReleased(t, store, "host-a", "host-b")
}

// 未声明基线时整个机制是 no-op：既不采集也不恢复，也不改变既有行为。
func TestRunSnapshotAbsentIsNoOp(t *testing.T) {
	store := newTestStore(t)
	exec := &mockExecutor{}
	rs := &recordingRunSnapshotter{exec: exec}
	cr := newRunnerForRunSnapshot(t, store, rs)

	p := newTestPlan([][]string{{"host-a"}})
	p.Rollback = &dsl.RollbackSpec{OnFailure: dsl.RollbackOnFailureAuto}

	_, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err)
	assert.Empty(t, rs.captures)
	assert.Empty(t, rs.restores)
}

// 恢复失败必须有自己的字段，而不是把 run 的结论改写掉。
func TestRunSnapshotRestoreFailureIsReportedSeparately(t *testing.T) {
	store := newTestStore(t)
	exec := &mockExecutor{}
	rs := &recordingRunSnapshotter{exec: exec, restoreErr: assert.AnError}
	cr := newRunnerForRunSnapshot(t, store, rs)

	p := planWithRunSnapshot([][]string{{"host-a"}})

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.NoError(t, err, "a failed baseline restore must not fail the run")
	assert.Equal(t, PhaseRolledBack, result.Phase,
		"the compensation verdict is the ledger's, not the baseline's")
	assert.ErrorIs(t, result.RunSnapshotRestoreError, assert.AnError)
}
