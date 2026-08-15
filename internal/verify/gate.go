// Package verify implements LEVEE's verification gate framework (design doc
// section 4.4.5, MVP task T027). A gate is a check inserted at one of three
// critical moments in the change pipeline:
//
//   - PhasePreApply  — before apply starts (e.g. target reachability, SLO baseline)
//   - PhasePostBatch — after each batch completes (e.g. service health, SLO probe)
//   - PhasePostApply — after apply finishes (e.g. final regression, full SLO window)
//
// The GateManager registers gates by name, groups them by phase, and runs all
// gates of a phase concurrently. RunPhase returns as soon as any gate fails
// (Passed == false); gates that have not yet returned are marked as skipped in
// the result slice but their goroutines are left to complete on their own so
// that long-running probes do not leak. The caller controls the overall
// deadline through the passed-in context.
//
// The framework is deliberately transport-agnostic: a Gate receives a
// channel.Channel through GateInput and drives the target through it, keeping
// the manager free of any SSH / WinRM / API specifics.
package verify

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nexus/levee/internal/channel"
)

// --- Phase ------------------------------------------------------------------

// GatePhase identifies one of the three verification moments in the change
// pipeline. The string values are stable identifiers used in logs, the audit
// trail and LEVEELang source ("pre_apply", "post_batch", "post_apply").
type GatePhase string

const (
	// PhasePreApply runs before apply begins. Typical gates: target
	// reachability, prerequisite state, SLO baseline confirmation. A failure
	// here aborts the run before any change is made.
	PhasePreApply GatePhase = "pre_apply"

	// PhasePostBatch runs after each batch completes. Typical gates: service
	// health, SLO probe, readiness check. A failure here blocks the next batch
	// and triggers rollback.
	PhasePostBatch GatePhase = "post_batch"

	// PhasePostApply runs after apply finishes. Typical gates: final
	// regression, full SLO window, end-to-end smoke. A failure here triggers
	// rollback of the whole run.
	PhasePostApply GatePhase = "post_apply"
)

// AllPhases returns the three gate phases in canonical order. It is intended
// for iteration in diagnostics and tests.
func AllPhases() []GatePhase {
	return []GatePhase{PhasePreApply, PhasePostBatch, PhasePostApply}
}

// --- Gate interface ---------------------------------------------------------

// Gate is a single verification check. Implementations must be safe for
// concurrent use: the manager may invoke Check on the same Gate from multiple
// goroutines simultaneously (one per phase run). Implementations must respect
// ctx for cancellation and timeouts.
type Gate interface {
	// Name returns the gate's unique identifier, e.g. "slo-error-rate",
	// "target-reachable". The manager uses Name() as the registry key.
	Name() string

	// Phase returns the phase at which this gate runs. A gate is bound to
	// exactly one phase; the same check at two phases must be registered
	// twice under different names.
	Phase() GatePhase

	// Check runs the verification and returns its result. A nil error with
	// Passed == true means the gate passed; a nil error with Passed == false
	// means the gate ran successfully and the result is a failure; a non-nil
	// error means the gate could not run at all (e.g. channel broken) and is
	// treated as a failure by the manager.
	Check(ctx context.Context, input GateInput) (GateResult, error)
}

// --- Input / Output ---------------------------------------------------------

// GateInput is the structured payload passed to Gate.Check. It carries
// everything a gate needs to evaluate the run state at its phase.
type GateInput struct {
	// RunID is the unique identifier of the change run this gate belongs to.
	RunID string `json:"run_id"`

	// BatchID identifies the batch that just completed. It is populated only
	// for PhasePostBatch gates; PreApply and PostApply gates leave it empty.
	BatchID string `json:"batch_id,omitempty"`

	// TargetIDs is the list of target host identifiers the gate should
	// evaluate. For PreApply it is the full target set; for PostBatch it is
	// the batch's targets; for PostApply it is the full target set again.
	TargetIDs []string `json:"target_ids"`

	// Channel is an optional live channel to a target. Gates that need to
	// execute remote commands (e.g. command gate, probe gate) use it; gates
	// that only query external systems (e.g. SLO gate) may leave it nil.
	Channel channel.Channel `json:"-"`

	// Params is the gate-specific parameter map. The keys and value types are
	// defined by each gate's documentation; the manager does not interpret
	// them. Typical examples:
	//   command gate: {"cmd": "systemctl is-active nginx", "expect_exit": 0}
	//   slo gate:     {"query": "rate(errors[5m])", "threshold": 0.01}
	Params map[string]any `json:"params,omitempty"`
}

