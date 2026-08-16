// Package cluster implements multi-node coordination for LEVEE cluster mode.
//
// It provides three coordinated primitives:
//   - NodeRegistry: an in-process registry of cluster members (master + workers)
//     with heartbeats and leader election.
//   - DistributedLock: a PostgreSQL advisory-lock-backed mutex for cross-node
//     mutual exclusion.
//   - ClusterManager: a facade that wires the registry, lock manager and a
//     background health-check loop into a single Start/Stop lifecycle.
//
// All shared state is guarded by sync.RWMutex; no method panics.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// NodeStatus enumerates the lifecycle state of a cluster node.
type NodeStatus string

const (
	// StatusActive indicates a healthy, participating node.
	StatusActive NodeStatus = "active"
	// StatusLeaving indicates a node that has requested graceful leave.
	StatusLeaving NodeStatus = "leaving"
	// StatusOffline indicates a node that has failed health checks or
	// explicitly deregistered.
	StatusOffline NodeStatus = "offline"
)

// NodeRole distinguishes the master (leader) from worker nodes.
type NodeRole string

const (
	// RoleMaster is the cluster leader that owns scheduling and orchestration.
	RoleMaster NodeRole = "master"
	// RoleWorker is a non-master node that executes assigned batches.
	RoleWorker NodeRole = "worker"
)

// Node describes a single cluster member. The Capabilities field is a
// JSON-encoded string so the registry can stay schema-agnostic.
type Node struct {
	ID            string     `json:"id"`
	Address       string     `json:"address"`
	Status        NodeStatus `json:"status"`
	Role          NodeRole   `json:"role"`
	LastHeartbeat time.Time  `json:"last_heartbeat"`
	Capabilities  string     `json:"capabilities"` // JSON encoded
	JoinedAt      time.Time  `json:"joined_at"`
}

// NodeRegistry maintains the set of cluster nodes in memory. It is safe for
// concurrent use. The registry is the source of truth for cluster membership
// within a single process; in a multi-process deployment each process keeps
// its own view, refreshed periodically from the database (cluster_nodes table).
type NodeRegistry struct {
	mu       sync.RWMutex
	nodes    map[string]*Node
	leaderID string
}

// NewNodeRegistry returns an empty registry.
func NewNodeRegistry() *NodeRegistry {
	return &NodeRegistry{nodes: make(map[string]*Node)}
}

