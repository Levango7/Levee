package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- InProcessMasterClient -------------------------------------------------

func TestNewInProcessMasterClient(t *testing.T) {
	c := NewInProcessMasterClient(nil)
	assert.NotNil(t, c.Registry())
	assert.Equal(t, 0, c.Registry().Count())
}

func TestInProcessRegisterAndDeregister(t *testing.T) {
	c := NewInProcessMasterClient(nil)
	require.NoError(t, c.Register(context.Background(), AgentInfo{ID: "a1"}))
	assert.Equal(t, 1, c.Registry().Count())

	require.NoError(t, c.Deregister(context.Background(), "a1"))
	assert.Equal(t, 0, c.Registry().Count())
}

func TestInProcessRegisterIdempotent(t *testing.T) {
	c := NewInProcessMasterClient(nil)
	info := AgentInfo{ID: "a1", MaxConcurrent: 2}
	require.NoError(t, c.Register(context.Background(), info))
	// Re-register should update rather than error.
	info.MaxConcurrent = 4
	require.NoError(t, c.Register(context.Background(), info))
	got, _ := c.Registry().Get("a1")
	assert.Equal(t, 4, got.MaxConcurrent)
}

func TestInProcessSendHeartbeat(t *testing.T) {
	c := NewInProcessMasterClient(nil)
	require.NoError(t, c.Register(context.Background(), AgentInfo{ID: "a1", MaxConcurrent: 2}))

	hb := Heartbeat{AgentID: "a1", ActiveTasks: 1, MaxConcurrent: 2, Timestamp: time.Now()}
	require.NoError(t, c.SendHeartbeat(context.Background(), hb))

	got, _ := c.Registry().Get("a1")
	assert.Equal(t, 1, got.ActiveTasks)
}

func TestInProcessSendHeartbeatAutoRegisters(t *testing.T) {
	c := NewInProcessMasterClient(nil)
	hb := Heartbeat{AgentID: "ghost", MaxConcurrent: 2, Timestamp: time.Now()}
	require.NoError(t, c.SendHeartbeat(context.Background(), hb))
	_, err := c.Registry().Get("ghost")
	require.NoError(t, err)
}

func TestInProcessStreamTasksNoSource(t *testing.T) {
	c := NewInProcessMasterClient(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := c.StreamTasks(ctx, "a1", func(ctx context.Context, task Task) Result {
		return Result{}
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// staticTaskSource is a TaskSource that returns a fixed list of tasks
// per agent, then signals exhaustion.
type staticTaskSource struct {
	mu     sync.Mutex
	tasks  map[string][]Task
	called atomic.Int32
}

func newStaticTaskSource() *staticTaskSource {
	return &staticTaskSource{tasks: make(map[string][]Task)}
}

func (s *staticTaskSource) Add(agentID string, tasks ...Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[agentID] = append(s.tasks[agentID], tasks...)
}

func (s *staticTaskSource) NextTask(ctx context.Context, agentID string) (Task, bool) {
	s.called.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tasks[agentID]) == 0 {
		return Task{}, false
	}
	t := s.tasks[agentID][0]
	s.tasks[agentID] = s.tasks[agentID][1:]
	return t, true
}

func TestInProcessStreamTasksWithSource(t *testing.T) {
	c := NewInProcessMasterClient(nil)
	src := newStaticTaskSource()
	src.Add("a1",
		Task{ID: "t1", Module: "shell"},
		Task{ID: "t2", Module: "shell"},
	)
	c.SetTaskSource(src)

	var results []Result
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)

	done := make(chan error, 1)
	go func() {
		done <- c.StreamTasks(ctx, "a1", func(ctx context.Context, task Task) Result {
			mu.Lock()
			results = append(results, Result{TaskID: task.ID, Success: true})
			mu.Unlock()
			return Result{TaskID: task.ID, Success: true}
		})
	}()

	// Wait for both tasks to be processed.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 2
	}, time.Second, 5*time.Millisecond)

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, results, 2)
	assert.Equal(t, "t1", results[0].TaskID)
	assert.Equal(t, "t2", results[1].TaskID)
}

func TestInProcessRegistryShared(t *testing.T) {
	reg := NewAgentRegistry()
	c := NewInProcessMasterClient(reg)
	assert.Same(t, reg, c.Registry())

	require.NoError(t, c.Register(context.Background(), AgentInfo{ID: "a1"}))
	assert.Equal(t, 1, reg.Count())
}