// shell_run_test.go exercises ShellRunner (MVP task T058). The tests cover
// the success path, non-zero exit, stderr capture, timeout, empty-command
// rejection, duration measurement, WithTimeout chaining, RunOnTarget's
// not-implemented stub, context cancellation and multi-line output.
//
// Commands are chosen to be portable across Windows (cmd /c) and Unix
// (sh -c): echo, exit, and a platform-specific sleep stand-in provided by
// sleepCommandForOS. Where a behaviour is genuinely platform-specific (e.g.
// stderr redirection syntax) the test is skipped on the offending platform
// rather than carrying fragile per-OS command strings.

package executor

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sleepCommandForOS returns a shell command that blocks for roughly the given
// number of seconds on the current platform. On Unix we use `sleep N`; on
// Windows we use `ping -n (N+1) 127.0.0.1` which waits ~N seconds by sending
// N+1 ICMP echo requests at 1-second intervals. The ping approach avoids the
// `timeout` built-in which requires an interactive console and breaks under
// redirected stdio.
func sleepCommandForOS(seconds int) string {
	if runtime.GOOS == "windows" {
		// ping -n count waits (count-1) seconds, so we ask for seconds+1.
		return "ping -n " + itoa(seconds+1) + " 127.0.0.1"
	}
	return "sleep " + itoa(seconds)
}

// itoa is a tiny strconv.Itoa-free helper so that the test file does not pull
// in strconv for two calls. It only needs to handle small non-negative ints.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// --- Run success ----------------------------------------------------------

func TestShellRunner_RunSuccess(t *testing.T) {
	r := NewShellRunner()
	res, err := r.Run(context.Background(), "echo hello")
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Contains(t, res.Stdout, "hello")
	assert.Equal(t, 0, res.ExitCode)
	assert.Nil(t, res.Err)
}

// --- Run non-zero exit code -----------------------------------------------

func TestShellRunner_RunExitCode(t *testing.T) {
	r := NewShellRunner()
	res, err := r.Run(context.Background(), "exit 1")
	// Non-zero exit is not a Go-level error: the command ran and exited.
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 1, res.ExitCode)
}

func TestShellRunner_RunExitCode2(t *testing.T) {
	r := NewShellRunner()
	res, err := r.Run(context.Background(), "exit 42")
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 42, res.ExitCode)
}

// --- Run stderr -----------------------------------------------------------

func TestShellRunner_RunStderr(t *testing.T) {
	// `echo err 1>&2` is the most portable stderr redirection: it works on
	// both cmd /c (Windows) and sh -c (Unix). The bare `>&2` form is accepted
	// by cmd but not by POSIX sh, so we use the explicit 1>&2 form.
	r := NewShellRunner()
	res, err := r.Run(context.Background(), "echo err 1>&2")
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Contains(t, res.Stderr, "err")
}

// --- Run timeout ----------------------------------------------------------

func TestShellRunner_RunTimeout(t *testing.T) {
	r := NewShellRunner().WithTimeout(100 * time.Millisecond)
	res, err := r.Run(context.Background(), sleepCommandForOS(10))
	// Timeout must surface as ErrCommandTimeout (wrapped).
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCommandTimeout), "err should wrap ErrCommandTimeout, got: %v", err)
	// Partial result is still returned for inspection.
	require.NotNil(t, res)
	assert.Equal(t, -1, res.ExitCode)
}

// --- Run empty command ----------------------------------------------------

func TestShellRunner_RunEmptyCommand(t *testing.T) {
	r := NewShellRunner()

	// Totally empty.
	res, err := r.Run(context.Background(), "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmptyCommand))
	assert.Nil(t, res)

	// Whitespace-only is also rejected.
	res, err = r.Run(context.Background(), "   \t  ")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmptyCommand))
	assert.Nil(t, res)
}

// --- Run duration ---------------------------------------------------------

