package plan

// targetcaps.go enforces the target-group size bounds the LEVEELang spec has
// promised since its first draft: `min_count` / `max_count` on a `target`
// block ("目标集最小数量，少于则阻断 plan", spec §4.1 and §4.1's semantics
// paragraph: "min_count / max_count 在 plan 阶段校验，不满足则阻断 plan，不进审批").
//
// The declaration was parsed, carried on the AST/IR, emitted back by
// emit.go, and used by three shipped workflows (examples/templates/
// patch-rolling.yaml, examples/workflows/batch-update.yaml,
// examples/workflows/dr-switch-orders-db.yaml) — and NOTHING ever read it. A
// bound that is never checked is worse than no bound: the author wrote a
// guardrail and the platform quietly ignored it.
//
// Why the generator and not the validator: the spec says the check happens at
// PLAN time, and only the generator knows the resolved target set. The
// validator (compile time) has no targets yet, so it can only refuse a
// meaningless DECLARATION — see docs/blocked-items.md E-3 (LE107 withdrawn
// 2026-10-31; will be re-registered when dsl.validateTargetCaps lands).
//
// Counting rule, stated plainly because the alternative is a guess:
//
//   - a STATIC group (hosts listed) counts the intersection of its declared
//     hosts with the resolved set. Asking for hosts a,b,c and resolving only
//     a is one host, not three.
//   - a QUERY group counts the WHOLE resolved set when it is the workflow's
//     only target — that is the shape every shipped workflow uses (one
//     `target:` with a query; the operator resolves the query and passes
//     those hosts to plan). The group has no host list to intersect with, and
//     as the only target it selects exactly what was resolved.
//   - a QUERY group alongside OTHERS cannot be attributed: the plan path
//     receives one flat host list and never splits it per group, so honouring
//     that bound would require a number the generator does not have. It
//     refuses rather than assume, which is the same posture as the window
//     gate's "an unusable declaration refuses too".
//
// A group that declares no bound is skipped: adding a bound is the opt-in, so
// workflows that never wrote one are byte-for-byte unaffected.

import (
	"fmt"

	"github.com/nexus/levee/internal/errors"
	"github.com/nexus/levee/internal/dsl"
)

// ResolvedTargetCounts, per group, what the generator counted. Exported so the
// caller-facing dry run and the tests can assert on the number rather than
// re-deriving it.
type targetGroupCount struct {
	index int
	name  string
	count int
}

// checkTargetCaps refuses a plan whose resolved target set violates any
// declared target-group bound. It returns nil when no group declares a bound.
func checkTargetCaps(wf *dsl.Workflow, resolvedTargets []string) error {
	if wf == nil {
		return nil
	}
	resolved := make(map[string]struct{}, len(resolvedTargets))
	for _, h := range resolvedTargets {
		resolved[h] = struct{}{}
	}

	// How many groups declare ANY bound? A query group can only be judged
	// against the whole resolved set when it is the sole selector.
	withBounds := 0
	for _, g := range wf.Targets {
		if g.MinCount > 0 || g.MaxCount > 0 {
			withBounds++
		}
	}
	if withBounds == 0 {
		return nil
	}

	singleSelector := len(wf.Targets) == 1
	for i, g := range wf.Targets {
		if g.MinCount <= 0 && g.MaxCount <= 0 {
			continue
		}
		name := g.Name
		if name == "" {
			name = fmt.Sprintf("targets[%d]", i)
		}

		var count int
		switch {
		case len(g.Hosts) > 0:
			// Static membership: declared hosts that actually resolved.
			seen := map[string]struct{}{}
			for _, h := range g.Hosts {
				if _, dup := seen[h]; dup {
					continue
				}
				seen[h] = struct{}{}
				if _, ok := resolved[h]; ok {
					count++
				}
			}
		case singleSelector:
			// The only target group, selected by query: everything the
			// caller resolved is what it selected.
			count = len(resolvedTargets)
		default:
			return errors.New(errors.LE105,
				fmt.Sprintf("workflow %q: target %q declares min_count/max_count but is a query group alongside other groups, "+
					"so its resolved size cannot be attributed; give it hosts, or make it the only target",
					wf.Meta.Name, name), errors.Fatal)
		}

		if g.MinCount > 0 && count < g.MinCount {
			return errors.New(errors.LE105,
				fmt.Sprintf("workflow %q: target %q resolved %d host(s), fewer than min_count %d",
					wf.Meta.Name, name, count, g.MinCount), errors.Fatal)
		}
		if g.MaxCount > 0 && count > g.MaxCount {
			return errors.New(errors.LE106,
				fmt.Sprintf("workflow %q: target %q resolved %d host(s), more than max_count %d",
					wf.Meta.Name, name, count, g.MaxCount), errors.Fatal)
		}
	}
	return nil
}
