package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mock Module -----------------------------------------------------------

// mockModule is a configurable Module stub. Each field toggles a behaviour so
// that tests can exercise the executor's error paths without defining a
// separate stub per case.
type mockModule struct {
	name      string
	actions   []string
	idem      bool
	execFn    func(ctx context.Context, action string, input ModuleInput) (*ModuleOutput, error)
	callCount int
	mu        sync.Mutex
}

func (m *mockModule) Name() string      { return m.name }
func (m *mockModule) Actions() []string { return m.actions }
func (m *mockModule) Idempotent() bool  { return m.idem }
func (m *mockModule) Execute(ctx context.Context, action string, input ModuleInput) (*ModuleOutput, error) {
	m.mu.Lock()
	m.callCount++
	m.mu.Unlock()
	if m.execFn != nil {
		return m.execFn(ctx, action, input)
	}
	return &ModuleOutput{ExitCode: 0, Stdout: "ok", Changed: true}, nil
}

func (m *mockModule) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCount
}

// --- registration ----------------------------------------------------------

func TestNewExecutorEmpty(t *testing.T) {
	e := NewExecutor()
	assert.Empty(t, e.Modules())
}

func TestRegisterAndLookup(t *testing.T) {
	e := NewExecutor()
	m := &mockModule{name: "shell", actions: []string{"exec"}}
	e.RegisterModule(m)

	got, ok := e.Module("shell")
	require.True(t, ok)
	assert.Same(t, m, got)

	_, ok = e.Module("file")
	assert.False(t, ok)
}

func TestRegisterOverwrite(t *testing.T) {
	e := NewExecutor()
	a := &mockModule{name: "shell", actions: []string{"exec"}}
	b := &mockModule{name: "shell", actions: []string{"exec", "script"}}
	e.RegisterModule(a)
	e.RegisterModule(b)
	got, ok := e.Module("shell")
	require.True(t, ok)
	assert.Same(t, b, got)
}

func TestUnregisterModule(t *testing.T) {
	e := NewExecutor()
	e.RegisterModule(&mockModule{name: "shell", actions: []string{"exec"}})
	e.UnregisterModule("shell")
	_, ok := e.Module("shell")
	assert.False(t, ok)
}

func TestModulesSorted(t *testing.T) {
	e := NewExecutor()
	e.RegisterModule(&mockModule{name: "svc", actions: []string{"start"}})
	e.RegisterModule(&mockModule{name: "file", actions: []string{"copy"}})
	e.RegisterModule(&mockModule{name: "shell", actions: []string{"exec"}})
	assert.Equal(t, []string{"file", "shell", "svc"}, e.Modules())
}

// --- Execute dispatch ------------------------------------------------------

