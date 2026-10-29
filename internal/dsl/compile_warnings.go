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
	"regexp"
	"sort"

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
	// codeNonIdempotentUndo is LE103: an undo step that does not declare
	// `idempotent: true`. Compensation is the path that runs when something has
	// already gone wrong, so it is the path most likely to be RE-run: a partially
	// applied forward batch leaves the executor retrying the undo, and a
	// non-idempotent undo run twice does the work twice. "Append the removed
	// line" duplicates it; "delete the created file" fails the second time and
	// surfaces as a compensation error that masks the original failure.
	codeNonIdempotentUndo = "LE103"
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

	// LE004 ("input parameters are declared or referenced, but nothing resolves
	// them") was WITHDRAWN when input resolution shipped: the advisory described a
	// gap that no longer exists, and an advisory that fires on every legitimate
	// use is noise. Inputs are now resolved at plan time (dsl.ResolveInputs), and
	// the two failure cases — an undeclared reference and an unsatisfied or
	// mistyped value — are plan-time errors, not advisories. Do not re-add a
	// compile-time input advisory here; see inputs.go.

	// LE103: an undo step without `idempotent: true`.
	//
	// Only the undo steps are linted, not the forward ones: a forward step's
	// non-idempotence is visible in the run (it ran once and its effect is what
	// the operator asked for), while an undo step's is invisible until the
	// second time it runs — and the second time is exactly the failure path,
	// where a doubled compensation either corrupts the target or fails and
	// hides the original cause.
	//
	// Warning, not error: the author may know the undo is naturally idempotent
	// (a snapshot restore, a config revert), and forcing the declaration would
	// break documents that compile today. The advisory asks for the annotation
	// that makes the property checkable and visible to the approver.
	if undo := nonIdempotentUndoSteps(wf); len(undo) > 0 {
		// One finding per step name, so a workflow with three sloppy undo steps
		// gets three actionable lines rather than one that names a list the
		// author has to cross-reference by hand.
		for _, name := range undo {
			out = append(out, ValidationError{
				Code:  codeNonIdempotentUndo,
				Field: "steps." + name + ".rollback",
				Message: fmt.Sprintf("undo step %q does not declare idempotent: true — a "+
					"compensation that runs twice does its work twice (add idempotent: true to the "+
					"undo step, or to the step it compensates, if re-running it is safe)", name),
			})
		}
	}

	return out
}

// nonIdempotentUndoSteps returns the sorted names of workflow steps whose
// rollback block contains at least one undo step without `idempotent: true`.
//
// It walks the step-level rollback blocks only. Workflow-level rollback content
// is rejected outright by LE097 before Advise runs, and the run-level snapshot
// primitive has no undo steps to annotate, so neither can reach this.
//
// The per-step flag is read off BOTH the nested undo steps and the step that
// owns them: `idempotent: true` on the forward step covers its undo block,
// because that is how the YAML reads (the field sits on the step, not on each
// of its rollback.steps entries).
func nonIdempotentUndoSteps(wf *Workflow) []string {
	var names []string
	for i := range wf.Steps {
		s := &wf.Steps[i]
		if s.Rollback == nil || len(s.Rollback.Steps) == 0 {
			continue
		}
		if s.Idempotent {
			continue
		}
		sloppy := false
		for j := range s.Rollback.Steps {
			if !s.Rollback.Steps[j].Idempotent {
				sloppy = true
				break
			}
		}
		if sloppy {
			names = append(names, s.Name)
		}
	}
	sort.Strings(names)
	return names
}

// referencesInputs reports whether any step argument — forward steps AND the
// undo steps nested under them, which are args a reader forgets are args —
// contains an `{{ input.x }}` reference. Both spellings the spec uses are
// covered: `{{input.table}}` and `{{ input.package_name }}`.
func referencesInputs(wf *Workflow) bool {
	for i := range wf.Steps {
		if stepReferencesInputs(&wf.Steps[i]) {
			return true
		}
	}
	return false
}

func stepReferencesInputs(s *Step) bool {
	if argsReferenceInputs(s.Args) {
		return true
	}
	if s.Rollback != nil {
		for i := range s.Rollback.Steps {
			if stepReferencesInputs(&s.Rollback.Steps[i]) {
				return true
			}
		}
	}
	return false
}

// argsReferenceInputs walks an argument value of any shape (string, nested map,
// list) because args is `map[string]any` all the way down.
func argsReferenceInputs(args map[string]any) bool {
	for _, v := range args {
		if valueReferencesInputs(v) {
			return true
		}
	}
	return false
}

func valueReferencesInputs(v any) bool {
	switch t := v.(type) {
	case string:
		return inputRefPattern.MatchString(t)
	case map[string]any:
		return argsReferenceInputs(t)
	case []any:
		for _, e := range t {
			if valueReferencesInputs(e) {
				return true
			}
		}
	}
	return false
}

// inputRefPattern matches the spec's input-reference shape: `{{`, optional
// whitespace, `input.`, optional whitespace, `}}`. The leading-dot-less form is
// deliberate: `{{.name}}` is the TEMPLATE instantiation syntax (internal/template
// fills it), which works, and must not be reported here.
var inputRefPattern = regexp.MustCompile(`\{\{\s*input\.`)

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
