// Package verify implements LEVEE's verification gate framework.
//
// This file implements CommandGate (design doc section 4.4.5, MVP task T028).
// A CommandGate runs a shell command on the target host through the channel
// abstraction and verifies that the exit code and (optionally) stdout match
// the expected values. It supports per-gate timeout and retry with a fixed
// delay between attempts. A typical use is a readiness probe such as
// `systemctl is-active nginx` expecting exit 0 and stdout "active".
package verify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/log"
)

// CommandGate default tuning. The defaults are conservative so that a
// misconfigured gate fails fast rather than hanging the pipeline.
const (
	// DefaultCommandTimeout is the default per-attempt timeout for a
	// CommandGate when WithCommandTimeout is not used.
	DefaultCommandTimeout = 30 * time.Second

	// DefaultCommandRetries is the default retry count (additional attempts
	// after the first failure). Zero means a single attempt with no retry.
	DefaultCommandRetries = 0

	// DefaultCommandRetryDelay is the default delay between retry attempts.
	DefaultCommandRetryDelay = 1 * time.Second
)

// Command-policy deviation from the shell module, documented deliberately:
//
// internal/executor/modules/shell validates commands against a strict
// ALLOWLIST (alphanumerics plus -_./= and spaces). Verify gates
// intentionally accept richer commands because readiness probes commonly
// need pipelines ("journalctl -u app | grep -c started"), output
// redirection ("... > /dev/null") and fd duplication ("2>&1"). Running the
// shell whitelist here would make most real-world gates unexpressible.
//
// Instead the gate applies a metacharacter BLACKLIST
// (validateGateCommand): it rejects the primitives that turn a single probe
// into arbitrary code execution —
//
//   - command substitution: "$(...)" and backticks;
//   - environment / parameter expansion: "$";
//   - unconditional backgrounding: "&" (except fd duplication such as 2>&1);
//   - sequential chaining: ";" and embedded newlines (which start a new
//     command line).
//
// Conditional chaining via "&&" is rejected implicitly (it contains "&").
// Unconditional "|" is allowed per the policy above; note that this makes
// the conditional-or "||" syntactically acceptable too — accepted as part
// of the same documented trade-off, since every element of an "||" chain
// still runs under the gate's exit-code expectation.
//
// Gate commands today come from compiled plans authored by operators; the
// blacklist is defence-in-depth for the case where plan content is derived
// from less-trusted input.
func validateGateCommand(cmd string) error {
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("empty command")
	}
	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; c {
		case ';':
			return fmt.Errorf("disallowed character ';' (sequential chaining is forbidden in gate commands)")
		case '&':
			// Allow fd-duplication redirections like 2>&1 / 1>&2; reject
			// every other use (backgrounding / &&).
			if i > 0 && cmd[i-1] == '>' && i+1 < len(cmd) && cmd[i+1] >= '0' && cmd[i+1] <= '9' {
				continue
			}
			return fmt.Errorf("disallowed character '&' (backgrounding and && chaining are forbidden in gate commands; fd duplication like 2>&1 is allowed)")
		case '`':
			return fmt.Errorf("disallowed character '`' (backtick command substitution is forbidden in gate commands)")
		case '$':
			return fmt.Errorf("disallowed character '$' (command substitution and parameter expansion are forbidden in gate commands)")
		case '\n', '\r':
			return fmt.Errorf("disallowed newline (multi-command gate lines are forbidden)")
		}
	}
	return nil
}

// ValidateGateCommand reports whether cmd satisfies the verify-gate command
// policy (see validateGateCommand). It is exported so that plan compilers
// and DSL tooling can reject unsafe gate commands before a workflow runs.
func ValidateGateCommand(cmd string) error { return validateGateCommand(cmd) }

// CommandGateOption configures a CommandGate at construction time. The
// functional-options pattern keeps the constructor signature small while
// allowing callers to override only the fields they care about.
type CommandGateOption func(*CommandGate)

// WithCommandTimeout sets the per-attempt timeout for the command. The
// timeout is applied on top of the caller's context deadline: the attempt
// ends as soon as either fires.
func WithCommandTimeout(d time.Duration) CommandGateOption {
	return func(g *CommandGate) { g.timeout = d }
}

// WithCommandRetries sets the number of additional attempts after the first
// failure. A value of 0 means a single attempt with no retry.
func WithCommandRetries(n int) CommandGateOption {
	return func(g *CommandGate) { g.retries = n }
}

// WithCommandRetryDelay sets the delay between retry attempts. The delay is
// applied only between attempts, not before the first one.
func WithCommandRetryDelay(d time.Duration) CommandGateOption {
	return func(g *CommandGate) { g.retryDelay = d }
}

