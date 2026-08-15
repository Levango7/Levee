// Package executor implements LEVEE's module execution framework (design doc
// section 4.3, MVP task T016). It defines two abstractions:
//
//   - Module: a self-registering unit of work (shell, file, pkg, svc, user,
//     ...). Each Module declares the actions it supports (e.g. "exec",
//     "copy", "install") and whether it is idempotent.
//   - Executor: a registry + dispatcher that looks up a Module by name and
//     invokes one of its actions with a structured ModuleInput.
//
// The framework deliberately stays transport-agnostic: a Module receives a
// channel.Channel through ModuleInput and drives the target through it. This
// keeps the executor free of any SSH / WinRM / API specifics and lets the same
// Module run unchanged across transports.
//
// All output is structured (ModuleOutput) so that the batch controller, audit
// log and trace chain can record exit_code / stdout / stderr / duration /
// changed uniformly regardless of which Module produced them.
package executor

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nexus/levee/internal/channel"
)

// --- Module ----------------------------------------------------------------

// Module is the unit of work that the executor dispatches. Each concrete
// module (shell, file, pkg, svc, user, ...) implements this interface and
// registers itself with the default Executor via RegisterModule in its
// init().
//
// A Module must be safe for concurrent use: the executor may invoke Execute
// on the same Module instance from multiple goroutines simultaneously (one
// per target host). Implementations should keep per-invocation state inside
// Execute itself rather than on the Module receiver.
type Module interface {
	// Name returns the module's unique identifier, e.g. "shell", "file",
	// "pkg". The executor uses Name() as the registry key.
	Name() string

	// Actions returns the list of action verbs this module supports, e.g.
	// ["exec", "script"] for the shell module or ["copy", "template"] for
	// the file module. The executor rejects unknown actions with a typed
	// error before invoking Execute.
	Actions() []string

	// Execute runs a single action on the target described by input. The
	// implementation must respect ctx for cancellation and timeouts. A
	// non-nil error indicates that the module failed to run; a nil error
	// with ModuleOutput.ExitCode != 0 indicates that the command ran but
	// the remote process exited non-zero. Both pieces of information are
	// preserved in the audit trail.
	Execute(ctx context.Context, action string, input ModuleInput) (*ModuleOutput, error)

	// Idempotent reports whether the module declares itself idempotent.
	// Idempotent modules may be re-run safely without changing target state
	// when the desired state is already present; the batch controller uses
	// this hint to skip redundant work and to decide whether a partial
	// failure warrants rollback.
	Idempotent() bool
}

// --- Input / Output --------------------------------------------------------

// ModuleInput is the structured payload passed to Module.Execute. It carries
// everything a module needs to act on a target: the action arguments, the
// target descriptor and a live channel connected to it.
type ModuleInput struct {
	// Action is the verb being executed (one of Module.Actions()). It is
	// also passed as the second argument to Execute so that implementations
	// can switch on it without having to read it back from input.
	Action string `json:"action"`

	// Args is the module-specific argument map. The keys and value types
	// are defined by each module's documentation; the executor does not
	// interpret them. Typical examples:
	//   shell.exec:   {"cmd": "systemctl restart nginx"}
	//   file.copy:    {"src": "/local/path", "dest": "/remote/path", "mode": "0644"}
	//   pkg.install:  {"name": "nginx", "version": "1.24.0"}
	Args map[string]any `json:"args"`

	// Target is the remote host this invocation applies to. Modules should
	// not call Target.Type() to switch behaviour — that is the channel's
	// job — but they may use Host() / Port() for logging.
	Target channel.Target `json:"-"`

	// Channel is the live, connected channel to Target. The executor
	// guarantees that Channel.IsConnected() is true when Execute is called.
	Channel channel.Channel `json:"-"`
}

