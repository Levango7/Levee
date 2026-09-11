// cluster_status_state_test.go pins the state-layer primitives behind the
// cluster status view: ListClusterNodes (PG-gated; SQLite returns empty) and
// AssignmentSummary (both backends).
package state

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClusterNodes_SQLiteEmpty(t *testing.T) {
	store := newTestStore(t)
	nodes, err := store.ListClusterNodes(context.Background())
	require.NoError(t, err)
	assert.Empty(t, nodes, "SQLite has no cluster_nodes table")
}

func TestAssignmentSummary_SQLiteEmpty(t *testing.T) {
	store := newTestStore(t)
	summary, err := store.AssignmentSummary(context.Background())
	require.NoError(t, err)
	assert.Empty(t, summary.Counts)
	assert.Empty(t, summary.NodeLoad)
	assert.Zero(t, summary.TotalActive)
}

func TestAssignmentSummary_SQLiteWithRows(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// Seed three assignments across two nodes.
	require.NoError(t, store.CreateAssignment(ctx, &Assignment{RunID: "r1", OwnerNode: "node-a", Epoch: 1, State: AssignStatePending}))
	require.NoError(t, store.CreateAssignment(ctx, &Assignment{RunID: "r2", OwnerNode: "node-a", Epoch: 1, State: AssignmentStateExecuting}))
	require.NoError(t, store.CreateAssignment(ctx, &Assignment{RunID: "r3", OwnerNode: "node-b", Epoch: 1, State: AssignmentStateDone}))

	summary, err := store.AssignmentSummary(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Counts[AssignStatePending])
	assert.Equal(t, 1, summary.Counts[AssignmentStateExecuting])
	assert.Equal(t, 1, summary.Counts[AssignmentStateDone])
	// node-a: pending (1) + executing (1) = 2 active
	assert.Equal(t, 2, summary.NodeLoad["node-a"])
	// node-b: done is not active
	assert.NotContains(t, summary.NodeLoad, "node-b")
	assert.Equal(t, 2, summary.TotalActive)
}

func TestClusterNodes_PGListsNodes(t *testing.T) {
	dsn := pgTestDSN()
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set")
	}
	ctx := context.Background()
	store, cleanup := newPGTestStore(t)
	defer cleanup()

	// cluster_nodes is NOT truncated by newPGTestStore (the cluster package's
	// membership tests own it and run concurrently against this same database),
	// so we key on unique IDs and assert presence rather than total count.
	idA, idB := "cs-node-a", "cs-node-b"
	_, err := store.DB().ExecContext(ctx, `INSERT INTO cluster_nodes (id, address, role, status) VALUES ($1,$2,$3,$4)`,
		idA, "10.0.0.1:9090", "master", "active")
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `INSERT INTO cluster_nodes (id, address, role, status) VALUES ($1,$2,$3,$4)`,
		idB, "10.0.0.2:9090", "worker", "active")
	require.NoError(t, err)

	nodes, err := store.ListClusterNodes(ctx)
	require.NoError(t, err)
	byID := map[string]string{}
	for _, n := range nodes {
		byID[n.ID] = n.Role
	}
	assert.Equal(t, "master", byID[idA])
	assert.Equal(t, "worker", byID[idB])
}

func TestAssignmentSummary_PGWithRows(t *testing.T) {
	dsn := pgTestDSN()
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set")
	}
	ctx := context.Background()
	store, cleanup := newPGTestStore(t)
	defer cleanup()

	// run_assignment is not truncated by newPGTestStore (dispatch tests own
	// it), so use ON CONFLICT to keep the test idempotent across reruns.
	for _, a := range []*Assignment{
		{RunID: "csr1", OwnerNode: "cs-node-a", Epoch: 1, State: AssignStatePending},
		{RunID: "csr2", OwnerNode: "cs-node-b", Epoch: 1, State: AssignmentStateExecuting},
		{RunID: "csr3", OwnerNode: "cs-node-a", Epoch: 1, State: AssignmentStateDone},
	} {
		_, err := store.DB().ExecContext(ctx, `INSERT INTO run_assignment
			(run_id, owner_node, epoch, state, assigned_at, updated_at)
			VALUES ($1,$2,$3,$4,NOW(),NOW())
			ON CONFLICT (run_id) DO NOTHING`,
			a.RunID, a.OwnerNode, a.Epoch, a.State)
		require.NoError(t, err)
	}

	summary, err := store.AssignmentSummary(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Counts[AssignStatePending])
	assert.Equal(t, 1, summary.Counts[AssignmentStateExecuting])
	assert.Equal(t, 1, summary.Counts[AssignmentStateDone])
	assert.Equal(t, 1, summary.NodeLoad["cs-node-a"]) // pending only (done excluded)
	assert.Equal(t, 1, summary.NodeLoad["cs-node-b"]) // executing
	assert.Equal(t, 2, summary.TotalActive)
}
