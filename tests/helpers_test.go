package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers.go tests
// ---------------------------------------------------------------------------

func TestTempDB(t *testing.T) {
	p := TempDB(t)
	assert.FileExists(t, p)
	assert.NotEmpty(t, p)
}

func TestTempDir(t *testing.T) {
	d := TempDir(t)
	assert.DirExists(t, d)
}

func TestWriteFile(t *testing.T) {
	dir := TempDir(t)
	p := WriteFile(t, dir, "test.txt", []byte("hello"))
	assert.FileExists(t, p)
}

func TestMockTarget(t *testing.T) {
	tgt := MockTarget()
	assert.Equal(t, "t-001", tgt.ID)
	assert.Equal(t, "127.0.0.1", tgt.Host)
	assert.Equal(t, 22, tgt.Port)
	assert.Equal(t, "linux", tgt.OS)
	assert.Equal(t, "ssh", tgt.Channel)
	assert.Equal(t, "levee-test", tgt.Username)
	assert.False(t, tgt.StrictHost)
	assert.NotEmpty(t, tgt.Tags)
}

func TestMockTargetList(t *testing.T) {
	list := MockTargetList(5)
	require.Len(t, list, 5)
	assert.Equal(t, "t-001", list[0].ID)
	assert.Equal(t, "t-005", list[4].ID)
	assert.Equal(t, 2200, list[0].Port)
	assert.Equal(t, 2204, list[4].Port)

	ports := make(map[int]bool)
	for _, tgt := range list {
		assert.False(t, ports[tgt.Port], "duplicate port %d", tgt.Port)
		ports[tgt.Port] = true
	}
}

func TestAssertJSON(t *testing.T) {
	expected := []byte(`{"a":1,"b":"x"}`)
	actual := []byte(`{"b":"x","a":1}`)
	AssertJSON(t, expected, actual) // order-insensitive
}

func TestAssertJSONEqual(t *testing.T) {
	type s struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	AssertJSONEqual(t, s{A: 1, B: "x"}, map[string]interface{}{"a": 1, "b": "x"})
}

func TestMustMarshal(t *testing.T) {
	b := MustMarshal(t, map[string]int{"x": 1})
	assert.JSONEq(t, `{"x":1}`, string(b))
}

func TestEnvVar(t *testing.T) {
	EnvVar(t, "LEVEE_TEST_FOO", "bar")
	v, ok := os.LookupEnv("LEVEE_TEST_FOO")
	require.True(t, ok)
	assert.Equal(t, "bar", v)
}

func TestEnvVars(t *testing.T) {
	EnvVars(t, map[string]string{
		"LEVEE_TEST_A": "1",
		"LEVEE_TEST_B": "2",
	})
	v, ok := os.LookupEnv("LEVEE_TEST_A")
	require.True(t, ok)
	assert.Equal(t, "1", v)
	v, ok = os.LookupEnv("LEVEE_TEST_B")
	require.True(t, ok)
	assert.Equal(t, "2", v)
}

// ---------------------------------------------------------------------------
// mocks.go — MockChannel tests
// ---------------------------------------------------------------------------

func TestMockChannel_ConnectExecClose(t *testing.T) {
	ch := NewMockChannel()
	tgt := MockTarget()
	ctx := context.Background()

	// Before connect, Exec should fail.
	_, err := ch.Exec(ctx, "uptime")
	require.Error(t, err)

	// Connect.
	require.NoError(t, ch.Connect(ctx, tgt))
	assert.True(t, ch.Connected)
	require.Len(t, ch.ConnectCalls, 1)

	// Script a response and exec.
	ch.Script("uptime", ExecResult{Stdout: "load average: 0.1", ExitCode: 0})
	r, err := ch.Exec(ctx, "uptime")
	require.NoError(t, err)
	assert.Equal(t, "load average: 0.1", r.Stdout)
	assert.Equal(t, 0, r.ExitCode)
	require.Len(t, ch.ExecCalls, 2) // failed pre-connect call + this one

	// Unscripted command returns default result.
	r2, err := ch.Exec(ctx, "uname -a")
	require.NoError(t, err)
	assert.Equal(t, 0, r2.ExitCode)

	// Close.
	require.NoError(t, ch.Close(ctx))
	assert.False(t, ch.Connected)
	assert.Equal(t, 1, ch.CloseCalls)

	// Exec after close fails.
	_, err = ch.Exec(ctx, "uptime")
	require.Error(t, err)
}