// ModuleOutput is the structured result of a single Module.Execute call. It
// mirrors channel.ExecResult but adds the Changed flag that drives LEVEE's
// idempotency and audit semantics.
type ModuleOutput struct {
	// ExitCode is the remote process exit code. 0 means success; non-zero
	// means the command ran but failed. A non-nil error from Execute means
	// the module could not run at all (e.g. channel broken).
	ExitCode int `json:"exit_code"`

	// Stdout is the captured standard output of the remote process.
	Stdout string `json:"stdout"`

	// Stderr is the captured standard error of the remote process.
	Stderr string `json:"stderr"`

	// Duration is the wall-clock time spent inside Execute, measured by
	// the executor wrapper. It includes any channel round-trip but not
	// registry lookup overhead.
	Duration time.Duration `json:"duration_ms"`

	// Changed reports whether the invocation modified target state. For
	// idempotent modules Changed should be false when the desired state
	// was already present. Non-idempotent modules (e.g. shell.exec) should
	// set Changed to true unconditionally, since they cannot know.
	Changed bool `json:"changed"`
}

// --- Executor --------------------------------------------------------------

// Executor is the module registry and dispatcher. The zero value is not
// usable; callers must use NewExecutor.
type Executor struct {
	mu      sync.RWMutex
	modules map[string]Module
}

// NewExecutor returns an empty Executor ready to register modules.
func NewExecutor() *Executor {
	return &Executor{modules: make(map[string]Module)}
}

// RegisterModule adds m to the registry under m.Name(). Registering a second
// module with the same name overwrites the previous registration; this is
// intentional so that tests can swap modules, but production code should
// register each name exactly once. RegisterModule is safe for concurrent use.
func (e *Executor) RegisterModule(m Module) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.modules[m.Name()] = m
}

// UnregisterModule removes the module registered under name, if any. It is
// primarily useful in tests.
func (e *Executor) UnregisterModule(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.modules, name)
}

// Module returns the module registered under name, or false when no module is
// registered. It is the read-side counterpart to RegisterModule.
func (e *Executor) Module(name string) (Module, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	m, ok := e.modules[name]
	return m, ok
}

// Modules returns the registered module names in sorted order. It is intended
// for diagnostics (e.g. `levee version --verbose` listing available modules).
func (e *Executor) Modules() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.modules))
	for k := range e.modules {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Execute looks up the module named moduleName, validates that it supports
// action, and invokes it with input. It wraps the call to measure Duration
// and to enforce that a nil ModuleOutput is never returned alongside a nil
// error (callers can rely on output != nil when err == nil).
//
// Error categories:
//   - unknown module:  returns a typed error mentioning the module name.
//   - unknown action:  returns a typed error mentioning module + action.
//   - module failure:  returns the error verbatim from Module.Execute.
//   - success:         returns (output, nil) with output != nil.
func (e *Executor) Execute(ctx context.Context, moduleName, action string, input ModuleInput) (*ModuleOutput, error) {
	m, ok := e.Module(moduleName)
	if !ok {
		return nil, fmt.Errorf("executor: unknown module %q", moduleName)
	}
	if !actionSupported(m, action) {
		return nil, fmt.Errorf("executor: module %q does not support action %q (supported: %v)", moduleName, action, m.Actions())
	}

	// Ensure the input carries the action verb consistently. Modules that
	// read input.Action instead of the action parameter get the same value.
	input.Action = action

	start := time.Now()
	out, err := m.Execute(ctx, action, input)
	duration := time.Since(start)

	if err != nil {
		return out, fmt.Errorf("executor: module %q action %q failed: %w", moduleName, action, err)
	}
	if out == nil {
		// Defend against modules that return (nil, nil); synthesise an
		// empty output so callers can rely on out != nil on success.
		out = &ModuleOutput{}
	}
	if out.Duration == 0 {
		out.Duration = duration
	}
	return out, nil
}

// actionSupported reports whether m lists action in its Actions() slice.
func actionSupported(m Module, action string) bool {
	for _, a := range m.Actions() {
		if a == action {
			return true
		}
	}
	return false
}

// --- default executor ------------------------------------------------------

// defaultExecutor is the process-wide registry used by RegisterModule /
// Module / Execute when the caller does not supply its own. Concrete module
// packages call RegisterModule from their init() to plug in.
var defaultExecutor = NewExecutor()

// DefaultExecutor returns the process-wide Executor.
func DefaultExecutor() *Executor { return defaultExecutor }

// RegisterModule registers m with the default Executor. It is a convenience
// wrapper around DefaultExecutor().RegisterModule so that init() call sites
// stay one-liners.
func RegisterModule(m Module) { defaultExecutor.RegisterModule(m) }
