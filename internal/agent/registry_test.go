package agent

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- AgentRegistry basics --------------------------------------------------

func TestNewAgentRegistry(t *testing.T) {
	r := NewAgentRegistry()
	assert.Equal(t, 0, r.Count())
	assert.Empty(t, r.List())
}

func TestRegistryRegister(t *testing.T) {
	r := NewAgentRegistry()
	info := AgentInfo{
		ID:            "a1",
		Address:       ":9091",
		Capabilities:  []string{"shell"},
		MaxConcurrent: 2,
	}
	require.NoError(t, r.Register(info))
	assert.Equal(t, 1, r.Count())

	got, err := r.Get("a1")
	require.NoError(t, err)
	assert.Equal(t, "a1", got.ID)
	assert.Equal(t, []string{"shell"}, got.Capabilities)
	assert.Equal(t, StatusRegistered, got.Status)
	assert.False(t, got.LastHeartbeat.IsZero())
}

func TestRegistryRegisterEmptyID(t *testing.T) {
	r := NewAgentRegistry()
	err := r.Register(AgentInfo{ID: ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty agent id")
}

func TestRegistryRegisterDuplicate(t *testing.T) {
	r := NewAgentRegistry()
	info := AgentInfo{ID: "a1", Capabilities: []string{"shell"}}
	require.NoError(t, r.Register(info))
	err := r.Register(info)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAgentAlreadyRegistered)
}

func TestRegistryRegisterDefensiveCopy(t *testing.T) {
	r := NewAgentRegistry()
	caps := []string{"shell"}
	require.NoError(t, r.Register(AgentInfo{ID: "a1", Capabilities: caps}))
	// Mutate the caller's slice; the registry must be unaffected.
	caps[0] = "mutated"
	got, err := r.Get("a1")
	require.NoError(t, err)
	assert.Equal(t, []string{"shell"}, got.Capabilities)
}

func TestRegistryDeregister(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1"}))
	require.NoError(t, r.Deregister("a1"))
	assert.Equal(t, 0, r.Count())
}

func TestRegistryDeregisterMissing(t *testing.T) {
	r := NewAgentRegistry()
	err := r.Deregister("nope")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAgentNotFound)
}

func TestRegistryGet(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1", MaxConcurrent: 4}))
	got, err := r.Get("a1")
	require.NoError(t, err)
	assert.Equal(t, 4, got.MaxConcurrent)

	_, err = r.Get("missing")
	require.Error(t, err)
}

func TestRegistryListSorted(t *testing.T) {
	r := NewAgentRegistry()
	for _, id := range []string{"c", "a", "b"} {
		require.NoError(t, r.Register(AgentInfo{ID: id}))
	}
	list := r.List()
	require.Len(t, list, 3)
	assert.Equal(t, "a", list[0].ID)
	assert.Equal(t, "b", list[1].ID)
	assert.Equal(t, "c", list[2].ID)
}

// --- Heartbeat / status ----------------------------------------------------

func TestRegistryHeartbeat(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1", MaxConcurrent: 2}))

	hb := Heartbeat{
		AgentID:       "a1",
		Timestamp:     time.Now(),
		ActiveTasks:   1,
		CompletedTasks: 5,
		FailedTasks:   2,
		MaxConcurrent: 2,
	}
	require.NoError(t, r.Heartbeat("a1", hb))

	got, err := r.Get("a1")
	require.NoError(t, err)
	assert.Equal(t, 1, got.ActiveTasks)
	assert.Equal(t, int64(5), got.CompletedTasks)
	assert.Equal(t, int64(2), got.FailedTasks)
	assert.Equal(t, StatusIdle, got.Status)
}

func TestRegistryHeartbeatBusy(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1", MaxConcurrent: 2}))

	hb := Heartbeat{AgentID: "a1", ActiveTasks: 2, MaxConcurrent: 2}
	require.NoError(t, r.Heartbeat("a1", hb))

	got, _ := r.Get("a1")
	assert.Equal(t, StatusBusy, got.Status)
}