// Register adds or replaces a node in the registry. If a node with the same ID
// already exists its fields are overwritten. The JoinedAt field is set to now
// if it is zero.
func (r *NodeRegistry) Register(node Node) error {
	if node.ID == "" {
		return fmt.Errorf("cluster: register: empty node id")
	}
	if node.Address == "" {
		return fmt.Errorf("cluster: register: empty node address")
	}
	if node.Status == "" {
		node.Status = StatusActive
	}
	if node.Role == "" {
		node.Role = RoleWorker
	}
	if node.JoinedAt.IsZero() {
		node.JoinedAt = time.Now().UTC()
	}
	if node.LastHeartbeat.IsZero() {
		node.LastHeartbeat = node.JoinedAt
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	n := node
	r.nodes[n.ID] = &n

	// Auto-elect the first master if no leader is set.
	if r.leaderID == "" && n.Role == RoleMaster && n.Status == StatusActive {
		r.leaderID = n.ID
	}
	return nil
}

// Deregister marks a node as offline and removes it from the registry. If the
// node was the leader, the leader slot is cleared and a new leader is elected
// from the remaining active masters (then active workers as a fallback).
func (r *NodeRegistry) Deregister(nodeID string) error {
	if nodeID == "" {
		return fmt.Errorf("cluster: deregister: empty node id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[nodeID]; !ok {
		return fmt.Errorf("cluster: deregister: node %q not found", nodeID)
	}
	delete(r.nodes, nodeID)
	if r.leaderID == nodeID {
		r.leaderID = ""
		r.electLeaderLocked()
	}
	return nil
}

// Heartbeat refreshes the LastHeartbeat timestamp of a node. It returns an
// error if the node is not registered.
func (r *NodeRegistry) Heartbeat(nodeID string, now time.Time) error {
	if nodeID == "" {
		return fmt.Errorf("cluster: heartbeat: empty node id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[nodeID]
	if !ok {
		return fmt.Errorf("cluster: heartbeat: node %q not found", nodeID)
	}
	n.LastHeartbeat = now.UTC()
	if n.Status == StatusOffline {
		n.Status = StatusActive
	}
	return nil
}

// Get returns a copy of the node with the given ID, or (nil, false).
func (r *NodeRegistry) Get(nodeID string) (*Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[nodeID]
	if !ok {
		return nil, false
	}
	cp := *n
	return &cp, true
}

// List returns a snapshot of all registered nodes. The order is unspecified;
// callers that need a stable order should sort the result.
func (r *NodeRegistry) List() []Node {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, *n)
	}
	return out
}

// GetLeader returns the current leader node, or (nil, false) if no leader is
// elected.
func (r *NodeRegistry) GetLeader() (*Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.leaderID == "" {
		return nil, false
	}
	n, ok := r.nodes[r.leaderID]
	if !ok {
		return nil, false
	}
	cp := *n
	return &cp, true
}

// ElectLeader forces a leader election. The election policy is:
//  1. Prefer active masters (role=master, status=active) with the smallest ID
//     (lexicographic, for determinism).
//  2. If no active master exists, fall back to the active worker with the
//     smallest ID.
//
// Returns the elected leader ID and nil on success, or ("", error) if no
// eligible node exists.
func (r *NodeRegistry) ElectLeader() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaderID = ""
	if !r.electLeaderLocked() {
		return "", errors.New("cluster: elect leader: no eligible node")
	}
	return r.leaderID, nil
}

// electLeaderLocked performs the election and returns true if a leader was
// chosen. Caller must hold r.mu.
func (r *NodeRegistry) electLeaderLocked() bool {
	// Phase 1: prefer active masters.
	var best *Node
	for _, n := range r.nodes {
		if n.Role != RoleMaster || n.Status != StatusActive {
			continue
		}
		if best == nil || n.ID < best.ID {
			best = n
		}
	}
	if best != nil {
		r.leaderID = best.ID
		return true
	}
	// Phase 2: fall back to active workers.
	for _, n := range r.nodes {
		if n.Role != RoleWorker || n.Status != StatusActive {
			continue
		}
		if best == nil || n.ID < best.ID {
			best = n
		}
	}
	if best != nil {
		r.leaderID = best.ID
		return true
	}
	return false
}

// MarkStale transitions nodes whose LastHeartbeat is older than maxAge to
// StatusOffline. It returns the IDs of the nodes that were marked stale.
// If the leader becomes stale, a new leader is elected.
func (r *NodeRegistry) MarkStale(now time.Time, maxAge time.Duration) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var stale []string
	for _, n := range r.nodes {
		if n.Status != StatusActive {
			continue
		}
		if now.UTC().Sub(n.LastHeartbeat) > maxAge {
			n.Status = StatusOffline
			stale = append(stale, n.ID)
		}
	}
	if r.leaderID != "" {
		if n, ok := r.nodes[r.leaderID]; !ok || n.Status != StatusActive {
			r.leaderID = ""
			r.electLeaderLocked()
		}
	}
	return stale
}

// Count returns the number of registered nodes.
func (r *NodeRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.nodes)
}

// CountByStatus returns the number of nodes with the given status.
func (r *NodeRegistry) CountByStatus(status NodeStatus) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var n int
	for _, node := range r.nodes {
		if node.Status == status {
			n++
		}
	}
	return n
}

// Reset clears all nodes and the leader slot. Intended for tests.
func (r *NodeRegistry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes = make(map[string]*Node)
	r.leaderID = ""
}

// EnsureContextCancelled is a tiny helper used by the cluster manager to
// detect shutdown. It returns true if ctx is done.
func EnsureContextCancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
