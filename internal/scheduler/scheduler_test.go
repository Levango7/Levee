package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/agent"
)

// --- Strategy --------------------------------------------------------------

func TestStrategyString(t *testing.T) {
	assert.Equal(t, "round_robin", RoundRobin.String())
	assert.Equal(t, "least_loaded", LeastLoaded.String())
	assert.Equal(t, "capability_aware", CapabilityAware.String())
	assert.Equal(t, "unknown", Strategy(99).String())
}

func TestParseStrategy(t *testing.T) {
	tests := []struct {
		in   string
		want Strategy
	}{
		{"round_robin", RoundRobin},
		{"least_loaded", LeastLoaded},
		{"capability_aware", CapabilityAware},
		{"unknown", CapabilityAware}, // default
		{"", CapabilityAware},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, ParseStrategy(tc.in), "input=%q", tc.in)
	}
}

// --- Balancer --------------------------------------------------------------

func TestNewBalancer(t *testing.T) {
	b := NewBalancer(LeastLoaded)
	assert.Equal(t, LeastLoaded, b.Strategy())
}

func TestBalancerSelectEmpty(t *testing.T) {
	b := NewBalancer(RoundRobin)
	_, err := b.Select(nil, "shell")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoCandidate)
}

func TestBalancerSelectNoCapability(t *testing.T) {
	b := NewBalancer(CapabilityAware)
	agents := []agent.AgentInfo{{ID: "a1", Capabilities: []string{"file"}}}
	_, err := b.Select(agents, "shell")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoCandidate)
}

func TestBalancerSelectRoundRobin(t *testing.T) {
	b := NewBalancer(RoundRobin)
	agents := []agent.AgentInfo{
		{ID: "a1", Capabilities: []string{"shell"}, MaxConcurrent: 2},
		{ID: "a2", Capabilities: []string{"shell"}, MaxConcurrent: 2},
		{ID: "a3", Capabilities: []string{"shell"}, MaxConcurrent: 2},
	}

	picks := map[string]int{}
	for i := 0; i < 9; i++ {
		got, err := b.Select(agents, "shell")
		require.NoError(t, err)
		picks[got.ID]++
	}
	assert.Equal(t, 3, picks["a1"])
	assert.Equal(t, 3, picks["a2"])
	assert.Equal(t, 3, picks["a3"])
}

func TestBalancerSelectLeastLoaded(t *testing.T) {
	b := NewBalancer(LeastLoaded)
	agents := []agent.AgentInfo{
		{ID: "a1", Capabilities: []string{"shell"}, MaxConcurrent: 2, ActiveTasks: 1},
		{ID: "a2", Capabilities: []string{"shell"}, MaxConcurrent: 2, ActiveTasks: 0},
	}
	got, err := b.Select(agents, "shell")
	require.NoError(t, err)
	assert.Equal(t, "a2", got.ID)
}

func TestBalancerSelectCapabilityAware(t *testing.T) {
	b := NewBalancer(CapabilityAware)
	agents := []agent.AgentInfo{
		{ID: "a1", Capabilities: []string{"file"}, MaxConcurrent: 2},
		{ID: "a2", Capabilities: []string{"shell"}, MaxConcurrent: 2},
	}
	got, err := b.Select(agents, "shell")
	require.NoError(t, err)
	assert.Equal(t, "a2", got.ID)
}

func TestBalancerSelectEmptyCapabilitySkipsFilter(t *testing.T) {
	b := NewBalancer(LeastLoaded)
	agents := []agent.AgentInfo{
		{ID: "a1", Capabilities: []string{"file"}, MaxConcurrent: 2},
	}
	got, err := b.Select(agents, "")
	require.NoError(t, err)
	assert.Equal(t, "a1", got.ID)
}

func TestBalancerSelectExcludesOffline(t *testing.T) {
	b := NewBalancer(LeastLoaded)
	agents := []agent.AgentInfo{
		{ID: "offline", Capabilities: []string{"shell"}, Status: agent.StatusOffline, MaxConcurrent: 2},
		{ID: "online", Capabilities: []string{"shell"}, MaxConcurrent: 2},
	}
	got, err := b.Select(agents, "shell")
	require.NoError(t, err)
	assert.Equal(t, "online", got.ID)
}

