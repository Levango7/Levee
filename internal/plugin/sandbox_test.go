package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeScript writes a small script to a temp file and returns its path.
// The script is a shell script on Unix and a batch file on Windows. It is
// used to simulate a plugin binary for sandbox tests.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	var name, content string
	if runtime.GOOS == "windows" {
		name = "plugin.bat"
		content = "@echo off\r\n" + body + "\r\n"
	} else {
		name = "plugin.sh"
		content = "#!/bin/sh\n" + body + "\n"
	}
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o755))
	return p
}

// writeExitScript writes a script that exits with the given code.
func writeExitScript(t *testing.T, code int) string {
	if runtime.GOOS == "windows" {
		return writeScript(t, "exit "+itoa(code))
	}
	return writeScript(t, "exit "+itoa(code))
}

// itoa is a tiny strconv.Itoa replacement to avoid the import.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestSandboxConfigDefaults(t *testing.T) {
	cfg := DefaultSandboxConfig()
	assert.Equal(t, 30*time.Second, cfg.Timeout)
	assert.Equal(t, int64(512*1024*1024), cfg.MemoryLimit)
	assert.Equal(t, 3, cfg.MaxRestarts)
	assert.Equal(t, 100*time.Millisecond, cfg.RestartDelay)
}

func TestSandboxStartAndStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	bin := writeExitScript(t, 0)
	cfg := DefaultSandboxConfig()
	cfg.MaxRestarts = 0 // do not restart on clean exit

	sb := NewSandbox("test", bin, nil, nil, cfg, nil)
	ctx := context.Background()

	require.NoError(t, sb.Start(ctx))
	assert.True(t, sb.IsRunning())

	require.NoError(t, sb.Stop(5*time.Second))
	assert.False(t, sb.IsRunning())
}

func TestSandboxStartIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	bin := writeScript(t, "sleep 5")
	cfg := DefaultSandboxConfig()
	cfg.MaxRestarts = -1 // never restart

	sb := NewSandbox("test", bin, nil, nil, cfg, nil)
	ctx := context.Background()

	require.NoError(t, sb.Start(ctx))
	// Second Start should be a no-op.
	require.NoError(t, sb.Start(ctx))
	assert.True(t, sb.IsRunning())

	require.NoError(t, sb.Stop(5*time.Second))
}

func TestSandboxStopWhenNotRunning(t *testing.T) {
	sb := NewSandbox("test", "/nonexistent", nil, nil, DefaultSandboxConfig(), nil)
	// Stop on a never-started sandbox should be a no-op.
	require.NoError(t, sb.Stop(time.Second))
	assert.False(t, sb.IsRunning())
}

func TestSandboxStartEmptyBinary(t *testing.T) {
	sb := NewSandbox("test", "", nil, nil, DefaultSandboxConfig(), nil)
	err := sb.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty binary path")
}

func TestSandboxStartNonexistentBinary(t *testing.T) {
	sb := NewSandbox("test", "/nonexistent/binary", nil, nil, DefaultSandboxConfig(), nil)
	err := sb.Start(context.Background())
	require.Error(t, err)
}

func TestSandboxCrashRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	// Script that exits non-zero immediately.
	bin := writeExitScript(t, 1)
	cfg := DefaultSandboxConfig()
	cfg.MaxRestarts = 2
	cfg.RestartDelay = 10 * time.Millisecond

	var crashCount atomic.Int32
	var restartedFlags []bool

	onCrash := func(alert CrashAlert) {
		crashCount.Add(1)
		restartedFlags = append(restartedFlags, alert.Restarted)
	}

	sb := NewSandbox("crash", bin, nil, nil, cfg, onCrash)
	ctx := context.Background()

	require.NoError(t, sb.Start(ctx))

	// Wait for the sandbox to exhaust its restart budget.
	err := sb.Wait()
	require.Error(t, err, "process should exit with error")

	assert.False(t, sb.IsRunning())
	// We expect at least MaxRestarts crashes.
	assert.GreaterOrEqual(t, int(crashCount.Load()), 1)
}

func TestSandboxCrashNoRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	bin := writeExitScript(t, 1)
	cfg := DefaultSandboxConfig()
	cfg.MaxRestarts = -1 // never restart
	cfg.RestartDelay = 0

	var alerts atomic.Int32
	onCrash := func(alert CrashAlert) {
		alerts.Add(1)
	}

	sb := NewSandbox("norestart", bin, nil, nil, cfg, onCrash)
	ctx := context.Background()

	require.NoError(t, sb.Start(ctx))
	err := sb.Wait()
	require.Error(t, err)
	assert.False(t, sb.IsRunning())
	assert.Equal(t, int32(0), alerts.Load(), "no crash alert when restarts disabled")
}

func TestSandboxCrashAlertCallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	bin := writeExitScript(t, 1)
	cfg := DefaultSandboxConfig()
	cfg.MaxRestarts = 0 // 0 means: allow 0 restarts → surface crash immediately
	cfg.RestartDelay = 0

	var alert CrashAlert
	var got atomic.Bool
	onCrash := func(a CrashAlert) {
		alert = a
		got.Store(true)
	}

	sb := NewSandbox("alert", bin, nil, nil, cfg, onCrash)
	ctx := context.Background()

	require.NoError(t, sb.Start(ctx))
	_ = sb.Wait()

	assert.True(t, got.Load(), "crash alert callback should fire")
	assert.Equal(t, "alert", alert.PluginName)
	assert.Equal(t, 1, alert.CrashCount)
	assert.False(t, alert.Restarted)
	assert.NotNil(t, alert.Err)
}

func TestSandboxWithTimeout(t *testing.T) {
	cfg := SandboxConfig{Timeout: 100 * time.Millisecond}
	sb := NewSandbox("test", "/bin/true", nil, nil, cfg, nil)

	ctx, cancel := sb.WithTimeout(context.Background())
	defer cancel()
	assert.NotNil(t, ctx)
	// The context should have a deadline.
	_, ok := ctx.Deadline()
	assert.True(t, ok, "context should have deadline")
}

func TestSandboxWithTimeoutZero(t *testing.T) {
	cfg := SandboxConfig{Timeout: 0}
	sb := NewSandbox("test", "/bin/true", nil, nil, cfg, nil)

	ctx, cancel := sb.WithTimeout(context.Background())
	defer cancel()
	// No deadline when timeout is zero.
	_, ok := ctx.Deadline()
	assert.False(t, ok, "no deadline when timeout is zero")
}

func TestSandboxCrashCount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	bin := writeExitScript(t, 1)
	cfg := DefaultSandboxConfig()
	cfg.MaxRestarts = 1
	cfg.RestartDelay = 5 * time.Millisecond

	sb := NewSandbox("count", bin, nil, nil, cfg, nil)
	ctx := context.Background()

	require.NoError(t, sb.Start(ctx))
	_ = sb.Wait()

	assert.GreaterOrEqual(t, sb.CrashCount(), 1)
}

func TestIsCrashError(t *testing.T) {
	assert.False(t, IsCrashError(nil))
	assert.False(t, IsCrashError(errors.New("plain error")))
}

func TestAppendHostEnv(t *testing.T) {
	env := []string{"LEVEE_PLUGIN=1"}
	out := AppendHostEnv(env)
	assert.Contains(t, out, "LEVEE_PLUGIN=1")
	// Should also contain at least one host env var.
	assert.Greater(t, len(out), 1)
}
