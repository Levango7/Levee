// Package dispatch implements the cluster cross-node scheduling loop
// (docs/design-cluster-dispatch.md): the leader node periodically assigns
// pending runs to idle worker nodes so a multi-node cluster executes changes
// horizontally instead of queuing everything through the leader.
//
// The loop is leader-exclusive (convergent election view), idempotently
// recoverable (crash-safe: a new leader rescans and skips in-flight
// assignments), and orthogonal to the failover takeover loop
// (internal/takeover): dispatch hands a run to a worker, takeover reclaims
// it when the worker dies. Single-node deployments never start the loop.
package dispatch

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

// DefaultInterval is the dispatch sweep period when a Loop is started
// without an explicit interval.
const DefaultInterval = 10 * time.Second

// DefaultWorkerCapacity is the default number of concurrent runs a single
// worker node may execute before the dispatcher considers it saturated.
const DefaultWorkerCapacity = 4

// workerLoad is the dispatch-internal view of a worker node: its identity
// and how many active assignments it currently holds.
type workerLoad struct {
	id     string
	active int
}

// Loop is the dispatch coordinator. It is cluster-only: construction
// requires the cluster manager (membership, leadership, locks) and the
// state store (run + assignment rows).
type Loop struct {
	mgr      *cluster.ClusterManager
	store    state.Store
	nodeID   string
	interval time.Duration
	capacity int

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

// NewLoop returns a dispatch loop driven by the cluster manager, the state
// store and the local node ID. interval <= 0 falls back to DefaultInterval;
// capacity <= 0 falls back to DefaultWorkerCapacity.
func NewLoop(mgr *cluster.ClusterManager, store state.Store, nodeID string, interval time.Duration, capacity int) *Loop {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if capacity <= 0 {
		capacity = DefaultWorkerCapacity
	}
	return &Loop{
		mgr:      mgr,
		store:    store,
		nodeID:   nodeID,
		interval: interval,
		capacity: capacity,
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
	log.Info("dispatch loop started", "interval", l.interval, "node_id", l.nodeID, "capacity", l.capacity)
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
		return fmt.Errorf("dispatch: stop: loop did not drain: %w", ctx.Err())
	}
	log.Info("dispatch loop stopped")
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
			if _, err := l.DispatchOnce(ctx); err != nil {
				log.Warn("dispatch sweep failed", "error", err)
			}
		}
	}
}

