package tests

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// This file defines mock implementations of the Channel and Executor
// interfaces that LEVEE's internal packages will consume. Because the
// real interfaces are not yet defined (T010/T016 land in W2/W3), we
// declare minimal local interfaces here that mirror the design-doc
// signatures. When the real interfaces ship, these mocks can either
// satisfy them directly or be adapted with a thin wrapper.
//
// The mocks are concurrency-safe: every method is guarded by a mutex so
// tests can run parallel sub-tests against the same mock instance.

// ---------------------------------------------------------------------------
// Channel interface + mock
// ---------------------------------------------------------------------------

// ExecResult is the structured output of a single command execution.
// It mirrors the design-doc "stdout + stderr + exit_code" contract.
type ExecResult struct {
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration"`
}

// Channel is the minimal channel abstraction used by mocks. The real
// internal/channel.Channel interface (T010) will be a superset of this.
type Channel interface {
	Connect(ctx context.Context, target TargetSpec) error
	Exec(ctx context.Context, cmd string) (ExecResult, error)
	Close(ctx context.Context) error
}

// MockChannel is a test double for Channel. It records every call and
// lets the test script responses via the OnExec map. Behaviour is
// deterministic: if a command has no scripted response, the mock
// returns a default zero-exit empty result.
type MockChannel struct {
	mu sync.Mutex

	// OnExec maps command string -> result to return. If a command is
	// not present, DefaultResult is returned. If OnExec is nil every
	// command succeeds with an empty result.
	OnExec map[string]ExecResult

	// DefaultResult is returned for commands not in OnExec.
	DefaultResult ExecResult

	// ErrOnConnect, if non-nil, is returned by Connect.
	ErrOnConnect error
	// ErrOnExec, if non-nil, is returned by Exec (overrides OnExec).
	ErrOnExec error
	// ErrOnClose, if non-nil, is returned by Close.
	ErrOnClose error

	// Call log — inspectors for assertions.
	ConnectCalls []TargetSpec
	ExecCalls    []string
	CloseCalls   int

	// Connected tracks whether Connect has been called more recently
	// than Close. Exec returns an error if called while disconnected
	// unless AllowDisconnectedExec is true.
	Connected             bool
	AllowDisconnectedExec bool
}

// NewMockChannel returns a ready-to-use MockChannel with empty call logs.
func NewMockChannel() *MockChannel {
	return &MockChannel{
		OnExec:        make(map[string]ExecResult),
		Connected:     false,
		DefaultResult: ExecResult{ExitCode: 0},
	}
}

// Connect records the call and flips Connected to true.
func (m *MockChannel) Connect(_ context.Context, target TargetSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ConnectCalls = append(m.ConnectCalls, target)
	if m.ErrOnConnect != nil {
		return m.ErrOnConnect
	}
	m.Connected = true
	return nil
}

// Exec records the command and returns the scripted or default result.
func (m *MockChannel) Exec(_ context.Context, cmd string) (ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ExecCalls = append(m.ExecCalls, cmd)

	if m.ErrOnExec != nil {
		return ExecResult{}, m.ErrOnExec
	}
	if !m.Connected && !m.AllowDisconnectedExec {
		return ExecResult{}, fmt.Errorf("mock channel: not connected")
	}

	if r, ok := m.OnExec[cmd]; ok {
		return r, nil
	}
	return m.DefaultResult, nil
}

// Close records the call and flips Connected to false.
func (m *MockChannel) Close(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CloseCalls++
	m.Connected = false
	if m.ErrOnClose != nil {
		return m.ErrOnClose
	}
	return nil
}

// Script installs a canned response for a command. Convenience wrapper
// around OnExec that returns the mock for chaining.
func (m *MockChannel) Script(cmd string, r ExecResult) *MockChannel {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.OnExec == nil {
		m.OnExec = make(map[string]ExecResult)
	}
	m.OnExec[cmd] = r
	return m
}