func TestMockChannel_ErrOnConnect(t *testing.T) {
	ch := NewMockChannel()
	ch.ErrOnConnect = assert.AnError
	err := ch.Connect(context.Background(), MockTarget())
	assert.ErrorIs(t, err, assert.AnError)
}

func TestMockChannel_ErrOnExec(t *testing.T) {
	ch := NewMockChannel()
	ch.AllowDisconnectedExec = true
	ch.ErrOnExec = assert.AnError
	_, err := ch.Exec(context.Background(), "cmd")
	assert.ErrorIs(t, err, assert.AnError)
}

func TestMockChannel_CallCount(t *testing.T) {
	ch := NewMockChannel()
	ch.AllowDisconnectedExec = true
	ctx := context.Background()
	_ = ch.Connect(ctx, MockTarget())
	_, _ = ch.Exec(ctx, "a")
	_, _ = ch.Exec(ctx, "b")
	_ = ch.Close(ctx)
	assert.Equal(t, 4, ch.CallCount())
}

func TestMockChannel_Concurrent(t *testing.T) {
	ch := NewMockChannel()
	ch.AllowDisconnectedExec = true
	ctx := context.Background()

	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func() {
			_, _ = ch.Exec(ctx, "ping")
			done <- struct{}{}
		}()
	}
	for i := 0; i < 50; i++ {
		<-done
	}
	assert.Equal(t, 50, ch.CallCount())
}

// ---------------------------------------------------------------------------
// mocks.go — MockExecutor tests
// ---------------------------------------------------------------------------

func TestMockExecutor_Default(t *testing.T) {
	ex := NewMockExecutor()
	r, err := ex.Execute(context.Background(), "shell", map[string]interface{}{"cmd": "echo hi"})
	require.NoError(t, err)
	assert.Equal(t, 0, r.ExitCode)
	assert.Equal(t, 1, ex.CallCount())
	assert.Equal(t, []string{"shell"}, ex.ModulesCalled())
}

func TestMockExecutor_RegisterResult(t *testing.T) {
	ex := NewMockExecutor().
		RegisterResult("shell", ModuleResult{ExitCode: 0, Output: map[string]interface{}{"stdout": "ok"}})
	r, err := ex.Execute(context.Background(), "shell", nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", r.Output["stdout"])
}

func TestMockExecutor_RegisterError(t *testing.T) {
	ex := NewMockExecutor().RegisterError("shell", assert.AnError)
	_, err := ex.Execute(context.Background(), "shell", nil)
	assert.ErrorIs(t, err, assert.AnError)
}

func TestMockExecutor_RegisterHandler(t *testing.T) {
	ex := NewMockExecutor().Register("custom", func(params map[string]interface{}) (ModuleResult, error) {
		return ModuleResult{
			ExitCode: 42,
			Output:   map[string]interface{}{"echo": params["msg"]},
		}, nil
	})
	r, err := ex.Execute(context.Background(), "custom", map[string]interface{}{"msg": "hello"})
	require.NoError(t, err)
	assert.Equal(t, 42, r.ExitCode)
	assert.Equal(t, "hello", r.Output["echo"])
}

func TestMockExecutor_ErrIfSet(t *testing.T) {
	ex := NewMockExecutor()
	ex.ErrIfSet = assert.AnError
	_, err := ex.Execute(context.Background(), "shell", nil)
	assert.ErrorIs(t, err, assert.AnError)
}

func TestMockExecutor_ModulesCalled(t *testing.T) {
	ex := NewMockExecutor()
	ctx := context.Background()
	_, _ = ex.Execute(ctx, "shell", nil)
	_, _ = ex.Execute(ctx, "file", nil)
	_, _ = ex.Execute(ctx, "shell", nil)
	assert.Equal(t, []string{"shell", "file", "shell"}, ex.ModulesCalled())
}

func TestMockExecutor_DurationPreserved(t *testing.T) {
	ex := NewMockExecutor().RegisterResult("timed", ModuleResult{
		ExitCode: 0,
		Duration: 250 * time.Millisecond,
	})
	r, err := ex.Execute(context.Background(), "timed", nil)
	require.NoError(t, err)
	assert.Equal(t, 250*time.Millisecond, r.Duration)
}
