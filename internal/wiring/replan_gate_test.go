package wiring

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/state"
)

// seedApproval writes one approval row straight through the store, which is
// the same row shape the approval service persists.
func seedApproval(t *testing.T, store state.Store, runID, status, planHash string) {
	t.Helper()
	ctx := context.Background()
	acted := time.Now().UTC()
	require.NoError(t, store.CreateApproval(ctx, &state.Approval{
		ID:        "apr-" + runID + "-" + status,
		RunID:     runID,
		Level:     "standard",
		Approver:  "alice",
		Status:    status,
		TimeoutAt: &acted,
		PlanHash:  planHash,
	}))
}

// approvalStatus returns the status of the approval row with the given id.
func approvalStatus(t *testing.T, store state.Store, runID, approvalID string) string {
	t.Helper()
	rows, err := store.ListApprovals(context.Background(), state.ApprovalFilter{RunID: runID})
	require.NoError(t, err)
	for _, a := range rows {
		if a.ID == approvalID {
			return a.Status
		}
	}
	return ""
}

// TestRetryReplan_UnapprovedNewPlanIsNotExecuted is the regression test for
// the P0-1 drift on the retry re-plan path, which D-1 recorded as a hanging
// item and D-2 never picked up. The run's approval covers the plan persisted
// BEFORE the re-plan; widening the re-plan's host set mints a new version
// nothing has approved. Executing it would be "approve v1, execute v2".
func TestRetryReplan_UnapprovedNewPlanIsNotExecuted(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1", "web-2")
	seedRun(t, store, "run-replan", execWorkflowYAML)
	planAndPersist(t, e, store, "run-replan", []string{"web-1"})
	setRunStatus(t, store, "run-replan", "failed")

	run, err := store.GetRun(context.Background(), "run-replan")
	require.NoError(t, err)
	oldHash := run.PlanHash
	// The approval on file attests to the pre-re-plan version only.
	seedApproval(t, store, "run-replan", "approved", oldHash)
	// A still-pending row from the same plan must be superseded, not left
	// decidable against the new version.
	seedApproval(t, store, "run-replan", "pending", oldHash)
	pendingID := "apr-run-replan-pending"
	cmdsBefore := len(rec.snapshotCmds())

	err = e.retryChange(context.Background(), "run-replan", true, []string{"web-1", "web-2"})
	require.Error(t, err)
	assert.ErrorIs(t, err, grpc.ErrReplanNeedsApproval,
		"an unapproved re-plan must be reported as needing approval, not as an engine fault")

	assert.Len(t, rec.snapshotCmds(), cmdsBefore,
		"an unapproved re-plan must not dispatch anything")

	run, err = store.GetRun(context.Background(), "run-replan")
	require.NoError(t, err)
	assert.NotEqual(t, oldHash, run.PlanHash, "re-plan must mint a new version")
	assert.Equal(t, "draft", run.Status, "the run goes back to the approval flow")
	assert.Equal(t, "pending", run.ApprovalStatus)
	assert.Equal(t, "expired", approvalStatus(t, store, "run-replan", pendingID),
		"a pending row for the previous version must be superseded")
}

// TestRetryReplan_ApprovedNewPlanExecutes pins the other half: the gate must
// not turn retry-replan into a dead API. An approval that attests to the
// regenerated version authorises it, and a pre-binding (legacy) row still
// does — exactly as the apply gate treats it.
func TestRetryReplan_ApprovedNewPlanExecutes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bindToPlan bool
	}{
		{"approval bound to the regenerated version", true},
		{"legacy unbound approval", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &loopRecorder{}
			e, store := newLoopEngine(t, rec)
			seedLocalTargets(t, store, "web-1", "web-2")
			seedRun(t, store, "run-replan-ok", execWorkflowYAML)
			planAndPersist(t, e, store, "run-replan-ok", []string{"web-1"})
			setRunStatus(t, store, "run-replan-ok", "failed")

			hash := ""
			if tc.bindToPlan {
				// GeneratePlan is deterministic for a given run + host
				// set, so the hash computed here is the one retryChange
				// will mint moments later.
				_, stored, err := e.GeneratePlan(context.Background(), "run-replan-ok", []string{"web-1", "web-2"})
				require.NoError(t, err)
				hash = stored.Hash
			}
			seedApproval(t, store, "run-replan-ok", "approved", hash)

			cmdsBefore := len(rec.snapshotCmds())
			require.NoError(t, e.retryChange(context.Background(), "run-replan-ok", true, []string{"web-1", "web-2"}))
			assert.Greater(t, len(rec.snapshotCmds()), cmdsBefore,
				"an approved re-plan must execute")
		})
	}
}
