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

	_, err := store.DB().ExecContext(ctx, `INSERT INTO cluster_nodes (id, address, role, status) VALUES ($1,$2,$3,$4)`,
		"node-a", "10.0.0.1:9090", "master", "active")
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `INSERT INTO cluster_nodes (id, address, role, status) VALUES ($1,$2,$3,$4)`,
		"node-b", "10.0.0.2:9090", "worker", "active")
	require.NoError(t, err)

	nodes, err := store.ListClusterNodes(ctx)
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	assert.Equal(t, "node-a", nodes[0].ID)
	assert.Equal(t, "master", nodes[0].Role)
	assert.Equal(t, "node-b", nodes[1].ID)
	assert.Equal(t, "worker", nodes[1].Role)
}

func TestAssignmentSummary_PGWithRows(t *testing.T) {
	dsn := pgTestDSN()
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set")
	}
	ctx := context.Background()
	store, cleanup := newPGTestStore(t)
	defer cleanup()

	require.NoError(t, store.CreateAssignment(ctx, &Assignment{RunID: "r1", OwnerNode: "node-a", Epoch: 1, State: AssignStatePending}))
	require.NoError(t, store.CreateAssignment(ctx, &Assignment{RunID: "r2", OwnerNode: "node-b", Epoch: 1, State: AssignmentStateExecuting}))
	require.NoError(t, store.CreateAssignment(ctx, &Assignment{RunID: "r3", OwnerNode: "node-a", Epoch: 1, State: AssignmentStateDone}))

	summary, err := store.AssignmentSummary(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Counts[AssignStatePending])
	assert.Equal(t, 1, summary.Counts[AssignmentStateExecuting])
	assert.Equal(t, 1, summary.Counts[AssignmentStateDone])
	assert.Equal(t, 1, summary.NodeLoad["node-a"]) // pending only (done excluded)
	assert.Equal(t, 1, summary.NodeLoad["node-b"]) // executing
	assert.Equal(t, 2, summary.TotalActive)
}