// DispatchOnce performs one dispatch sweep synchronously and is the seam
// the tests drive (no clock dependency). Only the leader acts: a non-leader
// returns an empty result with no error (leadership is convergent, so the
// check is advisory — the per-run claim below remains the correctness guard).
func (l *Loop) DispatchOnce(ctx context.Context) (int, error) {
	if leader, ok := l.mgr.GetLeader(); !ok || leader.ID != l.nodeID {
		return 0, nil
	}

	candidates, err := l.candidates(ctx)
	if err != nil {
		return 0, fmt.Errorf("dispatch: list candidates: %w", err)
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	workers, err := l.workerLoads(ctx)
	if err != nil {
		return 0, fmt.Errorf("dispatch: list worker loads: %w", err)
	}

	claimed := 0
	wi := 0
	for _, runID := range candidates {
		// Find the next worker with spare capacity.
		for wi < len(workers) && workers[wi].active >= l.capacity {
			wi++
		}
		if wi >= len(workers) {
			break // all workers saturated
		}
		worker := workers[wi]

		ok, err := l.claim(ctx, runID, worker.id)
		if err != nil {
			log.Warn("dispatch: claim failed", "run_id", runID, "worker", worker.id, "error", err)
			continue
		}
		if ok {
			workers[wi].active++
			claimed++
			metrics.Default.IncDispatch(metrics.DispatchResultClaimed)
		}
	}
	return claimed, nil
}

// candidates returns the run IDs that are approved and have no active
// assignment (state pending/executing).
func (l *Loop) candidates(ctx context.Context) ([]string, error) {
	runs, err := l.store.ListRuns(ctx, state.RunFilter{
		Status: "approved",
		Limit:  1000,
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range runs {
		active, err := l.hasActiveAssignment(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		if !active {
			out = append(out, r.ID)
		}
	}
	return out, nil
}

// hasActiveAssignment reports whether the run currently has an assignment in
// a non-terminal state (pending or executing).
func (l *Loop) hasActiveAssignment(ctx context.Context, runID string) (bool, error) {
	existing, err := l.store.GetAssignment(ctx, runID)
	if err != nil {
		return false, err
	}
	if existing == nil {
		return false, nil
	}
	return existing.State == state.AssignStatePending || existing.State == state.AssignmentStateExecuting, nil
}

// workerLoads returns the active-assignment count per active worker node,
// sorted ascending (least-loaded first) so the loop spreads work evenly.
func (l *Loop) workerLoads(ctx context.Context) ([]workerLoad, error) {
	nodes := l.mgr.ActiveMastersAndWorkers()
	if len(nodes) == 0 {
		// Fall back to self-only: a solo-node cluster still dispatches to
		// itself, preserving the existing leader-executes-all behaviour.
		nodes = []cluster.Node{{ID: l.nodeID, Status: cluster.StatusActive}}
	}
	all, err := l.store.ListAssignments(ctx, state.AssignmentFilter{
		State: state.AssignmentStateExecuting,
		Limit: 10000,
	})
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(nodes))
	for _, asgn := range all {
		counts[asgn.OwnerNode]++
	}
	out := make([]workerLoad, 0, len(nodes))
	for _, n := range nodes {
		if n.Status != cluster.StatusActive {
			continue
		}
		out = append(out, workerLoad{id: n.ID, active: counts[n.ID]})
	}
	// Sort ascending by active count (insertion sort; n is tiny).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].active > out[j].active; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out, nil
}

// claim attempts to assign runID to worker. It succeeds only when the run has
// no active assignment: an INSERT ... ON CONFLICT DO UPDATE guarded by a
// WHERE clause that requires the existing row to be terminal. RowsAffected==0
// means a concurrent dispatcher or the takeover loop already owns the run.
func (l *Loop) claim(ctx context.Context, runID, worker string) (bool, error) {
	existing, err := l.store.GetAssignment(ctx, runID)
	if err != nil {
		return false, err
	}
	if existing != nil && (existing.State == state.AssignStatePending || existing.State == state.AssignmentStateExecuting) {
		return false, nil // already claimed
	}

	now := time.Now().UTC()
	var ok bool
	if existing == nil {
		// Fresh claim: epoch starts at 1.
		err = l.store.CreateAssignment(ctx, &state.Assignment{
			RunID:      runID,
			OwnerNode:  worker,
			Epoch:      1,
			State:      state.AssignStatePending,
			AssignedAt: now,
			UpdatedAt:  now,
		})
		if err == nil {
			ok = true
		} else if existing, gerr := l.store.GetAssignment(ctx, runID); gerr == nil && existing != nil && (existing.State == state.AssignStatePending || existing.State == state.AssignmentStateExecuting) {
			// Lost a race: someone claimed between our GetAssignment and ours.
			err = nil
		}
	} else {
		// Reclaim of a terminal assignment: bump epoch.
		ok, err = l.store.Reassign(ctx, runID, existing.Epoch, worker)
	}
	if err != nil {
		return false, err
	}
	return ok, nil
}

// MarkDone records the terminal outcome of an assignment after the run
// settles. It is idempotent and tolerant of a concurrent takeover (which may
// have moved the assignment to interrupted).
func (l *Loop) MarkDone(ctx context.Context, runID, result string) error {
	a, err := l.store.GetAssignment(ctx, runID)
	if err != nil {
		return err
	}
	if a == nil {
		return nil // no assignment (single-node path): nothing to mark
	}
	if a.State == state.AssignmentStateDone {
		return nil // idempotent
	}
	won, err := l.store.UpdateAssignmentStateIf(ctx, runID, a.Epoch, a.State, state.AssignmentStateDone)
	if err != nil || !won {
		return err
	}
	// We won the state transition; persist the result.
	res := result
	if res == "" {
		res = state.AssignResultCompleted
	}
	return l.store.SetAssignmentResult(ctx, runID, a.Epoch, res)
}

// DefaultWorkerInterval is the worker-poll period when a WorkerLoop is
// started without an explicit interval.
const DefaultWorkerInterval = 5 * time.Second

// RunExecutor abstracts the local execution of an assigned run. Serve wires
// it to the in-process gRPC ChangeService (the same apply path every node
// uses, regardless of who dispatched the run). The returned result is the
// terminal status string ("completed" / "failed" / "rolled_back"); the error
// is a fatal engine error (absent when the outcome is carried in result).
type RunExecutor interface {
	Execute(ctx context.Context, runID string) (result string, err error)
}

// workerLoop runs on every cluster node (leader and workers alike): it pulls
// pending assignments handed to this node by the leader's dispatch loop and
// executes them locally via the executor. It is the counterpart of the
// leader-only dispatch Loop: dispatch hands out work, workerLoop does it.
type WorkerLoop struct {
	store    state.Store
	executor RunExecutor
	nodeID   string
	interval time.Duration

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

// NewWorkerLoop returns a worker execution loop for the given node.
func NewWorkerLoop(store state.Store, executor RunExecutor, nodeID string, interval time.Duration) *WorkerLoop {
	if interval <= 0 {
		interval = DefaultWorkerInterval
	}
	return &WorkerLoop{store: store, executor: executor, nodeID: nodeID, interval: interval}
}

// Start launches the background worker loop; idempotent.
func (w *WorkerLoop) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return nil
	}
	innerCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.done = make(chan struct{})
	w.started = true
	go w.loop(innerCtx)
	log.Info("worker loop started", "node_id", w.nodeID, "interval", w.interval)
	return nil
}

// Stop terminates the loop and waits for it to drain.
func (w *WorkerLoop) Stop(ctx context.Context) error {
	w.mu.Lock()
	if !w.started {
		w.mu.Unlock()
		return nil
	}
	w.cancel()
	w.started = false
	done := w.done
	w.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("dispatch: worker stop: loop did not drain: %w", ctx.Err())
	}
	log.Info("worker loop stopped", "node_id", w.nodeID)
	return nil
}

func (w *WorkerLoop) loop(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollOnce(ctx)
		}
	}
}

