// Package verify implements LEVEE's verification gate framework.
//
// This file implements HumanGate, a blocking human-approval checkpoint. At
// its phase the gate calls the configured HumanApprover (chat ops, ticketing
// system, CLI prompt — any transport implementing the one-method interface)
// and passes only when a human explicitly approves within the timeout.
//
// LIMITATIONS (deliberate, documented):
//
//   - The MVP HumanApprover abstraction is single-approver: it cannot express
//     quorum / min_approvers semantics. Multi-party approval lives in
//     internal/approval (workflow-level approval:) and is out of scope here.
//   - Check BLOCKS until the approver answers or the derived deadline fires.
//     Callers should therefore run human gates in phases whose context has a
//     sensible overall deadline (the GateManager runs gates concurrently, but
//     a slow approver delays the whole RunPhase return).
//   - A missing approver is a configuration error surfaced fail-closed: the
//     gate reports Passed=false rather than auto-passing.
//
// Configuration errors (unknown params, bad types) are detected at
// construction time and surfaced by Check (fail-closed), mirroring
// CommandGate.policyErr.
package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultHumanTimeout is how long a HumanGate waits for an approval when the
// "timeout_seconds" param is not supplied. Thirty minutes balances operator
// responsiveness against pipeline stall risk.
const DefaultHumanTimeout = 1800 * time.Second

// validHumanParamKeys lists every accepted HumanGate parameter, sorted, for
// inclusion in validation error messages. Keep in sync with applyParams.
var validHumanParamKeys = []string{
	"reason",
	"timeout_seconds",
}

// HumanApprover is the transport abstraction behind HumanGate. Implementations
// block until a human decision is available, the context expires, or an
// infrastructure error occurs. They must respect ctx cancellation so that an
// aborted run does not leak a pending request.
type HumanApprover interface {
	// RequestAndWait asks a human to approve the subject for the given run,
	// presenting reason as the justification, and blocks until a decision.
	// It returns true when approved. A nil error with false means the human
	// explicitly rejected; a non-nil error means the request could not be
	// completed (transport failure, context cancelled mid-wait, ...).
	RequestAndWait(ctx context.Context, runID, subject, reason string) (bool, error)
}

// HumanGate pauses the verification pipeline at its phase until a human
// approves the run. It is safe for concurrent use: all mutable state is
// confined to a single Check call.
type HumanGate struct {
	name     string
	phase    GatePhase
	approver HumanApprover
	timeout  time.Duration
	reason   string

	// paramsErr holds the first configuration violation found while applying
	// the params map. It is set at construction time and surfaced by Check
	// (fail-closed) so that a misconfigured approval can never report a pass.
	paramsErr error
}

// NewHumanGate returns a HumanGate with the given name, phase (parsed from
// its LEVEELang string form), approver transport and parameter map.
//
// Supported params (strict; unknown keys are rejected fail-closed via Check):
//
//	timeout_seconds int, default 1800 — approval wait budget;
//	reason          string, default "" — presented to the approver.
//
// Construction never fails; violations are stored and reported by Check as
// Passed=false plus an error.
func NewHumanGate(name, phase string, approver HumanApprover, params map[string]any) *HumanGate {
	g := &HumanGate{
		name:     name,
		phase:    GatePhase(phase),
		approver: approver,
		timeout:  DefaultHumanTimeout,
	}
	g.paramsErr = g.applyParams(params)
	return g
}

