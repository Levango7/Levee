// Package takeover implements the cluster failover takeover loop
// (docs/design-cluster-failover.md): the leader node periodically scans
// for execution leases that expired without renewal — the executor node
// died mid-flight — and settles those runs to the terminal "interrupted"
// state with a full audit trail, so an in-flight change never rots in
// "running" waiting for a human to edit the database.
//
// Takeover deliberately does NOT re-execute anything: the fencing design
// (internal/cluster exec_guard.go, internal/wiring) guarantees the old
// executor's writes fail closed, and the takeover's own writes are
// idempotent under three independent guards:
//
//  1. leader-only: only the converged-election leader runs the loop;
//  2. per-run takeover lock: a cluster-wide lease lock (key
//     takeover:<run_id>) that also excludes a non-leader that is
//     election-transiently wrong about leadership;
//  3. status CAS: UpdateRunStatusIf(running → interrupted) — if
//     anything else moved the run first (a late executor's terminal
//     write, a pause), the takeover stands down for that run.
//
// Resumption is a human decision by design: interrupted runs re-enter
// the lifecycle through RetryChange (Q1), never through the loop.
package takeover

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/cluster"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/metrics"
	"github.com/nexus/levee/internal/state"
)

// DefaultInterval is the takeover sweep period when a Loop is started
// without an explicit interval.
const DefaultInterval = 10 * time.Second

// Loop is the takeover coordinator. It is cluster-only: construction
// requires the PostgreSQL-backed cluster manager and execution guard.
type Loop struct {
	mgr    *cluster.ClusterManager
	guard  *cluster.ExecutionGuard
	store  state.Store
	nodeID string

	interval time.Duration
	ttl      time.Duration // takeover-lock TTL (per run, short)

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

// NewLoop returns a takeover loop driven by the cluster manager
// (membership/leadership + takeover locks), the execution guard (expired
// lease scan) and the state store (run transitions + audit traces).
// nodeID identifies this node in lock ownership.
func NewLoop(mgr *cluster.ClusterManager, guard *cluster.ExecutionGuard, store state.Store, nodeID string, interval time.Duration) *Loop {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Loop{
		mgr:      mgr,
		guard:    guard,
		store:    store,
		nodeID:   nodeID,
		interval: interval,
		ttl:      10 * time.Second,
	}
}

// Start launches the background sweep loop; idempotent.
func (l *Loop) Start(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		return nil
	}
	innerCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	l.done = make(chan struct{})
	l.started = true
	go l.loop(innerCtx)
	log.Info("takeover loop started", "interval", l.interval, "node_id", l.nodeID)
	return nil
}

// Stop terminates the loop and waits for it to drain.
func (l *Loop) Stop(ctx context.Context) error {
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return nil
	}
	l.cancel()
	l.started = false
	done := l.done
	l.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("takeover: stop: loop did not drain: %w", ctx.Err())
	}
	log.Info("takeover loop stopped")
	return nil
}

func (l *Loop) loop(ctx context.Context) {
	defer close(l.done)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := l.TakeoverOnce(ctx); err != nil {
				log.Warn("takeover sweep failed", "error", err)
			}
		}
	}
}

// TakeoverResult reports what one sweep settled.
type TakeoverResult struct {
	Settled []string // run IDs transitioned running → interrupted
	Skipped []string // expired candidates that stood down (not running / lost race)
}

// TakeoverOnce performs one takeover sweep synchronously and is the
// seam the tests drive (no clock dependency). Only the leader acts: a
// non-leader returns an empty result with no error (leadership is
// convergent, so the check is advisory — the per-run lock and status
// CAS below remain the correctness guards).
func (l *Loop) TakeoverOnce(ctx context.Context) (TakeoverResult, error) {
	result := TakeoverResult{}

	if leader, ok := l.mgr.GetLeader(); !ok || leader.ID != l.nodeID {
		return result, nil
	}

	candidates, err := l.guard.ExpiredExecutions(ctx)
	if err != nil {
		return result, fmt.Errorf("takeover: scan expired executions: %w", err)
	}

	for _, runID := range candidates {
		settled, err := l.settleOne(ctx, runID)
		if err != nil {
			// Log and continue: one poisoned run must not block the
			// settlement of the others.
			log.Warn("takeover: settle failed", "run_id", runID, "error", err)
			continue
		}
		if settled {
			result.Settled = append(result.Settled, runID)
		} else {
			result.Skipped = append(result.Skipped, runID)
		}
	}
	return result, nil
}

