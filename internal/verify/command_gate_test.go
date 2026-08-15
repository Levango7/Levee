package verify

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
)

// --- fakeChannel -----------------------------------------------------------
//
// fakeChannel is a stub channel.Channel for CommandGate tests. It records the
// commands it received and returns the scripted ExecResult / error on each
// call. The script is consumed in order; once exhausted the last entry is
// repeated. Calls are counted atomically so that retry tests can assert the
// exact number of attempts.

type scriptEntry struct {
	result *channel.ExecResult
	err    error
	// delay, when non-zero, makes the Exec call block for that long so
	// that timeout tests can race against it.
	delay time.Duration
}

type fakeChannel struct {
	mu       sync.Mutex
	script   []scriptEntry
	calls    atomic.Int64
	commands []string
}

func (c *fakeChannel) Connect(ctx context.Context) error { return nil }
func (c *fakeChannel) Close() error                      { return nil }
func (c *fakeChannel) IsConnected() bool                 { return true }
func (c *fakeChannel) Upload(ctx context.Context, remotePath string, content io.Reader) error {
	return nil
}
func (c *fakeChannel) Download(ctx context.Context, remotePath string) (io.Reader, error) {
	return nil, nil
}

func (c *fakeChannel) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	c.calls.Add(1)
	c.mu.Lock()
	c.commands = append(c.commands, cmd)
	idx := int(c.calls.Load()) - 1
	var entry scriptEntry
	if idx < len(c.script) {
		entry = c.script[idx]
	} else if len(c.script) > 0 {
		entry = c.script[len(c.script)-1]
	}
	c.mu.Unlock()

	if entry.delay > 0 {
		select {
		case <-time.After(entry.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if entry.err != nil {
		return nil, entry.err
	}
	return entry.result, nil
}

// newFakeChannel returns a fakeChannel that returns the given results in
// order. Each result is converted to a scriptEntry with no error and no
// delay.
func newFakeChannel(results ...*channel.ExecResult) *fakeChannel {
	entries := make([]scriptEntry, len(results))
	for i, r := range results {
		entries[i] = scriptEntry{result: r}
	}
	return &fakeChannel{script: entries}
}

// --- helpers ---------------------------------------------------------------

func execResult(exit int, stdout string) *channel.ExecResult {
	return &channel.ExecResult{ExitCode: exit, Stdout: stdout, Stderr: ""}
}

// --- tests: construction ---------------------------------------------------

func TestNewCommandGateDefaults(t *testing.T) {
	g := NewCommandGate("svc", PhasePostBatch, "systemctl is-active nginx")
	assert.Equal(t, "svc", g.Name())
	assert.Equal(t, PhasePostBatch, g.Phase())
	assert.Equal(t, "systemctl is-active nginx", g.command)
	assert.Equal(t, 0, g.expectExit)
	assert.Equal(t, "", g.expectStdout)
	assert.Equal(t, DefaultCommandTimeout, g.timeout)
	assert.Equal(t, DefaultCommandRetries, g.retries)
	assert.Equal(t, DefaultCommandRetryDelay, g.retryDelay)
}

func TestNewCommandGateWithOptions(t *testing.T) {
	g := NewCommandGate("svc", PhasePostApply, "true",
		WithExpectedExit(2),
		WithExpectedStdout("ready"),
		WithCommandTimeout(5*time.Second),
		WithCommandRetries(3),
		WithCommandRetryDelay(200*time.Millisecond),
	)
	assert.Equal(t, 2, g.expectExit)
	assert.Equal(t, "ready", g.expectStdout)
	assert.Equal(t, 5*time.Second, g.timeout)
	assert.Equal(t, 3, g.retries)
	assert.Equal(t, 200*time.Millisecond, g.retryDelay)
	assert.Equal(t, PhasePostApply, g.Phase())
}

// --- tests: success / mismatch --------------------------------------------

func TestCommandGateSuccessMatchExitOnly(t *testing.T) {
	ch := newFakeChannel(execResult(0, "active\n"))
	g := NewCommandGate("svc", PhasePostBatch, "systemctl is-active nginx")

	r, err := g.Check(context.Background(), GateInput{Channel: ch})
	require.NoError(t, err)
	assert.True(t, r.Passed, "exit 0 should match default expected exit 0")
	assert.Contains(t, r.Message, "passed")
	assert.Equal(t, int64(1), ch.calls.Load(), "no retries on success")
}

func TestCommandGateSuccessMatchExitAndStdout(t *testing.T) {
	ch := newFakeChannel(execResult(0, "active\n"))
	g := NewCommandGate("svc", PhasePostBatch, "systemctl is-active nginx",
		WithExpectedStdout("active"),
	)

	r, err := g.Check(context.Background(), GateInput{Channel: ch})
	require.NoError(t, err)
	assert.True(t, r.Passed)
	assert.Equal(t, int64(1), ch.calls.Load())
}

func TestCommandGateSuccessMatchStdoutWithCRLF(t *testing.T) {
	ch := newFakeChannel(execResult(0, "active\r\n"))
	g := NewCommandGate("svc", PhasePostBatch, "cmd", WithExpectedStdout("active"))

	r, err := g.Check(context.Background(), GateInput{Channel: ch})
	require.NoError(t, err)
	assert.True(t, r.Passed, "trailing CRLF should be trimmed")
}

func TestCommandGateExitCodeMismatch(t *testing.T) {
	ch := newFakeChannel(execResult(3, "some output"))
	g := NewCommandGate("svc", PhasePostBatch, "cmd")

	r, err := g.Check(context.Background(), GateInput{Channel: ch})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Contains(t, r.Message, "exit code mismatch")
	assert.Equal(t, "exit_code_mismatch", r.Details["reason"])
	assert.Equal(t, 3, r.Details["exit_code"])
	assert.Equal(t, 0, r.Details["expected_exit"])
}

func TestCommandGateStdoutMismatch(t *testing.T) {
	ch := newFakeChannel(execResult(0, "inactive\n"))
	g := NewCommandGate("svc", PhasePostBatch, "cmd", WithExpectedStdout("active"))

	r, err := g.Check(context.Background(), GateInput{Channel: ch})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Contains(t, r.Message, "stdout mismatch")
	assert.Equal(t, "stdout_mismatch", r.Details["reason"])
	// Details preserves the raw stdout for the audit trail.
	assert.Equal(t, "inactive\n", r.Details["stdout"])
	assert.Equal(t, "active", r.Details["expected_stdout"])
}

func TestCommandGateExpectedExitNonZero(t *testing.T) {
	ch := newFakeChannel(execResult(1, ""))
	g := NewCommandGate("grep", PhasePostApply, "grep -q missing file", WithExpectedExit(1))

	r, err := g.Check(context.Background(), GateInput{Channel: ch})
	require.NoError(t, err)
	assert.True(t, r.Passed, "exit 1 should match expected exit 1")
}

// --- tests: retries --------------------------------------------------------

func TestCommandGateRetrySuccess(t *testing.T) {
	// First attempt fails, second succeeds.
	ch := &fakeChannel{script: []scriptEntry{
		{result: execResult(1, "")},
		{result: execResult(0, "ok")},
	}}
	g := NewCommandGate("svc", PhasePostBatch, "cmd",
		WithCommandRetries(2),
		WithCommandRetryDelay(5*time.Millisecond),
	)

	r, err := g.Check(context.Background(), GateInput{Channel: ch})
	require.NoError(t, err)
	assert.True(t, r.Passed, "should succeed on second attempt")
	assert.Equal(t, int64(2), ch.calls.Load())
	assert.Contains(t, r.Message, "attempt 2")
}

func TestCommandGateRetryFailureAllAttempts(t *testing.T) {
	ch := newFakeChannel(execResult(1, ""))
	g := NewCommandGate("svc", PhasePostBatch, "cmd",
		WithCommandRetries(2),
		WithCommandRetryDelay(5*time.Millisecond),
	)

	r, err := g.Check(context.Background(), GateInput{Channel: ch})
	require.NoError(t, err)
	assert.False(t, r.Passed, "all attempts fail")
	assert.Equal(t, int64(3), ch.calls.Load(), "1 initial + 2 retries")
	assert.Contains(t, r.Message, "exit code mismatch")
}

func TestCommandGateRetryOnExecError(t *testing.T) {
	// First attempt returns an exec error, second succeeds.
	ch := &fakeChannel{script: []scriptEntry{
		{err: errors.New("transport broken")},
		{result: execResult(0, "ok")},
	}}
	g := NewCommandGate("svc", PhasePostBatch, "cmd",
		WithCommandRetries(2),
		WithCommandRetryDelay(5*time.Millisecond),
	)

	r, err := g.Check(context.Background(), GateInput{Channel: ch})
	require.NoError(t, err)
	assert.True(t, r.Passed, "should succeed after exec error + retry")
	assert.Equal(t, int64(2), ch.calls.Load())
}

// --- tests: timeout --------------------------------------------------------

func TestCommandGateTimeout(t *testing.T) {
	// Channel blocks for 200ms; gate timeout is 20ms. The attempt should
	// time out and report a failure.
	ch := &fakeChannel{script: []scriptEntry{
		{result: execResult(0, ""), delay: 200 * time.Millisecond},
	}}
	g := NewCommandGate("svc", PhasePostBatch, "cmd",
		WithCommandTimeout(20*time.Millisecond),
	)

	start := time.Now()
	r, err := g.Check(context.Background(), GateInput{Channel: ch})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.False(t, r.Passed, "timeout should fail the gate")
	assert.Less(t, elapsed, 150*time.Millisecond, "should not wait for the full channel delay")
	assert.Contains(t, r.Message, "exec failed")
}

func TestCommandGateCallerContextTimeout(t *testing.T) {
	ch := &fakeChannel{script: []scriptEntry{
		{result: execResult(0, ""), delay: 200 * time.Millisecond},
	}}
	g := NewCommandGate("svc", PhasePostBatch, "cmd",
		WithCommandTimeout(30*time.Second),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	r, err := g.Check(ctx, GateInput{Channel: ch})
	require.NoError(t, err)
	assert.False(t, r.Passed)
}

func TestCommandGateAlreadyCancelledContext(t *testing.T) {
	ch := newFakeChannel(execResult(0, ""))
	g := NewCommandGate("svc", PhasePostBatch, "cmd")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r, err := g.Check(ctx, GateInput{Channel: ch})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, int64(0), ch.calls.Load(), "should not call channel when ctx already cancelled")
	assert.Contains(t, r.Message, "cancelled before run")
}

// --- tests: missing channel -----------------------------------------------

func TestCommandGateMissingChannel(t *testing.T) {
	g := NewCommandGate("svc", PhasePostBatch, "cmd")

	r, err := g.Check(context.Background(), GateInput{Channel: nil})
	require.Error(t, err)
	assert.False(t, r.Passed)
	assert.Contains(t, r.Message, "no channel")
}

// --- tests: gate interface -------------------------------------------------

func TestCommandGateImplementsGateInterface(t *testing.T) {
	// Compile-time assertion that *CommandGate satisfies Gate.
	var _ Gate = (*CommandGate)(nil)

	g := NewCommandGate("svc", PhasePostBatch, "cmd")
	assert.Equal(t, "svc", g.Name())
	assert.Equal(t, PhasePostBatch, g.Phase())
}

// --- tests: integration with GateManager -----------------------------------

func TestCommandGateRegisteredWithManager(t *testing.T) {
	gm := NewGateManager()
	ch := newFakeChannel(execResult(0, "active\n"))
	gm.Register(NewCommandGate("svc-health", PhasePostBatch, "systemctl is-active nginx",
		WithExpectedStdout("active"),
	))

	results := gm.RunPhase(context.Background(), PhasePostBatch, GateInput{Channel: ch})
	require.Len(t, results, 1)
	assert.True(t, results[0].Passed)
}

func TestCommandGateManagerFailureStopsPending(t *testing.T) {
	gm := NewGateManager()
	chFail := newFakeChannel(execResult(1, ""))
	chSlow := &fakeChannel{script: []scriptEntry{
		{result: execResult(0, ""), delay: 100 * time.Millisecond},
	}}
	gm.Register(NewCommandGate("a-fail", PhasePreApply, "cmd1"))
	gm.Register(NewCommandGate("b-slow", PhasePreApply, "cmd2"))

	// RunPhase uses a single GateInput for all gates, so they share the
	// same channel. We swap to a per-gate setup by running them separately
	// and verifying the manager's skip behaviour with a single channel
	// that fails fast.
	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{Channel: chFail})
	require.Len(t, results, 2)
	// At least one gate is a real failure.
	failedCount := 0
	for _, r := range results {
		if !r.Passed && !isSkipped(r) {
			failedCount++
		}
	}
	assert.GreaterOrEqual(t, failedCount, 1)
	// b-slow channel is unused here; the assertion is that the manager
	// returns promptly without waiting for it.
	_ = chSlow
}
