package template

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// --- test helpers -----------------------------------------------------------

// newTestStore returns a fresh SQLite-backed state.Store for each test.
// The database file lives in a per-test temp dir and is closed via
// t.Cleanup.
func newTestStore(t *testing.T) *state.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "levee-template-test.db")
	store, err := state.NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestCloner returns a RunCloner backed by a fresh SQLite store.
func newTestCloner(t *testing.T) *RunCloner {
	t.Helper()
	return NewRunCloner(newTestStore(t))
}

// seedRun creates a run row with the given id and status. Returns the
// created run for convenience.
func seedRun(t *testing.T, st state.Store, id, status string) *state.Run {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	r := &state.Run{
		ID:             id,
		WorkflowName:   "deploy-nginx",
		TemplateName:   "nginx-v2",
		Params:         `{"version":"2.0","replicas":3}`,
		PlanHash:       "abc123",
		Status:         status,
		ApprovalStatus: "approved",
		ApprovalLevel:  "medium",
		CreatedAt:      now,
		UpdatedAt:      now,
		Creator:        "original-author",
		IncidentID:     "INC-001",
	}
	require.NoError(t, st.CreateRun(ctx, r))
	return r
}

// seedBatch creates a batch row under the given run with the given batch
// number and returns the created batch.
func seedBatch(t *testing.T, st state.Store, runID, batchID string, batchNo int) *state.Batch {
	t.Helper()
	ctx := context.Background()
	started := time.Now().UTC().Add(-10 * time.Minute)
	completed := time.Now().UTC().Add(-5 * time.Minute)
	b := &state.Batch{
		ID:          batchID,
		RunID:       runID,
		BatchNo:     batchNo,
		Status:      "completed",
		TotalHosts:  3,
		Succeeded:   2,
		Failed:      1,
		StartedAt:   &started,
		CompletedAt: &completed,
	}
	require.NoError(t, st.CreateBatch(ctx, b))
	return b
}

// seedStep creates a step row under the given run and batch and returns
// the created step.
func seedStep(t *testing.T, st state.Store, runID, batchID, stepID, host string) *state.Step {
	t.Helper()
	ctx := context.Background()
	exitCode := 0
	started := time.Now().UTC().Add(-8 * time.Minute)
	completed := time.Now().UTC().Add(-7 * time.Minute)
	s := &state.Step{
		ID:          stepID,
		RunID:       runID,
		BatchID:     batchID,
		Host:        host,
		StepName:    "deploy",
		Action:      "kubectl apply -f deploy.yaml",
		Status:      "success",
		ExitCode:    &exitCode,
		Stdout:      "deployment created",
		Stderr:      "",
		DurationMs:  1500,
		StartedAt:   &started,
		CompletedAt: &completed,
	}
	require.NoError(t, st.CreateStep(ctx, s))
	return s
}

// bgCtx is a short alias for a background context.
func bgCtx() context.Context { return context.Background() }

// listAudits returns all audit entries for the given action, sorted as
// returned by the store.
func listAudits(t *testing.T, st state.Store, action string) []*state.Audit {
	t.Helper()
	ctx := context.Background()
	audits, err := st.ListAudits(ctx, state.AuditFilter{Action: action})
	require.NoError(t, err)
	return audits
}

// =========================================================================
// Clone - success scenarios
// =========================================================================

func TestClone_Success_NewRunID(t *testing.T) {
	st := newTestStore(t)
	c := NewRunCloner(st)
	seedRun(t, st, "r1", "completed")

	result, err := c.Clone(bgCtx(), "r1", "alice")
	require.NoError(t, err)
	require.NotNil(t, result)

	// The cloned run ID must be different from the original.
	assert.NotEqual(t, "r1", result.ClonedRunID)
	assert.NotEmpty(t, result.ClonedRunID)
	assert.Equal(t, "r1", result.OriginalRunID)
	assert.Equal(t, "alice", result.ClonedBy)
	assert.False(t, result.ClonedAt.IsZero())

	// The cloned run must exist in the store.
	clonedRun, err := st.GetRun(bgCtx(), result.ClonedRunID)
	require.NoError(t, err)
	require.NotNil(t, clonedRun)
	assert.Equal(t, result.ClonedRunID, clonedRun.ID)
}