// settleOne settles a single expired run under its takeover lock and the
// running→interrupted status CAS. Reports whether THIS call performed the
// settlement (false = stood down; either lost the lock or the run was
// not in a takeable state).
func (l *Loop) settleOne(ctx context.Context, runID string) (bool, error) {
	lock, err := l.mgr.Locks().Acquire(ctx, "takeover:"+runID, l.nodeID, l.ttl)
	if err != nil {
		if errors.Is(err, cluster.ErrLockBusy) {
			// Another node is settling this run right now.
			return false, nil
		}
		return false, fmt.Errorf("acquire takeover lock for %q: %w", runID, err)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := l.mgr.Locks().Release(releaseCtx, lock.Key, lock.Owner); err != nil {
			log.Warn("takeover: release lock failed", "key", lock.Key, "error", err)
		}
	}()

	// Status CAS: only a still-running run may be interrupted. Anything
	// else (already terminal, paused) is not ours to touch.
	ok, err := l.store.UpdateRunStatusIf(ctx, runID, "running", "interrupted", time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("status CAS for %q: %w", runID, err)
	}
	if !ok {
		return false, nil
	}

	// Defensive terminal marking of any non-terminal step rows. With the
	// current "persist once after the closure" evidence model this matches
	// 0 rows by construction (step rows only ever land with terminal
	// statuses); the statement is kept so a future incremental-persistence
	// model cannot reintroduce the "step stuck in running" failure mode
	// silently.
	if _, err := l.store.MarkNonTerminalSteps(ctx, runID, "unknown"); err != nil {
		// Non-fatal: the CAS above already settled the run; log and keep
		// the settlement (the audit trace below records the takeover).
		log.Warn("takeover: defensive step marking failed", "run_id", runID, "error", err)
	}

	// Audit trace through the regular channel so the takeover lands in
	// the run's hash chain.
	_ = l.store.CreateTrace(ctx, &state.Trace{
		ID:        fmt.Sprintf("trc-takeover-%s-%d", runID, time.Now().UTC().UnixNano()),
		RunID:     runID,
		Event:     "interrupted_by_takeover",
		Actor:     "cluster-takeover",
		Timestamp: time.Now().UTC(),
	})

	// The lease row dies with the settlement: a resuming executor (Retry)
	// registers fresh and the old owner's writes are fenced out by the
	// missing row itself.
	if derr := l.deleteExecRow(ctx, runID); derr != nil {
		log.Warn("takeover: delete execution row failed", "run_id", runID, "error", derr)
	}

	metrics.Default.IncChange("interrupted")
	log.Info("takeover: run interrupted",
		"run_id", runID, "previous_owner", execOwner(ctx, l.guard, runID))
	return true, nil
}

// deleteExecRow removes the run_execution row. The guard's End is
// owner-checked (the zombie's End is a no-op), so the takeover deletes
// the row directly via its own SQL.
func (l *Loop) deleteExecRow(ctx context.Context, runID string) error {
	if l.guard == nil {
		return nil
	}
	// The guard intentionally exposes no delete; the takeover uses the
	// cluster package's SQL convention through a targeted statement.
	if err := l.guard.DeleteExecution(ctx, runID); err != nil {
		return fmt.Errorf("delete execution row for %q: %w", runID, err)
	}
	return nil
}

// execOwner returns the current owner recorded on the execution row, or
// "" when the row is already gone. Diagnostics only.
func execOwner(ctx context.Context, g *cluster.ExecutionGuard, runID string) string {
	lease, err := g.GetExecution(ctx, runID)
	if err != nil || lease == nil {
		return ""
	}
	return lease.Owner
}
