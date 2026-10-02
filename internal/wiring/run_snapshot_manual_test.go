package wiring

// run_snapshot_manual_test.go — 手动回滚路径恢复 run 级基线。
//
// 这是撤回 LE102 的依据。LE102 原本拒绝 `snapshot` + `on_failure: manual`，
// 因为手动路径（RollbackChange / `levee rollback`）当时不恢复基线——采了没人
// 用的基线比没有基线更糟。现在这条路径接上了同一个 runRemoteSnapshotter，
// 因此该组合合法，这条测试就是它合法的证据。
//
// 断言两件事，缺一件都不够：① 基线真的被恢复（远端收到了上传）；② 恢复发生在
// 补偿**之后**——否则 step 级恢复会盖掉 run 的前状态。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/state"
)

const manualBaselineYAML = `name: manual-baseline
target:
  type: host
  query: "env=test"
steps:
  - name: work
    action: shell.exec
    args:
      cmd: work-command
    rollback:
      strategy: undo-action
      steps:
        - name: undo
          action: shell.exec
          args:
            cmd: undo-command
snapshot:
  scope: run
  paths:
    - /etc/app.conf
`

// TestRollbackChangeRestoresRunBaseline drives the real manual path end to end:
// plan carries a run baseline, forward evidence exists, rollback compensates
// and then restores the baseline.
func TestRollbackChangeRestoresRunBaseline(t *testing.T) {
	rec := &loopRecorder{}
	snapDir := t.TempDir()
	e, store := newLoopEngine(t, rec, WithSnapshotDir(snapDir))
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-manual-baseline", manualBaselineYAML)
	planAndPersist(t, e, store, "run-manual-baseline", []string{"web-1"})

	// The plan must actually carry the declaration, otherwise this test
	// would pass for the wrong reason (no baseline → nothing to restore).
	p, err := e.loadStoredPlan(context.Background(), "run-manual-baseline")
	require.NoError(t, err)
	require.NotNil(t, p.RunSnapshot, "the stored plan must carry the run baseline")
	assert.Equal(t, "run", p.RunSnapshot.Scope)
	assert.Equal(t, []string{"/etc/app.conf"}, p.RunSnapshot.Paths)

	// Capture a baseline the way the closure path would, so the manual
	// rollback has something to restore. Keyed by the CHANGE id, which is
	// exactly the property the manual path depends on (it knows no run id).
	require.NoError(t, seedRunBaseline(t, snapDir, "run-manual-baseline", "web-1", p.RunSnapshot))

	// Forward evidence so the ledger compensates the step. The batch row is
	// not optional: steps carry a FK to batches, and the manual path derives
	// its ledger from persisted step evidence.
	ctx := context.Background()
	now := utcNowStub()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID:         "bat-manual-baseline",
		RunID:      "run-manual-baseline",
		BatchNo:    1,
		Status:     "completed",
		TotalHosts: 1,
		Succeeded:  1,
		Failed:     0,
		StartedAt:  &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID:       "stp-manual-baseline-1",
		RunID:    "run-manual-baseline",
		BatchID:  "bat-manual-baseline",
		Host:     "web-1",
		StepName: "work",
		Action:   "shell.exec",
		Status:   "success",
	}))

	_, hosts, err := e.rollbackChange(context.Background(), "run-manual-baseline", "", true)
	require.NoError(t, err, "manual rollback with a restorable baseline must succeed")
	assert.Contains(t, hosts, "web-1")

	cmds := rec.snapshotCmds()
	assert.Contains(t, cmds, "web-1\x00undo-command", "the step must be compensated")

	// The baseline must actually land on the target, with its real payload —
	// not merely be attempted. The recorded size is what distinguishes "the
	// pre-run content was written back" from "an empty file was created".
	//
	// This assertion is also what pins the ORDER: uploads are only ever
	// issued from restoreRunBaseline, which the manual path calls after
	// RollbackWithLedger has returned. (Comparing positions across the
	// recorded-command and recorded-upload slices would prove nothing — they
	// are separate ledgers.)
	ups := rec.uploadsTo("web-1", "/etc/app.conf")
	require.Len(t, ups, 1, "the run baseline must be restored exactly once on the target")
	assert.Equal(t, int64(len("baseline-content-for-/etc/app.conf")), ups[0].size,
		"the restored content must be the captured pre-run baseline")
}

