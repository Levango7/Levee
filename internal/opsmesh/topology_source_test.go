package opsmesh

// topology_source_test.go — the catalog-graph adapter: conversion fidelity
// (status/metadata/edges), pivot matching through metadata values, and the
// full source fetch against an httptest server speaking the platform's real
// contract.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/diagnosis/topology"
)

func sampleCatalog() CatalogGraph {
	return CatalogGraph{
		TenantID: "t-1",
		Nodes: []CatalogNode{
			{ID: "lb-1", Name: "edge-lb", Type: "service", Status: "running"},
			{ID: "host-1", Name: "web-1", Type: "host", Status: "online",
				Metadata: map[string]string{"ip": "10.0.0.11", "tenantID": "t-1"}},
			{ID: "db-1", Name: "postgres-main", Type: "database", Status: "degraded"},
		},
		Edges: []CatalogEdge{
			{From: "lb-1", To: "host-1", RelationType: "connects"},
			{From: "host-1", To: "db-1", RelationType: "depends"},
		},
	}
}

func TestConvertCatalogGraph_MapsStatusMetadataAndEdges(t *testing.T) {
	g := sampleCatalog()
	topo := ConvertCatalogGraph(&g)
	require.NotNil(t, topo)
	require.Len(t, topo.Nodes, 3)

	web := topo.Nodes[1]
	assert.Equal(t, "host-1", web.ID)
	assert.Equal(t, "web-1", web.Name)
	assert.Equal(t, "host", web.Type)
	assert.Equal(t, "online", web.Metadata["status"], "status must reach the findings layer")
	assert.Equal(t, "10.0.0.11", web.Metadata["ip"], "platform metadata is preserved verbatim")

	require.Len(t, topo.Edges, 2)
	assert.Equal(t, "lb-1", topo.Edges[0].Source)
	assert.Equal(t, "host-1", topo.Edges[0].Target)
	assert.Zero(t, topo.Edges[0].Metric.CallCount,
		"the catalog is a relationship graph — no traffic metrics")

	assert.Nil(t, ConvertCatalogGraph(nil))
}

func TestTopologySource_CollectsAndAnalyses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/catalog/topology" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		assert.Equal(t, "t-1", r.URL.Query().Get("tenantID"))
		_ = json.NewEncoder(w).Encode(sampleCatalog())
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	src := NewTopologySource(c, "t-1")
	require.NotNil(t, src)
	assert.Equal(t, "opsmesh", src.Name())

	graph, err := src.CollectTopology(context.Background(), "web-1", topology.TimeRange{})
	require.NoError(t, err)

	// Pivot matching works through metadata values too — the diagnosis target
	// may be a hostname or an IP and the platform's metadata carries it under
	// a key this adapter deliberately does not guess.
	if n, ok := graph.FindNode("10.0.0.11"); !ok || n.ID != "host-1" {
		t.Fatalf("metadata-value match failed: %+v ok=%v", n, ok)
	}

	// The impact-radius analysis runs on the converted graph.
	imp := topology.ImpactRadius(graph, "web-1", 0)
	require.NotNil(t, imp)
	require.Len(t, imp.Upstream, 1)
	assert.Equal(t, "edge-lb", imp.Upstream[0].Name)
	require.Len(t, imp.Downstream, 1)
	assert.Equal(t, "postgres-main", imp.Downstream[0].Name)
	assert.Empty(t, imp.UnhealthyEdges,
		"no traffic metrics means no unhealthy-edge evidence to report")

	// NodeNames renders the platform's status for the operator.
	assert.Equal(t, "edge-lb[running]", topology.NodeNames(imp.Upstream, 5))
}

func TestNewTopologySource_NilClientDisables(t *testing.T) {
	assert.Nil(t, NewTopologySource(nil, "t-1"))
}