// applyParams validates and applies the params map strictly.
func (g *HumanGate) applyParams(params map[string]any) error {
	for k, v := range params {
		var err error
		switch k {
		case "timeout_seconds":
			var n int
			n, err = paramInt(v, k) //nolint:gosec // paramInt validates integer shape
			if err == nil && n <= 0 {
				err = fmt.Errorf("human param %q must be > 0, got %d", k, n)
			}
			if err == nil {
				g.timeout = time.Duration(n) * time.Second
			}
		case "reason":
			g.reason, err = paramString(v, k)
		default:
			return fmt.Errorf("human param %q is not supported (valid keys: %s)", k, strings.Join(validHumanParamKeys, ", "))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Name returns the gate's unique identifier.
func (g *HumanGate) Name() string { return g.name }

// Phase returns the phase at which this gate runs.
func (g *HumanGate) Phase() GatePhase { return g.phase }

// ParamsError returns the configuration violation detected at construction
// time, or nil when the params map was valid.
func (g *HumanGate) ParamsError() error { return g.paramsErr }

// Check requests a human decision and blocks until it arrives, the derived
// deadline expires, or the caller's ctx is cancelled.
//
// A nil error with Passed == true means approved; a nil error with
// Passed == false means rejected, timed out or cancelled; a non-nil error
// means the gate could not obtain a decision at all (misconfiguration,
// missing approver, approver transport failure).
func (g *HumanGate) Check(ctx context.Context, input GateInput) (GateResult, error) {
	// Honour an already-cancelled context up front, mirroring CommandGate.
	if err := ctx.Err(); err != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("human gate %q cancelled before run: %v", g.name, err),
			Details: map[string]any{
				"gate":   "human",
				"name":   g.name,
				"reason": "context_cancelled",
				"cause":  err.Error(),
			},
		}, nil
	}

	// Fail closed on configuration violations before contacting anyone.
	if g.paramsErr != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("human gate %q rejected invalid params: %v", g.name, g.paramsErr),
			Details: map[string]any{
				"gate":   "human",
				"name":   g.name,
				"reason": "invalid_params",
				"cause":  g.paramsErr.Error(),
			},
		}, fmt.Errorf("human gate %q: invalid params: %w", g.name, g.paramsErr)
	}

	// A missing approver is a wiring mistake: fail honestly instead of
	// fabricating an approval.
	if g.approver == nil {
		err := fmt.Errorf("human gate %q: no approver configured", g.name)
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("human gate %q has no approver configured", g.name),
			Details: map[string]any{
				"gate":   "human",
				"name":   g.name,
				"reason": "missing_approver",
			},
		}, err
	}

	// Derived context honouring parent cancellation plus the gate's own wait
	// budget. Always cancel to release the timer regardless of which fires.
	dctx := ctx
	var cancel context.CancelFunc = func() {}
	if g.timeout > 0 {
		dctx, cancel = context.WithTimeout(ctx, g.timeout)
	}
	defer cancel()

	start := time.Now()
	details := map[string]any{
		"gate":            "human",
		"name":            g.name,
		"run_id":          input.RunID,
		"subject":         g.name,
		"reason":          g.reason,
		"timeout_seconds": int(g.timeout.Seconds()),
	}

	approved, err := g.approver.RequestAndWait(dctx, input.RunID, g.name, g.reason)
	details["latency"] = time.Since(start).String()

	switch {
	case err == nil && approved:
		details["decision"] = "approved"
		return GateResult{
			Passed:  true,
			Message: fmt.Sprintf("human gate %q passed: approved", g.name),
			Details: details,
		}, nil

	case err == nil:
		// Explicit rejection: a real human said no.
		details["decision"] = "rejected"
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("human gate %q failed: rejected by approver", g.name),
			Details: details,
		}, nil

	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		// Timeout or cancellation: neither approved nor rejected. Report a
		// clear failure without escalating to a terminal error, mirroring
		// how CommandGate treats a cancelled context.
		cause := "timed out waiting for approval"
		if errors.Is(err, context.Canceled) {
			cause = "cancelled while waiting for approval"
		}
		details["decision"] = "no_decision"
		details["cause"] = cause
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("human gate %q failed: %s after %s", g.name, cause, time.Since(start).Truncate(time.Millisecond)),
			Details: details,
		}, nil

	default:
		// Approver transport failure: surface as a terminal error; the
		// manager treats it as a failed gate either way, but the error keeps
		// the distinction visible in the audit trail.
		details["decision"] = "error"
		details["cause"] = err.Error()
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("human gate %q failed: approver error: %v", g.name, err),
			Details: details,
		}, err
	}
}