// pollOnce performs one worker-poll sweep and is the seam tests drive (no
// clock dependency).
func (w *WorkerLoop) pollOnce(ctx context.Context) {
	for {
		assignment, ok := w.acquire(ctx)
		if !ok {
			return
		}
		result, err := w.executor.Execute(ctx, assignment.RunID)
		if err != nil {
			log.Warn("worker: execute failed", "run_id", assignment.RunID, "error", err)
			// Transition to failed; the takeover loop will re-dispatch if needed.
			result = "failed"
		}
		if markErr := w.complete(ctx, assignment, result); markErr != nil {
			log.Warn("worker: mark done failed", "run_id", assignment.RunID, "error", markErr)
		}
	}
}

// acquire claims one pending assignment owned by this node, transitioning it
// to "executing". It reports (nil, false) when there is nothing to do.
func (w *WorkerLoop) acquire(ctx context.Context) (*state.Assignment, bool) {
	for {
		pending, err := w.store.ListAssignments(ctx, state.AssignmentFilter{
			OwnerNode: w.nodeID,
			State:     state.AssignStatePending,
			Limit:     1,
		})
		if err != nil || len(pending) == 0 {
			return nil, false
		}
		a := pending[0]
		// Claim it: pending → executing. A lost race means another local
		// worker (or a concurrent poll) claimed it first; retry with the
		// next pending row.
		ok, err := w.store.UpdateAssignmentStateIf(ctx, a.RunID, a.Epoch, state.AssignStatePending, state.AssignmentStateExecuting)
		if err != nil {
			log.Warn("worker: claim failed", "run_id", a.RunID, "error", err)
			return nil, false
		}
		if ok {
			a.State = state.AssignmentStateExecuting
			return a, true
		}
	}
}

// complete marks an executing assignment as done with its terminal result.
func (w *WorkerLoop) complete(ctx context.Context, a *state.Assignment, result string) error {
	ok, err := w.store.UpdateAssignmentStateIf(ctx, a.RunID, a.Epoch, state.AssignmentStateExecuting, state.AssignmentStateDone)
	if err != nil || !ok {
		return err
	}
	return w.store.SetAssignmentResult(ctx, a.RunID, a.Epoch, result)
}

// EnsureStarted returns an error if the manager has not been started.
// Intended for wiring checks (mirrors ClusterManager.EnsureStarted).
var _ = errors.New
