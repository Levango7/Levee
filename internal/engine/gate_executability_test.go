// gate_executability_test.go covers the executability judgement shared by the
// plan-time refusal (internal/wiring) and the phase-time materialisation here.
//
// The refactor that introduced walkPlanGates/executabilityProblem rewrote a
// hand-unrolled traversal. Two things had to stay exactly as they were — the
// deterministic gate names ("step:<name>:pre:<i>") and the phase each timing
// binds to — because a swapped phase means a declared check silently runs at the
// wrong moment (or never). Those are asserted here rather than trusted.

package engine

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/verify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedApprover is the minimal HumanApprover: it exists only so "an approver
// is wired" is expressible in a test.
type scriptedApprover struct{ approved bool }

func (a scriptedApprover) RequestAndWait(_ context.Context, _, _, _ string) (verify.HumanDecision, error) {
	return verify.HumanDecision{Approved: a.approved}, nil
}

func gatePlan(steps ...plan.PlanStep) *plan.Plan {
	return &plan.Plan{Batches: []plan.Batch{{Steps: steps}}}
}

func humanCheck() dsl.GateCheck {
	return dsl.GateCheck{Type: "human", Params: map[string]any{"reason": "release freeze"}}
}

func sloCheck() dsl.GateCheck {
	return dsl.GateCheck{Type: "slo", Command: "up", Params: map[string]any{"threshold": 1.0}}
}

func cmdCheck() dsl.GateCheck {
	return dsl.GateCheck{Type: "cmd", Command: "true", ExpectExit: 0}
}

func probeCheck() dsl.GateCheck {
	return dsl.GateCheck{Type: "probe", Params: map[string]any{"kind": "tcp", "host_port": "h:22"}}
}

// TestExecutabilityProblemIsBidirectional pins both halves: a declaration the
// process can honour must NOT be blocked, and one it cannot MUST be. Only the
// refusal side was ever exercised before, so the "allowed" side had no proof
// that the judge is about capability rather than about type.
func TestExecutabilityProblemIsBidirectional(t *testing.T) {
	cases := []struct {
		name    string
		check   dsl.GateCheck
		phase   verify.GatePhase
		rt      GateRuntime
		blocked bool
		want    string
	}{
		{name: "human without approver", check: humanCheck(), phase: verify.PhasePostApply, rt: GateRuntime{}, blocked: true, want: "requires an approver"},
		{name: "human with approver", check: humanCheck(), phase: verify.PhasePostApply, rt: GateRuntime{Approver: scriptedApprover{approved: true}}, blocked: false},
		{name: "slo without prometheus", check: sloCheck(), phase: verify.PhasePostBatch, rt: GateRuntime{}, blocked: true, want: "verify.prometheus_url"},
		{name: "slo with prometheus", check: sloCheck(), phase: verify.PhasePostBatch, rt: GateRuntime{PrometheusURL: "http://prom:9090"}, blocked: false},
		// An slo check in a phase the engine never evaluates is the second
		// unexecutable shape: it registers a gate nobody runs, which reads as a
		// passing verification. It is caught here, with the endpoint configured,
		// so the two causes cannot be confused for each other.
		{name: "slo in the wrong phase", check: sloCheck(), phase: verify.PhasePostApply, rt: GateRuntime{PrometheusURL: "http://prom:9090"}, blocked: true, want: "post_batch phase only"},
		// cmd/probe carry every knob themselves; blocking them would be a
		// behaviour change, so their non-blocking is pinned here.
		{name: "cmd needs no runtime", check: cmdCheck(), phase: verify.PhasePreApply, rt: GateRuntime{}, blocked: false},
		{name: "probe needs no runtime", check: probeCheck(), phase: verify.PhasePreApply, rt: GateRuntime{}, blocked: false},
	}
	for _, tc := range cases {
		err := GateRuntime(tc.rt).executabilityProblem("g", tc.phase, &tc.check)
		if tc.blocked {
			require.Error(t, err, tc.name)
			assert.Contains(t, err.Error(), `"g"`, tc.name+": the message must name the gate")
			assert.Contains(t, err.Error(), tc.want, tc.name)
		} else {
			assert.NoError(t, err, tc.name)
		}
	}
}

// TestPlanGateBlockersNamesEveryOffender is the refusal's content contract: one
// entry per unexecutable declaration, not a single first-hit abort — an operator
// fixing a workflow should see all of them at once.
func TestPlanGateBlockersNamesEveryOffender(t *testing.T) {
	p := gatePlan(plan.PlanStep{
		Name: "deploy",
		Gate: &dsl.GateSpec{
			Pre:   []dsl.GateCheck{humanCheck()},
			Batch: []dsl.GateCheck{sloCheck(), cmdCheck()},
			Post:  []dsl.GateCheck{cmdCheck(), humanCheck()},
		},
	})

	// One human check with no approver, one slo check with no endpoint, and the
	// second human check: three blockers from four+ unexecutable-looking lines,
	// because the two cmd checks need nothing and must not be reported.
	blockers := PlanGateBlockers(p, GateRuntime{})
	require.Len(t, blockers, 3, "one per unexecutable declaration, got %v", blockers)

	joined := ""
	for _, b := range blockers {
		joined += b + "\n"
	}
	assert.Contains(t, joined, "step:deploy:pre:0")
	assert.Contains(t, joined, "step:deploy:batch:0")
	assert.Contains(t, joined, "step:deploy:post:1")
	assert.NotContains(t, joined, "step:deploy:batch:1", "the cmd check must not be reported")
	assert.NotContains(t, joined, "step:deploy:post:0", "the cmd check must not be reported")

	// With both capabilities wired, the same plan is executable.
	assert.Empty(t, PlanGateBlockers(p, GateRuntime{
		PrometheusURL: "http://prom:9090",
		Approver:      scriptedApprover{approved: true},
	}))
}

