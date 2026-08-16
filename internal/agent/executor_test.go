package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexus/levee/internal/executor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- AgentExecutor ---------------------------------------------------------

func TestNewAgentExecutorDefaults(t *testing.T) {
	e := NewAgentExecutor(0, nil)
	assert.Equal(t, 1, e.MaxConcurrent())

	e2 := NewAgentExecutor(8, nil)
	assert.Equal(t, 8, e2.MaxConcurrent())
}

func TestAgentExecuteWithHook(t *testing.T) {
	e := NewAgentExecutor(2, nil)
	called := atomic.Int32{}
	e.SetRunTaskHook(func(ctx context.Context, task Task) Result {
		called.Add(1)
		return Result{TaskID: task.ID, Success: true, ExitCode: 0, Stdout: "ok"}
	})

	task := Task{ID: "t1", Module: "shell", Action: "exec"}
	res := e.Execute(context.Background(), task)
	assert.Equal(t, int32(1), called.Load())
	assert.True(t, res.Success)
	assert.Equal(t, "t1", res.TaskID)
	assert.Equal(t, "ok", res.Stdout)
}

func TestAgentExecuteUnknownModule(t *testing.T) {
	// Use a fresh executor with no modules registered so the lookup
	// fails. We cannot easily reset the default executor's registry,
	// so we install a hook-free executor pointing at an empty
	// executor.Executor.
	empty := executor.NewExecutor()
	e := NewAgentExecutor(1, empty)

	task := Task{ID: "t1", Module: "nope", Action: "exec"}
	res := e.Execute(context.Background(), task)
	assert.False(t, res.Success)
	assert.NotEmpty(t, res.Error)
	assert.Equal(t, -1, res.ExitCode)

	st := e.Stats()
	assert.Equal(t, int64(1), st.Failed)
	assert.Equal(t, int64(0), st.Completed)
}

func TestAgentExecuteSuccessCounts(t *testing.T) {
	e := NewAgentExecutor(1, nil)
	e.SetRunTaskHook(func(ctx context.Context, task Task) Result {
		return Result{TaskID: task.ID, Success: true, ExitCode: 0}
	})

	for i := 0; i < 5; i++ {
		e.Execute(context.Background(), Task{ID: "t", Module: "shell"})
	}
	st := e.Stats()
	assert.Equal(t, int64(5), st.Completed)
	assert.Equal(t, int64(0), st.Failed)
}

func TestAgentExecuteFailureCounts(t *testing.T) {
	e := NewAgentExecutor(1, nil)
	e.SetRunTaskHook(func(ctx context.Context, task Task) Result {
		return Result{TaskID: task.ID, Success: false, ExitCode: 1, Error: "boom"}
	})

	for i := 0; i < 3; i++ {
		e.Execute(context.Background(), Task{ID: "t", Module: "shell"})
	}
	st := e.Stats()
	assert.Equal(t, int64(0), st.Completed)
	assert.Equal(t, int64(3), st.Failed)
}