func TestRegistryHeartbeatMissing(t *testing.T) {
	r := NewAgentRegistry()
	err := r.Heartbeat("nope", Heartbeat{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAgentNotFound)
}

func TestRegistryHeartbeatUpdatesMaxConcurrent(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1", MaxConcurrent: 2}))
	hb := Heartbeat{AgentID: "a1", MaxConcurrent: 8}
	require.NoError(t, r.Heartbeat("a1", hb))
	got, _ := r.Get("a1")
	assert.Equal(t, 8, got.MaxConcurrent)
}

func TestComputeStatus(t *testing.T) {
	tests := []struct {
		active, max int
		want        AgentStatus
	}{
		{0, 2, StatusIdle},
		{1, 2, StatusIdle},
		{2, 2, StatusBusy},
		{3, 2, StatusBusy},
		{0, 0, StatusIdle},
	}
	for _, tc := range tests {
		got := computeStatus(&AgentInfo{ActiveTasks: tc.active, MaxConcurrent: tc.max})
		assert.Equal(t, tc.want, got, "active=%d max=%d", tc.active, tc.max)
	}
}

// --- Capability / least-loaded queries -------------------------------------

func TestRegistryFindByCapability(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1", Capabilities: []string{"shell"}}))
	require.NoError(t, r.Register(AgentInfo{ID: "a2", Capabilities: []string{"file"}}))
	require.NoError(t, r.Register(AgentInfo{ID: "a3", Capabilities: []string{"shell", "file"}}))

	shellAgents := r.FindByCapability("shell")
	require.Len(t, shellAgents, 2)
	assert.Equal(t, "a1", shellAgents[0].ID)
	assert.Equal(t, "a3", shellAgents[1].ID)

	fileAgents := r.FindByCapability("file")
	require.Len(t, fileAgents, 2)

	none := r.FindByCapability("pkg")
	assert.Empty(t, none)
}

func TestRegistryFindByCapabilityEmpty(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1"}))
	require.NoError(t, r.Register(AgentInfo{ID: "a2"}))
	all := r.FindByCapability("")
	assert.Len(t, all, 2)
}

func TestRegistryFindByCapabilityExcludesOffline(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1", Capabilities: []string{"shell"}}))
	require.NoError(t, r.Register(AgentInfo{ID: "a2", Capabilities: []string{"shell"}}))
	require.NoError(t, r.MarkOffline("a2"))

	agents := r.FindByCapability("shell")
	require.Len(t, agents, 1)
	assert.Equal(t, "a1", agents[0].ID)
}

func TestRegistryFindLeastLoaded(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1", Capabilities: []string{"shell"}, MaxConcurrent: 2}))
	require.NoError(t, r.Register(AgentInfo{ID: "a2", Capabilities: []string{"shell"}, MaxConcurrent: 2}))

	// a1 has 1 active task, a2 has 0 -> a2 should be picked.
	require.NoError(t, r.Heartbeat("a1", Heartbeat{AgentID: "a1", ActiveTasks: 1, MaxConcurrent: 2}))

	got, err := r.FindLeastLoaded("shell")
	require.NoError(t, err)
	assert.Equal(t, "a2", got.ID)
}

func TestRegistryFindLeastLoadedTieBreakByID(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "b", Capabilities: []string{"shell"}, MaxConcurrent: 2}))
	require.NoError(t, r.Register(AgentInfo{ID: "a", Capabilities: []string{"shell"}, MaxConcurrent: 2}))

	got, err := r.FindLeastLoaded("shell")
	require.NoError(t, err)
	assert.Equal(t, "a", got.ID)
}

func TestRegistryFindLeastLoadedAllBusy(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1", Capabilities: []string{"shell"}, MaxConcurrent: 1}))
	require.NoError(t, r.Heartbeat("a1", Heartbeat{AgentID: "a1", ActiveTasks: 1, MaxConcurrent: 1}))

	got, err := r.FindLeastLoaded("shell")
	require.NoError(t, err)
	assert.Equal(t, "a1", got.ID, "busy agent still returned when no alternative")
}

