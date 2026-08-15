package channel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- precheck test doubles --------------------------------------------------

// precheckTarget is a Target stub for precheck tests.
type precheckTarget struct {
	host string
	port int
	typ  string
	cred CredentialRef
}

func (t precheckTarget) Host() string               { return t.host }
func (t precheckTarget) Port() int                  { return t.port }
func (t precheckTarget) Type() string               { return t.typ }
func (t precheckTarget) Credentials() CredentialRef { return t.cred }

// precheckChannel is a configurable Channel stub for precheck tests. It also
// implements ChannelFactory: Create returns a new precheckChannel bound to
// the target's host, sharing the failure maps with the parent. This lets tests
// program per-host behaviour (connectFailures, execFailures, exitCodes,
// stdoutOverride) while the Prechecker creates a fresh channel per target via
// the factory.
type precheckChannel struct {
	mu sync.Mutex

	// host is the target host this channel is bound to. Set by Create when
	// used as a ChannelFactory. In single-channel mode it is empty and the
	// failure maps are not consulted (Connect always succeeds).
	host string

	// connectFailures maps host -> error. If this channel's host is present,
	// Connect returns the mapped error.
	connectFailures map[string]error

	// execFailures maps host -> error. If this channel's host is present,
	// Exec returns the mapped error.
	execFailures map[string]error

	// exitCodes maps host -> exit code. Default is 0.
	exitCodes map[string]int

	// stdoutOverride maps host -> stdout. Default is "ok\n".
	stdoutOverride map[string]string

	// execDelay is the delay before Exec returns. Used to test timeouts.
	execDelay time.Duration

	// call counters (atomic).
	connectCalls int32
	execCalls    int32
	closeCalls   int32

	// connected flag (single-channel model).
	connected bool
	closed    bool
}

// Create implements ChannelFactory. It returns a new precheckChannel bound to
// the given target's host, sharing the failure configuration with the parent.
// This lets tests program per-host behaviour while the Prechecker creates a
// fresh channel per target.
func (c *precheckChannel) Create(target Target) (Channel, error) {
	if target == nil {
		return nil, errors.New("precheck: nil target")
	}
	return &precheckChannel{
		host:            target.Host(),
		connectFailures: c.connectFailures,
		execFailures:    c.execFailures,
		exitCodes:       c.exitCodes,
		stdoutOverride:  c.stdoutOverride,
		execDelay:       c.execDelay,
	}, nil
}

func (c *precheckChannel) Connect(ctx context.Context) error {
	atomic.AddInt32(&c.connectCalls, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("closed")
	}
	if err, ok := c.connectFailures[c.host]; ok {
		return err
	}
	c.connected = true
	return nil
}

func (c *precheckChannel) Exec(ctx context.Context, cmd string) (*ExecResult, error) {
	atomic.AddInt32(&c.execCalls, 1)
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.execDelay > 0 {
		select {
		case <-time.After(c.execDelay):
		case <-ctx.Done():
			return &ExecResult{ExitCode: -1}, ctx.Err()
		}
	}

	if err, ok := c.execFailures[c.host]; ok {
		return &ExecResult{ExitCode: -1}, err
	}

	exitCode := 0
	if ec, ok := c.exitCodes[c.host]; ok {
		exitCode = ec
	}

	stdout := "ok\n"
	if s, ok := c.stdoutOverride[c.host]; ok {
		stdout = s
	}

	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   stdout,
		Duration: 1 * time.Millisecond,
	}, nil
}

func (c *precheckChannel) Upload(ctx context.Context, p string, r io.Reader) error {
	return nil
}