// TestPlanGateBlockersCatchesWrongPhaseSlo is the case a capability-only judge
// would miss: the deployment has Prometheus configured, so only the declaration
// itself makes the plan unexecutable — and it must still be refused before any
// target is touched.
func TestPlanGateBlockersCatchesWrongPhaseSlo(t *testing.T) {
	p := gatePlan(plan.PlanStep{Name: "deploy", Gate: &dsl.GateSpec{
		Post: []dsl.GateCheck{sloCheck()},
	}})
	blockers := PlanGateBlockers(p, GateRuntime{PrometheusURL: "http://prom:9090"})
	require.Len(t, blockers, 1, "%v", blockers)
	assert.Contains(t, blockers[0], "post_batch phase only")

	// Same check in the phase it belongs to is executable, so the refusal above
	// is about timing and not a blanket ban on slo gates.
	ok := gatePlan(plan.PlanStep{Name: "deploy", Gate: &dsl.GateSpec{
		Batch: []dsl.GateCheck{sloCheck()},
	}})
	assert.Empty(t, PlanGateBlockers(ok, GateRuntime{PrometheusURL: "http://prom:9090"}))
}

// TestWalkPlanGatesKeepsNamesAndPhases is the refactor guard: every timing must
// still bind to the phase materialisation used before, under the same name.
// A silent swap here would move when a gate runs without any test noticing,
// because both the old and new code register *something*.
func TestWalkPlanGatesKeepsNamesAndPhases(t *testing.T) {
	gm := verify.NewGateManager()
	p := gatePlan(
		plan.PlanStep{Name: "a", Gate: &dsl.GateSpec{
			Pre:   []dsl.GateCheck{cmdCheck()},
			Batch: []dsl.GateCheck{cmdCheck()},
			Post:  []dsl.GateCheck{cmdCheck()},
		}},
		plan.PlanStep{Name: "b", Gate: &dsl.GateSpec{
			Pre: []dsl.GateCheck{cmdCheck(), cmdCheck()},
		}},
	)
	require.NoError(t, materializeStepGates(gm, p, GateRuntime{}))

	assert.Equal(t, []string{"step:a:pre:0", "step:b:pre:0", "step:b:pre:1"},
		gateNames(gm.Gates(verify.PhasePreApply)))
	assert.Equal(t, []string{"step:a:batch:0"},
		gateNames(gm.Gates(verify.PhasePostBatch)))
	assert.Equal(t, []string{"step:a:post:0"},
		gateNames(gm.Gates(verify.PhasePostApply)))
}

func gateNames(gates []verify.Gate) []string {
	out := make([]string, 0, len(gates))
	for _, g := range gates {
		out = append(out, g.Name())
	}
	return out
}

// TestMaterialiseStillFailsClosed proves the shared judge did not weaken the
// runtime side: registering a gate the runtime cannot execute must still abort
// materialisation rather than register a gate that guesses or auto-passes.
func TestMaterialiseStillFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		label string
		check dsl.GateCheck
		want  string
	}{
		{"human", humanCheck(), "requires an approver"},
		{"slo", sloCheck(), "requires verify.prometheus_url configuration"},
	} {
		gm := verify.NewGateManager()
		p := gatePlan(plan.PlanStep{Name: "s", Gate: &dsl.GateSpec{
			Pre: []dsl.GateCheck{tc.check},
		}})
		err := materializeStepGates(gm, p, GateRuntime{})
		require.Error(t, err, tc.label)
		assert.Contains(t, err.Error(), tc.want, tc.label+": message must name the missing wiring")
		assert.Empty(t, gm.Gates(verify.PhasePreApply), tc.label+": nothing may be registered on failure")
	}
}

