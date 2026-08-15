// Package channel precheck implementation.
//
// The Prechecker probes target reachability before the apply phase by running
// a harmless noop command (e.g. "echo ok") through the Channel.Exec interface.
// It runs probes concurrently, bounded by a Limiter so that the precheck itself
// does not overwhelm the targets.
//
// Unreachable targets are reported as PrecheckResult{Reachable: false} rather
// than causing Check to return an error; the upper layers decide whether to
// exclude them from the batch.
package channel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// DefaultNoopCommand is the harmless command executed by the Prechecker to
// verify reachability. "echo ok" is chosen because:
//   - it exists on every POSIX shell and Windows cmd.exe;
//   - it produces no side effects;
//   - its output ("ok") is a simple reachability signal.
const DefaultNoopCommand = "echo ok"

// DefaultNoopTimeout is the per-target probe timeout when Prechecker.Timeout
// is zero. It is intentionally short: precheck should fail fast on unreachable
// hosts rather than waiting for the full TCP timeout.
const DefaultNoopTimeout = 10 * time.Second

// PrecheckResult is the outcome of probing a single target.
type PrecheckResult struct {
	// TargetID is the identifier of the probed target (host:port or
	// whatever the caller used as the target's unique key).
	TargetID string

	// Reachable is true when the noop command completed with exit code 0
	// and the expected output.
	Reachable bool

	// Latency is the wall-clock duration of the probe (Connect + Exec),
	// measured from the start of the probe to its completion. For
	// unreachable targets it is the time until the probe failed or timed
	// out.
	Latency time.Duration

	// Error is a human-readable description of the failure when
	// Reachable is false. It is empty for reachable targets.
	Error string
}

// PrecheckReport aggregates the results of probing a set of targets.
type PrecheckReport struct {
	// Results is the per-target outcome, in the same order as the input
	// targets slice.
	Results []PrecheckResult

	// ReachableCount is the number of targets that were successfully probed.
	ReachableCount int

	// UnreachableCount is the number of targets that failed the probe.
	UnreachableCount int

	// TotalLatency is the sum of all per-target latencies. It is an
	// upper bound on the wall-clock duration of Check when probes run
	// concurrently (the actual wall-clock duration is at most
	// max(Latency) + scheduling overhead).
	TotalLatency time.Duration
}

// Prechecker probes target reachability by running a noop command through a
// Channel. It uses a Limiter to bound concurrency during the probe.
//
// A Prechecker can operate in two modes:
//   - Single-channel mode (default): one Channel is reused for all targets.
//     Suitable when the Channel is a multiplexing transport (e.g. SSH with
//     ControlMaster) or when all targets share the same endpoint.
//   - Factory mode (via WithChannelFactory): a ChannelFactory creates a fresh
//     Channel per target. Suitable when each target needs its own connection
//     (the common case for prechecking a batch of distinct hosts).
type Prechecker struct {
	// ch is the Channel used to probe targets in single-channel mode. It
	// is nil when a factory is configured.
	ch Channel

	// factory creates a fresh Channel per target in factory mode. It is
	// nil in single-channel mode.
	factory ChannelFactory

	// limiter bounds the concurrency of concurrent probes. It may be nil
	// to disable rate limiting (not recommended for large target sets).
	limiter *Limiter

	// noopCommand is the command executed to verify reachability.
	noopCommand string

	// timeout is the per-target probe timeout.
	timeout time.Duration

	// expectedOutput is the substring that the noop command's stdout must
	// contain for the target to be considered reachable. For "echo ok" the
	// output is "ok\n"; we check for "ok" to be newline-tolerant.
	expectedOutput string
}

// PrecheckOption configures a Prechecker.
type PrecheckOption func(*Prechecker)

// WithNoopCommand overrides the default noop command.
func WithNoopCommand(cmd string) PrecheckOption {
	return func(p *Prechecker) {
		if cmd != "" {
			p.noopCommand = cmd
		}
	}
}

// WithNoopTimeout overrides the per-target probe timeout.
func WithNoopTimeout(d time.Duration) PrecheckOption {
	return func(p *Prechecker) {
		if d > 0 {
			p.timeout = d
		}
	}
}

// WithExpectedOutput overrides the expected stdout substring. When set to
// empty, the prechecker only checks the exit code.
func WithExpectedOutput(s string) PrecheckOption {
	return func(p *Prechecker) {
		p.expectedOutput = s
	}
}

// WithChannelFactory configures the Prechecker to use a ChannelFactory that
// creates a fresh Channel per target. This is the recommended mode for
// prechecking a batch of distinct hosts, since each target needs its own
// connection. When a factory is configured, the single-channel ch field is
// ignored.
func WithChannelFactory(f ChannelFactory) PrecheckOption {
	return func(p *Prechecker) {
		p.factory = f
	}
}

