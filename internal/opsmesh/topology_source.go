package opsmesh

// topology_source.go adapts the platform's catalog graph to the diagnosis
// topology stage's source contract. The diagnosis engine (internal/diagnosis)
// expects CollectTopology(ctx, target, topology.TimeRange); the catalog is
// current-state rather than windowed and returns the whole tenant graph, so
// target and time range are accepted for interface conformance and ignored —
// the engine matches its own pivot via topology.FindNode, exactly as it does
// for the SkyWalking/Pinpoint collectors.

import (
	"context"

	"github.com/nexus/levee/internal/diagnosis/topology"
)

// TopologySource implements the diagnosis engine's topology source over the
// OpsMesh catalog API.
type TopologySource struct {
	client   *OpsMeshClient
	tenantID string
}

// NewTopologySource wraps client for the diagnosis topology stage. A nil
// client yields nil (the stage stays disabled — the same contract as
// diagnosis.NewTopologyCollectorSource). An empty tenantID omits the
// parameter and lets the platform apply its default tenant.
func NewTopologySource(c *OpsMeshClient, tenantID string) *TopologySource {
	if c == nil {
		return nil
	}
	return &TopologySource{client: c, tenantID: tenantID}
}

// Name identifies the source in engine logs ("topology/opsmesh").
func (s *TopologySource) Name() string { return "opsmesh" }

// CollectTopology fetches the catalog graph for the configured tenant.
func (s *TopologySource) CollectTopology(ctx context.Context, _ string, _ topology.TimeRange) (*topology.Topology, error) {
	graph, err := s.client.GetTopology(ctx, s.tenantID)
	if err != nil {
		return nil, err
	}
	return ConvertCatalogGraph(graph), nil
}

// ConvertCatalogGraph projects the platform's catalog graph onto the unified
// topology model:
//
//   - node status lands in Metadata["status"] (the diagnosis findings surface
//     it as `name[status]`), and the platform's metadata map is copied
//     verbatim — pivot matching then works through topology.FindNode's
//     name/ID/endpoint/metadata-value containment without this converter
//     having to guess which metadata key holds a hostname.
//   - edges carry NO traffic metric: the catalog is a relationship graph, not
//     a call graph with SLAs. ImpactRadius therefore reports structure
//     (upstream/downstream) and unhealthy-edge findings simply never fire for
//     this source — correct, because there is no error-rate evidence to
//     report.
func ConvertCatalogGraph(g *CatalogGraph) *topology.Topology {
	if g == nil {
		return nil
	}
	out := &topology.Topology{
		Nodes: make([]topology.Node, 0, len(g.Nodes)),
		Edges: make([]topology.Edge, 0, len(g.Edges)),
	}
	for _, n := range g.Nodes {
		meta := make(map[string]string, len(n.Metadata)+1)
		for k, v := range n.Metadata {
			meta[k] = v
		}
		if n.Status != "" {
			meta["status"] = n.Status
		}
		out.Nodes = append(out.Nodes, topology.Node{
			ID:       n.ID,
			Name:     n.Name,
			Type:     n.Type,
			Metadata: meta,
		})
	}
	for _, e := range g.Edges {
		out.Edges = append(out.Edges, topology.Edge{Source: e.From, Target: e.To})
	}
	return out
}
