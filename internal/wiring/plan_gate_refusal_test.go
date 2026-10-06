package wiring

// plan_gate_refusal_test.go covers the plan-time half of the executability
// rule: a workflow declaring a gate this process cannot run is refused before
// any target is touched, instead of dying inside RunPhase after earlier batches
// already changed them.
//
// The pair that matters is "refused with nothing wired" and "accepted once the
// capability is wired". Only the first half would make the rule a ban on the
// gate type rather than a statement about the deployment.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/engine"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/verify"
)

// acceptApprover is a wired-but-inert approver transport: its only job is to
// make "an approver exists" expressible, exactly like a deployment that installed
// a chat-ops transport would.
type acceptApprover struct{}

func (acceptApprover) RequestAndWait(_ context.Context, _, _, _ string) (verify.HumanDecision, error) {
	return verify.HumanDecision{Approved: true}, nil
}

const humanGatedWorkflow = `name: human-gated
version: "1.0"
target:
  type: host
  hosts: ["web-1"]
steps:
  - name: restart
    action: svc.restart
    verify:
      human:
        message: "确认后继续"
`

const sloGatedWorkflow = `name: slo-gated
version: "1.0"
target:
  type: host
  hosts: ["web-1"]
steps:
  - name: restart
    action: svc.restart
    verify:
      slo:
        query: "rate(node_load1[5m]) < 4"
        source: prometheus
`

// sloPostBatchWorkflow is the same slo check declared where the refusal says it
// executes. Its only difference from sloGatedWorkflow is the position, which is
// exactly the advice under test.
const sloPostBatchWorkflow = `name: slo-post-batch
version: "1.0"
target:
  type: host
  hosts: ["web-1"]
batches:
  strategy: one-per-target
steps:
  - name: restart
    action: svc.restart
    verify:
      position: post_batch
      slo:
        query: "rate(node_load1[5m])"
        source: prometheus
      params:
        threshold: 4
        comparison: lte
`

const cmdGatedWorkflow = `name: cmd-gated
version: "1.0"
target:
  type: host
  hosts: ["web-1"]
steps:
  - name: restart
    action: svc.restart
    verify:
      cmd:
        run: "systemctl is-active nginx"
        expect_exit: 0
`

func planWorkflow(t *testing.T, src string, opts ...Option) error {
	t.Helper()
	_, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-gate", src)
	eng := NewEngine(store, opts...)
	_, _, err := eng.GeneratePlan(context.Background(), "run-gate", []string{"web-1"})
	return err
}

func TestGeneratePlan_RefusesHumanGateWithoutApprover(t *testing.T) {
	err := planWorkflow(t, humanGatedWorkflow)
	require.Error(t, err, "a human gate with no approver wired must not yield a plan")
	assert.True(t, errors.Is(err, engine.ErrGateNotExecutable),
		"the refusal must be matchable as a class, got: %v", err)
	assert.Contains(t, err.Error(), "requires an approver", err.Error())
	assert.Contains(t, err.Error(), "step:restart:post:0",
		"the refusal must name the offending gate, got: %v", err)
}

// TestGeneratePlan_AcceptsHumanGateOnceApproverIsWired is the other half of the
// rule, and the reason it is not a ban on human gates.
func TestGeneratePlan_AcceptsHumanGateOnceApproverIsWired(t *testing.T) {
	err := planWorkflow(t, humanGatedWorkflow, WithGateApprover(acceptApprover{}))
	require.NoError(t, err, "with an approver installed the same workflow must plan")
}

// TestGeneratePlan_RefusesSloAndNamesTheRealRemediation guards the message, not
// just the verdict, and the message must name something that works. An slo check
// only executes in the post_batch phase, so the refusal has to say where to put
// it — and that route must exist. It did not: convertGate filed every
// declaration in GateSpec.Post, so the honest advice back then was "no YAML path
// reaches that slot". Position routing is fixed now, so the test also drives the
// remediation it advertises: the same check declared with position: post_batch
// plans.
func TestGeneratePlan_RefusesSloAndNamesTheRealRemediation(t *testing.T) {
	err := planWorkflow(t, sloGatedWorkflow, WithGatePrometheusURL("http://prom:9090"))
	require.Error(t, err, "an slo check outside post_batch is not executable")
	assert.True(t, errors.Is(err, engine.ErrGateNotExecutable), "%v", err)
	assert.Contains(t, err.Error(), "post_batch phase only", err.Error())
	assert.Contains(t, err.Error(), "declare it with position: post_batch",
		"the refusal must name the declaration that actually reaches the phase, got: %v", err)
}

// TestGeneratePlan_AcceptsSloOnceDeclaredInPostBatch is the other side of that
// advice: with the position the message names, and an endpoint wired, the plan
// is produced.
func TestGeneratePlan_AcceptsSloOnceDeclaredInPostBatch(t *testing.T) {
	require.NoError(t, planWorkflow(t, sloPostBatchWorkflow, WithGatePrometheusURL("http://prom:9090")))
}

// TestGeneratePlan_RefusesSloAtPostBatchWithoutAnEndpoint keeps the position fix
// from turning slo into something that can guess: the same declaration is still
// refused when no Prometheus endpoint is wired.
func TestGeneratePlan_RefusesSloAtPostBatchWithoutAnEndpoint(t *testing.T) {
	err := planWorkflow(t, sloPostBatchWorkflow)
	require.Error(t, err, "an slo check with no Prometheus endpoint must not plan")
	assert.True(t, errors.Is(err, engine.ErrGateNotExecutable), "%v", err)
	assert.Contains(t, err.Error(), "requires verify.prometheus_url", err.Error())
}

