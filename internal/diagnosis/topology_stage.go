package diagnosis

// topology_stage.go wires the topology (service dependency graph) analysis
// into the diagnosis pipeline — flow step 3d of the OpsMesh integration
// design: "拓扑分析：从 SkyWalking/Pinpoint 获取调用链，分析影响半径".
//
// The stage is an OPTIONAL third evidence source beside the log pipeline and
// the health probe. It is deliberately absent-by-default (nil source): a
// deployment without an APM backend behaves exactly as before, and a
// collect failure is recorded on report.Errors without failing the run — the
// same best-effort contract as the other stages. A target that simply is not
// in the graph is a silent skip, not an error (not every host is an
// APM-instrumented service).

import (
	"context"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/diagnosis/topology"
)

// TopologySource supplies the service dependency graph behind a diagnosis
// target. The SkyWalking and Pinpoint collectors are adapted by
// NewTopologyCollectorSource; target is passed as a lookup hint only — the
// engine matches the pivot node itself, so returning the full graph is fine.
type TopologySource interface {
	CollectTopology(ctx context.Context, target string, tr topology.TimeRange) (*topology.Topology, error)
}

// topologyCollectorSource adapts a topology.Collector to TopologySource.
type topologyCollectorSource struct {
	collector topology.Collector
}

// NewTopologyCollectorSource adapts a topology.Collector (SkyWalking,
// Pinpoint) to the engine's TopologySource. Nil collector yields nil
// (disabling the stage) so callers can pass config-derived collectors
// unconditionally.
func NewTopologyCollectorSource(c topology.Collector) TopologySource {
	if c == nil {
		return nil
	}
	return topologyCollectorSource{collector: c}
}

func (s topologyCollectorSource) CollectTopology(ctx context.Context, _ string, tr topology.TimeRange) (*topology.Topology, error) {
	return s.collector.Collect(ctx, tr)
}

// runTopologyStage collects the graph for the engine's look-back window and
// analyses the impact radius around the target. A nil report with nil error
// means "target not present in the topology" — a benign skip.
func (e *DiagEngine) runTopologyStage(ctx context.Context, target string) (*topology.ImpactReport, error) {
	if e.topology == nil || target == "" {
		return nil, nil
	}
	now := time.Now().UTC()
	tr := topology.TimeRange{Start: now.Add(-e.window), End: now}
	graph, err := e.topology.CollectTopology(ctx, target, tr)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", e.topologyName(), err)
	}
	return topology.ImpactRadius(graph, target, 0), nil
}

// topologyName names the configured source for error messages. The adapter
// knows its collector's name; anything else is reported generically.
func (e *DiagEngine) topologyName() string {
	if s, ok := e.topology.(topologyCollectorSource); ok {
		return "topology/" + s.collector.Name()
	}
	return "topology"
}

// topologyFindings turns an impact-radius analysis into report findings:
// one finding per unhealthy edge touching the pivot — a materially failing
// dependency is the most actionable signal a topology can give (critical at
// >= 50% error rate, warning above the analysis threshold) — plus one info
// finding summarising the blast radius so the operator sees, at a glance,
// who this service can affect and what it depends on.
func topologyFindings(impact *topology.ImpactReport) []Finding {
	if impact == nil {
		return nil
	}
	var out []Finding
	for _, u := range impact.UnhealthyEdges {
		other := u.Edge.Target
		if u.Direction == "inbound" {
			// Callers of the pivot are seeing errors; name the caller.
			other = u.Edge.Source
		}
		severity := "warning"
		if u.ErrorRate >= 0.5 {
			severity = "critical"
		}
		out = append(out, Finding{
			ID:       fmt.Sprintf("TOPO-%s-%s", u.Edge.Source, u.Edge.Target),
			Category: "service",
			Severity: severity,
			Title:    fmt.Sprintf("unhealthy %s dependency: %s", u.Direction, other),
			Description: fmt.Sprintf(
				"Service graph edge %s → %s is at %.0f%% error rate (%d/%d calls in the window); the target's symptoms may originate here.",
				u.Edge.Source, u.Edge.Target, u.ErrorRate*100, u.Edge.Metric.ErrorCount, u.Edge.Metric.CallCount),
		})
	}
	out = append(out, Finding{
		ID:       "TOPO-RADIUS",
		Category: "service",
		Severity: "info",
		Title:    "service impact radius",
		Description: fmt.Sprintf(
			"service %q: %d upstream caller(s) (%s), %d downstream callee(s) (%s); graph:%d nodes/%d edges.",
			impact.Pivot.Name,
			len(impact.Upstream), topology.NodeNames(impact.Upstream, 5),
			len(impact.Downstream), topology.NodeNames(impact.Downstream, 5),
			impact.TotalNodes, impact.TotalEdges),
	})
	return out
}