func TestBalancerUpdateLoadNoOp(t *testing.T) {
	b := NewBalancer(LeastLoaded)
	// Should not panic.
	b.UpdateLoad("a1", 5)
}

func TestBetterSpare(t *testing.T) {
	a := agent.AgentInfo{ID: "a", MaxConcurrent: 4, ActiveTasks: 0}
	b := agent.AgentInfo{ID: "b", MaxConcurrent: 4, ActiveTasks: 2}
	assert.True(t, betterSpare(a, b))
	assert.False(t, betterSpare(b, a))
}

func TestBetterSpareTieBreakByID(t *testing.T) {
	a := agent.AgentInfo{ID: "a", MaxConcurrent: 4, ActiveTasks: 0}
	b := agent.AgentInfo{ID: "b", MaxConcurrent: 4, ActiveTasks: 0}
	assert.True(t, betterSpare(a, b))
	assert.False(t, betterSpare(b, a))
}

func TestFilterByCapability(t *testing.T) {
	agents := []agent.AgentInfo{
		{ID: "a1", Capabilities: []string{"shell"}},
		{ID: "a2", Capabilities: []string{"file"}},
		{ID: "a3", Capabilities: []string{"shell", "file"}},
	}
	out := filterByCapability(agents, "shell")
	assert.Len(t, out, 2)
	assert.Equal(t, "a1", out[0].ID)
	assert.Equal(t, "a3", out[1].ID)
}

func TestFilterByCapabilityEmpty(t *testing.T) {
	agents := []agent.AgentInfo{{ID: "a1"}}
	out := filterByCapability(agents, "")
	assert.Equal(t, agents, out)
}

// --- Shard -----------------------------------------------------------------

func TestShardTargetsEven(t *testing.T) {
	targets := []string{"h1", "h2", "h3", "h4", "h5", "h6"}
	shards := ShardTargets(targets, 3)
	require.Len(t, shards, 3)
	assert.Equal(t, []string{"h1", "h2"}, shards[0])
	assert.Equal(t, []string{"h3", "h4"}, shards[1])
	assert.Equal(t, []string{"h5", "h6"}, shards[2])
}

func TestShardTargetsUneven(t *testing.T) {
	targets := []string{"h1", "h2", "h3", "h4", "h5"}
	shards := ShardTargets(targets, 3)
	require.Len(t, shards, 3)
	// 5 / 3 = 1 remainder 2 -> first two shards get 2, last gets 1.
	assert.Equal(t, []string{"h1", "h2"}, shards[0])
	assert.Equal(t, []string{"h3", "h4"}, shards[1])
	assert.Equal(t, []string{"h5"}, shards[2])
}

func TestShardTargetsMoreShardsThanTargets(t *testing.T) {
	targets := []string{"h1"}
	shards := ShardTargets(targets, 3)
	require.Len(t, shards, 3)
	assert.Equal(t, []string{"h1"}, shards[0])
	assert.Empty(t, shards[1])
	assert.Empty(t, shards[2])
}

func TestShardTargetsZeroShards(t *testing.T) {
	shards := ShardTargets([]string{"h1"}, 0)
	require.Len(t, shards, 1)
	assert.Equal(t, []string{"h1"}, shards[0])
}

func TestShardTargetsEmpty(t *testing.T) {
	shards := ShardTargets(nil, 3)
	require.Len(t, shards, 3)
	for _, s := range shards {
		assert.Nil(t, s)
	}
}

func TestShardByLabel(t *testing.T) {
	targets := []Target{
		{Host: "h1", Labels: map[string]string{"zone": "a"}},
		{Host: "h2", Labels: map[string]string{"zone": "b"}},
		{Host: "h3", Labels: map[string]string{"zone": "a"}},
		{Host: "h4", Labels: map[string]string{"zone": "b"}},
	}
	shards := ShardByLabel(targets, "zone")
	require.Len(t, shards, 2)
	// Sorted by label value: "a" first, then "b".
	assert.Len(t, shards[0], 2)
	assert.Len(t, shards[1], 2)
	assert.Equal(t, "h1", shards[0][0].Host)
	assert.Equal(t, "h3", shards[0][1].Host)
}

