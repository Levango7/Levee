package wiring

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// collisionWorkflowYAML declares an undo step whose name is ALSO a forward step
// name — the ambiguity the compensation-idempotency gate must refuse instead
// of guessing at.
const collisionWorkflowYAML = `name: collide
target:
  type: host
  query: "env=test"
steps:
  - name: work
    action: shell.exec
    args:
      cmd: work-command
    rollback:
      steps:
        - name: work
          action: shell.exec
          args:
            cmd: undo-command
`

// seedFailedBatch writes the batch row step evidence hangs off. It is
// idempotent so a test that already seeded a specific batch id is not broken by
// a second call.
func seedFailedBatch(t *testing.T, store state.Store, runID, batchID string) {
	t.Helper()
	ctx := context.Background()
	if batchID == "" {
		batchID = "bat-" + runID
	}
	if existing, err := store.GetBatch(ctx, batchID); err == nil && existing != nil {
		return
	}
	now := utcNowStub()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: batchID, RunID: runID, BatchNo: 1, Status: "failed",
		TotalHosts: 1, StartedAt: &now, CompletedAt: &now,
	}))
}

// seedEvidenceStepAt writes one step-evidence row for the given run, stamped
// at `at` so the test can express ORDERING (a compensation only counts if it
// completed after the forward row it undoes). The batch row the steps
// foreign key requires is created on demand.
func seedEvidenceStepAt(t *testing.T, store state.Store, runID, host, name, status string, at time.Time) {
	t.Helper()
	batchID := "bat-" + runID
	seedFailedBatch(t, store, runID, batchID)
	require.NoError(t, store.CreateStep(context.Background(), &state.Step{
		ID:          newID("stp-"),
		RunID:       runID,
		BatchID:     batchID,
		Host:        host,
		StepName:    name,
		Action:      "shell.exec",
		Status:      status,
		StartedAt:   &at,
		CompletedAt: &at,
	}))
}

// evidenceBase is a fixed instant so ordering assertions never depend on the
// wall clock.
var evidenceBase = time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)

// seedEvidenceStep writes a row whose timestamp is derived from the step name:
// forward steps land at T+1m, compensation rows (undo-* / snapshot:*) at
// T+2m. That is the normal single-apply-then-rollback order the derivation has
// to recognise. Tests that need a different order call seedEvidenceStepAt.
func seedEvidenceStep(t *testing.T, store state.Store, runID, host, name, status string) {
	t.Helper()
	at := evidenceBase.Add(time.Minute)
	if strings.HasPrefix(name, "undo-") || strings.HasPrefix(name, "snapshot:") {
		at = evidenceBase.Add(2 * time.Minute)
	}
	seedEvidenceStepAt(t, store, runID, host, name, status, at)
}

// seedEvidenceStepUntimed writes a row with NO completion timestamp — the
// shape older or hand-written evidence can have, and the case the derivation
// cannot order against a prior compensation.
func seedEvidenceStepUntimed(t *testing.T, store state.Store, runID, host, name, status string) {
	t.Helper()
	batchID := "bat-" + runID
	seedFailedBatch(t, store, runID, batchID)
	require.NoError(t, store.CreateStep(context.Background(), &state.Step{
		ID:       newID("stp-"),
		RunID:    runID,
		BatchID:  batchID,
		Host:     host,
		StepName: name,
		Action:   "shell.exec",
		Status:   status,
	}))
}

// TestLedgerFromStoredSteps_MarksAlreadyCompensated covers the derivation
// itself: a forward success row plus a SUCCESSFUL undo row means the step is
// already restored and must not be compensated again.
func TestLedgerFromStoredSteps_MarksAlreadyCompensated(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-comp", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-comp", []string{"web-1"})

	ctx := context.Background()
	seedEvidenceStep(t, store, "run-comp", "web-1", "work", "success")
	seedEvidenceStep(t, store, "run-comp", "web-1", "undo-work", "success")

	p, err := e.loadStoredPlan(ctx, "run-comp")
	require.NoError(t, err)
	l, err := e.ledgerFromStoredSteps(ctx, "run-comp", p)
	require.NoError(t, err)

	assert.True(t, l.Ran("web-1", "work"), "forward evidence must still mark it ran")
	assert.True(t, l.AlreadyCompensated("web-1", "work"),
		"a successful undo row must mark the step already compensated")
	assert.Equal(t, 1, l.CompensatedCount())
}

