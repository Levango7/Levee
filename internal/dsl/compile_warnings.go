// Compile-time advisories: the `warning` half of the error catalogue.
//
// Closes the P1 row of docs/product-roadmap.md ("CompileWarning 一档没有产生点").
//
// internal/errors has always classified five codes as CompileWarning — the
// spec text says so in three places (§4.3 canary guidance, the field tables'
// "缺省 …（告警 LE0xx warning）", and the LE094-096 rows "仅 warning") — but
// nothing produced them: `Validator.Validate` was the only finding emitter and
// every consumer treats a non-empty result as fatal. Putting them there would
// have made every existing workflow that omits a window, approval or batches
// block fail to compile and every AI-drafted change be refused by the bridge —
// a behaviour break dressed up as a new gate.
//
// So advisories live beside the validator instead of inside it. `Advise` is a
// separate entry point returning only catalogue-classified warnings, which
// makes "a warning can never block" a structural property rather than a
// filter every consumer has to remember: `Validate`'s output is untouched, and
// the disjointness of the two sets is pinned by
// TestValidateAndAdviseNeverAgreeOnSeverity.
//
// LE052 ("batches missing post_batch gate") IS produced here now. It was held
// back because its remedy did not exist yet: `convertGate` routed every declared
// gate into `GateSpec.Post`, only `GateSpec.Batch` binds `verify.PhasePostBatch`,
// and nothing reached `.Batch` from YAML. Advising operators to add a check that
// either cannot be declared or is never materialised is the same fault as an
// error message that offers a configuration which does nothing. Position routing
// landed in gate_position.go, and `walkPlanGates` in internal/engine now covers
// all three declaration sites, so the advice names something that works.
package dsl

import (
	"fmt"

	"github.com/nexus/levee/internal/errors"
)

// The advisory codes. Names carry "Block" because each one reports an omitted
// declaration, not a malformed one — the malformed shapes are fatal and live in
// Validate.
const (
	// codeCanaryRatio is LE033: the first percent batch exceeds the 5% canary
	// guidance (spec §4.3).
	codeCanaryRatio = "LE033"
	// codeMissingApprovalBlock is LE094: no approval block, so only the plan's
	// risk floor decides the tier.
	codeMissingApprovalBlock = "LE094"
	// codeMissingWindowBlock is LE095: no window block, so nothing constrains
	// when the change may run.
	codeMissingWindowBlock = "LE095"
	// codeMissingBatchesBlock is LE096: no batches block, so every target is
	// changed in one batch.
	codeMissingBatchesBlock = "LE096"
	// codeMissingPostBatchGate is LE052: a batches block with no between-batches
	// check, so nothing can stop the next batch.
	codeMissingPostBatchGate = "LE052"
)

// maxCanaryPercent is the §4.3 guidance for a first batch: 5% of the fleet.
const maxCanaryPercent = 5

// Advise returns the compile-time advisories for wf: findings the catalogue
// classifies as CompileWarning, i.e. conditions that are legal per spec but
// worth telling the author and the approver about.
//
// A nil wf yields nothing — there is nothing to advise on, and refusing is
// Validate's job. A caller that wants a single list combines Validate and Advise
// and decides per severity; no caller may treat this output as fatal.
func (v *Validator) Advise(wf *Workflow) []ValidationError {
	if wf == nil {
		return nil
	}

	var out []ValidationError

	// LE095: an undeclared window is legal (§4.2 defaults to "unconstrained"),
	// but it means the change can be applied at 03:00 on a Sunday or during an
	// incident, and the approver sees nothing about timing. Declared-but-broken
	// windows are fatal (LE020/LE021) and never reach here.
	if wf.Window.Start == "" && wf.Window.End == "" && len(wf.Window.Days) == 0 {
		out = append(out, ValidationError{
			Code:  codeMissingWindowBlock,
			Field: "window",
			Message: "no window block: the change may be planned and applied at any " +
				"time (declare window.start/end to constrain it)",
		})
	}

	// LE094: with no approval block the tier comes from the plan's risk floor
	// alone, so an unremarkable change lands at standard with one approver.
	if wf.Approval == nil {
		out = append(out, ValidationError{
			Code:  codeMissingApprovalBlock,
			Field: "approval",
			Message: "no approval block: the approval tier falls to the plan's risk " +
				"floor alone (standard, one approver, unless risk raises it)",
		})
	}

	// LE096: an empty batches block takes the generator's default strategy
	// (serial = all targets in one batch, plan/generator.go:267-276), which is
	// the opposite of a canary.
	if wf.Batches.Strategy == "" && len(wf.Batches.Steps) == 0 && !wf.Batches.Serial {
		out = append(out, ValidationError{
			Code:  codeMissingBatchesBlock,
			Field: "batches",
			Message: "no batches block: every target is changed in a single batch " +
				"(strategy defaults to serial), so nothing is rolled out gradually",
		})
	}

	// LE033: percent batches only — with fixed/serial the first-batch size is a
	// target count and the fleet is not known until plan time resolves the
	// target query, so the 5% guidance cannot be evaluated from the document.
	if wf.Batches.Strategy == BatchStrategyPercent && len(wf.Batches.Steps) > 0 {
		if first := wf.Batches.Steps[0]; first > maxCanaryPercent {
			out = append(out, ValidationError{
				Code:  codeCanaryRatio,
				Field: "batches.steps",
				Message: fmt.Sprintf("first batch covers %d%% of targets, above the %d%% "+
					"canary guidance (spec §4.3)", first, maxCanaryPercent),
			})
		}
	}

	// LE052: a staged rollout with no between-batches check. §4.3 makes
	// post_batch the position that stops the next batch (设计红线 R5), and the
	// field table prescribes this warning when it is absent. Without it a bad
	// first batch is already followed by every remaining batch, and the first
	// verification the operator sees is the post_apply one at the very end.
	//
	// Only fires when the document declares a batches block at all: with none,
	// LE096 already says the change lands in a single batch, and there is no
	// "between batches" to check.
	if hasBatchesBlock(wf) {
		specs := make([]*GateSpec, 0, len(wf.Steps)+2)
		specs = append(specs, wf.Gate, wf.Batches.Gate)
		for i := range wf.Steps {
			specs = append(specs, wf.Steps[i].Gate)
		}
		if !HasBatchCheck(specs...) {
			out = append(out, ValidationError{
				Code:  codeMissingPostBatchGate,
				Field: "batches.gate",
				Message: "no post_batch gate: batches run back to back with nothing verified " +
					"in between (declare batches.gate, or a check with position: post_batch, " +
					"to stop the next batch when a check fails)",
			})
		}
	}

	return out
}

// hasBatchesBlock reports whether the document declared a batches block, using
// the same fields the LE096 advisory treats as its absence.
func hasBatchesBlock(wf *Workflow) bool {
	return wf.Batches.Strategy != "" || len(wf.Batches.Steps) > 0 || wf.Batches.Serial
}

// IsCompileWarning reports whether code is catalogued as a compile-time warning.
// The catalogue in internal/errors is the only definition of that: restating the
// set here would let a code's severity change in one place only.
func IsCompileWarning(code string) bool {
	ci, ok := errors.Lookup(code)
	return ok && ci.Compile == errors.CompileWarning
}
