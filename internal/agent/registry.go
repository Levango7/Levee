package agent

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// AgentStatus is the lifecycle state of an agent from the master's
// perspective. It is a typed string so that it serialises cleanly over
// the wire and in CLI output.
type AgentStatus string

const (
	// StatusRegistered means the agent has called Register but the
	// master has not yet received a heartbeat. This is a transient
	// state; the first heartbeat flips it to StatusIdle.
	StatusRegistered AgentStatus = "registered"

	// StatusIdle means the agent is alive and has spare capacity.
	StatusIdle AgentStatus = "idle"

	// StatusBusy means the agent is alive but its active task count
	// equals its MaxConcurrent. New tasks should not be assigned.
	StatusBusy AgentStatus = "busy"

	// StatusOffline means the master has not received a heartbeat
	// within the staleness window. The agent may still be running but
	// is considered unavailable for scheduling.
	StatusOffline AgentStatus = "offline"
)

// AgentInfo is the master-side record for a registered agent. It is
// kept in the AgentRegistry and updated by Register / Deregister /
// Heartbeat. All fields are value types so that the struct can be
// returned by value from List / FindByCapability without copying
// concerns.
type AgentInfo struct {
	// ID is the agent's unique identifier (typically a UUID).
	ID string `json:"id"`

	// Address is the host:port at which the agent can be reached for
	// task dispatch. It is supplied at registration time.
	Address string `json:"address"`

	// Capabilities is the list of module names the agent can execute
	// (e.g. ["shell", "file", "pkg"]). The scheduler uses it to filter
	// candidate agents for a task.
	Capabilities []string `json:"capabilities"`

	// Status is the current lifecycle state.
	Status AgentStatus `json:"status"`

	// LastHeartbeat is the timestamp of the most recent heartbeat
	// received. It is used by CleanupStale to detect offline agents.
	LastHeartbeat time.Time `json:"last_heartbeat"`

	// ActiveTasks is the number of tasks currently executing on the
	// agent, as reported by the most recent heartbeat.
	ActiveTasks int `json:"active_tasks"`

	// CompletedTasks is the cumulative count of successful task
	// completions reported by heartbeats.
	CompletedTasks int64 `json:"completed_tasks"`

	// FailedTasks is the cumulative count of failed task completions
	// reported by heartbeats.
	FailedTasks int64 `json:"failed_tasks"`

	// MaxConcurrent is the agent's configured concurrency limit. The
	// scheduler uses it to compute spare capacity.
	MaxConcurrent int `json:"max_concurrent"`
}

// SpareCapacity returns the number of additional tasks the agent can
// accept right now. It is always non-negative: an over-loaded agent
// reports zero rather than a negative number.
func (a AgentInfo) SpareCapacity() int {
	c := a.MaxConcurrent - a.ActiveTasks
	if c < 0 {
		return 0
	}
	return c
}

// HasCapability reports whether the agent advertises the given module
// capability. The match is case-sensitive, matching the executor's
// module-name semantics.
func (a AgentInfo) HasCapability(cap string) bool {
	for _, c := range a.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// ErrAgentNotFound is returned by registry methods that take an agent
// ID when no agent with that ID is registered.
var ErrAgentNotFound = errors.New("agent: not found")

// ErrAgentAlreadyRegistered is returned by Register when an agent with
// the same ID is already in the registry. Callers should use Heartbeat
// to update an existing agent instead.
var ErrAgentAlreadyRegistered = errors.New("agent: already registered")

// AgentRegistry is the master-side bookkeeping for all registered
// agents. It is safe for concurrent use: every read/write method takes
// the appropriate RWMutex lock.
//
// The registry is the single source of truth for the scheduler: the
// scheduler queries FindByCapability / FindLeastLoaded to pick an
// agent for a task, and updates ActiveTasks via Heartbeat as tasks
// start and finish.
type AgentRegistry struct {
	mu     sync.RWMutex
	agents map[string]*AgentInfo
}

// NewAgentRegistry returns an empty registry ready to use.
func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{agents: make(map[string]*AgentInfo)}
}