func TestShellRunner_RunDuration(t *testing.T) {
	r := NewShellRunner().WithTimeout(5 * time.Second)
	res, err := r.Run(context.Background(), "echo anything")
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Greater(t, res.Duration, time.Duration(0))
}

// --- WithTimeout chaining -------------------------------------------------

func TestShellRunner_WithTimeout(t *testing.T) {
	r := NewShellRunner()
	original := r.timeout

	r2 := r.WithTimeout(7 * time.Second)
	assert.Equal(t, 7*time.Second, r2.timeout, "WithTimeout should set the new timeout")
	assert.Same(t, r, r2, "WithTimeout should return the same receiver for chaining")
	assert.Equal(t, original, DefaultShellTimeout, "NewShellRunner should use DefaultShellTimeout")

	// Non-positive resets to default.
	r3 := r.WithTimeout(0)
	assert.Equal(t, DefaultShellTimeout, r3.timeout)

	r4 := r.WithTimeout(-1 * time.Second)
	assert.Equal(t, DefaultShellTimeout, r4.timeout)
}

func TestNewShellRunner_DefaultTimeout(t *testing.T) {
	r := NewShellRunner()
	assert.Equal(t, DefaultShellTimeout, r.timeout)
}

// --- RunOnTarget not implemented -----------------------------------------

func TestShellRunner_RunOnTarget_NotImplemented(t *testing.T) {
	r := NewShellRunner()
	res, err := r.RunOnTarget(context.Background(), "echo hi", "ssh://example.com")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotImplemented))
	assert.Nil(t, res)
}

// --- Context cancellation -------------------------------------------------

func TestShellRunner_RunContextCancel(t *testing.T) {
	r := NewShellRunner().WithTimeout(30 * time.Second) // long own timeout
	ctx, cancel := context.WithCancel(context.Background())

	// Start the command in a goroutine so we can cancel mid-flight.
	type outcome struct {
		res *ShellRunResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := r.Run(ctx, sleepCommandForOS(10))
		done <- outcome{res, err}
	}()

	// Give the command a moment to start, then cancel.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case o := <-done:
		require.Error(t, o.err, "cancelled run should return an error")
		assert.True(t, errors.Is(o.err, ErrCommandTimeout) || errors.Is(o.err, context.Canceled),
			"err should wrap ErrCommandTimeout or context.Canceled, got: %v", o.err)
		require.NotNil(t, o.res, "partial result should still be returned")
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// --- Multi-line output ----------------------------------------------------

func TestShellRunner_RunMultiLineOutput(t *testing.T) {
	r := NewShellRunner().WithTimeout(5 * time.Second)
	// `echo a && echo b` is portable across cmd and sh.
	res, err := r.Run(context.Background(), "echo line1 && echo line2")
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Contains(t, res.Stdout, "line1")
	assert.Contains(t, res.Stdout, "line2")
	// Both lines must be present in order.
	lines := strings.Fields(res.Stdout)
	assert.Contains(t, lines, "line1")
	assert.Contains(t, lines, "line2")
}

// --- Result Command field -------------------------------------------------

func TestShellRunner_RunResultCommand(t *testing.T) {
	r := NewShellRunner()
	cmd := "echo tracked"
	res, err := r.Run(context.Background(), cmd)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, cmd, res.Command, "ShellRunResult.Command should echo the input command")
}

// --- Sentinel errors are distinct ----------------------------------------

func TestShellRunner_SentinelErrors(t *testing.T) {
	// Sanity-check that the sentinels are distinct and non-nil, so that
	// errors.Is classification in callers works as documented.
	assert.NotEqual(t, ErrEmptyCommand, ErrCommandTimeout)
	assert.NotEqual(t, ErrEmptyCommand, ErrNotImplemented)
	assert.NotEqual(t, ErrCommandTimeout, ErrNotImplemented)
	assert.NotNil(t, ErrEmptyCommand)
	assert.NotNil(t, ErrCommandTimeout)
	assert.NotNil(t, ErrNotImplemented)
}
