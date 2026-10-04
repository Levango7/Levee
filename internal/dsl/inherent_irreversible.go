// Inherent irreversibility: the module.action pairs that destroy something by
// their own nature, whether or not the workflow author said so.
//
// This lives in the language package because three layers read the same verdict
// and must never disagree:
//
//   - the V14 compile gate below (validator.go) refuses any step judged
//     irreversible unless the workflow's allow_irreversible whitelist names it;
//   - plan.NewGenerator registers the same pairs on its IrreversibleChecker, so
//     plan artifacts carry the verdict (approval escalation, rollback gating);
//   - the plan-time defence in generator.go re-checks it for the wiring path
//     that reaches Generate without the validator.
//
// It could not live in internal/executor, which is where it was written first:
// internal/executor imports this package (ApprovalLevelHigh aliases
// dsl.ApprovalLevelHigh, so a tier rename cannot strand the suggestion), so a
// validator here importing executor back would close a cycle. The vocabulary is
// language knowledge — the same category as actionSignatures in typechecker.go —
// and the dependency direction now points one way.
package dsl

import "sort"

// inherentIrreversibleActions is the single source of truth for the module.action
// pairs that are destructive even when the author did not mark them.
var inherentIrreversibleActions = map[string]struct{}{
	"pkg.remove":           {},
	"file.delete":          {},
	"user.remove":          {},
	"mysql.replica_switch": {},
	"mysql.pt_osc":         {},
}

// DefaultIrreversibleActions returns the inherently irreversible module.action
// pairs in sorted order, so a caller can register them deterministically.
func DefaultIrreversibleActions() []string {
	out := make([]string, 0, len(inherentIrreversibleActions))
	for key := range inherentIrreversibleActions {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// IsInherentIrreversible reports whether the module.action pair is irreversible
// by nature — a member of the vocabulary above. The explicit step.Irreversible
// declaration is a separate, author-controlled signal that both the compile gate
// and IrreversibleChecker.Check weigh first.
func IsInherentIrreversible(module, action string) bool {
	_, ok := inherentIrreversibleActions[module+"."+action]
	return ok
}