func TestShardByLabelMissingLabel(t *testing.T) {
	targets := []Target{
		{Host: "h1", Labels: map[string]string{"zone": "a"}},
		{Host: "h2", Labels: nil},
	}
	shards := ShardByLabel(targets, "zone")
	require.Len(t, shards, 2)
	// "" sorts before "a".
	assert.Equal(t, "h2", shards[0][0].Host)
	assert.Equal(t, "h1", shards[1][0].Host)
}

func TestCreateShards(t *testing.T) {
	tasks := []agent.Task{
		{ID: "t1", Module: "shell"},
		{ID: "t2", Module: "file"},
		{ID: "t3", Module: "shell"},
	}
	agents := []agent.AgentInfo{
		{ID: "a1", Capabilities: []string{"shell"}, MaxConcurrent: 4},
		{ID: "a2", Capabilities: []string{"file"}, MaxConcurrent: 4},
	}
	b := NewBalancer(CapabilityAware)
	shards := CreateShards(tasks, agents, b)
	assert.NotEmpty(t, shards)

	// Every task should be assigned (no unassigned shard).
	total := 0
	for _, s := range shards {
		total += len(s.Tasks)
	}
	assert.Equal(t, 3, total)
}

func TestCreateShardsUnassigned(t *testing.T) {
	tasks := []agent.Task{
		{ID: "t1", Module: "pkg"},
	}
	agents := []agent.AgentInfo{
		{ID: "a1", Capabilities: []string{"shell"}, MaxConcurrent: 4},
	}
	b := NewBalancer(CapabilityAware)
	shards := CreateShards(tasks, agents, b)
	require.Len(t, shards, 1)
	assert.Empty(t, shards[0].AgentID, "no agent can handle pkg")
	assert.Len(t, shards[0].Tasks, 1)
}

func TestCreateShardsEmpty(t *testing.T) {
	b := NewBalancer(RoundRobin)
	assert.Nil(t, CreateShards(nil, nil, b))
}

func TestShardTargetsByCount(t *testing.T) {
	targets := []string{"h1", "h2", "h3", "h4"}
	agents := []agent.AgentInfo{
		{ID: "a1"},
		{ID: "a2"},
	}
	shards := ShardTargetsByCount(targets, agents)
	require.Len(t, shards, 2)
	assert.Equal(t, "a1", shards[0].AgentID)
	assert.Equal(t, "a2", shards[1].AgentID)
	assert.Equal(t, []string{"h1", "h2"}, shards[0].Targets)
	assert.Equal(t, []string{"h3", "h4"}, shards[1].Targets)
}

func TestShardTargetsByCountNoAgents(t *testing.T) {
	targets := []string{"h1", "h2"}
	shards := ShardTargetsByCount(targets, nil)
	require.Len(t, shards, 1)
	assert.Empty(t, shards[0].AgentID)
	assert.Equal(t, targets, shards[0].Targets)
}

// --- Scheduler -------------------------------------------------------------

func TestNewScheduler(t *testing.T) {
	r := agent.NewAgentRegistry()
	s := NewScheduler(r, LeastLoaded)
	assert.Same(t, r, s.Registry())
	assert.NotNil(t, s.Balancer())
}

func TestSchedulerSetDispatcher(t *testing.T) {
	s := NewScheduler(agent.NewAgentRegistry(), RoundRobin)
	s.SetDispatcher(NewLocalDispatcher(agent.NewAgentExecutor(1, nil)))
	// No direct getter; exercise via CollectResults which needs it.
	_, err := s.CollectResults(nil, nil)
	require.NoError(t, err)
}

