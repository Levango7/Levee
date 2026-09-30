package rollback

// post_verify.go implements post-rollback verification (MVP task T037).
// After a rollback run completes, the PostRollbackVerifier re-runs the
// configured verification gates to confirm that the target is in a
// healthy state. If any gate fails, the rollback is treated as a failure
// and the grading logic (grading.go) is invoked to classify the outcome
// and dispatch notify / escalate / audit actions.
//
// The verifier integrates with internal/verify/gate.go: it uses the
// GateManager to execute gates. Two execution modes are supported:
//
//   - Phase mode: when verifyGates is empty, the verifier runs all gates
//     registered for verify.PhasePostApply via GateManager.RunPhase. This
//     is the "run the standard post-apply checks" path.
//   - Named mode: when verifyGates is non-empty, the verifier looks up
//     each named gate via GateManager.Gate and runs only those gates. This
//     lets the caller pick a subset of gates (e.g. only "service-health"
//     and "slo-error-rate") for a faster post-rollback check.
//
// The verifier is transport-agnostic: it receives a verify.GateInput
// from the caller and passes it through to the gates. The caller is
// responsible for populating the channel and target IDs.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/verify"
)

// --- PostVerifyResult -----------------------------------------------------

// PostVerifyResult is the outcome of a PostRollbackVerifier.Verify call.
// It aggregates the per-gate results and a top-level success verdict.
type PostVerifyResult struct {
	// Success is true when every executed gate passed. A verification
	// with no gates (empty phase, empty verifyGates) is considered
	// successful: there was nothing to verify and no failure.
	Success bool

	// FailedGates is the list of gate names that failed (Passed == false
	// or Check returned an error). It is empty when Success is true. The
	// order is the sorted gate name order, not execution order.
	FailedGates []string

	// SkippedGates is the list of gate names that were skipped (e.g.
	// because a preceding gate failed and RunPhase short-circuited, or
	// because a named gate was not found in the registry).
	SkippedGates []string

	// GateResults is the per-gate result slice, in the same order as the
	// gates were executed. It is always non-nil when Verify returns a
	// non-nil result.
	GateResults []verify.GateResult

	// Error is the first error encountered during verification (e.g. a
	// gate that could not run), or nil when Success is true. It is
	// preserved separately from the per-gate errors so that callers can
	// use errors.Is / errors.As on the top-level result.
	Error error

	// Grade is the rollback grade computed from the combined outcome of
	// the rollback and the post-verify check. It is populated only when
	// the verifier is configured with a Grader; otherwise it is the
	// zero value (empty string).
	Grade RollbackGrade

	// Duration is the wall-clock time spent inside Verify, measured from
	// entry to the construction of the result.
	Duration time.Duration
}

// --- PostRollbackVerifier --------------------------------------------------

// PostRollbackVerifier runs verification gates after a rollback completes
// and, when a Grader is configured, classifies the combined outcome. It
// is configured at construction time with a GateManager (the source of
// gates) and an optional Grader (for failure grading).
type PostRollbackVerifier struct {
	gateMgr *verify.GateManager
	grader  *Grader

	// verifyTimeout bounds one Verify / VerifyAndGrade call. It exists
	// because the verification deliberately runs DETACHED from the
	// caller's cancellation (see Verify), and an unbounded detached
	// context is exactly as unsafe as it sounds: a gate that blocks
	// forever would wedge the run with no way out. Zero means
	// DefaultVerifyTimeout.
	verifyTimeout time.Duration
}

// DefaultVerifyTimeout is the bound applied to one post-rollback
// verification when WithVerifyTimeout is not used. It is deliberately
// larger than a single gate's own timeout (verify.CommandGate defaults
// to 30s per attempt) because the bound covers the whole phase — several
// gates, possibly with retries — not one gate.
const DefaultVerifyTimeout = 2 * time.Minute

// PostRollbackVerifierOption configures a PostRollbackVerifier at
// construction time.
type PostRollbackVerifierOption func(*PostRollbackVerifier)

