package wiring

// rollback_dryrun_test.go — 回滚的 dry-run 必须真的什么都不做。
//
// 这一层钉的是「预览不碰目标机」：补偿命令不派发、快照不写回、证据不落盘。
// 控制台的补救向导在确认之前先要这份证据；在修复之前后端根本不看 dry_run，
// 于是那份「证据」本身就是一次真的回滚——操作员还没点确认，主机已经被改了。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRollbackPreviewDispatchesNothing is the core proof: the compensation
// would run, and the preview still does not run it.
func TestRollbackPreviewDispatchesNothing(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-dry", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-dry", []string{"web-1"})

	// Forward evidence: the step provably ran, so a real rollback WOULD
	// compensate it. That is what makes the preview worth asking for.
	seedEvidenceStep(t, store, "run-dry", "web-1", "work", "success")

	out, err := e.rollbackChange(context.Background(), "run-dry", "", true, true)
	require.NoError(t, err)

	cmds := rec.snapshotCmds()
	assert.NotContains(t, cmds, "web-1\x00undo-command",
		"a preview must not dispatch the compensation")
	assert.NotContains(t, cmds, "web-1\x00work-command")

	// But the preview still says what would happen.
	assert.Equal(t, []string{"web-1"}, out.RolledBackHosts,
		"the preview must name the host a commit would compensate")
	assert.Empty(t, out.SkippedHosts)
	assert.Empty(t, out.Gaps)
	assert.Empty(t, out.RunID, "a preview mints no rollback run")

	// And it persisted nothing.
	steps := stepsOf(t, store, "run-dry")
	assert.Nil(t, stepByNameHost(steps, "undo-work", "web-1"),
		"a preview must not persist undo evidence")
}

// TestRollbackPreviewReportsAlreadyCompensatedHostAsSkipped covers the
// other answer the console shows: a host that an earlier rollback already
// restored needs nothing, and saying "待补偿" would send the operator to
// compensate it twice.
func TestRollbackPreviewReportsAlreadyCompensatedHostAsSkipped(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-dry-skip", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-dry-skip", []string{"web-1"})

	seedEvidenceStep(t, store, "run-dry-skip", "web-1", "work", "success")
	seedEvidenceStep(t, store, "run-dry-skip", "web-1", "undo-work", "success")

	out, err := e.rollbackChange(context.Background(), "run-dry-skip", "", true, true)
	require.NoError(t, err)

	assert.Empty(t, out.RolledBackHosts, "an already-compensated host needs nothing")
	assert.Equal(t, []string{"web-1"}, out.SkippedHosts,
		"the preview must say the host would be left alone")

	cmds := rec.snapshotCmds()
	assert.NotContains(t, cmds, "web-1\x00undo-command",
		"an already-compensated step must not be compensated again, not even in a preview")
}

// TestRollbackPreviewDoesNotRestoreTheRunBaseline pins the sharpest edge:
// the run-level baseline restore writes the captured pre-apply state back
// over the target — the most destructive thing a rollback does. A preview
// must not do it, even though the snapshot store is wired.
func TestRollbackPreviewDoesNotRestoreTheRunBaseline(t *testing.T) {
	rec := &loopRecorder{}
	snapDir := t.TempDir()
	e, store := newLoopEngine(t, rec, WithSnapshotDir(snapDir))
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-dry-baseline", manualBaselineYAML)
	planAndPersist(t, e, store, "run-dry-baseline", []string{"web-1"})

	p, err := e.loadStoredPlan(context.Background(), "run-dry-baseline")
	require.NoError(t, err)
	require.NotNil(t, p.RunSnapshot, "the stored plan must carry the run baseline")
	require.NoError(t, seedRunBaseline(t, snapDir, "run-dry-baseline", "web-1", p.RunSnapshot))

	seedEvidenceStep(t, store, "run-dry-baseline", "web-1", "work", "success")

	_, err = e.rollbackChange(context.Background(), "run-dry-baseline", "", true, true)
	require.NoError(t, err)

	cmds := rec.snapshotCmds()
	assert.NotContains(t, cmds, "web-1\x00base64 /etc/app.conf",
		"a preview must not write the captured baseline back over the target")
}

// TestRollbackPreviewReportsAHostWithBothACompensationAndAGap pins the
// overlap between the two lists.
//
// rbWorkflowYAML carries two steps: `work` declares a compensation and
// `boom` declares none. When BOTH ran on one host, the rollback would touch
// that host (work is undoable) and would still leave part of it changed
// (boom has no undo at all). Reporting it only as "would be compensated"
// hides the gap; reporting it only as a gap hides the touch. Both facts are
// what the operator needs, so the host is in both lists.
//
// `skipped` stays exclusive: it means "needs nothing", which is the opposite
// of a gap.
func TestRollbackPreviewReportsAHostWithBothACompensationAndAGap(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-dry-gap", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-dry-gap", []string{"web-1"})

	seedEvidenceStep(t, store, "run-dry-gap", "web-1", "work", "success")
	seedEvidenceStep(t, store, "run-dry-gap", "web-1", "boom", "success")

	out, err := e.rollbackChange(context.Background(), "run-dry-gap", "", true, true)
	require.NoError(t, err)

	assert.Equal(t, []string{"web-1"}, out.RolledBackHosts,
		"the host WOULD be touched: one of its steps has a usable compensation")
	assert.Equal(t, []string{"web-1"}, out.Gaps,
		"and it is also a gap: the other step declares no compensation at all")
	assert.Empty(t, out.SkippedHosts,
		"a host carrying a gap is never 'skipped' — skipped means it needs nothing")

	cmds := rec.snapshotCmds()
	assert.NotContains(t, cmds, "web-1\x00undo-command",
		"the preview still dispatches nothing")
}

// TestRollbackCommitStillDispatches is the regression pin in the other
// direction: the preview must not have softened the real thing.
func TestRollbackCommitStillDispatches(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-commit", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-commit", []string{"web-1"})
	seedEvidenceStep(t, store, "run-commit", "web-1", "work", "success")

	out, err := e.rollbackChange(context.Background(), "run-commit", "", true, false)
	require.NoError(t, err)

	assert.Contains(t, rec.snapshotCmds(), "web-1\x00undo-command",
		"a real rollback must still dispatch the compensation")
	assert.Equal(t, []string{"web-1"}, out.RolledBackHosts)
	assert.Contains(t, out.RunID, "rb-")
}