// TestPlanGateBlockersMatchesMaterialisation is the agreement test between the
// two halves of the same rule: any plan that yields no blockers must materialise
// cleanly, and any plan that yields blockers must fail materialisation. Without
// it, the refusal and the runtime could disagree and the disagreement would only
// surface as a mid-run failure — the exact bug being fixed.
func TestPlanGateBlockersMatchesMaterialisation(t *testing.T) {
	cases := []struct {
		name    string
		check   dsl.GateCheck
		rt      GateRuntime
		blocked bool
	}{
		{name: "human/no approver", check: humanCheck(), rt: GateRuntime{}, blocked: true},
		{name: "human/approver", check: humanCheck(), rt: GateRuntime{Approver: scriptedApprover{}}, blocked: false},
		{name: "slo/no url", check: sloCheck(), rt: GateRuntime{}, blocked: true},
		{name: "slo/url", check: sloCheck(), rt: GateRuntime{PrometheusURL: "http://p:9090"}, blocked: false},
		{name: "cmd", check: cmdCheck(), rt: GateRuntime{}, blocked: false},
	}
	for _, tc := range cases {
		// slo belongs in Batch and human/cmd in Post: the slots mirror what a real
		// workflow may declare, so this compares the two judges on plans that are
		// otherwise legal and cannot pass by accident.
		slot := &dsl.GateSpec{Post: []dsl.GateCheck{tc.check}}
		if tc.check.Type == "slo" {
			slot = &dsl.GateSpec{Batch: []dsl.GateCheck{tc.check}}
		}
		p := gatePlan(plan.PlanStep{Name: "s", Gate: slot})

		blocked := len(PlanGateBlockers(p, tc.rt)) > 0
		assert.Equal(t, tc.blocked, blocked, tc.name)

		gm := verify.NewGateManager()
		err := materializeStepGates(gm, p, tc.rt)
		if tc.blocked {
			require.Error(t, err, tc.name+": materialisation must agree with the refusal")
		} else {
			require.NoError(t, err, tc.name+": materialisation must agree with the acceptance")
		}
	}
}

// TestBatchLevelGateIsHashBoundAndMaterialised is the same pair of halves, now
// read the way it must work.
//
// `batches: gate:` flows through the generator into plan.Batch.Gate
// (internal/plan/generator.go) and IS covered by the plan hash
// (internal/plan/hash.go), so it is part of the artifact an approval binds to.
// walkPlanGates — the only thing that turns declarations into runnable gates —
// used to iterate Batches[].Steps[].Gate only, so the batch-level half of that
// promise was never kept: nothing registered, nothing ran, and the plan-time
// refusal did not see it either. examples/gate-templates/redis.yaml ships a
// batch probe gate whose comment claims it "runs after EVERY batch completes and
// blocks the next batch on failure"; that claim is now true instead of pinned as
// a defect.
//
// The behaviour change this carries is deliberate and documented in
// CHANGELOG.md: a batch-level gate that has never run can start failing batches
// that always passed. That is the point — the approved plan already promised the
// check — but it is not a refactor detail, so it is stated as a migration note.
func TestBatchLevelGateIsHashBoundAndMaterialised(t *testing.T) {
	batchGate := &dsl.GateSpec{Batch: []dsl.GateCheck{
		{Type: "cmd", Command: "exit 1", ExpectExit: 0},
	}}

	p := &plan.Plan{ID: "plan-batch-gate", Batches: []plan.Batch{{
		Targets: []string{"web-1"},
		Gate:    batchGate,
		Steps:   []plan.PlanStep{{Name: "restart"}},
	}}}

	// 1. It runs: the declaration lands in the phase that executes between
	//    batches, under the name the refusal reports.
	gm := verify.NewGateManager()
	require.NoError(t, materializeStepGates(gm, p, GateRuntime{}))
	assert.Empty(t, gateNames(gm.Gates(verify.PhasePreApply)))
	assert.Empty(t, gateNames(gm.Gates(verify.PhasePostApply)))
	assert.Equal(t, []string{"batches:batch:0"}, gateNames(gm.Gates(verify.PhasePostBatch)),
		"the batch-level gate must land in the post-batch phase")

	// 2. It is bound into the approved artifact: two plans differing only in
	//    their batch-level gate hash differently. Proving this half matters —
	//    without it the declaration would be invisible to governance as well,
	//    and the honest label would be "ignored field" instead of "promised and
	//    now delivered".
	withGate := &plan.Plan{ID: "plan-batch-gate", Batches: []plan.Batch{{
		Targets: []string{"web-1"}, Gate: batchGate,
		Steps: []plan.PlanStep{{Name: "restart"}},
	}}}
	withoutGate := &plan.Plan{ID: "plan-batch-gate", Batches: []plan.Batch{{
		Targets: []string{"web-1"},
		Steps:   []plan.PlanStep{{Name: "restart"}},
	}}}
	assert.NotEqual(t, plan.ComputeHash(withGate), plan.ComputeHash(withoutGate),
		"Batch.Gate must be inside the hash for this gate to be approval-bound")

	// 3. And the plan-time refusal sees it too, so a batch-level human gate with
	//    no approver is refused up front rather than vanishing at execution time.
	escalated := &plan.Plan{ID: "plan-escape", Batches: []plan.Batch{{
		Targets: []string{"web-1"},
		Gate:    &dsl.GateSpec{Batch: []dsl.GateCheck{humanCheck()}},
		Steps:   []plan.PlanStep{{Name: "restart"}},
	}}}
	blockers := PlanGateBlockers(escalated, GateRuntime{})
	require.Len(t, blockers, 1, "a batch-level human check with no approver wired must be reported")
	assert.Contains(t, blockers[0], "batches:batch:0", blockers[0])
}
