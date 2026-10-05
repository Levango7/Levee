package engine

// gate_position_wiring_test.go — the two gate declaration sites that are not
// step-level: a workflow-level `gates:` entry and `batches.gate:`.
//
// Both are parsed, both are copied into the plan, and both are folded into
// plan_hash by canonicalGateV2 — so a change's approved artifact already
// promises them. What they did not have was a consumer: walkPlanGates walked
// b.Steps[].Gate only, so materialise registered nothing and PlanGateBlockers
// judged nothing. A declared verification gate that runs neither is the exact
// failure mode this codebase calls a false compliance record, and it is worse
// than the slo case documented on executabilityProblem: an unexecutable human
// gate at workflow level was not even refused at plan time.
//
// The position keyword was the other half of the same hole — convertGate dropped
// it and filed every check in GateSpec.Post (which materialises as post_apply),
// so `position: post_batch` meant "run once at the end" and the Batch slot was
// unreachable from any YAML. These tests pin both halves from the document down.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/verify"
)

const positionedGatesWorkflow = `name: positioned-gates
version: "1.0"
target:
  type: host
  hosts: ["web-1", "web-2"]
batches:
  strategy: one-per-target
  gate:
    position: post_batch
    cmd:
      run: "systemctl is-active nginx"
      expect_exit: 0
gates:
  - position: pre_apply
    cmd:
      run: "test -d /etc"
  - position: post_batch
    cmd:
      run: "uptime"
  - position: post_apply
    cmd:
      run: "systemctl is-active sshd"
steps:
  - name: restart
    action: svc.restart
    verify:
      position: post_batch
      cmd:
        run: "curl -sf http://localhost/healthz"
`

func planPositionedGates(t *testing.T) *plan.Plan {
	t.Helper()
	wf, err := dsl.NewParser().ParseBytes([]byte(positionedGatesWorkflow))
	require.NoError(t, err)
	p, err := plan.NewGenerator().Generate(wf, []string{"web-1", "web-2"})
	require.NoError(t, err)
	return p
}

// TestGatePositions_RouteToTheSlotTheyDeclare is the parser half: a position
// decides which GateSpec slot holds the check, because the slot is what the
// engine maps to a phase.
func TestGatePositions_RouteToTheSlotTheyDeclare(t *testing.T) {
	p := planPositionedGates(t)

	require.NotNil(t, p.Gate, "workflow-level gates must reach the plan")
	assert.Len(t, p.Gate.Pre, 1, "position: pre_apply must land in the pre slot")
	assert.Len(t, p.Gate.Batch, 1, "position: post_batch must land in the batch slot")
	assert.Len(t, p.Gate.Post, 1, "position: post_apply must land in the post slot")

	require.Len(t, p.Batches, 2, "one-per-target: one batch per host")
	for _, b := range p.Batches {
		require.NotNil(t, b.Gate, "batches.gate must reach every batch")
		assert.Len(t, b.Gate.Batch, 1, "batches.gate is a between-batches check by definition")
		assert.Empty(t, b.Gate.Post, "batches.gate must not be filed as a post_apply check")
	}

	require.Len(t, p.Batches[0].Steps, 1)
	sg := p.Batches[0].Steps[0].Gate
	require.NotNil(t, sg)
	assert.Len(t, sg.Batch, 1, "a step verify gate may declare post_batch too")
}

// TestMaterializeGates_RegistersEveryDeclaredSite is the engine half: every one
// of those declarations becomes a gate the phase runner actually visits.
func TestMaterializeGates_RegistersEveryDeclaredSite(t *testing.T) {
	p := planPositionedGates(t)
	gm := verify.NewGateManager()
	require.NoError(t, materializeStepGates(gm, p, GateRuntime{}))

	names := func(phase verify.GatePhase) []string {
		var out []string
		for _, g := range gm.Gates(phase) {
			out = append(out, g.Name())
		}
		return out
	}

	pre := names(verify.PhasePreApply)
	assert.Contains(t, pre, "workflow:pre:0", "a workflow-level pre_apply check must run before the change")

	batch := names(verify.PhasePostBatch)
	assert.Contains(t, batch, "workflow:batch:0", "a workflow-level post_batch check must run after every batch")
	assert.Contains(t, batch, "batches:batch:0", "batches.gate must run after every batch")
	assert.Contains(t, batch, "step:restart:batch:0", "a step check declared post_batch must not be demoted")

	post := names(verify.PhasePostApply)
	assert.Contains(t, post, "workflow:post:0", "a workflow-level post_apply check must run at the end")
}

// TestPlanGateBlockers_SeesWorkflowLevelDeclarations covers the refusal half:
// the same judge must notice a workflow-level human check with no approver, or
// the plan yields an artifact that promises a gate nobody can run.
func TestPlanGateBlockers_SeesWorkflowLevelDeclarations(t *testing.T) {
	wf, err := dsl.NewParser().ParseBytes([]byte(`name: wf-human-gate
version: "1.0"
target:
  type: host
  hosts: ["web-1"]
gates:
  - position: post_apply
    human:
      message: "confirm rollout"
steps:
  - name: restart
    action: svc.restart
`))
	require.NoError(t, err)
	p, err := plan.NewGenerator().Generate(wf, []string{"web-1"})
	require.NoError(t, err)

	blockers := PlanGateBlockers(p, GateRuntime{})
	require.Len(t, blockers, 1, "a workflow-level human gate with no approver is a blocker")
	assert.Contains(t, blockers[0], "workflow:post:0", blockers[0])
	assert.Contains(t, blockers[0], "requires an approver", blockers[0])
}

// TestSloGateAtPostBatchIsExecutable post_batch phase, once a YAML route
// reaches it, is no longer a declaration that cannot be honoured: with a
// Prometheus endpoint wired, an slo check there materialises.
func TestSloGateAtPostBatchIsExecutable(t *testing.T) {
	wf, err := dsl.NewParser().ParseBytes([]byte(`name: slo-post-batch
version: "1.0"
target:
  type: host
  hosts: ["web-1"]
gates:
  - position: post_batch
    slo:
      query: "rate(node_load1[5m])"
      source: prometheus
    params:
      threshold: 4
      comparison: lte
steps:
  - name: restart
    action: svc.restart
`))
	require.NoError(t, err)
	p, err := plan.NewGenerator().Generate(wf, []string{"web-1"})
	require.NoError(t, err)

	assert.Empty(t, PlanGateBlockers(p, GateRuntime{PrometheusURL: "http://prom:9090"}),
		"slo at post_batch with a Prometheus endpoint must be executable")

	gm := verify.NewGateManager()
	require.NoError(t, materializeStepGates(gm, p, GateRuntime{PrometheusURL: "http://prom:9090"}))
	var found []string
	for _, g := range gm.Gates(verify.PhasePostBatch) {
		found = append(found, g.Name())
	}
	assert.Equal(t, []string{"workflow:batch:0"}, found)

	// The other half of the same judge stays true: no endpoint, no gate.
	blockers := PlanGateBlockers(p, GateRuntime{})
	require.Len(t, blockers, 1)
	assert.Contains(t, blockers[0], "requires verify.prometheus_url")
}
