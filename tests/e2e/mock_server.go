//go:build e2e

package e2e

// mock_server.go implements an in-process mock target cluster for E2E testing.
// Each MockTarget simulates a remote machine that accepts commands and returns
// scripted results. The MockCluster manages a collection of targets and
// provides failure injection, state tracking and audit recording.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/state"
)

// --- MockResult -------------------------------------------------------------

// MockResult is the simulated output of a single command execution on a mock
// target. It mirrors the exit-code / stdout / stderr contract of a real SSH
// channel execution.
type MockResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// --- MockTarget -------------------------------------------------------------

// MockTarget simulates a target machine for E2E testing. It holds a map of
// scripted command results and supports forced failure injection via FailNext.
type MockTarget struct {
	ID       string                `json:"id"`
	Host     string                `json:"host"`
	Commands map[string]MockResult `json:"commands"`
	FailNext bool                  `json:"fail_next"` // inject failure on next Execute

	mu    sync.Mutex
	calls []ExecCall        // recorded command invocations
	state map[string]string // simulated key-value state (for snapshot tests)
}

// ExecCall records a single command invocation on a mock target.
type ExecCall struct {
	Command   string     `json:"command"`
	Timestamp time.Time  `json:"timestamp"`
	Result    MockResult `json:"result"`
}

// SetState sets a key-value pair in the mock target's simulated state.
func (t *MockTarget) SetState(key, value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == nil {
		t.state = make(map[string]string)
	}
	t.state[key] = value
}

// GetState retrieves a value from the mock target's simulated state.
func (t *MockTarget) GetState(key string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == nil {
		return "", false
	}
	v, ok := t.state[key]
	return v, ok
}

// Calls returns a copy of the recorded command invocations.
func (t *MockTarget) Calls() []ExecCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ExecCall, len(t.calls))
	copy(out, t.calls)
	return out
}

// CallCount returns the number of commands executed on this target.
func (t *MockTarget) CallCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.calls)
}

// --- MockCluster ------------------------------------------------------------

// MockCluster manages a cluster of mock targets. It provides command execution,
// failure injection, state persistence and audit tracing.
type MockCluster struct {
	Targets  []*MockTarget        `json:"targets"`
	Store    state.Store          `json:"-"`
	Recorder *audit.TraceRecorder `json:"-"`

	mu sync.Mutex
}

// NewMockCluster creates a cluster of n mock targets with sequential IDs.
// Each target gets a host name of the form "mock-<i>" where i is 0-based.
// The store and recorder are used for state persistence and audit tracing;
// they may be nil for tests that do not need those features.
func NewMockCluster(n int, store state.Store) *MockCluster {
	targets := make([]*MockTarget, n)
	for i := 0; i < n; i++ {
		targets[i] = &MockTarget{
			ID:       fmt.Sprintf("mock-%03d", i),
			Host:     fmt.Sprintf("mock-%03d", i),
			Commands: make(map[string]MockResult),
			state:    make(map[string]string),
		}
	}
	c := &MockCluster{
		Targets: targets,
		Store:   store,
	}
	if store != nil {
		recorder, err := audit.NewTraceRecorder(store)
		if err == nil {
			c.Recorder = recorder
		}
	}
	return c
}

// Execute runs a command on the specified target and returns the scripted
// result. If the target has FailNext set, the command fails with exit code 1
// and FailNext is cleared. If the command is not in the target's Commands map,
// a default success result (exit 0, empty output) is returned.
func (c *MockCluster) Execute(ctx context.Context, targetID, command string) (MockResult, error) {
	if err := ctx.Err(); err != nil {
		return MockResult{}, fmt.Errorf("mock cluster: context cancelled: %w", err)
	}

	t := c.Target(targetID)
	if t == nil {
		return MockResult{ExitCode: 1, Stderr: fmt.Sprintf("target %s not found", targetID)},
			fmt.Errorf("mock cluster: target %s not found", targetID)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Check failure injection.
	if t.FailNext {
		t.FailNext = false
		result := MockResult{
			ExitCode: 1,
			Stderr:   "injected failure",
		}
		t.calls = append(t.calls, ExecCall{
			Command:   command,
			Timestamp: time.Now().UTC(),
			Result:    result,
		})
		return result, fmt.Errorf("injected failure on %s", targetID)
	}

	// Look up scripted result; default to success.
	result, ok := t.Commands[command]
	if !ok {
		result = MockResult{ExitCode: 0, Stdout: "ok"}
	}

	t.calls = append(t.calls, ExecCall{
		Command:   command,
		Timestamp: time.Now().UTC(),
		Result:    result,
	})
	return result, nil
}

// InjectFailure marks the specified target to fail on its next Execute call.
func (c *MockCluster) InjectFailure(targetID string) {
	t := c.Target(targetID)
	if t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.FailNext = true
	}
}

// Target returns the mock target with the given ID, or nil if not found.
func (c *MockCluster) Target(targetID string) *MockTarget {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.Targets {
		if t.ID == targetID {
			return t
		}
	}
	return nil
}

// TargetByHost returns the mock target with the given host name, or nil.
func (c *MockCluster) TargetByHost(host string) *MockTarget {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.Targets {
		if t.Host == host {
			return t
		}
	}
	return nil
}

// Hosts returns the host names of all targets in the cluster.
func (c *MockCluster) Hosts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	hosts := make([]string, len(c.Targets))
	for i, t := range c.Targets {
		hosts[i] = t.Host
	}
	return hosts
}

// Reset clears all call logs and failure injection flags on every target.
func (c *MockCluster) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.Targets {
		t.mu.Lock()
		t.calls = nil
		t.FailNext = false
		t.mu.Unlock()
	}
}

// --- MakeExecuteFunc --------------------------------------------------------

// MakeExecuteFunc returns a rollback.ExecuteFunc that dispatches commands
// through the mock cluster. The returned function maps dsl.Step fields to
// mock cluster commands: module.action is used as the command key, and the
// target string is used as the target ID lookup.
func (c *MockCluster) MakeExecuteFunc() func(ctx context.Context, target string, step interface {
	GetModule() string
	GetAction() string
}) error {
	return func(ctx context.Context, target string, step interface {
		GetModule() string
		GetAction() string
	}) error {
		command := step.GetModule() + "." + step.GetAction()
		result, err := c.Execute(ctx, target, command)
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("mock execution failed: %s: %s", command, result.Stderr)
		}
		return nil
	}
}

// --- DSLStepAdapter ---------------------------------------------------------

// DSLStepAdapter wraps a dsl.Step to implement the interface expected by
// MakeExecuteFunc. This avoids importing dsl directly in the mock server,
// keeping the mock transport-agnostic.
type DSLStepAdapter struct {
	Module string
	Action string
}

// GetModule returns the module name.
func (s DSLStepAdapter) GetModule() string { return s.Module }

// GetAction returns the action name.
func (s DSLStepAdapter) GetAction() string { return s.Action }
