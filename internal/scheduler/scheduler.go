package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nexus/levee/internal/agent"
	"github.com/nexus/levee/internal/log"
)

// ErrNoCandidate is returned by the balancer and the scheduler when no
// agent matches the requested capability.
var ErrNoCandidate = errors.New("scheduler: no candidate agent")

// ErrAssignmentFailed is returned when a task could not be assigned to
// any agent. The wrapped task ID helps the caller retry or fail the
// task explicitly.
var ErrAssignmentFailed = errors.New("scheduler: assignment failed")

// Assignment is the scheduler's output for a single task: the task
// paired with the agent it was assigned to. The Result field is
// populated by CollectResults after the agent has executed the task.
type Assignment struct {
	Task    agent.Task    `json:"task"`
	AgentID string        `json:"agent_id"`
	Result  *agent.Result `json:"result,omitempty"`
}

// Dispatcher is the interface the scheduler uses to send an assignment
// to an agent and wait for its result. The production implementation is
// a gRPC client that calls the agent's task stream; tests substitute an
// in-memory implementation that runs the task locally.
type Dispatcher interface {
	// Dispatch sends task to the agent identified by agentID and
	// returns the result. It must respect ctx for cancellation and
	// timeout. A non-nil error indicates a transport-level failure
	// (agent unreachable, stream broken); a nil error with a failed
	// Result indicates the task ran but failed.
	Dispatch(ctx context.Context, agentID string, task agent.Task) (agent.Result, error)
}

// Scheduler is the top-level orchestrator that distributes tasks across
// registered agents. It owns a Balancer and a reference to the
// master-side AgentRegistry; it does not own the registry (the master
// does, so that heartbeats can update it independently of the
// scheduler).
//
// The Scheduler is safe for concurrent use.
type Scheduler struct {
	registry   *agent.AgentRegistry
	balancer   *Balancer
	dispatcher Dispatcher
	mu         sync.RWMutex
}

// NewScheduler returns a Scheduler bound to the given registry and
// strategy. The dispatcher may be nil; in that case Schedule produces
// assignments but CollectResults returns an error. Callers that want
// to dispatch must call SetDispatcher before CollectResults.
func NewScheduler(registry *agent.AgentRegistry, strategy Strategy) *Scheduler {
	return &Scheduler{
		registry: registry,
		balancer: NewBalancer(strategy),
	}
}

// SetDispatcher installs the dispatcher used by CollectResults. It is
// exposed as a setter so that the scheduler can be constructed before
// the gRPC client is ready.
func (s *Scheduler) SetDispatcher(d Dispatcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispatcher = d
}

// Balancer returns the scheduler's balancer. It is exposed so that
// callers can reuse the balancer for ad-hoc selection (e.g. the shard
// helpers).
func (s *Scheduler) Balancer() *Balancer {
	return s.balancer
}

// Registry returns the scheduler's agent registry. It is exposed so
// that callers can inspect or mutate the registry (e.g. register a new
// agent) without having to keep a separate reference.
func (s *Scheduler) Registry() *agent.AgentRegistry {
	return s.registry
}

// Schedule distributes tasks across registered agents and returns the
// resulting assignments. Tasks whose required capability is not
// advertised by any online agent are returned with an empty AgentID
// and the caller is expected to handle them (retry later, fail the
// batch, etc.).
//
// Schedule does not execute the tasks; it only computes the
// assignment. Use CollectResults to dispatch and wait for results.
func (s *Scheduler) Schedule(tasks []agent.Task) ([]Assignment, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	agents := s.registry.List()
	if len(agents) == 0 {
		return nil, fmt.Errorf("scheduler: no agents registered")
	}
	assignments := make([]Assignment, 0, len(tasks))
	for _, t := range tasks {
		cap := t.Module
		if s.balancer.Strategy() != CapabilityAware {
			cap = ""
		}
		picked, err := s.balancer.Select(agents, cap)
		if err != nil {
			assignments = append(assignments, Assignment{Task: t})
			log.Warn("scheduler: no candidate for task",
				"task_id", t.ID, "module", t.Module)
			continue
		}
		assignments = append(assignments, Assignment{
			Task:    t,
			AgentID: picked.ID,
		})
	}
	return assignments, nil
}