func TestClone_Success_PreservesParams(t *testing.T) {
	st := newTestStore(t)
	c := NewRunCloner(st)
	original := seedRun(t, st, "r1", "completed")

	result, err := c.Clone(bgCtx(), "r1", "alice")
	require.NoError(t, err)

	clonedRun, err := st.GetRun(bgCtx(), result.ClonedRunID)
	require.NoError(t, err)

	// Parameters and workflow metadata must be preserved.
	assert.Equal(t, original.WorkflowName, clonedRun.WorkflowName)
	assert.Equal(t, original.TemplateName, clonedRun.TemplateName)
	assert.Equal(t, original.Params, clonedRun.Params)
	assert.Equal(t, original.PlanHash, clonedRun.PlanHash)
	assert.Equal(t, original.ApprovalLevel, clonedRun.ApprovalLevel)
	assert.Equal(t, original.IncidentID, clonedRun.IncidentID)
}

func TestClone_Success_StatusIsDraft(t *testing.T) {
	st := newTestStore(t)
	c := NewRunCloner(st)
	seedRun(t, st, "r1", "completed")

	result, err := c.Clone(bgCtx(), "r1", "alice")
	require.NoError(t, err)

	clonedRun, err := st.GetRun(bgCtx(), result.ClonedRunID)
	require.NoError(t, err)
	assert.Equal(t, StatusDraft, clonedRun.Status)
}

func TestClone_Success_CreatorIsActor(t *testing.T) {
	st := newTestStore(t)
	c := NewRunCloner(st)
	seedRun(t, st, "r1", "completed")

	result, err := c.Clone(bgCtx(), "r1", "bob-the-cloner")
	require.NoError(t, err)

	clonedRun, err := st.GetRun(bgCtx(), result.ClonedRunID)
	require.NoError(t, err)
	assert.Equal(t, "bob-the-cloner", clonedRun.Creator)
}

func TestClone_Success_PreservesBatches(t *testing.T) {
	st := newTestStore(t)
	c := NewRunCloner(st)
	seedRun(t, st, "r1", "completed")
	seedBatch(t, st, "r1", "b1", 1)
	seedBatch(t, st, "r1", "b2", 2)

	result, err := c.Clone(bgCtx(), "r1", "alice")
	require.NoError(t, err)

	origBatches, err := st.ListBatches(bgCtx(), state.BatchFilter{RunID: "r1"})
	require.NoError(t, err)
	clonedBatches, err := st.ListBatches(bgCtx(), state.BatchFilter{RunID: result.ClonedRunID})
	require.NoError(t, err)

	require.Len(t, clonedBatches, len(origBatches), "cloned run should have same number of batches")

	// Verify batch structure is preserved (batch_no and total_hosts).
	for i, ob := range origBatches {
		cb := clonedBatches[i]
		assert.Equal(t, ob.BatchNo, cb.BatchNo, "batch_no should match")
		assert.Equal(t, ob.TotalHosts, cb.TotalHosts, "total_hosts should match")
		assert.Equal(t, result.ClonedRunID, cb.RunID, "batch should belong to cloned run")
		assert.NotEqual(t, ob.ID, cb.ID, "batch ID should be new")
		// Cloned batch should be reset to pending.
		assert.Equal(t, StatusPending, cb.Status, "cloned batch status should be pending")
		assert.Equal(t, 0, cb.Succeeded, "cloned batch succeeded should be 0")
		assert.Equal(t, 0, cb.Failed, "cloned batch failed should be 0")
		assert.Nil(t, cb.StartedAt, "cloned batch started_at should be nil")
		assert.Nil(t, cb.CompletedAt, "cloned batch completed_at should be nil")
	}
}

