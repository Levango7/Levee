// idempotent_advisory_test.go pins the asymmetry between the FORWARD step's
// `idempotent` and a ROLLBACK sub-step's `idempotent` at the plan-hash
// boundary — the distinction docs/leveelang-spec.md previously got wrong.
//
// Two `idempotent: true` declarations exist in the DSL, on two different
// fields of the same dsl.Step type (ast.go reuses Step for both a batch step
// and an undo step), and they mean different things:
//
//   - ROLLBACK sub-step idempotent — a GOVERNANCE field. plan.hash's
//     canonicalRollbackStep carries it (omitempty), and rollback.
//     compensationRepeatable reads it to decide whether an already-compensated
//     step may be repeated. Because the runtime acts on it, it MUST be covered
//     by the plan hash so it cannot flip after approval. (Already asserted by
//     TestComputeHashGovernanceFieldsChangeHash / "undo step idempotency".)
//
//   - FORWARD step idempotent — ADVISORY metadata. plan.PlanStep (generator.go)
//     carries no Idempotent field at all, so it never reaches the plan and
//     never enters the hash. This is deliberate, not an omission: the
//     resumable-retry skip decision is keyed on the *executor module type's*
//     own idempotency (executor.IsIdempotent), never on the author's
//     per-step YAML claim — see docs/design-cluster-failover.md §7.5 Q3, which
//     chose module-type declarations as the safety source and explicitly ruled
//     that authoring it per step "是独立项目". Honouring an author-supplied
//     "safe to re-run" label as a skip gate would be the same class of error as
//     client-controlled autoApprove: letting the caller assert away a safety
//     property. So flipping the forward flag must NOT change the plan hash,
//     and it must NOT change what completedIdempotentBatches skips.
//
// Together these two tests are the executable statement of that contract.
package plan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

// forwardStepPlanHash renders a one-batch plan from a single forward step
// carrying the given Idempotent declaration and returns its hash.
func forwardStepPlanHash(t *testing.T, forwardIdempotent bool) string {
	t.Helper()
	step := dsl.Step{Name: "reload", Module: "shell", Action: "exec",
		Args:       map[string]any{"command": "systemctl reload nginx"},
		Idempotent: forwardIdempotent,
	}
	wf := makeWorkflow("wf-idem-advisory", dsl.BatchConfig{
		Strategy: "fixed", Steps: []int{2}, MaxConcurrency: 2,
	}, step)
	p, err := NewGenerator().Generate(wf, makeTargets(2))
	require.NoError(t, err)
	require.NotNil(t, p)
	return ComputeHash(p)
}

// TestForwardStepIdempotentIsNotHashBound is the characterisation that keeps
// the spec honest: two workflows that differ ONLY in a forward step's
// `idempotent` declaration plan to the SAME hash. The forward flag is author
// metadata preserved through compile/IR; it is not a governance field and is
// not made immutable-by-approval.
func TestForwardStepIdempotentIsNotHashBound(t *testing.T) {
	assert.NotEqual(t, "", forwardStepPlanHash(t, false), "sanity: hash must be non-empty")
	assert.Equal(t,
		forwardStepPlanHash(t, false),
		forwardStepPlanHash(t, true),
		"a forward step's idempotent is advisory and must not enter the plan hash "+
			"(skip decisions key on executor module type, not the author's claim)")
}

// TestRollbackStepIdempotentIsHashBound is the counterpart: it proves the two
// fields are NOT conflated by the generator. Flipping the UNDO step's
// idempotent — the one compensationRepeatable acts on — must change the hash,
// while the forward step's does not (previous test).
func TestRollbackStepIdempotentIsHashBound(t *testing.T) {
	build := func(undoIdempotent bool) string {
		step := dsl.Step{Name: "reload", Module: "shell", Action: "exec",
			Args: map[string]any{"command": "systemctl reload nginx"},
			Rollback: &dsl.RollbackSpec{Strategy: "undo-action", Steps: []dsl.Step{
				{Name: "restart", Module: "svc", Action: "restart", Idempotent: undoIdempotent},
			}},
		}
		wf := makeWorkflow("wf-undo-hash", dsl.BatchConfig{
			Strategy: "fixed", Steps: []int{2}, MaxConcurrency: 2,
		}, step)
		p, err := NewGenerator().Generate(wf, makeTargets(2))
		require.NoError(t, err)
		return ComputeHash(p)
	}
	assert.NotEqual(t, build(false), build(true),
		"the undo step's idempotent drives the compensation gate and MUST be hash-bound")
}
