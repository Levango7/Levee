// cluster.go implements ClusterManager, the top-level facade that wires
// together NodeRegistry, DistributedLockManager and a background health-check
// loop. The manager owns the cluster lifecycle: Start spawns the health-check
// goroutine, Stop cancels it and releases all held locks.
//
// In cluster mode (cmd_serve --cluster --pg-dsn ...) the manager is created
// once at server start and shared with the gRPC services so they can consult
// the registry (e.g. to route work only to active workers) and acquire
// distributed locks (e.g. to serialise run creation).

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// DefaultHealthCheckInterval is the period between health-check sweeps when
// the manager is started without an explicit interval.
const DefaultHealthCheckInterval = 10 * time.Second

// DefaultHeartbeatTimeout is the maximum age of a node's last heartbeat before
// it is marked stale.
const DefaultHeartbeatTimeout = 30 * time.Second

// ManagerConfig tunes the ClusterManager behaviour. Zero values fall back to
// the Default* constants above.
type ManagerConfig struct {
	HealthCheckInterval time.Duration
	HeartbeatTimeout    time.Duration
	// SelfID is the node ID of this process. Used so the health-check loop
	// can refresh our own heartbeat and skip marking ourselves stale.
	SelfID string
}

// ClusterManager is the top-level cluster coordinator. It owns a NodeRegistry
// and a DistributedLockManager and runs a background health-check loop.
type ClusterManager struct {
	cfg      ManagerConfig
	db       *sql.DB
	registry *NodeRegistry
	locks    *DistributedLockManager

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

// NewClusterManager returns a manager backed by the given *sql.DB. The db may
// be nil — in that case the manager runs in "in-process only" mode (useful for
// tests): the registry works, but the lock manager will reject Acquire calls.
func NewClusterManager(db *sql.DB, cfg ManagerConfig) *ClusterManager {
	if cfg.HealthCheckInterval <= 0 {
		cfg.HealthCheckInterval = DefaultHealthCheckInterval
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	return &ClusterManager{
		cfg:      cfg,
		db:       db,
		registry: NewNodeRegistry(),
		locks:    NewDistributedLockManager(db),
	}
}

// Registry returns the node registry. The returned pointer is safe to share
// because NodeRegistry methods are concurrency-safe.
func (m *ClusterManager) Registry() *NodeRegistry { return m.registry }

// Locks returns the distributed lock manager.
func (m *ClusterManager) Locks() *DistributedLockManager { return m.locks }

// Start launches the background health-check loop. It is idempotent: calling
// Start twice without an intervening Stop is a no-op. The loop runs until
// ctx is cancelled or Stop is called.
func (m *ClusterManager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return nil
	}
	innerCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})
	m.started = true
	go m.healthCheckLoop(innerCtx)
	log.Info("cluster manager started",
		"interval", m.cfg.HealthCheckInterval,
		"timeout", m.cfg.HeartbeatTimeout)
	return nil
}

// Stop signals the health-check loop to exit, waits for it to drain, and
// releases all distributed locks held by this process. It is idempotent.
func (m *ClusterManager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return nil
	}
	m.cancel()
	m.started = false
	done := m.done
	m.mu.Unlock()

	// Wait for the loop to exit, but respect the caller's deadline.
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("cluster: stop: health check did not drain: %w", ctx.Err())
	}

	// Release all locks. We use a fresh context here because the caller's
	// ctx may already be expired; we still want to release locks promptly.
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer releaseCancel()
	if errs := m.locks.ReleaseAll(releaseCtx); len(errs) > 0 {
		return fmt.Errorf("cluster: stop: %d lock release errors, first: %w", len(errs), errs[0])
	}
	log.Info("cluster manager stopped")
	return nil
}

// Join registers a node with the cluster. It is a thin wrapper around
// Registry.Register so callers do not need to reach into the registry.
func (m *ClusterManager) Join(node Node) error {
	return m.registry.Register(node)
}

// Leave deregisters a node and, if it was the leader, triggers a new election.
func (m *ClusterManager) Leave(nodeID string) error {
	return m.registry.Deregister(nodeID)
}

// HealthCheckResult is the outcome of a single health-check sweep.
type HealthCheckResult struct {
	StaleNodes []string // node IDs marked stale in this sweep
	Total      int      // total registered nodes
	Active     int      // nodes with StatusActive after the sweep
	LeaderID   string   // current leader ID (may be empty)
}

// HealthCheck performs a single health-check sweep without waiting for the
// background loop. It is useful for tests and for ad-hoc status queries.
func (m *ClusterManager) HealthCheck() HealthCheckResult {
	now := time.Now().UTC()
	stale := m.registry.MarkStale(now, m.cfg.HeartbeatTimeout)
	result := HealthCheckResult{
		StaleNodes: stale,
		Total:      m.registry.Count(),
		Active:     m.registry.CountByStatus(StatusActive),
	}
	if leader, ok := m.registry.GetLeader(); ok {
		result.LeaderID = leader.ID
	}
	return result
}

// GetNodes returns a snapshot of all registered nodes.
func (m *ClusterManager) GetNodes() []Node {
	return m.registry.List()
}

// GetLeader returns the current leader node, or (nil, false).
func (m *ClusterManager) GetLeader() (*Node, bool) {
	return m.registry.GetLeader()
}

// SelfHeartbeat refreshes the heartbeat of this process's own node. It is
// called by the background loop and may also be called explicitly (e.g. after
// completing a unit of work).
func (m *ClusterManager) SelfHeartbeat() error {
	if m.cfg.SelfID == "" {
		return errors.New("cluster: self heartbeat: SelfID not configured")
	}
	return m.registry.Heartbeat(m.cfg.SelfID, time.Now().UTC())
}

// healthCheckLoop is the background goroutine started by Start. It refreshes
// this node's heartbeat and marks stale nodes on every tick.
func (m *ClusterManager) healthCheckLoop(ctx context.Context) {
	defer close(m.done)
	ticker := time.NewTicker(m.cfg.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Refresh our own heartbeat first so we do not accidentally mark
		// ourselves stale.
		if m.cfg.SelfID != "" {
			if err := m.registry.Heartbeat(m.cfg.SelfID, time.Now().UTC()); err != nil {
				log.Warn("self heartbeat failed", "error", err)
			}
		}

		result := m.HealthCheck()
		if len(result.StaleNodes) > 0 {
			log.Warn("marked stale nodes", "count", len(result.StaleNodes), "ids", result.StaleNodes)
		}
	}
}

// EnsureStarted returns an error if the manager has not been started. It is
// intended for use by gRPC services that depend on the cluster being live.
func (m *ClusterManager) EnsureStarted() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return errors.New("cluster: manager not started")
	}
	return nil
}
