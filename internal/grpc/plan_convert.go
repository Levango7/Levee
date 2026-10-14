package grpc

// plan_convert.go turns the canonical domain plan into the client-facing pb
// message. It lives here (not in internal/wiring) because BOTH the planning
// path (wiring.GeneratePlan) and the read path (ChangeService.GetChange with
// include_plan) need it, and internal/wiring already depends on internal/grpc —
// a grpc→wiring call would close a cycle. Relocating the single converter to
// grpc keeps one source of truth (wiring calls back into grpc.PlanToPB) instead
// of the two packages each keeping a copy that could drift on the impact-summary
// wording or the de-duplicated target set.

import (
	"fmt"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"
)

// PlanToPB converts the canonical plan into the client-facing pb message.
func PlanToPB(changeID string, p *plan.Plan) *pb.Plan {
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