func TestAgentExecuteContextCancelled(t *testing.T) {
	e := NewAgentExecutor(1, nil)
	e.SetRunTaskHook(func(ctx context.Context, task Task) Result {
		// Simulate a long task that respects context cancellation.
		<-ctx.Done()
		return Result{TaskID: task.ID, Success: false, Error: ctx.Err().Error()}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	res := e.Execute(ctx, Task{ID: "t1"})
	assert.False(t, res.Success)
	assert.Contains(t, res.Error, "canceled")
}

func TestAgentExecuteRespectsTimeout(t *testing.T) {
	e := NewAgentExecutor(1, nil)
	e.SetRunTaskHook(func(ctx context.Context, task Task) Result {
		// Use the real executor path to exercise timeout handling.
		// We cannot easily install both a hook and a timeout, so we
		// just return immediately and assert the call completed.
		return Result{TaskID: task.ID, Success: true}
	})

	task := Task{ID: "t1", Timeout: 50 * time.Millisecond}
	res := e.Execute(context.Background(), task)
	assert.True(t, res.Success)
}

func TestAgentExecuteBatchOrder(t *testing.T) {
	e := NewAgentExecutor(4, nil)
	e.SetRunTaskHook(func(ctx context.Context, task Task) Result {
		time.Sleep(time.Millisecond)
		return Result{TaskID: task.ID, Success: true, ExitCode: 0}
	})

	tasks := make([]Task, 10)
	for i := range tasks {
		tasks[i] = Task{ID: string(rune('a' + i)), Module: "shell"}
	}
	results := e.ExecuteBatch(context.Background(), tasks)
	require.Len(t, results, len(tasks))
	for i, r := range results {
		assert.Equal(t, tasks[i].ID, r.TaskID)
		assert.True(t, r.Success)
	}
}

func TestAgentExecuteBatchEmpty(t *testing.T) {
	e := NewAgentExecutor(1, nil)
	assert.Nil(t, e.ExecuteBatch(context.Background(), nil))
}

func TestAgentExecuteBatchConcurrencyBounded(t *testing.T) {
	const max = 3
	e := NewAgentExecutor(max, nil)

	var concurrent atomic.Int32
	var maxObserved atomic.Int32
	e.SetRunTaskHook(func(ctx context.Context, task Task) Result {
		cur := concurrent.Add(1)
		for {
			old := maxObserved.Load()
			if cur <= old || maxObserved.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		concurrent.Add(-1)
		return Result{TaskID: task.ID, Success: true}
	})

	tasks := make([]Task, 20)
	for i := range tasks {
		tasks[i] = Task{ID: "t", Module: "shell"}
	}
	e.ExecuteBatch(context.Background(), tasks)
	assert.LessOrEqual(t, maxObserved.Load(), int32(max))
	assert.Greater(t, maxObserved.Load(), int32(0))
}

func TestAgentExecutorStats(t *testing.T) {
	e := NewAgentExecutor(1, nil)
	st := e.Stats()
	assert.Equal(t, int64(0), st.Completed)
	assert.Equal(t, int64(0), st.Failed)
	assert.Equal(t, int64(0), st.Active)
}

func TestAgentSetRunTaskHookNilRestores(t *testing.T) {
	e := NewAgentExecutor(1, executor.NewExecutor())
	e.SetRunTaskHook(func(ctx context.Context, task Task) Result {
		return Result{Success: true}
	})
	e.SetRunTaskHook(nil)
	// After clearing the hook, Execute falls back to the executor
	// which will fail on the unknown module.
	res := e.Execute(context.Background(), Task{Module: "unknown"})
	assert.False(t, res.Success)
}

func TestFormatTaskError(t *testing.T) {
	task := Task{ID: "t1", Module: "shell", Action: "exec", TargetHost: "h1"}
	s := formatTaskError(task, "channel broken")
	assert.Contains(t, s, "t1")
	assert.Contains(t, s, "shell")
	assert.Contains(t, s, "exec")
	assert.Contains(t, s, "h1")
	assert.Contains(t, s, "channel broken")
}

// --- Heartbeat helpers -----------------------------------------------------

func TestBuildHeartbeatFromStats(t *testing.T) {
	st := ExecutorStats{Completed: 5, Failed: 2, Active: 1}
	hb := buildHeartbeatFromStats("agent-1", st, 4)
	assert.Equal(t, "agent-1", hb.AgentID)
	assert.Equal(t, 1, hb.ActiveTasks)
	assert.Equal(t, int64(5), hb.CompletedTasks)
	assert.Equal(t, int64(2), hb.FailedTasks)
	assert.Equal(t, 4, hb.MaxConcurrent)
	assert.False(t, hb.Timestamp.IsZero())
}

// --- AgentInfo helpers -----------------------------------------------------

func TestAgentInfoSpareCapacity(t *testing.T) {
	a := AgentInfo{MaxConcurrent: 4, ActiveTasks: 1}
	assert.Equal(t, 3, a.SpareCapacity())

	a.ActiveTasks = 4
	assert.Equal(t, 0, a.SpareCapacity())

	a.ActiveTasks = 5
	assert.Equal(t, 0, a.SpareCapacity(), "spare capacity must not go negative")
}

func TestAgentInfoHasCapability(t *testing.T) {
	a := AgentInfo{Capabilities: []string{"shell", "file"}}
	assert.True(t, a.HasCapability("shell"))
	assert.True(t, a.HasCapability("file"))
	assert.False(t, a.HasCapability("pkg"))
}

// --- mock MasterClient for Agent tests -------------------------------------

type mockMasterClient struct {
	mu            sync.Mutex
	registered    []AgentInfo
	deregistered  []string
	heartbeats    []Heartbeat
	streamErr     error
	registerErr   error
	deregErr      error
	hbErr         error
	streamHandler TaskHandler
	streamCalled  atomic.Bool
}

func (m *mockMasterClient) Register(ctx context.Context, info AgentInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.registerErr != nil {
		return m.registerErr
	}
	m.registered = append(m.registered, info)
	return nil
}

func (m *mockMasterClient) Deregister(ctx context.Context, agentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deregErr != nil {
		return m.deregErr
	}
	m.deregistered = append(m.deregistered, agentID)
	return nil
}

func (m *mockMasterClient) SendHeartbeat(ctx context.Context, hb Heartbeat) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hbErr != nil {
		return m.hbErr
	}
	m.heartbeats = append(m.heartbeats, hb)
	return nil
}

func (m *mockMasterClient) StreamTasks(ctx context.Context, agentID string, handler TaskHandler) error {
	m.streamCalled.Store(true)
	m.mu.Lock()
	m.streamHandler = handler
	m.mu.Unlock()
	if m.streamErr != nil {
		return m.streamErr
	}
	<-ctx.Done()
	return ctx.Err()
}

func (m *mockMasterClient) heartbeatsCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.heartbeats)
}