// TestLedgerFromStoredSteps_FailedUndoIsStillRetried is the other half, and
// the one that protects the remedy path: a compensation that FAILED is exactly
// what rolled_back_partial / rollback_incomplete exist to retry, so it must not
// be recorded as compensated.
func TestLedgerFromStoredSteps_FailedUndoIsStillRetried(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-comp-fail", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-comp-fail", []string{"web-1"})

	ctx := context.Background()
	seedEvidenceStep(t, store, "run-comp-fail", "web-1", "work", "success")
	seedEvidenceStep(t, store, "run-comp-fail", "web-1", "undo-work", "failed")

	p, err := e.loadStoredPlan(ctx, "run-comp-fail")
	require.NoError(t, err)
	l, err := e.ledgerFromStoredSteps(ctx, "run-comp-fail", p)
	require.NoError(t, err)

	assert.True(t, l.Ran("web-1", "work"))
	assert.False(t, l.AlreadyCompensated("web-1", "work"),
		"a failed compensation must stay eligible for retry")
}

// TestLedgerFromStoredSteps_SnapshotRestoreCounts pins that a prior snapshot
// restore is recognised: the manager records it under the synthetic name
// "snapshot:<step>", so a second rollback does not restore the same capture
// again (which would overwrite whatever changed after the first restore).
func TestLedgerFromStoredSteps_SnapshotRestoreCounts(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-comp-snap", snapWorkflowYAML)
	planAndPersist(t, e, store, "run-comp-snap", []string{"web-1"})

	ctx := context.Background()
	seedEvidenceStep(t, store, "run-comp-snap", "web-1", "snap-step", "success")
	seedEvidenceStep(t, store, "run-comp-snap", "web-1", "snapshot:snap-step", "success")

	p, err := e.loadStoredPlan(ctx, "run-comp-snap")
	require.NoError(t, err)
	l, err := e.ledgerFromStoredSteps(ctx, "run-comp-snap", p)
	require.NoError(t, err)

	assert.True(t, l.AlreadyCompensated("web-1", "snap-step"),
		"a successful snapshot restore must count as compensation")
}

// TestLedgerFromStoredSteps_CollidingUndoNameRefused turns the documented
// ambiguity into an explicit refusal. The undo rows of such a workflow are
// indistinguishable from forward evidence, so compensating would be a guess.
func TestLedgerFromStoredSteps_CollidingUndoNameRefused(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-collide", collisionWorkflowYAML)
	planAndPersist(t, e, store, "run-collide", []string{"web-1"})

	ctx := context.Background()
	p, err := e.loadStoredPlan(ctx, "run-collide")
	require.NoError(t, err)

	_, err = e.ledgerFromStoredSteps(ctx, "run-collide", p)
	require.Error(t, err, "an undo step named like a forward step must be refused")
	assert.Contains(t, err.Error(), "also a forward step name")
}

// TestRollbackChange_SkipsAlreadyCompensatedStep is the end-to-end proof:
// a second manual rollback over a step whose compensation already succeeded
// must dispatch nothing. Before the idempotency gate this re-ran the undo
// command, which for a non-idempotent undo means the side effect lands twice.
func TestRollbackChange_SkipsAlreadyCompensatedStep(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-rb-twice", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-rb-twice", []string{"web-1"})

	seedEvidenceStep(t, store, "run-rb-twice", "web-1", "work", "success")
	seedEvidenceStep(t, store, "run-rb-twice", "web-1", "undo-work", "success")
	cmdsBefore := len(rec.snapshotCmds())

	_, _, err := e.rollbackChange(context.Background(), "run-rb-twice", "", true)
	require.NoError(t, err)

	after := rec.snapshotCmds()[cmdsBefore:]
	for _, c := range after {
		assert.NotEqual(t, "web-1\x00undo-command", c,
			"an already-compensated step must not run its undo a second time (cmd=%q)", c)
	}

	// The skip is recorded as its own thing, not folded into "never ran".
	var skip *state.Step
	for _, s := range stepsOf(t, store, "run-rb-twice") {
		if s.StepName == "work" && s.Status == "skipped" {
			skip = s
		}
	}
	require.NotNil(t, skip, "the already-compensated step must still leave evidence")
	assert.Contains(t, skip.Stderr, "already compensated")
}

