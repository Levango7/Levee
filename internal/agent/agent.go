package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nexus/levee/internal/log"
)

// MasterClient is the transport-agnostic interface the Agent uses to
// talk to the master node. The production implementation is a gRPC
// stream client; tests substitute an in-memory implementation.
//
// All methods must be safe for concurrent use. A non-nil error from any
// method is logged by the Agent but does not stop the agent; the agent
// keeps retrying heartbeats and accepting locally-queued tasks.
type MasterClient interface {
	// Register informs the master that this agent is online and
	// available for task dispatch. It must be idempotent from the
	// master's perspective: a re-register after a transient network
	// failure should update the existing record rather than fail.
	Register(ctx context.Context, info AgentInfo) error

	// Deregister informs the master that this agent is going away
	// permanently. The master should remove the agent from its
	// registry and reassign any in-flight tasks.
	Deregister(ctx context.Context, agentID string) error

	// SendHeartbeat delivers a heartbeat to the master. It is
	// invoked once per heartbeat interval by the heartbeat loop.
	SendHeartbeat(ctx context.Context, hb Heartbeat) error

	// StreamTasks opens a long-lived stream over which the master
	// pushes tasks and the agent pushes results. The stream must
	// honour ctx cancellation. The agent calls StreamTasks exactly
	// once per Start invocation.
	StreamTasks(ctx context.Context, agentID string, handler TaskHandler) error
}

// TaskHandler is the callback the agent supplies to StreamTasks. The
// master invokes it for each task it wants the agent to execute; the
// agent runs the task locally and returns the Result, which the master
// client implementation forwards back over the stream.
type TaskHandler func(ctx context.Context, task Task) Result

