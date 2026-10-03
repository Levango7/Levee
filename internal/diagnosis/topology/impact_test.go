package topology

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nodeNames(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

// sampleGraph: gateway → order-service → {orders-db, reporting}.
// The gateway→orders edge is 30% errors; orders→reporting is 90%.
func sampleGraph() *Topology {
	return &Topology{
		Nodes: []Node{
			{ID: "gateway", Name: "api-gateway"},
			{ID: "orders", Name: "order-service", Endpoint: "orders-1.prod:8080"},
			{ID: "db", Name: "orders-db"},
			{ID: "reporting", Name: "reporting"},
		},
		Edges: []Edge{
			{Source: "gateway", Target: "orders", Metric: EdgeMetric{CallCount: 100, ErrorCount: 30}},
			{Source: "orders", Target: "db", Metric: EdgeMetric{CallCount: 200, ErrorCount: 10}},
			{Source: "orders", Target: "reporting", Metric: EdgeMetric{CallCount: 10, ErrorCount: 9}},
		},
	}
}

func TestImpactRadius_AnalysesPivotAndFlagsUnhealthyEdges(t *testing.T) {
	rep := ImpactRadius(sampleGraph(), "orders-1.prod:8080", 0)
	require.NotNil(t, rep, "the pivot must be found by endpoint match")

	assert.Equal(t, "order-service", rep.Pivot.Name)
	assert.Equal(t, []string{"api-gateway"}, nodeNames(rep.Upstream))
	assert.Equal(t, []string{"orders-db", "reporting"}, nodeNames(rep.Downstream))
	assert.Equal(t, 4, rep.TotalNodes)
	assert.Equal(t, 3, rep.TotalEdges)

	// 30% (gateway→orders, inbound) and 90% (orders→reporting, outbound)
	// cross the default threshold; 5% (orders→db) does not. Sorted by rate
	// descending.
	require.Len(t, rep.UnhealthyEdges, 2)
	assert.Equal(t, "reporting", rep.UnhealthyEdges[0].Edge.Target)
	assert.InDelta(t, 0.9, rep.UnhealthyEdges[0].ErrorRate, 0.0001)
	assert.Equal(t, "outbound", rep.UnhealthyEdges[0].Direction)
	assert.Equal(t, "inbound", rep.UnhealthyEdges[1].Direction)
}

func TestImpactRadius_UnknownTargetIsNilNotError(t *testing.T) {
	assert.Nil(t, ImpactRadius(sampleGraph(), "not-in-graph", 0),
		"a target absent from the topology is a benign skip")
	assert.Nil(t, ImpactRadius(nil, "anything", 0))
	assert.Nil(t, ImpactRadius(sampleGraph(), "", 0))
}

func TestImpactRadius_ThresholdIsConfigurable(t *testing.T) {
	// At a 95% threshold only the 90% edge… is below it too — nothing
	// qualifies; at 5% the healthy 5% edge sits exactly on the boundary and
	// IS included (>= comparison).
	strict := ImpactRadius(sampleGraph(), "orders-1.prod:8080", 0.95)
	require.NotNil(t, strict)
	assert.Empty(t, strict.UnhealthyEdges)

	loose := ImpactRadius(sampleGraph(), "orders-1.prod:8080", 0.05)
	require.NotNil(t, loose)
	assert.Len(t, loose.UnhealthyEdges, 3)
}

func TestFindNode_MatchPrecedence(t *testing.T) {
	g := sampleGraph()
	if n, ok := g.FindNode("orders"); !ok || n.ID != "orders" {
		t.Fatalf("exact ID match failed: %+v ok=%v", n, ok)
	}
	if n, ok := g.FindNode("ORDERS-DB"); !ok || n.ID != "db" {
		t.Fatalf("case-insensitive name match failed: %+v ok=%v", n, ok)
	}
	if n, ok := g.FindNode("orders-1.prod"); !ok || n.ID != "orders" {
		t.Fatalf("endpoint substring match failed: %+v ok=%v", n, ok)
	}
	if _, ok := g.FindNode("nope"); ok {
		t.Fatal("unexpected match for absent node")
	}
}
