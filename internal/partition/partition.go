// Package partition answers one question for a serving LEVEE process: is the
// control-plane database reachable right now?
//
// It exists because of a policy decision, not a mechanism gap. The failover
// mechanisms (design-cluster-failover.md B1–B5) already converge an in-flight
// run to `interrupted` when a node loses the database, and the container-lab
// acceptance run (#96) proved that convergence. What they do not do is stop
// NEW work from entering while the platform cannot prove execution authority:
// a partitioned serve that keeps admitting applies would dispatch them with no
// lease, no epoch and no fence — the physical double-execution face the whole
// design is built to avoid.
//
// The policy (docs/design-disconnect-policy.md §6, Q1 = A 全拒) is therefore:
// refuse new apply and new approval while the database is unreachable, with a
// reason the operator can read, instead of letting the request fail deeper
// inside the call with a generic internal error.
//
// The check is deliberately a fresh probe per guarded request rather than a
// cached flag. A cached flag can go stale in the exact window that matters —
// the database can drop between two pings — and a stale "reachable" is a
// refusal to refuse, which is the failure mode the policy exists to remove.
// The probe is bounded by a timeout, so a hung database costs a bounded wait
// rather than an unbounded one.
package partition

import (
	"context"
	"time"
)

// DefaultTimeout bounds a single probe. It is a liveness question, not a
// throughput one: the database either answers a bare ping quickly or it does
// not, and a ping that has not answered in a few seconds is not going to
// answer in a few more. Callers that need a different budget can pass their
// own; serve installs the default.
const DefaultTimeout = 3 * time.Second

// Pinger is the subset of a store that can answer a liveness probe. Both
// concrete stores satisfy it structurally (PGStore pings its pool,
// SQLiteStore pings its single connection); the interface lives here so this
// package does not depend on internal/state and so a test double needs only
// this one method.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Checker probes a Pinger and reports whether the database answered. A nil
// Checker (or one built with a nil Pinger) is a no-op that always reports
// reachable — the same "no policy configured" default the rest of the codebase
// uses for optional dependencies, so a deployment that never wires a checker
// behaves exactly as before.
type Checker struct {
	pinger  Pinger
	timeout time.Duration
}

// NewChecker builds a Checker around p. A non-positive timeout falls back to
// DefaultTimeout, so a zero-value construction is safe rather than a
// no-deadline probe.
func NewChecker(p Pinger, timeout time.Duration) *Checker {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Checker{pinger: p, timeout: timeout}
}

// Check returns nil when the database answered within the timeout and an error
// describing the failure otherwise. The error is wrapped with the probe's own
// text so an operator reading a refusal sees which stage failed (connection
// refused vs. deadline exceeded) rather than a bare boolean.
func (c *Checker) Check(ctx context.Context) error {
	if c == nil || c.pinger == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.pinger.Ping(probeCtx)
}

// Available is the boolean form of Check, for status surfaces that report a
// marker rather than an error. It performs a probe with the same timeout; a
// nil Checker reports true.
func (c *Checker) Available(ctx context.Context) bool {
	return c.Check(ctx) == nil
}
