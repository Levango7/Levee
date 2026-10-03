package diagnosis

// topology_stage_test.go — the optional topology stage: findings when the
// target is in the graph, a recorded-but-non-fatal error when the source
// fails, and a silent skip when the target is absent.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nexus/levee/internal/diagnosis/topology"
)

type fakeTopologySource struct {
	graph     *topology.Topology
	err       error
	gotTarget string
}

func (f *fakeTopologySource) CollectTopology(_ context.Context, target string, _ topology.TimeRange) (*topology.Topology, error) {
	f.gotTarget = target
	return f.graph, f.err
}

func graphAroundWeb1() *topology.Topology {
	return &topology.Topology{
		Nodes: []topology.Node{
			{ID: "lb", Name: "edge-lb"},
			{ID: "web", Name: "web-1"},
			{ID: "db", Name: "primary-db"},
		},
		Edges: []topology.Edge{
			{Source: "lb", Target: "web", Metric: topology.EdgeMetric{CallCount: 100, ErrorCount: 60}},
			{Source: "web", Target: "db", Metric: topology.EdgeMetric{CallCount: 100, ErrorCount: 2}},
		},
	}
}

func findFinding(report []Finding, id string) *Finding {
	for i := range report {
		if report[i].ID == id {
			return &report[i]
		}
	}
	return nil
}

func TestDiagEngine_TopologyStageAddsFindings(t *testing.T) {
	src := &fakeTopologySource{graph: graphAroundWeb1()}
	e := NewDiagEngine(DiagEngineConfig{Topology: src})

	report := e.Diagnose(context.Background(), "web-1")
	assert.Equal(t, "web-1", src.gotTarget, "the target must be passed to the source")

	// The 60% inbound edge is critical; the 2% outbound edge is not flagged.
	unhealthy := findFinding(report.Findings, "TOPO-lb-web")
	if unhealthy == nil {
		t.Fatalf("expected an unhealthy-edge finding, got %+v", report.Findings)
	}
	assert.Equal(t, "critical", unhealthy.Severity)
	assert.Equal(t, "service", unhealthy.Category)
	assert.Contains(t, unhealthy.Title, "inbound")
	assert.Contains(t, unhealthy.Description, "60%")
	assert.Nil(t, findFinding(report.Findings, "TOPO-web-db"), "healthy edges must not produce findings")

	radius := findFinding(report.Findings, "TOPO-RADIUS")
	if radius == nil {
		t.Fatalf("expected the impact-radius info finding, got %+v", report.Findings)
	}
	assert.Equal(t, "info", radius.Severity)
	assert.Contains(t, radius.Description, "1 upstream")
	assert.Contains(t, radius.Description, "primary-db")

	// Critical findings sort above info ones.
	if report.Findings[0].ID != "TOPO-lb-web" {
		t.Errorf("findings not severity-sorted: %v", report.Findings[0].ID)
	}
	// The stage failure path was not taken.
	for _, e := range report.Errors {
		if strings.Contains(e, "topology stage") {
			t.Errorf("unexpected topology error: %s", e)
		}
	}
}

func TestDiagEngine_TopologyStageCollectFailureIsRecordedNotFatal(t *testing.T) {
	src := &fakeTopologySource{err: errors.New("apm backend down")}
	e := NewDiagEngine(DiagEngineConfig{Topology: src})

	report := e.Diagnose(context.Background(), "web-1")
	found := false
	for _, rerr := range report.Errors {
		if strings.Contains(rerr, "topology stage: topology: apm backend down") {
			found = true
		}
	}
	assert.True(t, found, "the collect failure must be recorded on the report: %v", report.Errors)
	assert.Nil(t, findFinding(report.Findings, "TOPO-RADIUS"),
		"a failed collect contributes no findings")
}

func TestDiagEngine_TopologyStageSkipsTargetAbsentFromGraph(t *testing.T) {
	src := &fakeTopologySource{graph: graphAroundWeb1()}
	e := NewDiagEngine(DiagEngineConfig{Topology: src})

	report := e.Diagnose(context.Background(), "app-99")
	assert.Nil(t, findFinding(report.Findings, "TOPO-RADIUS"),
		"a target absent from the graph is a silent skip, not a finding")
	for _, rerr := range report.Errors {
		if strings.Contains(rerr, "topology stage") {
			t.Errorf("absent target must not be an error: %s", rerr)
		}
	}
}

func TestDiagEngine_NoTopologySourceBehavesAsBefore(t *testing.T) {
	e := NewDiagEngine(DiagEngineConfig{})
	report := e.Diagnose(context.Background(), "web-1")
	for _, f := range report.Findings {
		if strings.HasPrefix(f.ID, "TOPO-") {
			t.Errorf("no source configured must produce no topology findings: %s", f.ID)
		}
	}
}

func TestNewTopologyCollectorSource_NilCollectorDisablesStage(t *testing.T) {
	if got := NewTopologyCollectorSource(nil); got != nil {
		t.Fatalf("nil collector must yield a nil source (stage disabled), got %T", got)
	}
}