// WithExpectedExit sets the expected exit code. The default is 0.
func WithExpectedExit(code int) CommandGateOption {
	return func(g *CommandGate) { g.expectExit = code }
}

// WithExpectedStdout sets the expected stdout. An empty string (the default)
// means stdout is not checked. The comparison is exact after trimming the
// trailing newline pair ("\r\n" or "\n") so that callers do not have to
// worry about platform line-ending differences.
func WithExpectedStdout(s string) CommandGateOption {
	return func(g *CommandGate) { g.expectStdout = s }
}

// CommandGate runs a command on the target host through the channel and
// verifies the exit code and (optionally) stdout. It is safe for concurrent
// use: the manager may invoke Check on the same CommandGate from multiple
// goroutines simultaneously. All mutable state is confined to a single Check
// call.
type CommandGate struct {
	name         string
	phase        GatePhase
	command      string
	expectExit   int
	expectStdout string
	timeout      time.Duration
	retries      int
	retryDelay   time.Duration

	// policyErr holds the result of validateGateCommand for g.command. It is
	// set at construction time and surfaced by Check so that an unsafe gate
	// command can never reach a channel.
	policyErr error
}

// NewCommandGate returns a CommandGate with the given name, phase and command.
// The expected exit code defaults to 0 and the expected stdout defaults to
// "" (not checked). Override the defaults with the provided options.
//
// The command is validated against the verify-gate command policy (see
// validateGateCommand); violations do not panic here — they are reported by
// Check (and available via PolicyError) so that construction remains total.
func NewCommandGate(name string, phase GatePhase, cmd string, opts ...CommandGateOption) *CommandGate {
	g := &CommandGate{
		name:         name,
		phase:        phase,
		command:      cmd,
		expectExit:   0,
		expectStdout: "",
		timeout:      DefaultCommandTimeout,
		retries:      DefaultCommandRetries,
		retryDelay:   DefaultCommandRetryDelay,
	}
	for _, opt := range opts {
		opt(g)
	}
	g.policyErr = validateGateCommand(cmd)
	return g
}

// Name returns the gate's unique identifier.
func (g *CommandGate) Name() string { return g.name }

// Phase returns the phase at which this gate runs.
func (g *CommandGate) Phase() GatePhase { return g.phase }

// PolicyError returns the command-policy violation for this gate's command,
// or nil when the command satisfies the policy.
func (g *CommandGate) PolicyError() error { return g.policyErr }

// Check runs the command on the target host through input.Channel and verifies
// the exit code and stdout. It retries up to g.retries times on failure, with
// g.retryDelay between attempts. The per-attempt timeout is g.timeout; the
// caller's ctx deadline is also honoured.
//
// A nil error with Passed == true means the command matched the expectations.
// A nil error with Passed == false means the command ran but did not match
// (e.g. wrong exit code or stdout). A non-nil error means the command could
// not be executed at all (e.g. channel missing, context cancelled before the
// first attempt).
func (g *CommandGate) Check(ctx context.Context, input GateInput) (GateResult, error) {
	// Honour an already-cancelled context up front so that the manager's
	// fast-path skip is reflected without spinning up a goroutine.
	if err := ctx.Err(); err != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("command gate %q cancelled before run: %v", g.name, err),
			Details: map[string]any{
				"gate":   "command",
				"name":   g.name,
				"reason": "context_cancelled",
				"cause":  err.Error(),
			},
		}, nil
	}

	// Enforce the gate command policy before touching the channel: an unsafe
	// command must never execute, not even once.
	if g.policyErr != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("command gate %q rejected unsafe command: %v", g.name, g.policyErr),
			Details: map[string]any{
				"gate":    "command",
				"name":    g.name,
				"command": g.command,
				"reason":  "unsafe_command",
				"cause":   g.policyErr.Error(),
			},
		}, fmt.Errorf("command gate %q: unsafe command: %w", g.name, g.policyErr)
	}

	if input.Channel == nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("command gate %q has no channel", g.name),
			Details: map[string]any{
				"gate":   "command",
				"name":   g.name,
				"reason": "missing_channel",
			},
		}, fmt.Errorf("command gate %q: channel is nil", g.name)
	}

	// Retry loop. We keep the last attempt's evidence so that the result
	// carries the most recent stdout / exit code for the audit trail.
	var lastResult GateResult
	for attempt := 0; attempt <= g.retries; attempt++ {
		// Stop retrying if the caller's context has expired.
		if err := ctx.Err(); err != nil {
			lastResult = GateResult{
				Passed:  false,
				Message: fmt.Sprintf("command gate %q cancelled on attempt %d: %v", g.name, attempt+1, err),
				Details: map[string]any{
					"gate":    "command",
					"name":    g.name,
					"attempt": attempt + 1,
					"reason":  "context_cancelled",
					"cause":   err.Error(),
				},
			}
			return lastResult, nil
		}

		result, err := g.runOnce(ctx, input.Channel, attempt+1)
		if err != nil {
			// An error from runOnce means the channel itself failed
			// (e.g. context cancelled mid-exec). We treat this as a
			// failed attempt and retry, unless the context is done in
			// which case we stop.
			lastResult = result
			if ctx.Err() != nil {
				return lastResult, nil
			}
			log.Warn("command gate attempt failed",
				"gate", g.name,
				"attempt", attempt+1,
				"err", err)
		} else if result.Passed {
			// Success: return immediately, no more retries.
			return result, nil
		} else {
			// Mismatch (exit code or stdout): record and retry.
			lastResult = result
			log.Debug("command gate attempt mismatch",
				"gate", g.name,
				"attempt", attempt+1,
				"message", result.Message)
		}

		// Sleep before the next retry, but stop early if ctx expires.
		if attempt < g.retries {
			if !sleepCtx(ctx, g.retryDelay) {
				lastResult = GateResult{
					Passed:  false,
					Message: fmt.Sprintf("command gate %q cancelled during retry delay", g.name),
					Details: map[string]any{
						"gate":    "command",
						"name":    g.name,
						"attempt": attempt + 1,
						"reason":  "context_cancelled",
					},
				}
				return lastResult, nil
			}
		}
	}

	return lastResult, nil
}