// WithGrader attaches a Grader to the verifier. When set, Verify computes
// a RollbackGrade from the combined rollback + verify outcome and
// populates PostVerifyResult.Grade. When not set, Grade is left empty.
func WithGrader(g *Grader) PostRollbackVerifierOption {
	return func(v *PostRollbackVerifier) { v.grader = g }
}

// WithVerifyTimeout bounds a single verification. d <= 0 selects
// DefaultVerifyTimeout.
func WithVerifyTimeout(d time.Duration) PostRollbackVerifierOption {
	return func(v *PostRollbackVerifier) {
		if d > 0 {
			v.verifyTimeout = d
		}
	}
}

// VerifyTimeout returns the effective bound (never zero).
func (v *PostRollbackVerifier) VerifyTimeout() time.Duration {
	if v.verifyTimeout <= 0 {
		return DefaultVerifyTimeout
	}
	return v.verifyTimeout
}

// NewPostRollbackVerifier returns a PostRollbackVerifier backed by gateMgr.
// The gateMgr must be non-nil; the grader is optional (attach via
// WithGrader).
func NewPostRollbackVerifier(gateMgr *verify.GateManager, opts ...PostRollbackVerifierOption) (*PostRollbackVerifier, error) {
	if gateMgr == nil {
		return nil, fmt.Errorf("post rollback verifier: gate manager is nil")
	}
	v := &PostRollbackVerifier{gateMgr: gateMgr}
	for _, opt := range opts {
		opt(v)
	}
	return v, nil
}

// GateManager returns the verifier's underlying gate manager. It is
// intended for diagnostics and tests.
func (v *PostRollbackVerifier) GateManager() *verify.GateManager { return v.gateMgr }

