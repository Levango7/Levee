// assignment_reclaim_test.go pins the stale-assignment reclaim CAS (D-4):
// ReclaimAssignment must bump the epoch for a pending row while standing down
// on any row a worker already claimed — that state guard is what keeps the
// reclaim sweep from dragging an executing run back to pending (which would
// let two nodes run the same change).
package state

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAssignmentTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(context.Background(), filepath.Join(t.TempDir(), "assignment.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSQLiteStore_ReclaimAssignmentCAS(t *testing.T) {
	ctx := context.Background()
	store := newAssignmentTestStore(t)

	require.NoError(t, store.CreateAssignment(ctx, &Assignment{
		RunID: "run-1", OwnerNode: "node-dead", Epoch: 1, State: AssignStatePending,
	}))

	// A stray probe with the wrong epoch must not touch the row either.
	ok, err := store.ReclaimAssignment(ctx, "run-1", 99, "node-ignored")
	require.NoError(t, err)
	require.False(t, ok, "a wrong epoch must not reclaim anything")

	ok, err = store.ReclaimAssignment(ctx, "run-1", 1, "node-b")
	require.NoError(t, err)
	require.True(t, ok, "reclaim of a stale pending row must apply")

	a, err := store.GetAssignment(ctx, "run-1")
	require.NoError(t, err)
	assert.Equal(t, "node-b", a.OwnerNode)
	assert.Equal(t, int64(2), a.Epoch, "reclaim must fence the stale owner out by bumping the epoch")
	assert.Equal(t, AssignStatePending, a.State)
	assert.Empty(t, a.Result)

	// The stale owner's late claim (old epoch) is now fenced out.
	ok, err = store.UpdateAssignmentStateIf(ctx, "run-1", 1, AssignStatePending, AssignmentStateExecuting)
	require.NoError(t, err)
	assert.False(t, ok, "the stale epoch must no longer claim the assignment")

	// A second reclaim on the bumped epoch applies (idempotent sweep retry)...
	ok, err = store.ReclaimAssignment(ctx, "run-1", 2, "node-c")
	require.NoError(t, err)
	assert.True(t, ok)

	// ...but once the row is claimed (pending→executing, same epoch) the
	// reclaim must stand down: that is the anti-double-execution guard.
	ok, err = store.UpdateAssignmentStateIf(ctx, "run-1", 3, AssignStatePending, AssignmentStateExecuting)
	require.NoError(t, err)
	require.True(t, ok, "the new owner must be able to claim its assignment")

	ok, err = store.ReclaimAssignment(ctx, "run-1", 3, "node-d")
	require.NoError(t, err)
	assert.False(t, ok, "an executing assignment must never be reclaimed back to pending")

	a, err = store.GetAssignment(ctx, "run-1")
	require.NoError(t, err)
	assert.Equal(t, "node-c", a.OwnerNode)
	assert.Equal(t, int64(3), a.Epoch)
}

func TestSQLiteStore_ReclaimAssignmentTerminalRows(t *testing.T) {
	ctx := context.Background()
	store := newAssignmentTestStore(t)

	require.NoError(t, store.CreateAssignment(ctx, &Assignment{
		RunID: "run-2", OwnerNode: "node-a", Epoch: 4, State: AssignmentStateDone,
	}))

	ok, err := store.ReclaimAssignment(ctx, "run-2", 4, "node-b")
	require.NoError(t, err)
	assert.False(t, ok, "terminal rows are not reclaimable")

	// A missing row is (false, nil), never an error.
	ok, err = store.ReclaimAssignment(ctx, "ghost", 1, "node-b")
	require.NoError(t, err)
	assert.False(t, ok)
}