func TestClone_Success_PreservesSteps(t *testing.T) {
	st := newTestStore(t)
	c := NewRunCloner(st)
	seedRun(t, st, "r1", "completed")
	seedBatch(t, st, "r1", "b1", 1)
	seedStep(t, st, "r1", "b1", "s1", "host-a.example.com")
	seedStep(t, st, "r1", "b1", "s2", "host-b.example.com")

	result, err := c.Clone(bgCtx(), "r1", "alice")
	require.NoError(t, err)

	origSteps, err := st.ListSteps(bgCtx(), state.StepFilter{RunID: "r1"})
	require.NoError(t, err)
	clonedSteps, err := st.ListSteps(bgCtx(), state.StepFilter{RunID: result.ClonedRunID})
	require.NoError(t, err)

	require.Len(t, clonedSteps, len(origSteps), "cloned run should have same number of steps")

	// Build a map of original host -> step for comparison.
	origByHost := make(map[string]*state.Step, len(origSteps))
	for _, s := range origSteps {
		origByHost[s.Host] = s
	}

	for _, cs := range clonedSteps {
		os, ok := origByHost[cs.Host]
		require.Truef(t, ok, "cloned step host %q not found in original", cs.Host)
		assert.Equal(t, os.StepName, cs.StepName, "step_name should match")
		assert.Equal(t, os.Action, cs.Action, "action should match")
		assert.Equal(t, result.ClonedRunID, cs.RunID, "step should belong to cloned run")
		assert.NotEqual(t, os.ID, cs.ID, "step ID should be new")
		assert.NotEqual(t, os.BatchID, cs.BatchID, "step batch ID should be new")
		// Cloned step should be reset to pending.
		assert.Equal(t, StatusPending, cs.Status, "cloned step status should be pending")
		assert.Nil(t, cs.ExitCode, "cloned step exit_code should be nil")
		assert.Empty(t, cs.Stdout, "cloned step stdout should be empty")
		assert.Empty(t, cs.Stderr, "cloned step stderr should be empty")
		assert.Equal(t, 0, cs.DurationMs, "cloned step duration should be 0")
		assert.Nil(t, cs.StartedAt, "cloned step started_at should be nil")
		assert.Nil(t, cs.CompletedAt, "cloned step completed_at should be nil")
	}
}

func TestClone_Success_MultipleBatches(t *testing.T) {
	st := newTestStore(t)
	c := NewRunCloner(st)
	seedRun(t, st, "r1", "completed")
	// Three batches with steps in each.
	seedBatch(t, st, "r1", "b1", 1)
	seedBatch(t, st, "r1", "b2", 2)
	seedBatch(t, st, "r1", "b3", 3)
	seedStep(t, st, "r1", "b1", "s1", "host-a")
	seedStep(t, st, "r1", "b1", "s2", "host-b")
	seedStep(t, st, "r1", "b2", "s3", "host-c")
	seedStep(t, st, "r1", "b3", "s4", "host-d")
	seedStep(t, st, "r1", "b3", "s5", "host-e")

	result, err := c.Clone(bgCtx(), "r1", "alice")
	require.NoError(t, err)

	clonedBatches, err := st.ListBatches(bgCtx(), state.BatchFilter{RunID: result.ClonedRunID})
	require.NoError(t, err)
	require.Len(t, clonedBatches, 3, "all 3 batches should be cloned")

	clonedSteps, err := st.ListSteps(bgCtx(), state.StepFilter{RunID: result.ClonedRunID})
	require.NoError(t, err)
	require.Len(t, clonedSteps, 5, "all 5 steps should be cloned")

	// Verify batch numbers are preserved and in order.
	assert.Equal(t, 1, clonedBatches[0].BatchNo)
	assert.Equal(t, 2, clonedBatches[1].BatchNo)
	assert.Equal(t, 3, clonedBatches[2].BatchNo)

	// Verify each cloned step is linked to one of the cloned batches.
	clonedBatchIDs := make(map[string]struct{}, len(clonedBatches))
	for _, b := range clonedBatches {
		clonedBatchIDs[b.ID] = struct{}{}
	}
	for _, s := range clonedSteps {
		_, ok := clonedBatchIDs[s.BatchID]
		assert.Truef(t, ok, "cloned step %q references unknown batch %q", s.ID, s.BatchID)
	}
}

func TestClone_Success_EmptyRun(t *testing.T) {
	// A run with no batches and no steps should still clone successfully.
	st := newTestStore(t)
	c := NewRunCloner(st)
	seedRun(t, st, "r1", "completed")

	result, err := c.Clone(bgCtx(), "r1", "alice")
	require.NoError(t, err)
	require.NotNil(t, result)

	clonedBatches, err := st.ListBatches(bgCtx(), state.BatchFilter{RunID: result.ClonedRunID})
	require.NoError(t, err)
	assert.Empty(t, clonedBatches)

	clonedSteps, err := st.ListSteps(bgCtx(), state.StepFilter{RunID: result.ClonedRunID})
	require.NoError(t, err)
	assert.Empty(t, clonedSteps)
}

// =========================================================================
// Clone - audit recording
// =========================================================================