// GateResult is the outcome of a single Gate.Check call. It is always
// populated by the manager, even when the gate is skipped.
type GateResult struct {
	// Passed is true when the gate succeeded. A false value triggers the
	// phase's failure action (block / rollback / escalate).
	Passed bool `json:"passed"`

	// Message is a human-readable description of the outcome. For a passing
	// gate it may be a brief confirmation; for a failing gate it should
	// explain why; for a skipped gate it carries the skip reason.
	Message string `json:"message"`

	// Details carries optional structured information about the check, such
	// as metric values, command output or probe response. It is preserved in
	// the audit trail for post-hoc analysis. The manager does not interpret
	// the keys.
	Details map[string]any `json:"details,omitempty"`

	// Latency is the wall-clock time spent inside Check, measured by the
	// manager wrapper. It includes any channel round-trip but not registry
	// lookup overhead. If the gate populates Latency itself the manager
	// preserves it; otherwise the manager fills it in.
	Latency time.Duration `json:"latency_ms"`
}

// --- GateManager ------------------------------------------------------------

// GateManager is the gate registry and phase runner. The zero value is not
// usable; callers must use NewGateManager.
type GateManager struct {
	mu    sync.RWMutex
	gates map[string]Gate
}

// NewGateManager returns an empty GateManager ready to register gates.
func NewGateManager() *GateManager {
	return &GateManager{gates: make(map[string]Gate)}
}

// Register adds gate to the registry under gate.Name(). Registering a second
// gate with the same name overwrites the previous registration; this is
// intentional so that tests can swap gates, but production code should
// register each name exactly once. Register is safe for concurrent use.
func (gm *GateManager) Register(gate Gate) {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	gm.gates[gate.Name()] = gate
}

// Unregister removes the gate registered under name, if any. It is primarily
// useful in tests to guarantee a clean slate between cases. Unregister is
// safe for concurrent use.
func (gm *GateManager) Unregister(name string) {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	delete(gm.gates, name)
}

// Gate returns the gate registered under name, or false when no gate is
// registered. It is the read-side counterpart to Register.
func (gm *GateManager) Gate(name string) (Gate, bool) {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	g, ok := gm.gates[name]
	return g, ok
}