func TestRegistryFindLeastLoadedNoMatch(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1", Capabilities: []string{"file"}}))

	_, err := r.FindLeastLoaded("shell")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAgentNotFound)
}

func TestRegistryFindLeastLoadedPrefersSpare(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "busy", Capabilities: []string{"shell"}, MaxConcurrent: 1}))
	require.NoError(t, r.Register(AgentInfo{ID: "free", Capabilities: []string{"shell"}, MaxConcurrent: 1}))
	require.NoError(t, r.Heartbeat("busy", Heartbeat{AgentID: "busy", ActiveTasks: 1, MaxConcurrent: 1}))

	got, err := r.FindLeastLoaded("shell")
	require.NoError(t, err)
	assert.Equal(t, "free", got.ID)
}

// --- MarkOffline / CleanupStale --------------------------------------------

func TestRegistryMarkOffline(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1"}))
	require.NoError(t, r.MarkOffline("a1"))
	got, _ := r.Get("a1")
	assert.Equal(t, StatusOffline, got.Status)
}

func TestRegistryMarkOfflineMissing(t *testing.T) {
	r := NewAgentRegistry()
	err := r.MarkOffline("nope")
	require.Error(t, err)
}

func TestRegistryCleanupStale(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "fresh"}))
	require.NoError(t, r.Register(AgentInfo{ID: "stale"}))

	// Backdate the stale agent's last heartbeat.
	r.mu.Lock()
	r.agents["stale"].LastHeartbeat = time.Now().Add(-2 * time.Minute)
	r.mu.Unlock()

	stale := r.CleanupStale(time.Minute)
	assert.Equal(t, []string{"stale"}, stale)

	got, _ := r.Get("stale")
	assert.Equal(t, StatusOffline, got.Status)

	gotFresh, _ := r.Get("fresh")
	assert.NotEqual(t, StatusOffline, gotFresh.Status)
}

func TestRegistryCleanupStaleSkipsAlreadyOffline(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1"}))
	require.NoError(t, r.MarkOffline("a1"))

	r.mu.Lock()
	r.agents["a1"].LastHeartbeat = time.Now().Add(-2 * time.Minute)
	r.mu.Unlock()

	stale := r.CleanupStale(time.Minute)
	assert.Empty(t, stale, "already-offline agent not reported again")
}

func TestRegistryCleanupStaleNone(t *testing.T) {
	r := NewAgentRegistry()
	require.NoError(t, r.Register(AgentInfo{ID: "a1"}))
	stale := r.CleanupStale(time.Hour)
	assert.Empty(t, stale)
}

// --- Concurrency -----------------------------------------------------------

func TestRegistryConcurrentAccess(t *testing.T) {
	r := NewAgentRegistry()
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = r.Register(AgentInfo{ID: fmt.Sprintf("a-%d", i)})
		}(i)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.List()
		}()
	}
	wg.Wait()
	assert.Equal(t, n, r.Count())
}

func TestRegistryConcurrentHeartbeats(t *testing.T) {
	r := NewAgentRegistry()
	const n = 100
	for i := 0; i < n; i++ {
		require.NoError(t, r.Register(AgentInfo{ID: fmt.Sprintf("a-%d", i), MaxConcurrent: 2}))
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = r.Heartbeat(fmt.Sprintf("a-%d", i), Heartbeat{
				AgentID: fmt.Sprintf("a-%d", i),
				ActiveTasks: 1,
				MaxConcurrent: 2,
			})
		}(i)
	}
	wg.Wait()
	// All agents should be idle (1 < 2).
	for i := 0; i < n; i++ {
		got, _ := r.Get(fmt.Sprintf("a-%d", i))
		assert.Equal(t, StatusIdle, got.Status)
	}
}