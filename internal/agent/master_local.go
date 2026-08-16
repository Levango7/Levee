package agent


import (
	"context"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// InProcessMasterClient is a MasterClient implementation that runs
// entirely in the current process. It is the MVP production path when
// the master and the agent live in the same binary, and the test path
// for agent unit tests.
//
// The client owns an AgentRegistry (shared with the caller so that the
// CLI `agent list` command can see registered agents) and an optional
// task source. When a task source is set, StreamTasks drains it; when
// it is not set, StreamTasks blocks until the context is cancelled.
type InProcessMasterClient struct {
	registry *AgentRegistry
	source   TaskSource
	mu       sync.Mutex
}

// TaskSource is the interface the InProcessMasterClient uses to pull
// tasks to dispatch to agents. The master's real scheduler implements
// this; tests substitute a static source.
type TaskSource interface {
	// NextTask returns the next task to dispatch to agentID, or
	// false when no task is currently available. It must be safe to
	// call concurrently.
	NextTask(ctx context.Context, agentID string) (Task, bool)
}

// NewInProcessMasterClient returns an InProcessMasterClient backed by
// the given registry. The registry may be shared with other components
// (e.g. the CLI's global registry).
func NewInProcessMasterClient(registry *AgentRegistry) *InProcessMasterClient {
	if registry == nil {
		registry = NewAgentRegistry()
	}
	return &InProcessMasterClient{registry: registry}
}

// SetTaskSource installs the task source used by StreamTasks. It is
// optional; when not set, StreamTasks blocks until the context is
// cancelled.
func (c *InProcessMasterClient) SetTaskSource(s TaskSource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.source = s
}

// Registry returns the underlying agent registry. It is exposed so
// that the CLI can list / show / remove agents through the same
// registry the master client uses.
func (c *InProcessMasterClient) Registry() *AgentRegistry {
	return c.registry
}

// Register adds the agent to the local registry.
func (c *InProcessMasterClient) Register(ctx context.Context, info AgentInfo) error {
	// Re-registration is treated as an update: if the agent is already
	// present we deregister first so that Register succeeds. This
	// makes the master client idempotent as required by the
	// MasterClient contract.
	if existing, err := c.registry.Get(info.ID); err == nil && existing.ID == info.ID {
		_ = c.registry.Deregister(info.ID)
	}
	return c.registry.Register(info)
}

// Deregister removes the agent from the local registry.
func (c *InProcessMasterClient) Deregister(ctx context.Context, agentID string) error {
	return c.registry.Deregister(agentID)
}

// SendHeartbeat updates the agent's load counters in the local
// registry.
func (c *InProcessMasterClient) SendHeartbeat(ctx context.Context, hb Heartbeat) error {
	if _, err := c.registry.Get(hb.AgentID); err != nil {
		// Auto-register on first heartbeat so that an agent that
		// started before the master can still join.
		_ = c.registry.Register(AgentInfo{
			ID:            hb.AgentID,
			LastHeartbeat: hb.Timestamp,
			MaxConcurrent: hb.MaxConcurrent,
			Status:        StatusRegistered,
		})
	}
	return c.registry.Heartbeat(hb.AgentID, hb)
}

// StreamTasks pulls tasks from the configured source and dispatches
// them to the handler until the context is cancelled or the source is
// exhausted. When no source is set, StreamTasks blocks until ctx is
// cancelled (this is the idle agent case).
func (c *InProcessMasterClient) StreamTasks(ctx context.Context, agentID string, handler TaskHandler) error {
	c.mu.Lock()
	source := c.source
	c.mu.Unlock()

	if source == nil {
		// No source: block until cancelled. This is the idle agent
		// case; the agent stays alive and ready for the master to
		// push tasks through a future source.
		<-ctx.Done()
		return ctx.Err()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		task, ok := source.NextTask(ctx, agentID)
		if !ok {
			// Source exhausted: wait a short beat then retry, so
			// that a transient empty source does not spin the
			// CPU. Honour ctx for prompt cancellation.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		res := handler(ctx, task)
		log.DebugCtx(ctx, "agent: task completed",
			"agent_id", agentID, "task_id", res.TaskID,
			"success", res.Success)
	}
}