package topology

// impact.go implements the impact-radius analysis: given the service graph
// and a pivot service, report who depends on it (upstream callers), what it
// depends on (downstream callees) and which of those edges look unhealthy.
//
// This is the analysis step the OpsMesh integration design names as flow 3d
// ("拓扑分析：…分析影响半径") — until this the topology package could fetch
// graphs but nothing turned a graph into a diagnosis signal.

import (
	"fmt"
	"sort"
	"strings"
)

// ImpactReport summarises one service's position in the graph.
type ImpactReport struct {
	// Pivot is the node the analysis pivoted on.
	Pivot Node
	// Upstream lists the NODES that call the pivot (edge Source → pivot).
	Upstream []Node
	// Downstream lists the NODES the pivot calls (edge pivot → Target).
	Downstream []Node
	// UnhealthyEdges lists edges touching the pivot whose error rate is at
	// or above the analysis threshold, together with the rate.
	UnhealthyEdges []ImpactEdge
	// TotalNodes / TotalEdges describe the graph the analysis saw.
	TotalNodes int
	TotalEdges int
}

// ImpactEdge is an edge flagged as unhealthy by the analysis.
type ImpactEdge struct {
	Edge      Edge
	ErrorRate float64
	// Direction says which side of the pivot the failing edge is on:
	// "inbound" when callers of the pivot are seeing errors (edge Target ==
	// pivot), "outbound" when the pivot's own calls are failing (edge
	// Source == pivot).
	Direction string
}

// DefaultErrorRateThreshold is the error-rate above which an edge is
// reported as unhealthy when the caller does not supply one. 10% is high
// enough to ignore background noise on chatty RPCs and low enough that a
// materially broken dependency is never silent.
const DefaultErrorRateThreshold = 0.10

// FindNode returns the node that best matches needle: exact ID or Name
// first, then case-insensitive name equality, then a substring match on
// Name/Endpoint/Metadata values (APM platforms label instances with the
// host). ok is false when nothing matches.
func (t *Topology) FindNode(needle string) (Node, bool) {
	if t == nil || needle == "" {
		return Node{}, false
	}
	lower := strings.ToLower(needle)
	for _, n := range t.Nodes {
		if n.ID == needle || n.Name == needle {
			return n, true
		}
	}
	for _, n := range t.Nodes {
		if strings.EqualFold(n.Name, needle) {
			return n, true
		}
	}
	for _, n := range t.Nodes {
		if strings.Contains(strings.ToLower(n.Name), lower) ||
			strings.Contains(strings.ToLower(n.Endpoint), lower) {
			return n, true
		}
		for _, v := range n.Metadata {
			if strings.Contains(strings.ToLower(v), lower) {
				return n, true
			}
		}
	}
	return Node{}, false
}

// ImpactRadius analyses the graph around the node matching service. It
// returns nil when the graph has no matching node — the caller should treat
// that as "target not present in this topology", not as an error (not every
// host is an APM-instrumented service). threshold <= 0 applies
// DefaultErrorRateThreshold.
func ImpactRadius(t *Topology, service string, threshold float64) *ImpactReport {
	if t == nil {
		return nil
	}
	pivot, ok := t.FindNode(service)
	if !ok {
		return nil
	}
	if threshold <= 0 {
		threshold = DefaultErrorRateThreshold
	}

	byID := make(map[string]Node, len(t.Nodes))
	for _, n := range t.Nodes {
		byID[n.ID] = n
	}

	rep := &ImpactReport{
		Pivot:      pivot,
		TotalNodes: len(t.Nodes),
		TotalEdges: len(t.Edges),
	}
	for _, e := range t.Edges {
		switch {
		case e.Target == pivot.ID:
			if n, ok := byID[e.Source]; ok {
				rep.Upstream = append(rep.Upstream, n)
			}
			if e.Metric.ErrorRate() >= threshold && e.Metric.CallCount > 0 {
				rep.UnhealthyEdges = append(rep.UnhealthyEdges, ImpactEdge{
					Edge: e, ErrorRate: e.Metric.ErrorRate(), Direction: "inbound",
				})
			}
		case e.Source == pivot.ID:
			if n, ok := byID[e.Target]; ok {
				rep.Downstream = append(rep.Downstream, n)
			}
			if e.Metric.ErrorRate() >= threshold && e.Metric.CallCount > 0 {
				rep.UnhealthyEdges = append(rep.UnhealthyEdges, ImpactEdge{
					Edge: e, ErrorRate: e.Metric.ErrorRate(), Direction: "outbound",
				})
			}
		}
	}
	sort.Slice(rep.Upstream, func(i, j int) bool { return rep.Upstream[i].Name < rep.Upstream[j].Name })
	sort.Slice(rep.Downstream, func(i, j int) bool { return rep.Downstream[i].Name < rep.Downstream[j].Name })
	sort.Slice(rep.UnhealthyEdges, func(i, j int) bool {
		if rep.UnhealthyEdges[i].ErrorRate != rep.UnhealthyEdges[j].ErrorRate {
			return rep.UnhealthyEdges[i].ErrorRate > rep.UnhealthyEdges[j].ErrorRate
		}
		return rep.UnhealthyEdges[i].Edge.Source < rep.UnhealthyEdges[j].Edge.Source
	})
	return rep
}

// NodeNames renders up to max node names for a report line, appending "…"
// when the list is longer.
func NodeNames(nodes []Node, max int) string {
	if len(nodes) == 0 {
		return "(none)"
	}
	if max <= 0 || max > len(nodes) {
		max = len(nodes)
	}
	names := make([]string, 0, max)
	for _, n := range nodes[:max] {
		names = append(names, n.Name)
	}
	out := strings.Join(names, ", ")
	if len(nodes) > max {
		out = fmt.Sprintf("%s … (+%d)", out, len(nodes)-max)
	}
	return out
}