func TestExecuteUnknownModule(t *testing.T) {
	e := NewExecutor()
	_, err := e.Execute(context.Background(), "nope", "exec", ModuleInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown module")
	assert.Contains(t, err.Error(), "nope")
}

func TestExecuteUnknownAction(t *testing.T) {
	e := NewExecutor()
	e.RegisterModule(&mockModule{name: "shell", actions: []string{"exec"}})
	_, err := e.Execute(context.Background(), "shell", "script", ModuleInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support action")
	assert.Contains(t, err.Error(), "script")
}

func TestExecuteSuccess(t *testing.T) {
	e := NewExecutor()
	m := &mockModule{name: "shell", actions: []string{"exec"}}
	e.RegisterModule(m)

	out, err := e.Execute(context.Background(), "shell", "exec", ModuleInput{
		Args: map[string]any{"cmd": "echo hi"},
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, 0, out.ExitCode)
	assert.Equal(t, "ok", out.Stdout)
	assert.True(t, out.Changed)
	assert.Equal(t, 1, m.calls())
}

func TestExecutePropagatesModuleError(t *testing.T) {
	e := NewExecutor()
	want := errors.New("channel broken")
	e.RegisterModule(&mockModule{
		name:    "shell",
		actions: []string{"exec"},
		execFn:  func(context.Context, string, ModuleInput) (*ModuleOutput, error) { return nil, want },
	})
	_, err := e.Execute(context.Background(), "shell", "exec", ModuleInput{})
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
	assert.Contains(t, err.Error(), "module \"shell\" action \"exec\" failed")
}

func TestExecuteSynthesisesNilOutput(t *testing.T) {
	e := NewExecutor()
	e.RegisterModule(&mockModule{
		name:    "shell",
		actions: []string{"exec"},
		execFn:  func(context.Context, string, ModuleInput) (*ModuleOutput, error) { return nil, nil },
	})
	out, err := e.Execute(context.Background(), "shell", "exec", ModuleInput{})
	require.NoError(t, err)
	require.NotNil(t, out, "executor must synthesise non-nil output on success")
	assert.Equal(t, 0, out.ExitCode)
}

func TestExecuteMeasuresDuration(t *testing.T) {
	e := NewExecutor()
	e.RegisterModule(&mockModule{
		name:    "slow",
		actions: []string{"run"},
		execFn: func(context.Context, string, ModuleInput) (*ModuleOutput, error) {
			time.Sleep(5 * time.Millisecond)
			return &ModuleOutput{ExitCode: 0}, nil
		},
	})
	out, err := e.Execute(context.Background(), "slow", "run", ModuleInput{})
	require.NoError(t, err)
	assert.Greater(t, out.Duration, time.Duration(0))
}

func TestExecutePreservesModuleDuration(t *testing.T) {
	e := NewExecutor()
	want := 42 * time.Millisecond
	e.RegisterModule(&mockModule{
		name:    "shell",
		actions: []string{"exec"},
		execFn: func(context.Context, string, ModuleInput) (*ModuleOutput, error) {
			return &ModuleOutput{ExitCode: 0, Duration: want}, nil
		},
	})
	out, err := e.Execute(context.Background(), "shell", "exec", ModuleInput{})
	require.NoError(t, err)
	assert.Equal(t, want, out.Duration, "module-supplied duration should be preserved")
}

func TestExecuteSetsActionOnInput(t *testing.T) {
	e := NewExecutor()
	var seen string
	e.RegisterModule(&mockModule{
		name:    "shell",
		actions: []string{"exec"},
		execFn: func(_ context.Context, _ string, in ModuleInput) (*ModuleOutput, error) {
			seen = in.Action
			return &ModuleOutput{ExitCode: 0}, nil
		},
	})
	_, err := e.Execute(context.Background(), "shell", "exec", ModuleInput{})
	require.NoError(t, err)
	assert.Equal(t, "exec", seen)
}

func TestExecuteConcurrent(t *testing.T) {
	e := NewExecutor()
	m := &mockModule{name: "shell", actions: []string{"exec"}}
	e.RegisterModule(m)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := e.Execute(context.Background(), "shell", "exec", ModuleInput{})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
	assert.Equal(t, n, m.calls())
}

// --- idempotency -----------------------------------------------------------

func TestIdempotentFlagHonoured(t *testing.T) {
	e := NewExecutor()
	e.RegisterModule(&mockModule{name: "file", actions: []string{"copy"}, idem: true})
	e.RegisterModule(&mockModule{name: "shell", actions: []string{"exec"}, idem: false})

	m, _ := e.Module("file")
	assert.True(t, m.Idempotent())
	m, _ = e.Module("shell")
	assert.False(t, m.Idempotent())
}

// --- default executor ------------------------------------------------------

func TestDefaultExecutorIsSingleton(t *testing.T) {
	a := DefaultExecutor()
	b := DefaultExecutor()
	assert.Same(t, a, b)
}

func TestDefaultExecutorRegisterModule(t *testing.T) {
	// Use a unique module name to avoid colliding with any real module that
	// may register itself via init().
	RegisterModule(&mockModule{name: "test-mock-module", actions: []string{"ping"}})
	defer DefaultExecutor().UnregisterModule("test-mock-module")

	m, ok := DefaultExecutor().Module("test-mock-module")
	require.True(t, ok)
	assert.Equal(t, "test-mock-module", m.Name())
}

// --- ModuleInput / ModuleOutput -------------------------------------------

func TestModuleInputZeroValue(t *testing.T) {
	var in ModuleInput
	assert.Empty(t, in.Action)
	assert.Nil(t, in.Args)
	assert.Nil(t, in.Target)
	assert.Nil(t, in.Channel)
}

func TestModuleOutputZeroValue(t *testing.T) {
	var out ModuleOutput
	assert.Equal(t, 0, out.ExitCode)
	assert.Empty(t, out.Stdout)
	assert.Empty(t, out.Stderr)
	assert.Equal(t, time.Duration(0), out.Duration)
	assert.False(t, out.Changed)
}
