// approval_revision_cas_test.go pins the concurrency fix for multi-approver
// lost-updates (D-1 v2). Previously UpdateApprovalIfPending compared only on
// status='pending', so two concurrent partial votes (both leaving the row
// pending) could both succeed and the later write silently overwrite the
// earlier one. Now the WHERE clause also carries the reader's revision, so a
// stale writer loses the CAS and the decision loop must re-read.
package state

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpdateApprovalIfPending_RevisionCAS(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, store.CreateRun(ctx, &Run{
		ID: "rev-r1", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "draft", ApprovalStatus: "pending", ApprovalLevel: "high",
		CreatedAt: now, UpdatedAt: now, Creator: "u",
	}))
	require.NoError(t, store.CreateApproval(ctx, &Approval{
		ID: "rev-a1", RunID: "rev-r1", Level: "high",
		Status: "pending", Comment: `{}`,
	}))

	// Both deciders read the same row at revision 0.
	a, err := store.GetApproval(ctx, "rev-a1")
	require.NoError(t, err)
	require.Equal(t, int64(0), a.Revision, "fresh rows start at revision 0")
	b, err := store.GetApproval(ctx, "rev-a1")
	require.NoError(t, err)

	// First writer wins at revision 0 -> applied, revision becomes 1.
	a.Status = "pending" // stays pending: partial quorum, the exact lost-update case
	ok, err := store.UpdateApprovalIfPending(ctx, a)
	require.NoError(t, err)
	require.True(t, ok, "first writer at revision 0 must win")

	// Second writer carries the STALE revision 0 -> must lose, not overwrite.
	ok, err = store.UpdateApprovalIfPending(ctx, b)
	require.NoError(t, err)
	require.False(t, ok, "stale writer at revision 0 must lose the CAS against revision 1")

	// Fresh read sees revision 1; a fresh CAS at revision 1 wins.
	got, err := store.GetApproval(ctx, "rev-a1")
	require.NoError(t, err)
	require.Equal(t, int64(1), got.Revision)
	got.Status = "approved"
	ok, err = store.UpdateApprovalIfPending(ctx, got)
	require.NoError(t, err)
	require.True(t, ok, "fresh reader at revision 1 must win")
}

func TestUpdateApprovalIfPending_StaleRevisionNeverMovesStatus(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, store.CreateRun(ctx, &Run{
		ID: "rev-r2", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "draft", CreatedAt: now, UpdatedAt: now, Creator: "u",
	}))
	require.NoError(t, store.CreateApproval(ctx, &Approval{
		ID: "rev-a2", RunID: "rev-r2", Level: "high", Status: "pending",
	}))
	require.NoError(t, store.UpdateApproval(ctx, &Approval{ID: "rev-a2", RunID: "rev-r2", Level: "high", Status: "pending"}))

	stale := &Approval{ID: "rev-a2", RunID: "rev-r2", Level: "high", Status: "approved", Revision: 0}
	ok, err := store.UpdateApprovalIfPending(ctx, stale)
	require.NoError(t, err)
	require.False(t, ok, "stale revision is refused even to near-terminal states")

	want, err := store.GetApproval(ctx, "rev-a2")
	require.NoError(t, err)
	require.Equal(t, "pending", want.Status, "stale writer must not have moved the row")
}