// TestRollbackChange_RetriesFailedCompensation guards the remedy path against
// an over-eager fix: when the earlier undo FAILED, the next rollback must run
// it again.
func TestRollbackChange_RetriesFailedCompensation(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-rb-retry", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-rb-retry", []string{"web-1"})

	seedEvidenceStep(t, store, "run-rb-retry", "web-1", "work", "success")
	seedEvidenceStep(t, store, "run-rb-retry", "web-1", "undo-work", "failed")
	cmdsBefore := len(rec.snapshotCmds())

	_, _, err := e.rollbackChange(context.Background(), "run-rb-retry", "", true)
	require.NoError(t, err)

	cmds := rec.snapshotCmds()[cmdsBefore:]
	assert.Contains(t, cmds, "web-1\x00undo-command",
		"a failed compensation must be retried — it is what the remedy path is for")
}

// TestRollbackChange_ReappliedStepIsCompensatedAgain pins the re-apply case:
// retrying the stored plan rewrites the forward evidence, so the step is a
// fresh side effect and its compensation must run again even though an earlier
// instance of it was already undone.
func TestRollbackChange_ReappliedStepIsCompensatedAgain(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-rb-reapply", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-rb-reapply", []string{"web-1"})

	ctx := context.Background()
	// A previous apply was already undone...
	seedEvidenceStep(t, store, "run-rb-reapply", "web-1", "work", "success")
	seedEvidenceStep(t, store, "run-rb-reapply", "web-1", "undo-work", "success")
	// ...then the plan was re-applied: a NEWER forward row for the same step,
	// stamped after the undo. The old compensation no longer restores anything.
	seedEvidenceStepAt(t, store, "run-rb-reapply", "web-1", "work", "success",
		evidenceBase.Add(3*time.Minute))

	p, err := e.loadStoredPlan(ctx, "run-rb-reapply")
	require.NoError(t, err)
	l, err := e.ledgerFromStoredSteps(ctx, "run-rb-reapply", p)
	require.NoError(t, err)
	assert.True(t, l.Ran("web-1", "work"))
	assert.False(t, l.AlreadyCompensated("web-1", "work"),
		"a re-applied step has fresh side effects and must be compensated again")
}

// TestLedgerFromStoredSteps_MarksUncertainWhenUnorderable covers the residual
// case the timestamp ordering cannot settle: the forward row carries no
// completion time, so a prior successful compensation on record can neither
// be ruled in nor ruled out. The derivation must hand that to the manager as
// uncertainty rather than silently assume "not compensated".
func TestLedgerFromStoredSteps_MarksUncertainWhenUnorderable(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-unc", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-unc", []string{"web-1"})

	ctx := context.Background()
	// Forward row WITHOUT a completion time, but a successful undo row on
	// record: nothing can order the two.
	seedEvidenceStepUntimed(t, store, "run-unc", "web-1", "work", "success")
	seedEvidenceStep(t, store, "run-unc", "web-1", "undo-work", "success")

	p, err := e.loadStoredPlan(ctx, "run-unc")
	require.NoError(t, err)
	l, err := e.ledgerFromStoredSteps(ctx, "run-unc", p)
	require.NoError(t, err)

	assert.True(t, l.Ran("web-1", "work"), "forward evidence must still mark it ran")
	assert.False(t, l.AlreadyCompensated("web-1", "work"),
		"an unorderable prior compensation must not be treated as proof")
	assert.True(t, l.CompensationUncertain("web-1", "work"),
		"it must be surfaced as uncertainty for the manager to gate")
}

// TestLedgerFromStoredSteps_NoUndoNoUncertainty is the other side: an untimed
// forward row with NO prior compensation on record is simply a first
// compensation — there is no repeat question to ask.
func TestLedgerFromStoredSteps_NoUndoNoUncertainty(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-unc2", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-unc2", []string{"web-1"})

	ctx := context.Background()
	seedEvidenceStepUntimed(t, store, "run-unc2", "web-1", "work", "success")

	p, err := e.loadStoredPlan(ctx, "run-unc2")
	require.NoError(t, err)
	l, err := e.ledgerFromStoredSteps(ctx, "run-unc2", p)
	require.NoError(t, err)

	assert.True(t, l.Ran("web-1", "work"))
	assert.False(t, l.CompensationUncertain("web-1", "work"))
}
