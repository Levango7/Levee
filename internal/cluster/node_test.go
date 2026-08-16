// node_test.go exercises the in-process NodeRegistry and leader election. These
// tests do not require a database — the registry is pure in-memory.

package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNodeRegistryRegisterAndGet covers Register + Get happy path.
func TestNodeRegistryRegisterAndGet(t *testing.T) {
	r := NewNodeRegistry()
	n := Node{
		ID: "n1", Address: "127.0.0.1:9090", Role: RoleWorker, Capabilities: "{}",
	}
	require.NoError(t, r.Register(n))

	got, ok := r.Get("n1")
	require.True(t, ok)
	assert.Equal(t, "n1", got.ID)
	assert.Equal(t, StatusActive, got.Status) // defaulted
	assert.Equal(t, RoleWorker, got.Role)
	assert.False(t, got.JoinedAt.IsZero())
}

// TestNodeRegistryRegisterValidation checks that empty ID/Address are rejected.
func TestNodeRegistryRegisterValidation(t *testing.T) {
	r := NewNodeRegistry()
	assert.Error(t, r.Register(Node{Address: "a"}))
	assert.Error(t, r.Register(Node{ID: "x"}))
}

// TestNodeRegistryDeregister checks deregistration and leader re-election.
func TestNodeRegistryDeregister(t *testing.T) {
	r := NewNodeRegistry()
	require.NoError(t, r.Register(Node{ID: "m1", Address: "a1", Role: RoleMaster}))
	require.NoError(t, r.Register(Node{ID: "m2", Address: "a2", Role: RoleMaster}))

	// First registered master should be leader (smallest ID).
	leader, ok := r.GetLeader()
	require.True(t, ok)
	assert.Equal(t, "m1", leader.ID)

	// Deregister the leader; m2 should take over.
	require.NoError(t, r.Deregister("m1"))
	leader, ok = r.GetLeader()
	require.True(t, ok)
	assert.Equal(t, "m2", leader.ID)

	// Deregistering an unknown node is an error.
	assert.Error(t, r.Deregister("nope"))
}

// TestNodeRegistryHeartbeat covers heartbeat refresh and resurrection.
func TestNodeRegistryHeartbeat(t *testing.T) {
	r := NewNodeRegistry()
	require.NoError(t, r.Register(Node{ID: "n1", Address: "a"}))

	now := time.Now().UTC()
	require.NoError(t, r.Heartbeat("n1", now))
	got, ok := r.Get("n1")
	require.True(t, ok)
	assert.True(t, got.LastHeartbeat.Equal(now) || got.LastHeartbeat.After(now.Add(-time.Second)))

	// Unknown node.
	assert.Error(t, r.Heartbeat("ghost", now))
}

// TestNodeRegistryElectLeaderFallback verifies that workers are elected when no
// active master is available.
func TestNodeRegistryElectLeaderFallback(t *testing.T) {
	r := NewNodeRegistry()
	require.NoError(t, r.Register(Node{ID: "w1", Address: "a1", Role: RoleWorker}))
	require.NoError(t, r.Register(Node{ID: "w2", Address: "a2", Role: RoleWorker}))

	id, err := r.ElectLeader()
	require.NoError(t, err)
	assert.Equal(t, "w1", id)
}

// TestNodeRegistryElectLeaderNoEligible verifies the error path.
func TestNodeRegistryElectLeaderNoEligible(t *testing.T) {
	r := NewNodeRegistry()
	_, err := r.ElectLeader()
	assert.Error(t, err)
}

// TestNodeRegistryMarkStale transitions nodes with stale heartbeats to offline.
func TestNodeRegistryMarkStale(t *testing.T) {
	r := NewNodeRegistry()
	require.NoError(t, r.Register(Node{ID: "n1", Address: "a"}))
	require.NoError(t, r.Register(Node{ID: "n2", Address: "b"}))

	// Make n2's heartbeat old.
	r.mu.Lock()
	r.nodes["n2"].LastHeartbeat = time.Now().UTC().Add(-2 * time.Minute)
	r.mu.Unlock()

	stale := r.MarkStale(time.Now().UTC(), 30*time.Second)
	assert.Contains(t, stale, "n2")

	_, ok := r.Get("n1")
	assert.True(t, ok)
	n2, ok := r.Get("n2")
	require.True(t, ok)
	assert.Equal(t, StatusOffline, n2.Status)
}

// TestNodeRegistryListAndCount checks the snapshot helpers.
func TestNodeRegistryListAndCount(t *testing.T) {
	r := NewNodeRegistry()
	require.NoError(t, r.Register(Node{ID: "n1", Address: "a"}))
	require.NoError(t, r.Register(Node{ID: "n2", Address: "b"}))

	assert.Equal(t, 2, r.Count())
	assert.Equal(t, 2, r.CountByStatus(StatusActive))

	list := r.List()
	assert.Len(t, list, 2)
}

// TestNodeRegistryReset checks the test helper.
func TestNodeRegistryReset(t *testing.T) {
	r := NewNodeRegistry()
	require.NoError(t, r.Register(Node{ID: "n1", Address: "a"}))
	r.Reset()
	assert.Equal(t, 0, r.Count())
	_, ok := r.GetLeader()
	assert.False(t, ok)
}

// TestEnsureContextCancelled checks the tiny helper.
func TestEnsureContextCancelled(t *testing.T) {
	// Use a background context that is never cancelled.
	assert.False(t, EnsureContextCancelled(context.Background()))
}
