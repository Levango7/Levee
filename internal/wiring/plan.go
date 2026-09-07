// plan.go implements plan generation and persistence artifacts: resolve the
// run's workflow source, verify the requested hosts exist in inventory,
// generate the canonical plan and hand back both the client-facing pb.Plan
// and the StoredPlan artifact ChangeService persists on the run.

package wiring

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
)

// GeneratePlan builds the plan for changeID over targetHosts and returns
// the client-facing pb.Plan plus the canonical StoredPlan artifact to
// persist (JSON = encoding/json of the plan.Plan; Hash = plan.ComputeHash).
//
// The workflow source is run.WorkflowName, which carries either the
// rendered workflow YAML (InstantiateTemplate) or a workflow file path
// (CreateChange). Targets must exist in the inventory; frozen hosts are
// rejected upstream (ChangeService.PlanChange) and re-checked at execution.
func (e *Engine) GeneratePlan(ctx context.Context, changeID string, targetHosts []string) (*pb.Plan, *grpc.StoredPlan, error) {
	run, err := e.store.GetRun(ctx, changeID)
	if err != nil {
		return nil, nil, fmt.Errorf("wiring: get run: %w", err)
	}
	if run == nil {
		return nil, nil, fmt.Errorf("wiring: change %q not found", changeID)
	}

	wf, err := resolveWorkflow(run)
	if err != nil {
		return nil, nil, err
	}

	targets, err := e.validateTargets(ctx, targetHosts)
	if err != nil {
		return nil, nil, err
	}

	p, err := plan.NewGenerator().Generate(wf, targets)
	if err != nil {
		return nil, nil, fmt.Errorf("wiring: generate plan: %w", err)
	}

	raw, err := json.Marshal(p)
	if err != nil {
		return nil, nil, fmt.Errorf("wiring: marshal plan: %w", err)
	}
	hash := plan.ComputeHash(p)
	if hash == "" {
		return nil, nil, fmt.Errorf("wiring: plan hash computation failed")
	}
	return planToPB(changeID, p), &grpc.StoredPlan{JSON: string(raw), Hash: hash}, nil
}

// resolveWorkflow parses the run's workflow source. WorkflowName holds the
// rendered YAML inline for template-instantiated runs and a file path for
// CreateChange runs; inline parse is attempted first, then (if the source
// also looks like an existing path) the file is parsed and its error is
// surfaced — so a corrupt inline document reports the parse failure, not a
// misleading "not a file".
func resolveWorkflow(run *state.Run) (*dsl.Workflow, error) {
	src := strings.TrimSpace(run.WorkflowName)
	if src == "" {
		return nil, fmt.Errorf("wiring: change %q has no workflow source", run.ID)
	}
	parser := dsl.NewParser()
	if wf, err := parser.ParseBytes([]byte(src)); err == nil {
		return wf, nil
	} else if !looksLikeWorkflowPath(src) {
		return nil, fmt.Errorf("wiring: parse inline workflow for change %q: %w", run.ID, err)
	}
	if _, statErr := os.Stat(src); statErr != nil {
		// Path-shaped but unreadable: report the inline parse failure and
		// the stat outcome so the operator sees both candidates.
		return nil, fmt.Errorf("wiring: change %q workflow source %q is neither valid inline YAML nor a readable file (inline: %v; stat: %v)",
			run.ID, src, statErr, statErr)
	}
	wf, err := parser.ParseFile(src)
	if err != nil {
		return nil, fmt.Errorf("wiring: parse workflow file %q for change %q: %w", src, run.ID, err)
	}
	return wf, nil
}

// looksLikeWorkflowPath reports whether the workflow source plausibly
// references a file rather than an inline document: YAML sources contain
// newlines or colon-space mapping markers; paths do not.
func looksLikeWorkflowPath(src string) bool {
	if strings.ContainsAny(src, "\n\r") {
		return false
	}
	// A single line like "steps: []" is YAML, not a path; require a path
	// separator or an extension marker to treat it as a file.
	if strings.ContainsAny(src, `/\`) {
		return true
	}
	i := strings.LastIndex(src, ".")
	return i > 0 && !strings.Contains(src, " ")
}

// validateTargets checks every requested host against the inventory and
// returns the de-duplicated, order-preserving host list. Unknown hosts and
// retired targets are refused; frozen hosts are handled by the caller
// (inventory.ValidateNotFrozen) and the execution-time host guard.
func (e *Engine) validateTargets(ctx context.Context, hosts []string) ([]string, error) {
	if len(hosts) == 0 {
		return nil, fmt.Errorf("wiring: target_hosts is required to plan a change")
	}
	all, err := e.store.ListTargets(ctx, state.TargetFilter{})
	if err != nil {
		return nil, fmt.Errorf("wiring: list inventory targets: %w", err)
	}
	byHost := make(map[string]*state.Target, len(all))
	for _, t := range all {
		if _, dup := byHost[t.Hostname]; !dup {
			byHost[t.Hostname] = t
		}
	}
	var unknown, retired []string
	out := make([]string, 0, len(hosts))
	seen := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if seen[h] {
			continue
		}
		seen[h] = true
		t, ok := byHost[h]
		if !ok {
			unknown = append(unknown, h)
			continue
		}
		if t.Status == "retired" {
			retired = append(retired, h)
			continue
		}
		out = append(out, h)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("wiring: targets not found in inventory: %s", strings.Join(unknown, ", "))
	}
	if len(retired) > 0 {
		return nil, fmt.Errorf("wiring: targets are retired: %s", strings.Join(retired, ", "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("wiring: no usable targets after inventory validation")
	}
	return out, nil
}

// planToPB converts the canonical plan into the client-facing pb message.
func planToPB(changeID string, p *plan.Plan) *pb.Plan {
	batches := make([]*pb.Batch, 0, len(p.Batches))
	for _, b := range p.Batches {
		batches = append(batches, &pb.Batch{
			Index:          int32(b.Index),
			Hosts:          append([]string(nil), b.Targets...),
			MaxConcurrency: int32(b.MaxConcurrency),
		})
	}
	out := &pb.Plan{
		ChangeId:    changeID,
		TargetHosts: inventoryTargets(p),
		Batches:     batches,
	}
	if r := plan.NewImpactAnalyzer().Analyze(p); r != nil {
		out.ImpactSummary = fmt.Sprintf("%d direct target(s), %d indirect, risk: %s",
			len(r.DirectTargets), len(r.IndirectTargets), r.RiskLevel)
	}
	return out
}

// inventoryTargets returns the de-duplicated global target set in plan order.
func inventoryTargets(p *plan.Plan) []string {
	seen := make(map[string]bool)
	var out []string
	for _, b := range p.Batches {
		for _, t := range b.Targets {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}