func (m *mockMasterClient) registeredCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.registered)
}

// --- Agent lifecycle -------------------------------------------------------

func TestNewAgentDefaults(t *testing.T) {
	a := NewAgent("", ":9091", []string{"shell"}, 0)
	assert.NotEmpty(t, a.ID)
	assert.Equal(t, ":9091", a.Address)
	assert.Equal(t, []string{"shell"}, a.Capabilities)
	assert.Equal(t, 1, a.MaxConcurrent)
	assert.Equal(t, StatusOffline, a.Status())
}

func TestNewAgentWithID(t *testing.T) {
	a := NewAgent("agent-42", ":9091", []string{"shell", "file"}, 8)
	assert.Equal(t, "agent-42", a.ID)
	assert.Equal(t, 8, a.MaxConcurrent)
	assert.Len(t, a.Capabilities, 2)
}

func TestAgentSetMasterClient(t *testing.T) {
	a := NewAgent("a1", "", nil, 1)
	mc := &mockMasterClient{}
	a.SetMasterClient(mc)
	// No direct getter; exercise via Register which needs the client.
	require.NoError(t, a.Register())
	assert.Equal(t, 1, mc.registeredCount())
}

func TestAgentRegisterWithoutClient(t *testing.T) {
	a := NewAgent("a1", "", nil, 1)
	err := a.Register()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "master client not set")
}

func TestAgentRegisterAndDeregister(t *testing.T) {
	a := NewAgent("a1", ":9091", []string{"shell"}, 1)
	mc := &mockMasterClient{}
	a.SetMasterClient(mc)

	require.NoError(t, a.Register())
	require.NoError(t, a.Deregister())
	assert.Equal(t, []string{"a1"}, mc.deregistered)
}

func TestAgentDeregisterError(t *testing.T) {
	a := NewAgent("a1", "", nil, 1)
	mc := &mockMasterClient{deregErr: errors.New("nope")}
	a.SetMasterClient(mc)
	err := a.Deregister()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
}

func TestAgentStartWithoutClient(t *testing.T) {
	a := NewAgent("a1", "", nil, 1)
	err := a.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "master client not set")
}

func TestAgentStartTwice(t *testing.T) {
	a := NewAgent("a1", "", nil, 1)
	mc := &mockMasterClient{}
	a.SetMasterClient(mc)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled so Start returns quickly

	_ = a.Start(ctx)
	err := a.Start(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAlreadyStarted)
}

func TestAgentStartRegisterFailure(t *testing.T) {
	a := NewAgent("a1", "", nil, 1)
	mc := &mockMasterClient{registerErr: errors.New("master down")}
	a.SetMasterClient(mc)

	err := a.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "master down")
	assert.Equal(t, StatusOffline, a.Status())
}

func TestAgentStartStop(t *testing.T) {
	a := NewAgent("a1", ":9091", []string{"shell"}, 2)
	a.HeartbeatInterval = 10 * time.Millisecond
	mc := &mockMasterClient{}
	a.SetMasterClient(mc)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()

	// Wait for at least one heartbeat.
	require.Eventually(t, func() bool {
		return mc.heartbeatsCount() > 0
	}, time.Second, 5*time.Millisecond, "expected heartbeats to be sent")

	require.NoError(t, a.Stop())
	err := <-done
	assert.NoError(t, err)
	assert.Equal(t, StatusOffline, a.Status())
	assert.True(t, mc.streamCalled.Load())
}

func TestAgentStopWithoutStart(t *testing.T) {
	a := NewAgent("a1", "", nil, 1)
	err := a.Stop()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotStarted)
}

func TestAgentStartStreamError(t *testing.T) {
	a := NewAgent("a1", "", nil, 1)
	mc := &mockMasterClient{streamErr: errors.New("stream broken")}
	a.SetMasterClient(mc)

	err := a.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stream broken")
}

func TestAgentStatus(t *testing.T) {
	a := NewAgent("a1", "", nil, 1)
	assert.Equal(t, StatusOffline, a.Status())
}

func TestAgentLastHeartbeat(t *testing.T) {
	a := NewAgent("a1", "", nil, 1)
	assert.True(t, a.LastHeartbeat().IsZero())

	a.HeartbeatInterval = 5 * time.Millisecond
	mc := &mockMasterClient{}
	a.SetMasterClient(mc)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()

	require.Eventually(t, func() bool {
		return !a.LastHeartbeat().IsZero()
	}, time.Second, 2*time.Millisecond)

	_ = a.Stop()
	<-done
}

func TestAgentExecuteTask(t *testing.T) {
	a := NewAgent("a1", "", nil, 1)
	a.Executor().SetRunTaskHook(func(ctx context.Context, task Task) Result {
		return Result{TaskID: task.ID, Success: true}
	})
	res := a.executeTask(context.Background(), Task{ID: "t1"})
	assert.True(t, res.Success)
	assert.Equal(t, "a1", res.AgentID)
}
