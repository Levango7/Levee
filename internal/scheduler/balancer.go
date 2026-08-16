// Package scheduler implements multi-node task scheduling for distributed
// execution. The scheduler distributes tasks across registered Agents based
// on capabilities and current load, with support for task sharding and load
// balancing.
//
// The package is organised around three files:
//
//   - balancer.go: the Balancer that picks a single agent for a task given a
//     strategy (round-robin, least-loaded, capability-aware).
//   - shard.go: the Shard helpers that split a large target list or task
//     batch into per-agent shards.
//   - scheduler.go: the top-level Scheduler that ties balancer + shard +
//     the agent registry together and exposes Schedule / Reassign /
//     CollectResults.
package scheduler

import (
	"sync"
	"sync/atomic"

	"github.com/nexus/levee/internal/agent"
)

// Strategy is the load-balancing strategy used by the Balancer and the
// Scheduler. It is a typed int so that it serialises as a small integer
// over the wire and in CLI flags.
type Strategy int

const (
	// RoundRobin cycles through the candidate agent list, ignoring
	// current load. It is the simplest strategy and is useful when
	// all agents are homogeneous and the task cost is uniform.
	RoundRobin Strategy = iota

	// LeastLoaded picks the agent with the most spare capacity
	// (MaxConcurrent - ActiveTasks). Ties are broken by agent ID
	// for determinism.
	LeastLoaded

	// CapabilityAware is like LeastLoaded but additionally filters
	// candidates by the module capability required by each task. It
	// is the default strategy for heterogeneous agent pools.
	CapabilityAware
)

// String returns a human-readable name for the strategy. It is used by
// the CLI flag formatter and log messages.
func (s Strategy) String() string {
	switch s {
	case RoundRobin:
		return "round_robin"
	case LeastLoaded:
		return "least_loaded"
	case CapabilityAware:
		return "capability_aware"
	default:
		return "unknown"
	}
}

// ParseStrategy converts a string name to a Strategy. Unknown names
// return CapabilityAware as a safe default (it never assigns a task to
// an agent that cannot run it).
func ParseStrategy(name string) Strategy {
	switch name {
	case "round_robin":
		return RoundRobin
	case "least_loaded":
		return LeastLoaded
	case "capability_aware":
		return CapabilityAware
	}
	return CapabilityAware
}

// Balancer picks a single agent from a candidate list according to the
// configured strategy. It is safe for concurrent use.
//
// The Balancer is stateful for the round-robin strategy (it keeps an
// atomic counter); the other strategies are stateless apart from the
// mutex that protects the candidate list during a Select call.
type Balancer struct {
	strategy Strategy
	rrIndex atomic.Int64
	mu      sync.Mutex
}

// NewBalancer returns a Balancer with the given strategy.
func NewBalancer(strategy Strategy) *Balancer {
	return &Balancer{strategy: strategy}
}

// Strategy returns the balancer's configured strategy.
func (b *Balancer) Strategy() Strategy {
	return b.strategy
}

// Select picks a single agent from agents that advertises requiredCap.
// When requiredCap is empty the capability filter is skipped. Select
// returns ErrNoCandidate when no agent matches.
//
// Select does not mutate the registry: it reads the supplied snapshot
// and returns a pointer to one of the entries. The caller is free to
// mutate the returned entry without affecting the registry.
func (b *Balancer) Select(agents []agent.AgentInfo, requiredCap string) (*agent.AgentInfo, error) {
	if len(agents) == 0 {
		return nil, ErrNoCandidate
	}
	candidates := filterByCapability(agents, requiredCap)
	if len(candidates) == 0 {
		return nil, ErrNoCandidate
	}

	switch b.strategy {
	case RoundRobin:
		return b.selectRoundRobin(candidates), nil
	case LeastLoaded:
		return b.selectLeastLoaded(candidates), nil
	default: // CapabilityAware falls back to least-loaded on the
		// already-filtered candidate list.
		return b.selectLeastLoaded(candidates), nil
	}
}

// selectRoundRobin picks the next candidate in round-robin order. The
// index is kept in an atomic so that concurrent Select calls do not
// need to take the mutex for the index increment.
func (b *Balancer) selectRoundRobin(candidates []agent.AgentInfo) *agent.AgentInfo {
	idx := b.rrIndex.Add(1) - 1
	chosen := int(idx) % len(candidates)
	if chosen < 0 {
		chosen += len(candidates)
	}
	cp := candidates[chosen]
	return &cp
}

// selectLeastLoaded picks the candidate with the most spare capacity.
// Ties are broken by agent ID for determinism.
func (b *Balancer) selectLeastLoaded(candidates []agent.AgentInfo) *agent.AgentInfo {
	best := candidates[0]
	for _, c := range candidates[1:] {
		if betterSpare(c, best) {
			best = c
		}
	}
	cp := best
	return &cp
}

// betterSpare reports whether a has strictly better spare capacity
// than b. Ties are broken by ID for determinism. The definition mirrors
// agent.AgentRegistry.FindLeastLoaded so that the two paths agree.
func betterSpare(a, b agent.AgentInfo) bool {
	sa, sb := a.SpareCapacity(), b.SpareCapacity()
	if sa != sb {
		return sa > sb
	}
	return a.ID < b.ID
}

// filterByCapability returns the subset of agents that advertise cap.
// An empty cap returns the input unchanged.
func filterByCapability(agents []agent.AgentInfo, cap string) []agent.AgentInfo {
	if cap == "" {
		return agents
	}
	out := make([]agent.AgentInfo, 0, len(agents))
	for _, a := range agents {
		if a.HasCapability(cap) && a.Status != agent.StatusOffline {
			out = append(out, a)
		}
	}
	return out
}

// UpdateLoad is a no-op hook reserved for future strategies that need
// to track observed load separately from the registry's view (e.g.
// exponentially-decaying load). It is safe to call concurrently with
// Select.
func (b *Balancer) UpdateLoad(agentID string, activeTasks int) {
	// Intentionally empty: the current strategies read load from the
	// agent snapshot passed to Select, so there is no separate state
	// to update. The method is kept in the API so that future
	// strategies can add state without breaking callers.
	_ = agentID
	_ = activeTasks
}