// TestGeneratePlan_CmdAndProbeGatesAreNeverRefused keeps the refusal from
// becoming a blanket gate ban: cmd and probe checks carry every knob themselves
// and must plan against the zero-value runtime.
func TestGeneratePlan_CmdAndProbeGatesAreNeverRefused(t *testing.T) {
	require.NoError(t, planWorkflow(t, cmdGatedWorkflow),
		"a cmd gate needs no deployment capability and must plan")
}

// TestGeneratePlan_RefusalLeavesNoPlanArtifact pins the side-effect-free
// property: the plan is refused before persistence, so the run stays plannable
// once the operator wires the capability (a half-written plan would otherwise
// bind an approval to a plan that can never execute).
func TestGeneratePlan_RefusalLeavesNoPlanArtifact(t *testing.T) {
	_, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-artifact", humanGatedWorkflow)
	eng := NewEngine(store)

	p, stored, err := eng.GeneratePlan(context.Background(), "run-artifact", []string{"web-1"})
	require.Error(t, err)
	assert.Nil(t, p)
	assert.Nil(t, stored)

	run, err := store.GetRun(context.Background(), "run-artifact")
	require.NoError(t, err)
	assert.Empty(t, run.PlanJSON, "a refused plan must not be persisted on the run")
	assert.Empty(t, run.PlanHash)
}

// TestGateRefusalHasExactlyOneEnforcementPoint mirrors
// TestWindowGateHasExactlyOneEnforcementPoint for the same reason: the refusal
// only protects operators if it lives in the one funnel every plan goes through.
// A second call site (e.g. inside the gRPC layer) would be satisfiable by one
// entry point and silently miss the others, and rollback must never be put
// behind it — a failed change has to stay recoverable.
func TestGateRefusalHasExactlyOneEnforcementPoint(t *testing.T) {
	hits := map[string]int{}
	rollbackBody := ""

	for _, root := range []string{"../../internal", "../../cmd"} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel := relativePOSIX(path)
			if n := strings.Count(string(src), "engine.PlanGateBlockers("); n > 0 {
				hits[rel] = n
			}
			if strings.HasSuffix(rel, "internal/wiring/run.go") {
				rollbackBody = functionBody(string(src), "func (e *Engine) rollbackChange")
			}
			return nil
		})
	}

	assert.Equal(t, map[string]int{"internal/wiring/plan.go": 1}, hits,
		"PlanGateBlockers must be consulted by exactly one production call site: the plan funnel")
	require.NotEmpty(t, rollbackBody, "rollbackChange must be found to prove it stays outside the gate")
	assert.NotContains(t, rollbackBody, "GeneratePlan",
		"rollback loads the stored plan; regenerating it would put recovery behind this refusal")
	assert.NotContains(t, rollbackBody, "PlanGateBlockers")
}

// TestGateRuntimeIsBuiltInOnePlace is the anti-drift guard for the wiring itself:
// if a second GateRuntime literal appears, the plan-time judgement and the
// phase-time materialisation can disagree again — which is the bug being fixed.
func TestGateRuntimeIsBuiltInOnePlace(t *testing.T) {
	hits := map[string]int{}
	_ = filepath.Walk("../../internal", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		// engine.GateRuntime{...} construction outside the engine package itself.
		// Only Engine.gateRuntime's own body may appear, in internal/wiring/wiring.go.
		if n := strings.Count(string(src), "engine.GateRuntime{"); n > 0 {
			hits[relativePOSIX(path)] = n
		}
		return nil
	})
	assert.Equal(t, map[string]int{"internal/wiring/wiring.go": 1}, hits,
		"the gate runtime must be built by Engine.gateRuntime alone (one literal), found: %v", hits)
}

// TestPlanChange_ReportsGateRefusalAsFailedPrecondition is the third segment of
// the wiring: the sentinel exists, GeneratePlan returns it, and the gRPC layer
// must translate it. Left unmapped it would surface as codes.Internal — a server
// fault the operator cannot act on, for what is a plain configuration gap. The
// companion case below proves the same call succeeds once wired, so the mapping
// is not just swallowing everything.
func TestPlanChange_ReportsGateRefusalAsFailedPrecondition(t *testing.T) {
	ctx := context.Background()
	_, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-grpc", humanGatedWorkflow)
	eng := NewEngine(store)
	svc := grpc.NewChangeService(store, eng.Adapter(), nil, nil)

	_, err := svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId:    "run-grpc",
		TargetHosts: []string{"web-1"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Convert(err).Code(),
		"a gate refusal must not surface as Internal, got: %v", err)
	assert.Contains(t, err.Error(), "requires an approver")
}

func TestPlanChange_HumanGateSucceedsWithApprover(t *testing.T) {
	ctx := context.Background()
	_, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-grpc-ok", humanGatedWorkflow)
	eng := NewEngine(store, WithGateApprover(acceptApprover{}))
	svc := grpc.NewChangeService(store, eng.Adapter(), nil, nil)

	p, err := svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId:    "run-grpc-ok",
		TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)
	require.NotNil(t, p)

	run, err := store.GetRun(ctx, "run-grpc-ok")
	require.NoError(t, err)
	assert.NotEmpty(t, run.PlanJSON, "a plan whose gates are executable must persist as usual")
}

var _ verify.HumanApprover = acceptApprover{}