func TestSchedulerScheduleEmpty(t *testing.T) {
	s := NewScheduler(agent.NewAgentRegistry(), RoundRobin)
	out, err := s.Schedule(nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestSchedulerScheduleNoAgents(t *testing.T) {
	s := NewScheduler(agent.NewAgentRegistry(), RoundRobin)
	_, err := s.Schedule([]agent.Task{{ID: "t1"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no agents")
}

func TestSchedulerScheduleRoundRobin(t *testing.T) {
	r := agent.NewAgentRegistry()
	require.NoError(t, r.Register(agent.AgentInfo{ID: "a1", Capabilities: []string{"shell"}, MaxConcurrent: 4}))
	require.NoError(t, r.Register(agent.AgentInfo{ID: "a2", Capabilities: []string{"shell"}, MaxConcurrent: 4}))

	s := NewScheduler(r, RoundRobin)
	tasks := []agent.Task{
		{ID: "t1", Module: "shell"},
		{ID: "t2", Module: "shell"},
	}
	out, err := s.Schedule(tasks)
	require.NoError(t, err)
	require.Len(t, out, 2)
	// Round-robin should distribute across both agents.
	ids := map[string]int{}
	for _, a := range out {
		ids[a.AgentID]++
	}
	assert.Equal(t, 1, ids["a1"])
	assert.Equal(t, 1, ids["a2"])
}

func TestSchedulerScheduleCapabilityAwareUnassigned(t *testing.T) {
	r := agent.NewAgentRegistry()
	require.NoError(t, r.Register(agent.AgentInfo{ID: "a1", Capabilities: []string{"file"}, MaxConcurrent: 4}))

	s := NewScheduler(r, CapabilityAware)
	tasks := []agent.Task{{ID: "t1", Module: "shell"}}
	out, err := s.Schedule(tasks)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Empty(t, out[0].AgentID, "no agent can handle shell")
}

func TestSchedulerReassignNoCandidate(t *testing.T) {
	r := agent.NewAgentRegistry()
	require.NoError(t, r.Register(agent.AgentInfo{ID: "a1"}))
	s := NewScheduler(r, RoundRobin)
	_, err := s.Reassign("t1", "a1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAssignmentFailed)
}

func TestSchedulerReassignSuccess(t *testing.T) {
	r := agent.NewAgentRegistry()
	require.NoError(t, r.Register(agent.AgentInfo{ID: "a1"}))
	require.NoError(t, r.Register(agent.AgentInfo{ID: "a2"}))
	s := NewScheduler(r, RoundRobin)
	asg, err := s.Reassign("t1", "a1")
	require.NoError(t, err)
	assert.Equal(t, "t1", asg.Task.ID)
}

func TestSchedulerCollectResultsNoDispatcher(t *testing.T) {
	s := NewScheduler(agent.NewAgentRegistry(), RoundRobin)
	_, err := s.CollectResults(nil, []Assignment{{Task: agent.Task{ID: "t1"}, AgentID: "a1"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dispatcher not set")
}

func TestSchedulerCollectResultsEmpty(t *testing.T) {
	s := NewScheduler(agent.NewAgentRegistry(), RoundRobin)
	s.SetDispatcher(NewLocalDispatcher(agent.NewAgentExecutor(1, nil)))
	_, err := s.CollectResults(nil, nil)
	require.NoError(t, err)
}

func TestSchedulerCollectResultsUnassignedSkipped(t *testing.T) {
	s := NewScheduler(agent.NewAgentRegistry(), RoundRobin)
	s.SetDispatcher(NewLocalDispatcher(agent.NewAgentExecutor(1, nil)))
	out, err := s.CollectResults(nil, []Assignment{{Task: agent.Task{ID: "t1"}}})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.NotEmpty(t, out[0].Error)
}

// mockDispatcher counts Dispatch calls for deterministic testing.
type mockDispatcher struct {
	mu       sync.Mutex
	calls    atomic.Int32
	results  map[string]agent.Result
	failWith error
	panicOn  map[string]any // task ID -> panic value
}

func newMockDispatcher() *mockDispatcher {
	return &mockDispatcher{results: make(map[string]agent.Result), panicOn: make(map[string]any)}
}

func (d *mockDispatcher) Dispatch(ctx context.Context, agentID string, task agent.Task) (agent.Result, error) {
	d.calls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if v, ok := d.panicOn[task.ID]; ok {
		panic(v)
	}
	if d.failWith != nil {
		return agent.Result{}, d.failWith
	}
	if r, ok := d.results[task.ID]; ok {
		return r, nil
	}
	return agent.Result{TaskID: task.ID, AgentID: agentID, Success: true}, nil
}

func TestSchedulerCollectResultsWithMock(t *testing.T) {
	r := agent.NewAgentRegistry()
	require.NoError(t, r.Register(agent.AgentInfo{ID: "a1", Capabilities: []string{"shell"}, MaxConcurrent: 4}))
	s := NewScheduler(r, RoundRobin)

	d := newMockDispatcher()
	d.results["t1"] = agent.Result{TaskID: "t1", Success: true, ExitCode: 0, Stdout: "ok"}
	s.SetDispatcher(d)

	assignments := []Assignment{
		{Task: agent.Task{ID: "t1", Module: "shell"}, AgentID: "a1"},
	}
	out, err := s.CollectResults(nil, assignments)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.True(t, out[0].Success)
	assert.Equal(t, "ok", out[0].Stdout)
	assert.Equal(t, int32(1), d.calls.Load())
}

func TestSchedulerCollectResultsError(t *testing.T) {
	r := agent.NewAgentRegistry()
	require.NoError(t, r.Register(agent.AgentInfo{ID: "a1", Capabilities: []string{"shell"}, MaxConcurrent: 4}))
	s := NewScheduler(r, RoundRobin)

	d := newMockDispatcher()
	d.failWith = assert.AnError
	s.SetDispatcher(d)

	assignments := []Assignment{
		{Task: agent.Task{ID: "t1"}, AgentID: "a1"},
	}
	out, err := s.CollectResults(nil, assignments)
	require.Error(t, err)
	require.Len(t, out, 1)
	assert.False(t, out[0].Success)
	assert.NotEmpty(t, out[0].Error)
}

func TestSchedulerCollectResultsRecoversPanic(t *testing.T) {
	r := agent.NewAgentRegistry()
	require.NoError(t, r.Register(agent.AgentInfo{ID: "a1"}))
	s := NewScheduler(r, RoundRobin)

	d := newMockDispatcher()
	d.panicOn["t-bad"] = "dispatch boom"
	d.results["t-good"] = agent.Result{TaskID: "t-good", Success: true}
	s.SetDispatcher(d)

	out, err := s.CollectResults(nil, []Assignment{
		{Task: agent.Task{ID: "t-good"}, AgentID: "a1"},
		{Task: agent.Task{ID: "t-bad", Module: "shell"}, AgentID: "a1"},
	})
	require.Error(t, err, "the panic must surface as a dispatch error")
	assert.Contains(t, err.Error(), "panicked")
	require.Len(t, out, 2)
	assert.True(t, out[0].Success)
	assert.False(t, out[1].Success, "panicking dispatch must be recorded as a failed result")
	assert.Contains(t, out[1].Error, "panicked")
	assert.Contains(t, out[1].Error, "dispatch boom")
}

func TestSchedulerSnapshot(t *testing.T) {
	r := agent.NewAgentRegistry()
	require.NoError(t, r.Register(agent.AgentInfo{ID: "b"}))
	require.NoError(t, r.Register(agent.AgentInfo{ID: "a"}))
	s := NewScheduler(r, RoundRobin)
	snap := s.Snapshot()
	require.Len(t, snap, 2)
	assert.Equal(t, "a", snap[0].ID)
	assert.Equal(t, "b", snap[1].ID)
}

// --- LocalDispatcher -------------------------------------------------------

func TestLocalDispatcherDispatch(t *testing.T) {
	exec := agent.NewAgentExecutor(1, nil)
	exec.SetRunTaskHook(func(ctx context.Context, task agent.Task) agent.Result {
		return agent.Result{TaskID: task.ID, Success: true, Stdout: "done"}
	})
	d := NewLocalDispatcher(exec)
	res, err := d.Dispatch(nil, "a1", agent.Task{ID: "t1"})
	require.NoError(t, err)
	assert.True(t, res.Success)
	assert.Equal(t, "done", res.Stdout)
}