// Register adds a new agent to the registry. If an agent with the same
// ID is already registered, ErrAgentAlreadyRegistered is returned. The
// caller should instead call Heartbeat to update an existing agent.
//
// The supplied AgentInfo is copied by value; the registry does not
// retain a pointer to the caller's struct. LastHeartbeat is set to
// now so that a freshly-registered agent is not immediately considered
// stale.
func (r *AgentRegistry) Register(info AgentInfo) error {
	if info.ID == "" {
		return fmt.Errorf("agent: register: empty agent id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[info.ID]; ok {
		return fmt.Errorf("agent: register %q: %w", info.ID, ErrAgentAlreadyRegistered)
	}
	if info.LastHeartbeat.IsZero() {
		info.LastHeartbeat = time.Now()
	}
	if info.Status == "" {
		info.Status = StatusRegistered
	}
	// Defensive copy of the capabilities slice so that later caller
	// mutations do not affect the registry.
	info.Capabilities = append([]string(nil), info.Capabilities...)
	r.agents[info.ID] = &info
	return nil
}

// Deregister removes an agent from the registry. It is a no-op (returns
// ErrAgentNotFound) when the agent is not present, which is the
// expected outcome when an agent crashes and CleanupStale has already
// reaped it.
func (r *AgentRegistry) Deregister(agentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[agentID]; !ok {
		return fmt.Errorf("agent: deregister %q: %w", agentID, ErrAgentNotFound)
	}
	delete(r.agents, agentID)
	return nil
}

// Heartbeat updates an existing agent's load counters and
// LastHeartbeat. It is the hot path: every agent calls it once per
// heartbeat interval. The agent's Status is recomputed from the new
// load: idle when ActiveTasks == 0, busy when ActiveTasks >=
// MaxConcurrent, otherwise idle.
func (r *AgentRegistry) Heartbeat(agentID string, hb Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.agents[agentID]
	if !ok {
		return fmt.Errorf("agent: heartbeat %q: %w", agentID, ErrAgentNotFound)
	}
	info.LastHeartbeat = hb.Timestamp
	if info.LastHeartbeat.IsZero() {
		info.LastHeartbeat = time.Now()
	}
	info.ActiveTasks = hb.ActiveTasks
	info.CompletedTasks = hb.CompletedTasks
	info.FailedTasks = hb.FailedTasks
	if hb.MaxConcurrent > 0 {
		info.MaxConcurrent = hb.MaxConcurrent
	}
	info.Status = computeStatus(info)
	return nil
}

// computeStatus derives the agent's lifecycle state from its load. It
// is a free function so that tests can exercise it directly.
func computeStatus(info *AgentInfo) AgentStatus {
	if info.ActiveTasks <= 0 {
		return StatusIdle
	}
	if info.MaxConcurrent > 0 && info.ActiveTasks >= info.MaxConcurrent {
		return StatusBusy
	}
	return StatusIdle
}

// List returns a snapshot of all registered agents sorted by ID. The
// returned slice is a value copy and may be stale by the time the
// caller reads it; that is acceptable for CLI display and scheduler
// snapshots which only need a point-in-time view.
func (r *AgentRegistry) List() []AgentInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AgentInfo, 0, len(r.agents))
	for _, info := range r.agents {
		out = append(out, *info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns a snapshot of a single agent by ID. It returns
// ErrAgentNotFound (wrapped) when the agent is not registered.
func (r *AgentRegistry) Get(agentID string) (AgentInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.agents[agentID]
	if !ok {
		return AgentInfo{}, fmt.Errorf("agent: get %q: %w", agentID, ErrAgentNotFound)
	}
	return *info, nil
}

// FindByCapability returns all agents that advertise the given
// capability and are not offline. The result is sorted by ID for
// deterministic output. An empty capability returns all non-offline
// agents.
func (r *AgentRegistry) FindByCapability(cap string) []AgentInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []AgentInfo
	for _, info := range r.agents {
		if info.Status == StatusOffline {
			continue
		}
		if cap == "" || info.HasCapability(cap) {
			out = append(out, *info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// FindLeastLoaded returns the agent with the most spare capacity among
// those advertising cap. Ties are broken by ID for determinism. When no
// agent matches, ErrAgentNotFound is returned.
//
// "Spare capacity" is MaxConcurrent - ActiveTasks; an agent with zero
// spare capacity is still returned (the caller may decide to queue the
// task rather than reject it) unless there is at least one agent with
// positive spare capacity, in which case only positive-capacity agents
// are considered.
func (r *AgentRegistry) FindLeastLoaded(cap string) (*AgentInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var (
		bestPositive *AgentInfo
		bestAny      *AgentInfo
	)
	for _, info := range r.agents {
		if info.Status == StatusOffline {
			continue
		}
		if cap != "" && !info.HasCapability(cap) {
			continue
		}
		// Work on a local copy so we can take its address safely.
		copy := *info
		if copy.SpareCapacity() > 0 {
			if bestPositive == nil || betterSpare(copy, *bestPositive) {
				cp := copy
				bestPositive = &cp
			}
		}
		if bestAny == nil || betterSpare(copy, *bestAny) {
			cp := copy
			bestAny = &cp
		}
	}
	if bestPositive != nil {
		return bestPositive, nil
	}
	if bestAny != nil {
		return bestAny, nil
	}
	return nil, fmt.Errorf("agent: find least loaded for cap %q: %w", cap, ErrAgentNotFound)
}

// betterSpare reports whether a has strictly better spare capacity than
// b. Ties are broken by ID so that the result is deterministic.
func betterSpare(a, b AgentInfo) bool {
	sa, sb := a.SpareCapacity(), b.SpareCapacity()
	if sa != sb {
		return sa > sb
	}
	return a.ID < b.ID
}

// MarkOffline flips an agent's status to offline without removing it
// from the registry. The agent's record is retained so that, if it
// comes back online and re-registers, the master can detect the
// duplicate and update instead of erroring.
func (r *AgentRegistry) MarkOffline(agentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.agents[agentID]
	if !ok {
		return fmt.Errorf("agent: mark offline %q: %w", agentID, ErrAgentNotFound)
	}
	info.Status = StatusOffline
	return nil
}

// CleanupStale marks as offline every agent whose LastHeartbeat is
// older than now-timeout. It returns the IDs of the agents that were
// flipped. Agents already offline are not included in the result.
//
// CleanupStale is intended to be called periodically by the master
// (e.g. once per heartbeat interval) to detect crashed or network-
// partitioned agents.
func (r *AgentRegistry) CleanupStale(timeout time.Duration) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	threshold := time.Now().Add(-timeout)
	var stale []string
	for _, info := range r.agents {
		if info.Status == StatusOffline {
			continue
		}
		if info.LastHeartbeat.Before(threshold) {
			info.Status = StatusOffline
			stale = append(stale, info.ID)
		}
	}
	sort.Strings(stale)
	return stale
}

// Count returns the number of registered agents. It is a cheaper
// alternative to len(List()) for monitoring / metrics.
func (r *AgentRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.agents)
}