// Gates returns the gates registered for the given phase in stable order
// (sorted by name). It is the read-side counterpart to Register for a single
// phase. The returned slice is a copy and may be safely modified by the
// caller.
func (gm *GateManager) Gates(phase GatePhase) []Gate {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	out := make([]Gate, 0)
	for _, g := range gm.gates {
		if g.Phase() == phase {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Names returns the registered gate names in sorted order. It is intended for
// diagnostics (e.g. `levee version --verbose` listing available gates).
func (gm *GateManager) Names() []string {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	out := make([]string, 0, len(gm.gates))
	for k := range gm.gates {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- RunPhase --------------------------------------------------------------

// indexedResult pairs a gate result with its position in the phase's sorted
// gate list so that RunPhase can place results in the correct slot regardless
// of completion order.
type indexedResult struct {
	idx    int
	result GateResult
	err    error
}

// RunPhase executes all gates registered for phase concurrently and returns
// the aggregated results in the same order as Gates(phase).
//
// Behaviour:
//   - All gates of the phase are launched in their own goroutine.
//   - As soon as any gate fails (Passed == false or Check returns an error),
//     the remaining in-flight gates are marked as skipped and RunPhase
//     returns. The in-flight goroutines are not cancelled; they continue to
//     run to completion so that long-running probes do not leak, but their
//     results are discarded.
//   - If ctx is cancelled or reaches its deadline before all gates complete,
//     the uncompleted gates are marked as skipped with the context error.
//   - The returned slice always has length len(Gates(phase)); on an empty
//     phase it is nil.
//   - Each GateResult.Latency is populated with the measured wall-clock time
//     when the gate itself does not supply it.
func (gm *GateManager) RunPhase(ctx context.Context, phase GatePhase, input GateInput) []GateResult {
	gates := gm.Gates(phase)
	n := len(gates)
	if n == 0 {
		return nil
	}

	results := make([]GateResult, n)
	done := make(chan indexedResult, n)

	// Launch all gates concurrently. Each goroutine respects the caller's ctx
	// so that a deadline is propagated to the gate implementation.
	for i, g := range gates {
		i, g := i, g
		go func() {
			start := time.Now()
			r, err := g.Check(ctx, input)
			latency := time.Since(start)
			// Preserve a gate-supplied latency when it is non-zero; otherwise
			// fill in the measured value.
			if r.Latency == 0 {
				r.Latency = latency
			}
			done <- indexedResult{idx: i, result: r, err: err}
		}()
	}

	completed := make([]bool, n)
	completedCount := 0
	failed := false

	for completedCount < n {
		select {
		case <-ctx.Done():
			// Context expired: mark every uncompleted gate as skipped and
			// return immediately. In-flight goroutines continue running but
			// their results are discarded (the buffered channel absorbs them).
			for i := 0; i < n; i++ {
				if !completed[i] {
					results[i] = skippedResult(ctx.Err())
					completed[i] = true
					completedCount++
				}
			}
			return results
		case r := <-done:
			if completed[r.idx] {
				// Should not happen: a gate reported twice. Ignore the
				// duplicate to keep the result slice consistent.
				continue
			}
			results[r.idx] = r.result
			completed[r.idx] = true
			completedCount++

			// A non-nil error from Check is treated as a gate failure with
			// the error message surfaced to the caller.
			if r.err != nil {
				results[r.idx].Passed = false
				if results[r.idx].Message == "" {
					results[r.idx].Message = fmt.Sprintf("gate %q failed: %v", gates[r.idx].Name(), r.err)
				} else {
					results[r.idx].Message = fmt.Sprintf("%s: %v", results[r.idx].Message, r.err)
				}
			}

			// On the first failure, mark every still-pending gate as skipped
			// and return. The pending goroutines keep running to completion
			// (their results land in the buffered channel and are dropped).
			if !results[r.idx].Passed && !failed {
				failed = true
				for i := 0; i < n; i++ {
					if !completed[i] {
						results[i] = skippedResult(fmt.Errorf("gate %q failed", gates[r.idx].Name()))
						completed[i] = true
						completedCount++
					}
				}
				return results
			}
		}
	}
	return results
}

// skippedResult builds a GateResult for a gate that did not run because a
// preceding gate failed or the context expired. The reason is preserved in
// both Message and Details so that the audit trail can distinguish skip
// causes.
func skippedResult(reason error) GateResult {
	msg := "skipped"
	if reason != nil {
		msg = fmt.Sprintf("skipped: %v", reason)
	}
	return GateResult{
		Passed:  false,
		Message: msg,
		Details: map[string]any{
			"reason": "skipped",
			"cause":  reasonString(reason),
		},
	}
}

// reasonString returns a stable string representation of reason for the
// Details map, guarding against nil.
func reasonString(reason error) string {
	if reason == nil {
		return ""
	}
	return reason.Error()
}

// --- NoopGate ---------------------------------------------------------------

// NoopGate is a built-in gate that unconditionally returns the configured
// pass/fail result. It is primarily intended for testing the manager and for
// acting as a placeholder in workflows that have not yet wired a real gate.
type NoopGate struct {
	name  string
	phase GatePhase
	pass  bool
}

// NewNoopGate returns a NoopGate with the given name, phase and pass flag.
// The returned gate is safe for concurrent use.
func NewNoopGate(name string, phase GatePhase, pass bool) *NoopGate {
	return &NoopGate{name: name, phase: phase, pass: pass}
}

// Name returns the gate's identifier.
func (g *NoopGate) Name() string { return g.name }

// Phase returns the phase at which this gate runs.
func (g *NoopGate) Phase() GatePhase { return g.phase }

// Check returns a GateResult with Passed set to the configured pass flag. It
// never returns an error and respects ctx only by returning early when ctx
// is already cancelled (in which case the result is a failure).
func (g *NoopGate) Check(ctx context.Context, _ GateInput) (GateResult, error) {
	if err := ctx.Err(); err != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("noop gate %q cancelled: %v", g.name, err),
		}, nil
	}
	msg := "noop gate passed"
	if !g.pass {
		msg = "noop gate failed"
	}
	return GateResult{
		Passed:  g.pass,
		Message: msg,
		Details: map[string]any{"gate": "noop", "configured_pass": g.pass},
	}, nil
}

// --- default manager --------------------------------------------------------

// defaultManager is the process-wide registry used by Register / Unregister /
// RunPhase when the caller does not supply its own. Concrete gate packages
// call Register from their init() to plug in.
var defaultManager = NewGateManager()

// DefaultManager returns the process-wide GateManager.
func DefaultManager() *GateManager { return defaultManager }

// Register registers gate with the default GateManager. It is a convenience
// wrapper around DefaultManager().Register so that init() call sites stay
// one-liners.
func Register(gate Gate) { defaultManager.Register(gate) }
