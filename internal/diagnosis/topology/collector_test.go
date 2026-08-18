package topology

// collector_test.go exercises the unified Topology model and helper methods
// declared in collector.go. The collectors themselves are tested in their
// own _test.go files; this file focuses on the value-object behaviour that
// every backend must satisfy.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTopology_BasicOperations covers AddNode/AddEdge, the NodeByID lookup
// and the EdgesFrom/EdgesTo/TotalCallCount aggregators. It also verifies
// EdgeMetric.ErrorRate for the no-traffic and partial-error cases.
func TestTopology_BasicOperations(t *testing.T) {
	topo := &Topology{}

	topo.AddNode(Node{ID: "a", Name: "svc-a", Type: "HTTP", Endpoint: "a:8080"})
	topo.AddNode(Node{ID: "b", Name: "svc-b", Type: "RPC", Endpoint: "b:9090"})
	topo.AddNode(Node{ID: "c", Name: "db-c", Type: "DATABASE", Endpoint: "c:5432"})

	topo.AddEdge(Edge{Source: "a", Target: "b", Metric: EdgeMetric{CallCount: 100, ErrorCount: 5, AvgLatency: 12.5}})
	topo.AddEdge(Edge{Source: "b", Target: "c", Metric: EdgeMetric{CallCount: 80, ErrorCount: 0, AvgLatency: 3.2}})
	topo.AddEdge(Edge{Source: "a", Target: "c", Metric: EdgeMetric{CallCount: 20, ErrorCount: 20, AvgLatency: 9.9}})

	// NodeByID: existing and missing.
	n, ok := topo.NodeByID("b")
	require.True(t, ok)
	assert.Equal(t, "svc-b", n.Name)
	assert.Equal(t, "RPC", n.Type)

	_, ok = topo.NodeByID("missing")
	assert.False(t, ok)

	// EdgesFrom returns every outgoing edge of the given node.
	fromA := topo.EdgesFrom("a")
	require.Len(t, fromA, 2)
	assert.ElementsMatch(t, []string{"b", "c"}, []string{fromA[0].Target, fromA[1].Target})

	fromB := topo.EdgesFrom("b")
	require.Len(t, fromB, 1)
	assert.Equal(t, "c", fromB[0].Target)

	assert.Empty(t, topo.EdgesFrom("c"))

	// EdgesTo returns every incoming edge of the given node.
	toC := topo.EdgesTo("c")
	require.Len(t, toC, 2)
	assert.ElementsMatch(t, []string{"b", "a"}, []string{toC[0].Source, toC[1].Source})

	toA := topo.EdgesTo("a")
	assert.Empty(t, toA)

	// TotalCallCount sums every edge.
	assert.Equal(t, int64(200), topo.TotalCallCount())

	// ErrorRate: partial errors, no errors and no traffic.
	partial := EdgeMetric{CallCount: 100, ErrorCount: 5}
	assert.InDelta(t, 0.05, partial.ErrorRate(), 1e-9)

	noErrors := EdgeMetric{CallCount: 80, ErrorCount: 0}
	assert.InDelta(t, 0.0, noErrors.ErrorRate(), 1e-9)

	noTraffic := EdgeMetric{CallCount: 0, ErrorCount: 0}
	assert.InDelta(t, 0.0, noTraffic.ErrorRate(), 1e-9)

	allErrors := EdgeMetric{CallCount: 20, ErrorCount: 20}
	assert.InDelta(t, 1.0, allErrors.ErrorRate(), 1e-9)
}

// TestTimeRange_DurationMillis verifies the millisecond conversion used to
// build APM-specific duration parameters.
func TestTimeRange_DurationMillis(t *testing.T) {
	tr := TimeRange{
		Start: time.UnixMilli(0),
		End:   time.UnixMilli(1500),
	}
	assert.Equal(t, int64(1500), tr.DurationMillis())
}