// Agent is the常驻 process that registers with the master, receives
// tasks, executes them locally and reports results back. An Agent is
// safe to Start at most once; calling Start twice on the same instance
// returns an error.
//
// The zero value is not usable; callers must use NewAgent.
type Agent struct {
	// ID is the agent's unique identifier. When the caller passes an
	// empty string to NewAgent a UUID is generated.
	ID string `json:"id"`

	// Address is the host:port at which the agent can be reached.
	Address string `json:"address"`

	// Capabilities is the list of module names the agent can execute.
	Capabilities []string `json:"capabilities"`

	// MaxConcurrent is the upper bound on simultaneously running tasks.
	MaxConcurrent int `json:"max_concurrent"`

	// HeartbeatInterval is the period between heartbeat sends. Zero
	// means DefaultHeartbeatInterval.
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`

	// Status is the agent's view of its own lifecycle state. It is
	// updated by Start / Stop and read by Status().
	status AgentStatus

	// master is the client used to talk to the master node.
	master MasterClient

	// exec runs tasks locally.
	exec *AgentExecutor

	// runCtx is the context passed to Start, stored so that Stop can
	// cancel it. It is nil before Start and after Stop.
	runCtx    context.Context
	runCancel context.CancelFunc

	// started guards against double Start.
	started bool

	// lastHeartbeat is updated by the heartbeat loop and read by
	// Status() for diagnostics.
	lastHeartbeat time.Time

	mu sync.Mutex
}

// ErrAlreadyStarted is returned by Start when the agent has already
// been started. An agent is single-use; create a new instance to run
// again.
var ErrAlreadyStarted = errors.New("agent: already started")

// ErrNotStarted is returned by Stop when the agent has not been
// started or has already been stopped.
var ErrNotStarted = errors.New("agent: not started")

// NewAgent constructs an Agent with the given parameters. If id is
// empty a UUID is generated. If maxConcurrent <= 0 it defaults to 1.
// The AgentExecutor is created with the given concurrency limit and
// the default executor; callers wanting a custom executor can replace
// the exec field after construction (typically in tests).
func NewAgent(id, addr string, caps []string, maxConcurrent int) *Agent {
	if id == "" {
		id = uuid.NewString()
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	a := &Agent{
		ID:            id,
		Address:       addr,
		Capabilities:  append([]string(nil), caps...),
		MaxConcurrent: maxConcurrent,
		status:        StatusOffline,
	}
	a.exec = NewAgentExecutor(maxConcurrent, nil)
	return a
}

// SetMasterClient installs the MasterClient used to talk to the master.
// It must be called before Start. It is exposed as a setter rather than
// a constructor parameter so that the Agent can be created early (e.g.
// by cobra flag parsing) and wired to a gRPC client later.
func (a *Agent) SetMasterClient(c MasterClient) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.master = c
}

// SetExecutor replaces the default AgentExecutor. It is intended for
// tests that want to inject a stub executor. It must be called before
// Start.
func (a *Agent) SetExecutor(e *AgentExecutor) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.exec = e
}

// Status returns the agent's view of its own lifecycle state. It is
// approximate: the agent does not push state changes to the master, so
// the value reflects the last Start / Stop / heartbeat tick.
func (a *Agent) Status() AgentStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// LastHeartbeat returns the timestamp of the most recent heartbeat
// sent by the heartbeat loop. It is zero before Start.
func (a *Agent) LastHeartbeat() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastHeartbeat
}

// Executor returns the agent's task executor. It is exposed so that
// the CLI `agent status` command can read cumulative counters.
func (a *Agent) Executor() *AgentExecutor {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exec
}

// Start registers the agent with the master, starts the heartbeat loop
// and the task stream, and blocks until ctx is cancelled or the master
// stream terminates. Start is single-use: a second call returns
// ErrAlreadyStarted.
//
// Start performs the following steps in order:
//  1. Mark the agent as started and set up the run context.
//  2. Register with the master.
//  3. Start the heartbeat loop in a goroutine.
//  4. Open the task stream and dispatch incoming tasks to the
//     AgentExecutor. This blocks until the stream closes or ctx is
//     cancelled.
//  5. On return, deregister and flip status to offline.
func (a *Agent) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return ErrAlreadyStarted
	}
	if a.master == nil {
		a.mu.Unlock()
		return fmt.Errorf("agent: start: master client not set")
	}
	a.started = true
	runCtx, cancel := context.WithCancel(ctx)
	a.runCtx = runCtx
	a.runCancel = cancel
	a.status = StatusRegistered
	master := a.master
	exec := a.exec
	a.mu.Unlock()

	log.InfoCtx(runCtx, "agent: starting",
		"agent_id", a.ID, "addr", a.Address, "caps", a.Capabilities,
		"max_concurrent", a.MaxConcurrent)

	if err := a.registerWithMaster(runCtx); err != nil {
		cancel()
		a.mu.Lock()
		a.status = StatusOffline
		a.mu.Unlock()
		return fmt.Errorf("agent: start: %w", err)
	}

	// Start the heartbeat loop in the background.
	interval := a.HeartbeatInterval
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	heartbeatCtx, heartbeatCancel := context.WithCancel(runCtx)
	go a.heartbeatLoop(heartbeatCtx, interval, master, func() Heartbeat {
		st := exec.Stats()
		hb := buildHeartbeatFromStats(a.ID, st, a.MaxConcurrent)
		a.mu.Lock()
		a.lastHeartbeat = hb.Timestamp
		a.status = computeStatus(&AgentInfo{
			ActiveTasks:   int(st.Active),
			MaxConcurrent: a.MaxConcurrent,
		})
		a.mu.Unlock()
		return hb
	})

	// Open the task stream. This blocks until the stream closes or
	// runCtx is cancelled.
	streamErr := master.StreamTasks(runCtx, a.ID, func(taskCtx context.Context, task Task) Result {
		res := exec.Execute(taskCtx, task)
		res.AgentID = a.ID
		return res
	})

	// Best-effort shutdown: cancel heartbeat, deregister, flip status.
	heartbeatCancel()
	a.deregisterFromMaster(context.Background())
	a.mu.Lock()
	a.status = StatusOffline
	a.runCancel = nil
	a.mu.Unlock()

	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		return fmt.Errorf("agent: task stream: %w", streamErr)
	}
	log.InfoCtx(ctx, "agent: stopped", "agent_id", a.ID)
	return nil
}

// registerWithMaster is a small wrapper around master.Register that
// builds the AgentInfo from the agent's fields.
func (a *Agent) registerWithMaster(ctx context.Context) error {
	info := AgentInfo{
		ID:            a.ID,
		Address:       a.Address,
		Capabilities:  append([]string(nil), a.Capabilities...),
		Status:        StatusRegistered,
		MaxConcurrent: a.MaxConcurrent,
		LastHeartbeat: time.Now(),
	}
	if err := a.master.Register(ctx, info); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	return nil
}

// deregisterFromMaster is a best-effort wrapper around master.Deregister.
// Errors are logged but not returned because Stop is called from a
// cleanup path where the caller has no good way to react.
func (a *Agent) deregisterFromMaster(ctx context.Context) {
	if err := a.master.Deregister(ctx, a.ID); err != nil {
		log.WarnCtx(ctx, "agent: deregister failed", "agent_id", a.ID, "err", err)
	}
}

// Register performs a one-shot registration with the master without
// starting the full lifecycle. It is useful for probes and tests. In
// normal operation Start calls Register internally.
func (a *Agent) Register() error {
	a.mu.Lock()
	master := a.master
	a.mu.Unlock()
	if master == nil {
		return fmt.Errorf("agent: register: master client not set")
	}
	return a.registerWithMaster(context.Background())
}

// Deregister performs a one-shot deregistration. It is the counterpart
// of Register and is safe to call even when the agent is not running.
func (a *Agent) Deregister() error {
	a.mu.Lock()
	master := a.master
	a.mu.Unlock()
	if master == nil {
		return fmt.Errorf("agent: deregister: master client not set")
	}
	if err := master.Deregister(context.Background(), a.ID); err != nil {
		return fmt.Errorf("agent: deregister: %w", err)
	}
	return nil
}

// Stop cancels the run context and waits for the heartbeat loop and
// task stream to wind down. It is safe to call at most once per Start.
// Calling Stop before Start returns ErrNotStarted.
//
// Stop does not block until in-flight tasks finish: the run context
// cancellation propagates to the executor which respects context
// cancellation. Long-running tasks may be interrupted; callers that
// need graceful drain should cancel their own context instead.
func (a *Agent) Stop() error {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return ErrNotStarted
	}
	cancel := a.runCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// executeTask is a package-internal helper used by the task stream
// handler. It is exposed (lowercase) so that tests can call it
// directly without going through the master client.
func (a *Agent) executeTask(ctx context.Context, task Task) Result {
	a.mu.Lock()
	exec := a.exec
	a.mu.Unlock()
	res := exec.Execute(ctx, task)
	res.AgentID = a.ID
	return res
}