// CallCount returns the total number of method invocations. Useful as a
// quick "did anything happen?" assertion.
func (m *MockChannel) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ConnectCalls) + len(m.ExecCalls) + m.CloseCalls
}

// ---------------------------------------------------------------------------
// Executor interface + mock
// ---------------------------------------------------------------------------

// ModuleResult is the structured output of a module execution. It
// mirrors the design-doc "input / output / elapsed / exit_code" contract
// for the L2 executor layer.
type ModuleResult struct {
	Output   map[string]interface{} `json:"output"`
	ExitCode int                    `json:"exit_code"`
	Duration time.Duration          `json:"duration"`
	Err      error                  `json:"-"`
}

// Executor is the minimal executor abstraction used by mocks. The real
// internal/executor.Executor interface (T016) will be a superset.
type Executor interface {
	Execute(ctx context.Context, module string, params map[string]interface{}) (ModuleResult, error)
}

// MockExecutor is a test double for Executor. It dispatches by module
// name to a scripted handler, or falls back to DefaultHandler.
type MockExecutor struct {
	mu sync.Mutex

	// Handlers maps module name -> handler func. If a module has no
	// entry, DefaultHandler is invoked.
	Handlers       map[string]func(params map[string]interface{}) (ModuleResult, error)
	DefaultHandler func(module string, params map[string]interface{}) (ModuleResult, error)

	// Calls records every (module, params) pair passed to Execute.
	Calls []struct {
		Module string
		Params map[string]interface{}
	}

	// ErrIfSet, if non-nil, is returned for every call (overrides handlers).
	ErrIfSet error
}

// NewMockExecutor returns a ready-to-use MockExecutor with a permissive
// default handler that returns exit 0 and empty output.
func NewMockExecutor() *MockExecutor {
	return &MockExecutor{
		Handlers: make(map[string]func(map[string]interface{}) (ModuleResult, error)),
		DefaultHandler: func(_ string, _ map[string]interface{}) (ModuleResult, error) {
			return ModuleResult{ExitCode: 0, Output: map[string]interface{}{}}, nil
		},
	}
}

// Execute records the call and dispatches to the matching handler.
func (m *MockExecutor) Execute(_ context.Context, module string, params map[string]interface{}) (ModuleResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Calls = append(m.Calls, struct {
		Module string
		Params map[string]interface{}
	}{Module: module, Params: params})

	if m.ErrIfSet != nil {
		return ModuleResult{}, m.ErrIfSet
	}

	if h, ok := m.Handlers[module]; ok {
		return h(params)
	}
	if m.DefaultHandler != nil {
		return m.DefaultHandler(module, params)
	}
	return ModuleResult{ExitCode: 0}, nil
}

// Register installs a handler for a module name, returning the mock for
// chaining.
func (m *MockExecutor) Register(module string, h func(params map[string]interface{}) (ModuleResult, error)) *MockExecutor {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Handlers == nil {
		m.Handlers = make(map[string]func(map[string]interface{}) (ModuleResult, error))
	}
	m.Handlers[module] = h
	return m
}

// RegisterResult is a convenience that registers a handler returning a
// fixed result, ignoring params.
func (m *MockExecutor) RegisterResult(module string, r ModuleResult) *MockExecutor {
	return m.Register(module, func(_ map[string]interface{}) (ModuleResult, error) {
		return r, nil
	})
}

// RegisterError is a convenience that registers a handler returning a
// fixed error.
func (m *MockExecutor) RegisterError(module string, err error) *MockExecutor {
	return m.Register(module, func(_ map[string]interface{}) (ModuleResult, error) {
		return ModuleResult{}, err
	})
}

// CallCount returns the total number of Execute invocations.
func (m *MockExecutor) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls)
}

// ModulesCalled returns the ordered list of module names invoked.
func (m *MockExecutor) ModulesCalled() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.Calls))
	for i, c := range m.Calls {
		out[i] = c.Module
	}
	return out
}
