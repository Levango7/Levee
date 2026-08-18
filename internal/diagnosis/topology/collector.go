
package topology

// collector.go implements the unified topology model for LEVEE's distributed
// diagnosis subsystem (Phase D1). A Topology is a directed graph of service
// Nodes connected by Edges that describe call dependencies together with
// aggregate traffic metrics.
//
// The model is intentionally APM-agnostic: SkyWalkingCollector and
// PinpointCollector both project their vendor-specific representations onto
// the same Topology/Node/Edge types so that downstream consumers (root-cause
// analysis, blast-radius estimation, visualisation) never need to know which
// backend produced the data.
//
// Collector is the seam through which the engine acquires a topology. Each
// concrete collector wraps an *http.Client so transports can be injected for
// testing (httptest.Server) and so collectors share retry/timeout policy with
// the rest of the binary.
//
// All types in this file are plain value objects; they are safe for concurrent
// use by any number of goroutines once constructed.

import (
	"context"
	"time"
)

// Topology represents a service dependency graph.
//
// The graph is stored as parallel slices rather than an adjacency map so that
// the structure serialises cleanly to JSON and can be streamed without
// resolving pointers. Lookups should be performed through the helper methods
// on *Topology (NodeByID, EdgesFrom, EdgesTo).
type Topology struct {
	Nodes []Node
	Edges []Edge
}

// Node represents a service instance in the topology.
//
// ID is the APM-native identifier of the node and is used as the Source/Target
// reference on Edge. Name is the human-readable service name. Type classifies
// the service (e.g. "HTTP", "RPC", "DATABASE", "MQ"). Endpoint is the
// host:port the service listens on, or empty when the APM does not report it.
// Metadata carries vendor-specific extras (agent id, language, cluster, …).
type Node struct {
	ID       string
	Name     string
	Type     string
	Endpoint string
	Metadata map[string]string
}

// Edge represents a dependency relationship between two nodes.
//
// Source and Target are node IDs. The direction follows the call flow:
// Source calls Target. Metric aggregates the observed traffic over the
// TimeRange passed to Collector.Collect.
type Edge struct {
	Source string
	Target string
	Metric EdgeMetric
}

// EdgeMetric holds call statistics between two nodes.
//
// AvgLatency is expressed in milliseconds. CallCount and ErrorCount are
// monotonic counters over the collection window. ErrorRate is derived
// lazily through EdgeMetric.ErrorRate.
type EdgeMetric struct {
	CallCount  int64
	ErrorCount int64
	AvgLatency float64
}

// ErrorRate returns the observed error ratio in [0, 1]. It returns 0 when
// there were no calls to avoid a divide-by-zero; callers that need to
// distinguish "no traffic" from "no errors" should check CallCount first.
func (m EdgeMetric) ErrorRate() float64 {
	if m.CallCount == 0 {
		return 0
	}
	return float64(m.ErrorCount) / float64(m.CallCount)
}

// Collector is the interface for topology collectors.
//
// Implementations must be safe for concurrent use: the engine may share a
// single collector across goroutines when diagnosing multiple targets in
// parallel. Name returns a stable lowercase identifier ("skywalking",
// "pinpoint") used in logs and metrics. Collect fetches the topology for the
// supplied time window; implementations should honour ctx and abort promptly
// on cancellation.
type Collector interface {
	Name() string
	Collect(ctx context.Context, timeRange TimeRange) (*Topology, error)
}

// TimeRange specifies the time window for topology collection.
//
// Both bounds are inclusive. Start must be before End; collectors are not
// required to validate this and may return an empty topology or an error if
// the window is degenerate. Callers should use TimeRange.Duration to obtain
// the window length in milliseconds for APM-specific duration parameters.
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// DurationMillis returns the length of the window in milliseconds. It is the
// form most APM APIs expect (SkyWalking Duration, Pinpoint "to" - "from").
func (tr TimeRange) DurationMillis() int64 {
	return tr.End.Sub(tr.Start).Milliseconds()
}

// --- Topology helpers ------------------------------------------------------

// NodeByID returns the node with the given id and true, or a zero Node and
// false when no such node exists. The lookup is linear; topologies produced
// by the collectors in this package are small (tens to low hundreds of
// nodes) so a map is not warranted.
func (t *Topology) NodeByID(id string) (Node, bool) {
	for i := range t.Nodes {
		if t.Nodes[i].ID == id {
			return t.Nodes[i], true
		}
	}
	return Node{}, false
}

// AddNode appends n to the topology. Duplicate IDs are not merged; callers
// are expected to de-duplicate upstream when merging multiple windows.
func (t *Topology) AddNode(n Node) {
	t.Nodes = append(t.Nodes, n)
}

// AddEdge appends e to the topology.
func (t *Topology) AddEdge(e Edge) {
	t.Edges = append(t.Edges, e)
}

// EdgesFrom returns every edge whose Source equals nodeID.
func (t *Topology) EdgesFrom(nodeID string) []Edge {
	var out []Edge
	for i := range t.Edges {
		if t.Edges[i].Source == nodeID {
			out = append(out, t.Edges[i])
		}
	}
	return out
}

// EdgesTo returns every edge whose Target equals nodeID.
func (t *Topology) EdgesTo(nodeID string) []Edge {
	var out []Edge
	for i := range t.Edges {
		if t.Edges[i].Target == nodeID {
			out = append(out, t.Edges[i])
		}
	}
	return out
}

// TotalCallCount sums CallCount across all edges. It is used by the engine to
// grade topology richness before running expensive graph analyses.
func (t *Topology) TotalCallCount() int64 {
	var total int64
	for i := range t.Edges {
		total += t.Edges[i].Metric.CallCount
	}
	return total
}