// TestRollbackChangeManualPolicyWithBaselineCompiles is the compile-side half
// of the same withdrawal: the document that LE102 used to reject must now be
// accepted, and its declaration must survive parsing.
func TestRollbackChangeManualPolicyWithBaselineCompiles(t *testing.T) {
	const manualPolicyYAML = `name: manual-policy-baseline
target:
  type: host
  hosts: [web-1]
steps:
  - name: work
    action: shell.exec
    args:
      cmd: work-command
rollback:
  on_failure: manual
snapshot:
  scope: run
  paths:
    - /etc/app.conf
`
	wf, err := dsl.NewParser().ParseBytes([]byte(manualPolicyYAML))
	require.NoError(t, err)
	assert.Empty(t, dsl.NewValidator().Validate(wf),
		"snapshot + on_failure: manual is legal now that the manual path restores it")
	require.NotNil(t, wf.Snapshot)
	assert.Equal(t, dsl.RunSnapshotScopeRun, wf.Snapshot.Scope)
	require.NotNil(t, wf.Rollback)
	assert.Equal(t, dsl.RollbackOnFailureManual, wf.Rollback.OnFailure)
}

// TestManualRollbackWithoutSnapshotStoreDoesNotClaimARestore pins the guard:
// with no snapshot store wired there is nothing to restore, and the path must
// neither attempt it nor report having done it. (An unwired store must not
// fail a manual rollback either — the compensations are what the operator
// asked for.)
func TestManualRollbackWithoutSnapshotStoreDoesNotClaimARestore(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec) // no WithSnapshotDir
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-manual-nosnap", manualBaselineYAML)
	planAndPersist(t, e, store, "run-manual-nosnap", []string{"web-1"})
	ctx := context.Background()
	now := utcNowStub()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID:         "bat-manual-nosnap",
		RunID:      "run-manual-nosnap",
		BatchNo:    1,
		Status:     "completed",
		TotalHosts: 1,
		Succeeded:  1,
		Failed:     0,
		StartedAt:  &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID:       "stp-manual-nosnap-1",
		RunID:    "run-manual-nosnap",
		BatchID:  "bat-manual-nosnap",
		Host:     "web-1",
		StepName: "work",
		Action:   "shell.exec",
		Status:   "success",
	}))

	_, _, err := e.rollbackChange(context.Background(), "run-manual-nosnap", "", true)
	require.NoError(t, err, "an unwired snapshot store must not fail a manual rollback")
	cmds := rec.snapshotCmds()
	assert.Contains(t, cmds, "web-1\x00undo-command", "the compensation still runs")
	assert.NotContains(t, cmds, "web-1\x00base64 /etc/app.conf",
		"with no snapshot store wired there is nothing to restore, and nothing may claim otherwise")
}

// seedRunBaseline writes a run-level baseline record into the snapshot store,
// tagged exactly the way runRemoteSnapshotter.CaptureRun tags it (scope=run
// under the shared metadata key). Seeding it through the store rather than
// through a capture keeps the test honest about the one property the manual
// path actually depends on: records are found by CHANGE id.
func seedRunBaseline(t *testing.T, dir, changeID, target string, spec *dsl.RunSnapshotSpec) error {
	t.Helper()
	ctx := context.Background()
	store, err := rollback.NewFileSnapshotStore(dir)
	if err != nil {
		return err
	}
	mgr, err := rollback.NewSnapshotManager(store)
	if err != nil {
		return err
	}
	snap, err := mgr.CreateSnapshot(ctx, changeID, target, nil, rollback.SnapshotTypeFile, map[string]any{
		runSnapshotMetadataKey: runSnapshotScopeValue,
		"paths":                len(spec.Paths),
	})
	if err != nil {
		return err
	}
	payload := make(map[string]string, len(spec.Paths))
	for _, p := range spec.Paths {
		payload[p] = "baseline-content-for-" + p
	}
	return rollback.WriteSnapshotPayloads(snap.Path, payload)
}