func TestClone_Success_WritesAudit(t *testing.T) {
	st := newTestStore(t)
	c := NewRunCloner(st)
	seedRun(t, st, "r1", "completed")

	result, err := c.Clone(bgCtx(), "r1", "alice")
	require.NoError(t, err)

	audits := listAudits(t, st, ActionClone)
	require.Len(t, audits, 1, "exactly one clone audit entry should be written")
	a := audits[0]
	assert.Equal(t, "alice", a.Actor, "audit actor should be the cloner")
	assert.Equal(t, "r1", a.Target, "audit target should be the original run ID")
	assert.Equal(t, result.ClonedRunID, a.RunID, "audit run_id should be the cloned run ID")
	assert.Equal(t, ResultSuccess, a.Result, "audit result should be success")
	assert.False(t, a.Timestamp.IsZero(), "audit timestamp should be set")
	assert.NotEmpty(t, a.ID, "audit ID should be set")
}

// =========================================================================
// Clone - error scenarios
// =========================================================================

func TestClone_Fail_RunNotFound(t *testing.T) {
	c := newTestCloner(t)

	result, err := c.Clone(bgCtx(), "nonexistent-run", "alice")
	assert.ErrorIs(t, err, ErrRunNotFound)
	assert.Nil(t, result)
}

func TestClone_Fail_EmptyRunID(t *testing.T) {
	c := newTestCloner(t)

	result, err := c.Clone(bgCtx(), "", "alice")
	assert.ErrorIs(t, err, ErrEmptyRunID)
	assert.Nil(t, result)
}

func TestClone_Fail_EmptyActor(t *testing.T) {
	c := newTestCloner(t)

	result, err := c.Clone(bgCtx(), "r1", "")
	assert.ErrorIs(t, err, ErrEmptyActor)
	assert.Nil(t, result)
}

func TestClone_Fail_EmptyArgs_BothEmpty(t *testing.T) {
	c := newTestCloner(t)

	// Empty run ID is checked first.
	result, err := c.Clone(bgCtx(), "", "")
	assert.ErrorIs(t, err, ErrEmptyRunID)
	assert.Nil(t, result)
}

// =========================================================================
// Clone - original run is untouched
// =========================================================================

func TestClone_OriginalRunUntouched(t *testing.T) {
	st := newTestStore(t)
	c := NewRunCloner(st)
	original := seedRun(t, st, "r1", "completed")
	seedBatch(t, st, "r1", "b1", 1)
	seedStep(t, st, "r1", "b1", "s1", "host-a")

	_, err := c.Clone(bgCtx(), "r1", "alice")
	require.NoError(t, err)

	// The original run must be unchanged.
	origRun, err := st.GetRun(bgCtx(), "r1")
	require.NoError(t, err)
	assert.Equal(t, original.Status, origRun.Status, "original run status should be unchanged")
	assert.Equal(t, original.Creator, origRun.Creator, "original run creator should be unchanged")
	assert.Equal(t, original.Params, origRun.Params, "original run params should be unchanged")

	// Original batches and steps must still be there.
	origBatches, err := st.ListBatches(bgCtx(), state.BatchFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, origBatches, 1)
	assert.Equal(t, "b1", origBatches[0].ID)

	origSteps, err := st.ListSteps(bgCtx(), state.StepFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, origSteps, 1)
	assert.Equal(t, "s1", origSteps[0].ID)
}

// =========================================================================
// Clone - repeated clones produce distinct runs
// =========================================================================

func TestClone_RepeatedClonesDistinct(t *testing.T) {
	st := newTestStore(t)
	c := NewRunCloner(st)
	seedRun(t, st, "r1", "completed")

	r1, err := c.Clone(bgCtx(), "r1", "alice")
	require.NoError(t, err)
	r2, err := c.Clone(bgCtx(), "r1", "alice")
	require.NoError(t, err)

	// Two clones of the same run must have different IDs.
	assert.NotEqual(t, r1.ClonedRunID, r2.ClonedRunID, "repeated clones should have distinct run IDs")
	assert.Equal(t, "r1", r1.OriginalRunID)
	assert.Equal(t, "r1", r2.OriginalRunID)
}

// =========================================================================
// Sentinel errors are distinguishable
// =========================================================================

func TestSentinelErrors_Distinguishable(t *testing.T) {
	// Ensure the sentinels are distinct values, so callers can use
	// errors.Is to branch on the specific failure.
	assert.NotEqual(t, ErrRunNotFound, ErrEmptyRunID)
	assert.NotEqual(t, ErrEmptyRunID, ErrEmptyActor)
	assert.NotEqual(t, ErrEmptyActor, ErrCloneFailed)
	assert.NotEqual(t, ErrCloneFailed, ErrRunNotFound)
}