// NewPrechecker returns a Prechecker that probes targets through ch, bounded
// by limiter. If limiter is nil, no rate limiting is applied.
//
// The Prechecker does not take ownership of ch or limiter; the caller is
// responsible for closing them after Check returns.
//
// To use the per-target factory mode instead of a shared channel, pass nil
// for ch and supply the WithChannelFactory option.
func NewPrechecker(ch Channel, limiter *Limiter, opts ...PrecheckOption) *Prechecker {
	p := &Prechecker{
		ch:             ch,
		limiter:        limiter,
		noopCommand:    DefaultNoopCommand,
		timeout:        DefaultNoopTimeout,
		expectedOutput: "ok",
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// targetID returns a stable identifier for a target, used as the limiter key
// and in the PrecheckResult. It is host:port (or host:0 when port is zero).
func targetID(t Target) string {
	if t == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s:%d", t.Host(), t.Port())
}

// Check probes all targets concurrently and returns a PrecheckReport. Each
// probe is bounded by the Prechecker's timeout and the Limiter's concurrency
// caps. Check itself respects ctx: when ctx is cancelled, in-flight probes are
// allowed to complete but not-yet-started probes are skipped.
//
// Check never returns an error; unreachable targets are reported in the
// PrecheckReport. The only error that stops a probe early is context
// cancellation, which is recorded as an unreachable result.
func (p *Prechecker) Check(ctx context.Context, targets []Target) PrecheckReport {
	results := make([]PrecheckResult, len(targets))

	var wg sync.WaitGroup
	for i, tgt := range targets {
		// Skip if the parent context is already cancelled.
		if ctx.Err() != nil {
			results[i] = PrecheckResult{
				TargetID:  targetID(tgt),
				Reachable: false,
				Error:     ctx.Err().Error(),
			}
			continue
		}

		wg.Add(1)
		go func(idx int, target Target) {
			defer wg.Done()
			results[idx] = p.probeOne(ctx, target)
		}(i, tgt)
	}
	wg.Wait()

	// Aggregate.
	report := PrecheckReport{Results: results}
	for _, r := range results {
		report.TotalLatency += r.Latency
		if r.Reachable {
			report.ReachableCount++
		} else {
			report.UnreachableCount++
		}
	}
	return report
}

// probeOne probes a single target: acquire a limiter permit (if configured),
// connect the channel, run the noop command, and record the result. The
// channel is disconnected after each probe so that the next probe starts fresh
// (this matches the "single-connection-single-command" WinRM model and is
// harmless for SSH which pools underneath).
func (p *Prechecker) probeOne(ctx context.Context, target Target) PrecheckResult {
	id := targetID(target)
	result := PrecheckResult{TargetID: id}

	// Per-target timeout.
	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	start := time.Now()
	defer func() {
		result.Latency = time.Since(start)
	}()

	// Acquire a limiter permit if configured.
	if p.limiter != nil {
		if err := p.limiter.Acquire(target.Type(), id); err != nil {
			result.Reachable = false
			result.Error = fmt.Sprintf("limiter: %s", err)
			return result
		}
		defer p.limiter.Release(target.Type(), id)
	}

	// Obtain a Channel for this probe: in factory mode we create a fresh
	// channel per target; in single-channel mode we reuse the shared channel.
	ch, chOwned := p.acquireChannel(target)
	if ch == nil {
		result.Reachable = false
		result.Error = "no channel available (neither ch nor factory configured)"
		return result
	}
	if chOwned {
		defer func() { _ = ch.Close() }()
	}

	// Connect. We call Connect on the channel; for SSH this is idempotent
	// (reuses the existing connection), for WinRM it builds the client. If
	// Connect fails the target is unreachable.
	if err := ch.Connect(probeCtx); err != nil {
		result.Reachable = false
		result.Error = fmt.Sprintf("connect: %s", err)
		return result
	}

	// Run the noop command.
	res, err := ch.Exec(probeCtx, p.noopCommand)
	if err != nil {
		result.Reachable = false
		result.Error = fmt.Sprintf("exec: %s", err)
		return result
	}

	// Check exit code.
	if res.ExitCode != 0 {
		result.Reachable = false
		result.Error = fmt.Sprintf("exit code %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		return result
	}

	// Check expected output (if configured).
	if p.expectedOutput != "" && !strings.Contains(strings.TrimSpace(res.Stdout), p.expectedOutput) {
		result.Reachable = false
		result.Error = fmt.Sprintf("unexpected output: %q", strings.TrimSpace(res.Stdout))
		return result
	}

	result.Reachable = true
	return result
}

// acquireChannel returns a Channel to use for probing the given target. When
// the Prechecker is configured with a ChannelFactory, a fresh channel is
// created and the caller owns it (chOwned == true, caller must Close). When
// the Prechecker uses a shared single channel, that channel is returned and
// chOwned is false.
func (p *Prechecker) acquireChannel(target Target) (Channel, bool) {
	if p.factory != nil {
		ch, err := p.factory.Create(target)
		if err != nil {
			return nil, false
		}
		return ch, true
	}
	if p.ch != nil {
		return p.ch, false
	}
	return nil, false
}