// runOnce executes the command a single time and compares the result against
// the expectations. It applies the per-attempt timeout on top of the caller's
// context. The attempt number is 1-indexed and used only for log correlation.
func (g *CommandGate) runOnce(ctx context.Context, ch channel.Channel, attempt int) (GateResult, error) {
	// Derive a per-attempt context that expires at the earlier of the
	// caller's deadline and g.timeout. We always cancel it to release the
	// timer regardless of which deadline fired.
	attemptCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	start := time.Now()
	res, err := ch.Exec(attemptCtx, g.command)
	latency := time.Since(start)

	if err != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("command gate %q exec failed on attempt %d: %v", g.name, attempt, err),
			Details: map[string]any{
				"gate":    "command",
				"name":    g.name,
				"attempt": attempt,
				"command": g.command,
				"reason":  "exec_error",
				"cause":   err.Error(),
				"latency": latency.String(),
			},
		}, err
	}

	details := map[string]any{
		"gate":      "command",
		"name":      g.name,
		"attempt":   attempt,
		"command":   g.command,
		"exit_code": res.ExitCode,
		"stdout":    res.Stdout,
		"stderr":    res.Stderr,
		"latency":   latency.String(),
	}

	// Compare exit code.
	if res.ExitCode != g.expectExit {
		details["reason"] = "exit_code_mismatch"
		details["expected_exit"] = g.expectExit
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("command gate %q exit code mismatch: got %d, want %d", g.name, res.ExitCode, g.expectExit),
			Details: details,
		}, nil
	}

	// Compare stdout when configured. We trim a single trailing newline
	// pair so that callers can match "active" against "active\n".
	if g.expectStdout != "" {
		got := trimTrailingNewline(res.Stdout)
		want := trimTrailingNewline(g.expectStdout)
		if got != want {
			details["reason"] = "stdout_mismatch"
			details["expected_stdout"] = g.expectStdout
			return GateResult{
				Passed:  false,
				Message: fmt.Sprintf("command gate %q stdout mismatch: got %q, want %q", g.name, got, want),
				Details: details,
			}, nil
		}
	}

	details["reason"] = "match"
	return GateResult{
		Passed:  true,
		Message: fmt.Sprintf("command gate %q passed on attempt %d (exit=%d)", g.name, attempt, res.ExitCode),
		Details: details,
	}, nil
}

// trimTrailingNewline strips a single trailing "\r\n" or "\n" from s. This
// lets callers match "active" against the shell output "active\n" without
// having to embed the newline in their expectation.
func trimTrailingNewline(s string) string {
	s = strings.TrimSuffix(s, "\r\n")
	s = strings.TrimSuffix(s, "\n")
	return s
}

// sleepCtx sleeps for d, but returns false as soon as ctx is cancelled. It
// does not allocate a goroutine when d <= 0.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