// Verify executes the post-rollback verification gates and returns the
// aggregated result.
//
// Parameters:
//
//   - ctx: the caller's context. It supplies values (trace ids, tenant)
//     and the caller's deadline is honoured as an UPPER bound, but
//     cancellation is deliberately NOT propagated to the gates — see the
//     "Cancellation" section below.
//   - rollbackResult: the outcome of the rollback run. It is used for
//     grading when a Grader is attached. It may be nil (the verifier
//     will still run the gates, but grading will treat nil as
//     GradeFailure).
//   - verifyGates: the gate names to run. When empty, the verifier runs
//     all gates registered for verify.PhasePostApply (phase mode). When
//     non-empty, only the named gates are run (named mode); a name not
//     found in the registry is recorded as skipped.
//   - input: the verify.GateInput passed to each gate. The caller is
//     responsible for populating RunID, TargetIDs, Channel and Params.
//
// Behaviour:
//
//   - No gates to run (empty phase in phase mode, or all named gates
//     missing): Success is true, FailedGates is empty.
//   - Any gate fails: Success is false, FailedGates lists the failed
//     gate names, Error holds the first failure cause.
//   - When a Grader is attached: Grade is computed from the combined
//     outcome. If any gate fails, the rollback is treated as a failure
//     regardless of rollbackResult.Success; otherwise the grade reflects
//     rollbackResult alone.
//
// Cancellation:
//
// The gates run on a context that carries the caller's values but NOT the
// caller's cancellation: it is context.WithoutCancel(ctx) plus
// WithTimeout(VerifyTimeout()). A caller that goes away does not abort the
// check. The bound is what makes that safe, and it is the reason the knob
// exists at all.
//
// Rationale: a cancelled caller is one of the three ways a rollback gets
// triggered (Ctrl-C, fencing, an upstream deadline), and by the time
// post-rollback verification runs the target has ALREADY been mutated
// back. "Did that land somewhere healthy?" is precisely the question the
// operator needs answered, and inheriting the cancellation guarantees the
// answer is always "context canceled" — a verdict about the request, not
// about the system. Note that the rollback dispatch immediately above is
// already detached for the same reason (interrupting it would leave
// applied batches unwound); verification inherits that reasoning one step
// later. What it does not inherit is the absence of a bound: a detached
// context with no deadline is an unbounded fan-out of gate commands at
// production targets, so WithVerifyTimeout caps it.
//
// Consequences callers can rely on: the caller's own earlier deadline
// still shortens the work (WithoutCancel drops Done but keeps Deadline),
// and a verification that outlives the bound returns with
// Success=false and Error wrapping context.DeadlineExceeded, detectable
// with errors.Is.
//
// The returned *PostVerifyResult is always non-nil.
func (v *PostRollbackVerifier) Verify(ctx context.Context, rollbackResult *RollbackResult, verifyGates []string, input verify.GateInput) *PostVerifyResult {
	start := time.Now()
	result := &PostVerifyResult{Success: true}

	// Detached from the caller's cancellation, bounded by the configured
	// timeout. See the "Cancellation" section on the doc comment above for
	// why this is the right trade for an advisory post-mortem check.
	verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.VerifyTimeout())
	defer cancel()

	var gateResults []verify.GateResult
	var failedNames, skippedNames []string

	if len(verifyGates) == 0 {
		// Phase mode: run all gates registered for PhasePostApply.
		gateResults = v.gateMgr.RunPhase(verifyCtx, verify.PhasePostApply, input)
	} else {
		// Named mode: run only the named gates.
		gateResults = v.runNamedGates(verifyCtx, verifyGates, input, &skippedNames)
	}

	// Inspect the results to build failed / skipped lists. GateResults
	// from RunPhase are in sorted-gate-name order; from runNamedGates
	// they are in the caller-specified order. We sort the failed / skipped
	// lists for stable comparison in tests and logs.
	for i, gr := range gateResults {
		name := v.gateNameForResult(verifyGates, i)
		if !gr.Passed {
			result.Success = false
			failedNames = append(failedNames, name)
			if result.Error == nil {
				result.Error = fmt.Errorf("post-rollback gate %q failed: %s", name, gr.Message)
			}
		}
	}
	sort.Strings(failedNames)
	sort.Strings(skippedNames)

	result.GateResults = gateResults
	result.FailedGates = failedNames
	result.SkippedGates = skippedNames
	result.Duration = time.Since(start)

	// When a grader is attached, compute the grade from the combined
	// outcome. A verify failure overrides the rollback result: even if
	// the rollback succeeded, a failed post-verify means the system is
	// not healthy, so we grade as failure.
	if v.grader != nil {
		result.Grade = v.computeGrade(rollbackResult, result)
	}

	log.Info("post-rollback verify completed",
		"success", result.Success,
		"failed_gates", len(result.FailedGates),
		"skipped_gates", len(result.SkippedGates),
		"duration_ms", result.Duration.Milliseconds())
	return result
}

// runNamedGates runs the gates listed by name. A name not found in the
// registry is recorded as skipped (with a placeholder GateResult). The
// gates are run concurrently, mirroring GateManager.RunPhase's
// behaviour, and the results are returned in the caller-specified order.
func (v *PostRollbackVerifier) runNamedGates(ctx context.Context, names []string, input verify.GateInput, skippedNames *[]string) []verify.GateResult {
	results := make([]verify.GateResult, len(names))
	type indexed struct {
		idx int
		r   verify.GateResult
	}
	done := make(chan indexed, len(names))

	var wg sync.WaitGroup
	for i, name := range names {
		gate, ok := v.gateMgr.Gate(name)
		if !ok {
			// Gate not registered: record as skipped synchronously.
			results[i] = verify.GateResult{
				Passed:  false,
				Message: fmt.Sprintf("gate %q not registered", name),
				Details: map[string]any{"reason": "not registered"},
			}
			*skippedNames = append(*skippedNames, name)
			continue
		}
		wg.Add(1)
		go func(idx int, g verify.Gate) {
			defer wg.Done()
			start := time.Now()
			r, err := g.Check(ctx, input)
			latency := time.Since(start)
			if r.Latency == 0 {
				r.Latency = latency
			}
			if err != nil {
				r.Passed = false
				if r.Message == "" {
					r.Message = fmt.Sprintf("gate %q failed: %v", g.Name(), err)
				} else {
					r.Message = fmt.Sprintf("%s: %v", r.Message, err)
				}
			}
			done <- indexed{idx: idx, r: r}
		}(i, gate)
	}

	// Collect goroutine results.
	go func() {
		wg.Wait()
		close(done)
	}()
	for r := range done {
		results[r.idx] = r.r
	}
	return results
}