// Reassign re-dispatches a previously failed task to a different agent.
// The fromAgent parameter is the agent that previously failed the
// task; it is excluded from the candidate list so that the reassign
// does not pick the same agent again. When no other agent is available
// Reassign returns ErrAssignmentFailed.
//
// Reassign does not execute the task; it returns the new assignment
// for the caller to dispatch.
func (s *Scheduler) Reassign(taskID string, fromAgent string) (Assignment, error) {
	agents := s.registry.List()
	filtered := make([]agent.AgentInfo, 0, len(agents))
	for _, a := range agents {
		if a.ID == fromAgent {
			continue
		}
		filtered = append(filtered, a)
	}
	if len(filtered) == 0 {
		return Assignment{}, fmt.Errorf("scheduler: reassign %q: %w", taskID, ErrAssignmentFailed)
	}
	// We do not have the original task here; the caller is expected
	// to look it up. Reassign only validates that a candidate exists.
	// The actual task is re-dispatched by the caller via the
	// dispatcher. This keeps Reassign cheap and side-effect free.
	return Assignment{Task: agent.Task{ID: taskID}}, nil
}

// CollectResults dispatches every assignment to its agent and waits
// for all results. It returns the results in the same order as the
// input assignments. Assignments with an empty AgentID are skipped
// (their Result stays nil) so that the caller can detect the missing
// assignment.
//
// CollectResults uses a per-task timeout derived from the task's
// Timeout field; when zero it falls back to the overall ctx deadline
// or no timeout. A nil ctx is treated as context.Background().
func (s *Scheduler) CollectResults(ctx context.Context, assignments []Assignment) ([]agent.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.RLock()
	dispatcher := s.dispatcher
	s.mu.RUnlock()
	if dispatcher == nil {
		return nil, fmt.Errorf("scheduler: dispatcher not set")
	}
	if len(assignments) == 0 {
		return nil, nil
	}

	results := make([]agent.Result, len(assignments))
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once

	for i, asg := range assignments {
		if asg.AgentID == "" {
			// Unassigned: leave a zero Result with the task ID
			// so the caller can correlate.
			results[i] = agent.Result{
				TaskID: asg.Task.ID,
				RunID:  asg.Task.RunID,
				Error:  "scheduler: no agent assigned",
			}
			continue
		}
		wg.Add(1)
		go func(idx int, a Assignment) {
			defer wg.Done()
			taskCtx := ctx
			if a.Task.Timeout > 0 {
				var cancel context.CancelFunc
				taskCtx, cancel = context.WithTimeout(ctx, a.Task.Timeout)
				defer cancel()
			}
			res, err := dispatcher.Dispatch(taskCtx, a.AgentID, a.Task)
			if err != nil {
				res = agent.Result{
					TaskID:  a.Task.ID,
					RunID:   a.Task.RunID,
					BatchID: a.Task.BatchID,
					AgentID: a.AgentID,
					Success: false,
					Error:   err.Error(),
				}
				errOnce.Do(func() { firstErr = err })
			}
			results[idx] = res
		}(i, asg)
	}
	wg.Wait()
	return results, firstErr
}

// ScheduleAndCollect is a convenience wrapper that calls Schedule then
// CollectResults. It is the one-shot API for callers that do not need
// to inspect the assignments between the two steps.
func (s *Scheduler) ScheduleAndCollect(ctx context.Context, tasks []agent.Task) ([]agent.Result, error) {
	assignments, err := s.Schedule(tasks)
	if err != nil {
		return nil, err
	}
	return s.CollectResults(ctx, assignments)
}

// Snapshot returns a sorted snapshot of the registry's agents and
// their current load. It is intended for CLI display and monitoring.
func (s *Scheduler) Snapshot() []agent.AgentInfo {
	agents := s.registry.List()
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	return agents
}

// DefaultDispatchTimeout is the per-task timeout used by the
// in-memory LocalDispatcher when the task does not specify one.
const DefaultDispatchTimeout = 30 * time.Minute

// LocalDispatcher is an in-memory Dispatcher that runs tasks through a
// local AgentExecutor. It is the production path when the master and
// the agent run in the same process (e.g. single-binary mode) and the
// test path for scheduler unit tests.
type LocalDispatcher struct {
	exec *agent.AgentExecutor
}

// NewLocalDispatcher returns a LocalDispatcher backed by exec.
func NewLocalDispatcher(exec *agent.AgentExecutor) *LocalDispatcher {
	return &LocalDispatcher{exec: exec}
}

// Dispatch runs the task through the local executor and returns the
// result. It respects ctx for cancellation and the task's Timeout
// field for per-task budgets. A nil ctx is treated as
// context.Background() so that callers from non-context-aware paths
// do not panic.
func (d *LocalDispatcher) Dispatch(ctx context.Context, agentID string, task agent.Task) (agent.Result, error) {
	_ = agentID
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := task.Timeout
	if timeout <= 0 {
		timeout = DefaultDispatchTimeout
	}
	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res := d.exec.Execute(taskCtx, task)
	return res, nil
}