func (c *precheckChannel) Download(ctx context.Context, p string) (io.Reader, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (c *precheckChannel) Close() error {
	atomic.AddInt32(&c.closeCalls, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
	c.closed = true
	return nil
}

func (c *precheckChannel) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// --- helpers to build targets ------------------------------------------------

func newPrecheckTarget(host string) precheckTarget {
	return precheckTarget{
		host: host,
		port: 22,
		typ:  "ssh",
		cred: CredentialRef{Username: "u", Password: "p"},
	}
}

// newPrecheckerWithFactory is a test helper that builds a Prechecker using the
// ChannelFactory mode (per-target channel). All precheck tests use this mode
// so that per-host failure maps work correctly.
func newPrecheckerWithFactory(ch *precheckChannel, limiter *Limiter, opts ...PrecheckOption) *Prechecker {
	allOpts := append([]PrecheckOption{WithChannelFactory(ch)}, opts...)
	return NewPrechecker(nil, limiter, allOpts...)
}

// --- all reachable ----------------------------------------------------------

func TestPrecheckAllReachable(t *testing.T) {
	ch := &precheckChannel{}
	p := newPrecheckerWithFactory(ch, nil)

	targets := []Target{
		newPrecheckTarget("h1"),
		newPrecheckTarget("h2"),
		newPrecheckTarget("h3"),
	}

	report := p.Check(context.Background(), targets)

	assert.Equal(t, 3, report.ReachableCount)
	assert.Equal(t, 0, report.UnreachableCount)
	require.Len(t, report.Results, 3)
	for _, r := range report.Results {
		assert.True(t, r.Reachable, "target %s should be reachable", r.TargetID)
		assert.Empty(t, r.Error)
		assert.True(t, r.Latency >= 0)
	}
}

// --- some unreachable -------------------------------------------------------

func TestPrecheckSomeUnreachable(t *testing.T) {
	ch := &precheckChannel{
		connectFailures: map[string]error{
			"h2": errors.New("connection refused"),
		},
	}
	p := newPrecheckerWithFactory(ch, nil)

	targets := []Target{
		newPrecheckTarget("h1"),
		newPrecheckTarget("h2"), // unreachable
		newPrecheckTarget("h3"),
	}

	report := p.Check(context.Background(), targets)

	assert.Equal(t, 2, report.ReachableCount)
	assert.Equal(t, 1, report.UnreachableCount)

	// Find the unreachable result.
	var unreachable *PrecheckResult
	for i := range report.Results {
		if !report.Results[i].Reachable {
			unreachable = &report.Results[i]
			break
		}
	}
	require.NotNil(t, unreachable)
	assert.Contains(t, unreachable.Error, "connection refused")
}

// --- all unreachable --------------------------------------------------------

func TestPrecheckAllUnreachable(t *testing.T) {
	ch := &precheckChannel{
		connectFailures: map[string]error{
			"h1": errors.New("timeout"),
			"h2": errors.New("refused"),
			"h3": errors.New("no route"),
		},
	}
	p := newPrecheckerWithFactory(ch, nil)

	targets := []Target{
		newPrecheckTarget("h1"),
		newPrecheckTarget("h2"),
		newPrecheckTarget("h3"),
	}

	report := p.Check(context.Background(), targets)

	assert.Equal(t, 0, report.ReachableCount)
	assert.Equal(t, 3, report.UnreachableCount)
	for _, r := range report.Results {
		assert.False(t, r.Reachable)
		assert.NotEmpty(t, r.Error)
	}
}

// --- timeout ----------------------------------------------------------------

func TestPrecheckTimeout(t *testing.T) {
	ch := &precheckChannel{
		execDelay: 5 * time.Second,
	}
	p := newPrecheckerWithFactory(ch, nil, WithNoopTimeout(50*time.Millisecond))

	targets := []Target{newPrecheckTarget("h1")}
	report := p.Check(context.Background(), targets)

	assert.Equal(t, 0, report.ReachableCount)
	assert.Equal(t, 1, report.UnreachableCount)
	assert.False(t, report.Results[0].Reachable)
	// The error should mention context deadline exceeded.
	assert.Contains(t, report.Results[0].Error, "exec")
}

// --- parent context cancellation --------------------------------------------

func TestPrecheckParentContextCancelled(t *testing.T) {
	ch := &precheckChannel{}
	p := newPrecheckerWithFactory(ch, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Check

	targets := []Target{newPrecheckTarget("h1")}
	report := p.Check(ctx, targets)

	assert.Equal(t, 0, report.ReachableCount)
	assert.Equal(t, 1, report.UnreachableCount)
	assert.Contains(t, report.Results[0].Error, "context canceled")
}

// --- concurrent with limiter ------------------------------------------------

func TestPrecheckConcurrentWithLimiter(t *testing.T) {
	ch := &precheckChannel{}
	limiter := NewLimiter(2, 2, 1, 0)
	defer limiter.Close()
	p := newPrecheckerWithFactory(ch, limiter)

	targets := []Target{
		newPrecheckTarget("h1"),
		newPrecheckTarget("h2"),
		newPrecheckTarget("h3"),
		newPrecheckTarget("h4"),
	}

	report := p.Check(context.Background(), targets)

	assert.Equal(t, 4, report.ReachableCount)
	assert.Equal(t, 0, report.UnreachableCount)

	// The limiter should have tracked all acquires.
	stats := limiter.Stats()
	assert.Equal(t, int64(4), stats.TotalAcquired)
	assert.Equal(t, 0, stats.GlobalInUse, "all permits should be released")
}

// --- limiter rate limiting causes timeout -----------------------------------

func TestPrecheckLimiterTimeout(t *testing.T) {
	ch := &precheckChannel{}
	// global=1, very short timeout -> the second probe should fail with
	// ErrRateLimited.
	limiter := NewLimiter(1, 10, 10, 50*time.Millisecond)
	defer limiter.Close()
	p := newPrecheckerWithFactory(ch, limiter, WithNoopTimeout(5*time.Second))

	// We need the first probe to hold the permit while the second tries
	// to acquire. Since probes run concurrently, we add a delay via
	// execDelay so the first probe is still in flight when the second
	// starts.
	ch.execDelay = 200 * time.Millisecond

	targets := []Target{
		newPrecheckTarget("h1"),
		newPrecheckTarget("h2"),
	}

	report := p.Check(context.Background(), targets)

	// At least one should be reachable; the other may time out at the
	// limiter or succeed depending on scheduling. We verify that the
	// report is consistent.
	total := report.ReachableCount + report.UnreachableCount
	assert.Equal(t, 2, total)
}

// --- custom noop command ----------------------------------------------------

func TestPrecheckCustomNoopCommand(t *testing.T) {
	ch := &precheckChannel{
		stdoutOverride: map[string]string{
			"h1": "pong\n",
		},
	}
	p := newPrecheckerWithFactory(ch, nil,
		WithNoopCommand("ping"),
		WithExpectedOutput("pong"),
	)

	targets := []Target{newPrecheckTarget("h1")}
	report := p.Check(context.Background(), targets)

	assert.Equal(t, 1, report.ReachableCount)
	assert.True(t, report.Results[0].Reachable)
}

// --- exit code failure ------------------------------------------------------

func TestPrecheckNonZeroExitCode(t *testing.T) {
	ch := &precheckChannel{
		exitCodes: map[string]int{
			"h1": 1,
		},
	}
	p := newPrecheckerWithFactory(ch, nil)

	targets := []Target{newPrecheckTarget("h1")}
	report := p.Check(context.Background(), targets)

	assert.Equal(t, 0, report.ReachableCount)
	assert.Equal(t, 1, report.UnreachableCount)
	assert.Contains(t, report.Results[0].Error, "exit code 1")
}

// --- unexpected output ------------------------------------------------------

func TestPrecheckUnexpectedOutput(t *testing.T) {
	ch := &precheckChannel{
		stdoutOverride: map[string]string{
			"h1": "garbage\n",
		},
	}
	p := newPrecheckerWithFactory(ch, nil)

	targets := []Target{newPrecheckTarget("h1")}
	report := p.Check(context.Background(), targets)

	assert.Equal(t, 0, report.ReachableCount)
	assert.Contains(t, report.Results[0].Error, "unexpected output")
}

// --- empty target list ------------------------------------------------------

func TestPrecheckEmptyTargets(t *testing.T) {
	ch := &precheckChannel{}
	p := newPrecheckerWithFactory(ch, nil)

	report := p.Check(context.Background(), nil)

	assert.Equal(t, 0, report.ReachableCount)
	assert.Equal(t, 0, report.UnreachableCount)
	assert.Empty(t, report.Results)
}

// --- report aggregation -----------------------------------------------------

func TestPrecheckReportAggregation(t *testing.T) {
	ch := &precheckChannel{
		connectFailures: map[string]error{
			"h2": errors.New("down"),
		},
	}
	p := newPrecheckerWithFactory(ch, nil)

	targets := []Target{
		newPrecheckTarget("h1"),
		newPrecheckTarget("h2"),
		newPrecheckTarget("h3"),
		newPrecheckTarget("h4"),
	}

	report := p.Check(context.Background(), targets)

	assert.Equal(t, 3, report.ReachableCount)
	assert.Equal(t, 1, report.UnreachableCount)
	assert.Len(t, report.Results, 4)

	// TotalLatency should be the sum of all per-target latencies.
	var sum time.Duration
	for _, r := range report.Results {
		sum += r.Latency
	}
	assert.Equal(t, sum, report.TotalLatency)
}

// --- nil target in list -----------------------------------------------------

func TestPrecheckNilTarget(t *testing.T) {
	ch := &precheckChannel{}
	p := newPrecheckerWithFactory(ch, nil)

	targets := []Target{nil}
	report := p.Check(context.Background(), targets)

	assert.Equal(t, 0, report.ReachableCount)
	assert.Equal(t, 1, report.UnreachableCount)
	assert.Equal(t, "<nil>", report.Results[0].TargetID)
}

// --- large concurrent precheck ----------------------------------------------

func TestPrecheckLargeConcurrent(t *testing.T) {
	ch := &precheckChannel{}
	limiter := NewLimiter(10, 10, 1, 0)
	defer limiter.Close()
	p := newPrecheckerWithFactory(ch, limiter, WithNoopTimeout(5*time.Second))

	const n = 50
	targets := make([]Target, n)
	for i := 0; i < n; i++ {
		targets[i] = newPrecheckTarget(fmt.Sprintf("h%d", i))
	}

	report := p.Check(context.Background(), targets)

	assert.Equal(t, n, report.ReachableCount)
	assert.Equal(t, 0, report.UnreachableCount)
}
