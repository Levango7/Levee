package scheduler

import (
	"sort"

	"github.com/nexus/levee/internal/agent"
)

// Target is a labelled target host used by ShardByLabel. The labels
// express affinity constraints (e.g. {"zone": "us-east-1a"}) that the
// sharder uses to keep targets with the same label together on a
// single agent.
type Target struct {
	Host   string            `json:"host"`
	Labels map[string]string `json:"labels"`
}

// Shard is a per-agent slice of work produced by the sharding helpers.
// It bundles the assigned agent ID, the target hosts and the concrete
// tasks the agent should run.
type Shard struct {
	// ID is the shard's positional index (0-based) for diagnostics.
	ID int `json:"id"`

	// AgentID is the agent that should execute this shard. It may be
	// empty when the shard has not yet been bound to an agent.
	AgentID string `json:"agent_id"`

	// Targets is the list of target hosts in this shard.
	Targets []string `json:"targets"`

	// Tasks is the list of tasks in this shard. When the shard is
	// built from targets only, Tasks is empty and the caller is
	// expected to expand targets into tasks later.
	Tasks []agent.Task `json:"tasks"`
}

// ShardTargets splits targets into numShards contiguous shards. When
// numShards <= 0 it defaults to 1. When numShards > len(targets) the
// extra shards are empty (this keeps the shard count stable even when
// the target list is short, which is useful for fan-out visualisation).
//
// The split is "even" in the sense that shard sizes differ by at most
// one; the first len(targets) % numShards shards get the extra element.
func ShardTargets(targets []string, numShards int) [][]string {
	if numShards <= 0 {
		numShards = 1
	}
	if len(targets) == 0 {
		return make([][]string, numShards)
	}
	out := make([][]string, numShards)
	n := len(targets)
	base := n / numShards
	rem := n % numShards
	idx := 0
	for i := 0; i < numShards; i++ {
		size := base
		if i < rem {
			size++
		}
		end := idx + size
		if end > n {
			end = n
		}
		if idx < n {
			out[i] = append([]string(nil), targets[idx:end]...)
		}
		idx = end
	}
	return out
}

// ShardByLabel splits targets into shards keyed by the value of a
// specific label. Targets sharing the same label value end up in the
// same shard. Targets without the label are grouped under the empty
// label. The returned shards are sorted by label value for determinism.
//
// This is the affinity-aware counterpart to ShardTargets: it keeps
// targets that should be handled together (e.g. same availability
// zone) on the same agent, reducing cross-zone traffic.
func ShardByLabel(targets []Target, label string) [][]Target {
	groups := make(map[string][]Target)
	for _, t := range targets {
		key := t.Labels[label]
		groups[key] = append(groups[key], t)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][]Target, 0, len(keys))
	for _, k := range keys {
		out = append(out, groups[k])
	}
	return out
}

// CreateShards distributes tasks across agents using the balancer's
// configured strategy. Each task is assigned to a single agent; an
// agent may receive multiple tasks. Tasks that no agent can handle
// (missing capability or all agents offline) are placed in a shard
// with an empty AgentID so that the caller can detect the failure.
//
// The returned shards are sorted by shard ID (which mirrors the agent
// order returned by the balancer) for deterministic output.
func CreateShards(tasks []agent.Task, agents []agent.AgentInfo, b *Balancer) []Shard {
	if len(tasks) == 0 {
		return nil
	}
	// Group tasks by assigned agent.
	assignments := make(map[string][]agent.Task)
	unassigned := []agent.Task{}
	for _, t := range tasks {
		cap := t.Module
		picked, err := b.Select(agents, cap)
		if err != nil {
			unassigned = append(unassigned, t)
			continue
		}
		assignments[picked.ID] = append(assignments[picked.ID], t)
	}

	// Build shards sorted by agent ID for determinism.
	agentIDs := make([]string, 0, len(assignments))
	for id := range assignments {
		agentIDs = append(agentIDs, id)
	}
	sort.Strings(agentIDs)

	shards := make([]Shard, 0, len(agentIDs)+1)
	for i, id := range agentIDs {
		shards = append(shards, Shard{
			ID:      i,
			AgentID: id,
			Tasks:   assignments[id],
		})
	}
	if len(unassigned) > 0 {
		shards = append(shards, Shard{
			ID:      len(shards),
			Tasks:   unassigned,
			Targets: nil,
		})
	}
	return shards
}

// ShardTargetsByCount is a convenience wrapper that shards targets and
// returns Shard structs (without tasks) bound to the given agents in
// round-robin order. It is the simplest way to fan out a target list
// when there is no per-task capability constraint.
func ShardTargetsByCount(targets []string, agents []agent.AgentInfo) []Shard {
	numShards := len(agents)
	if numShards == 0 {
		return []Shard{{ID: 0, Targets: targets}}
	}
	parts := ShardTargets(targets, numShards)
	shards := make([]Shard, numShards)
	for i, part := range parts {
		shards[i] = Shard{
			ID:      i,
			AgentID: agents[i].ID,
			Targets: part,
		}
	}
	return shards
}