// gateNameForResult returns the gate name for the result at position i.
// In phase mode (verifyGates empty) we cannot recover the name from the
// result alone, so we look up the phase's gate list. In named mode the
// name is verifyGates[i].
func (v *PostRollbackVerifier) gateNameForResult(verifyGates []string, i int) string {
	if len(verifyGates) > 0 && i < len(verifyGates) {
		return verifyGates[i]
	}
	// Phase mode: look up the sorted gate list for PhasePostApply.
	gates := v.gateMgr.Gates(verify.PhasePostApply)
	if i < len(gates) {
		return gates[i].Name()
	}
	return fmt.Sprintf("gate-%d", i)
}

// computeGrade derives the RollbackGrade from the combined rollback +
// verify outcome. A verify failure always overrides the rollback grade:
// even a fully successful rollback that fails post-verify is a failure
// (the system is not healthy). When verify passes, the grade reflects
// the rollback result alone.
func (v *PostRollbackVerifier) computeGrade(rollbackResult *RollbackResult, verifyResult *PostVerifyResult) RollbackGrade {
	if !verifyResult.Success {
		// Verify failed: treat as a rollback failure regardless of the
		// rollback result. If the rollback itself also failed, the grade
		// stays failure; if the rollback succeeded but verify failed,
		// the grade is failure (the system is not healthy).
		log.Warn("post-rollback verify failed; grading as failure",
			"failed_gates", verifyResult.FailedGates)
		return GradeFailure
	}
	// Verify passed: grade reflects the rollback result alone.
	if v.grader != nil {
		return v.grader.Grade(rollbackResult)
	}
	return GradeSuccess
}

// --- VerifyAndGrade convenience -------------------------------------------

// VerifyAndGrade is a convenience method that runs Verify and, when a
// Grader is attached, immediately dispatches the corresponding
// GradeAction's Notify / Escalate / Audit callbacks (when non-nil). It
// returns the verify result and the first error encountered while
// dispatching actions.
//
// When no Grader is attached, the method is equivalent to Verify and
// returns a nil dispatch error.
//
// This is the one-stop entry point for callers that want verify + grade
// + action dispatch in a single call.
func (v *PostRollbackVerifier) VerifyAndGrade(ctx context.Context, rollbackResult *RollbackResult, verifyGates []string, input verify.GateInput) (*PostVerifyResult, error) {
	result := v.Verify(ctx, rollbackResult, verifyGates, input)

	if v.grader == nil {
		return result, nil
	}

	// Re-grade using the combined outcome so that the action dispatch
	// matches the grade stored in the result.
	grade := result.Grade
	action := v.grader.GetAction(grade)

	// Dispatch on a detached, bounded context too, for the same reason the
	// gates do: the common case for reaching here is a rollback triggered
	// BY cancellation, so dispatching notify/escalate on the caller's
	// already-cancelled context would fail exactly when the notification
	// matters most. Notify and escalate are best-effort by design, so a
	// failure here is reported, not fatal.
	dispatchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.VerifyTimeout())
	defer cancel()

	if action.Notify != nil {
		if err := action.Notify(dispatchCtx, grade, rollbackResult); err != nil {
			return result, fmt.Errorf("post-verify grade notify: %w", err)
		}
	}
	if action.Escalate != nil {
		if err := action.Escalate(dispatchCtx, grade, rollbackResult); err != nil {
			return result, fmt.Errorf("post-verify grade escalate: %w", err)
		}
	}
	if action.Audit != nil {
		if err := action.Audit(dispatchCtx, grade, rollbackResult); err != nil {
			return result, fmt.Errorf("post-verify grade audit: %w", err)
		}
	}
	return result, nil
}
