// cmd_opsmesh_test.go — the read-only platform CLI: rendering of the catalog
// graph + impact radius, PromQL sample rendering, and the config-naming
// failures when the opsmesh section is unusable.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func opsmeshTestConfig(t *testing.T, baseURL string) {
	t.Helper()
	prev := optConfigPath
	t.Cleanup(func() { optConfigPath = prev })
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(
		"opsmesh:\n  enabled: true\n  base_url: "+baseURL+"\n  api_key: test-key\n"), 0o600))
	optConfigPath = cfgPath
}

func TestOpsMeshTopologyCmd_RendersGraphAndImpactRadius(t *testing.T) {
	resetOpsMeshFlags()
	defer resetOpsMeshFlags()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/catalog/topology", r.URL.Path)
		require.Equal(t, "t-1", r.URL.Query().Get("tenantID"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tenantID": "t-1",
			"nodes": []map[string]any{
				{"id": "lb-1", "name": "edge-lb", "type": "service", "status": "running"},
				{"id": "host-1", "name": "web-1", "type": "host", "status": "online"},
				{"id": "db-1", "name": "postgres-main", "type": "database", "status": "degraded"},
			},
			"edges": []map[string]any{
				{"from": "lb-1", "to": "host-1", "relationType": "connects"},
				{"from": "host-1", "to": "db-1", "relationType": "depends"},
			},
		})
	}))
	defer srv.Close()
	opsmeshTestConfig(t, srv.URL)

	cmd := newOpsMeshTopologyCmd()
	// The cmd constructor resets the flag vars (StringVar writes defaults),
	// so set them AFTER construction.
	opsmeshOptTenant = "t-1"
	opsmeshOptService = "web-1"
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	require.NoError(t, runOpsMeshTopology(cmd, nil))
	out := buf.String()
	assert.Contains(t, out, "3 node(s), 2 edge(s)")
	assert.Contains(t, out, "edge-lb")
	assert.Contains(t, out, "web-1")
	assert.Contains(t, out, "impact radius")
	assert.Contains(t, out, "upstream")
	assert.Contains(t, out, "edge-lb[running]", "neighbour statuses are surfaced")
	assert.Contains(t, out, "database") // postgres-main's type column
}

func TestOpsMeshMetricsCmd_RendersSamples(t *testing.T) {
	resetOpsMeshFlags()
	defer resetOpsMeshFlags()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/prometheus/query", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []any{
					map[string]any{"metric": map[string]string{"host": "h1"}, "value": []any{1.0, "0.42"}},
				},
			},
		})
	}))
	defer srv.Close()
	opsmeshTestConfig(t, srv.URL)

	cmd := newOpsMeshMetricsCmd()
	// The cmd constructor resets the flag vars (StringVar writes defaults),
	// so set them AFTER construction.
	opsmeshOptQuery = `up{job="node"}`
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	require.NoError(t, runOpsMeshMetrics(cmd, nil))
	out := buf.String()
	assert.Contains(t, out, "1 series")
	assert.Contains(t, out, "host=h1")
	assert.Contains(t, out, "0.42")
}

func TestOpsMeshMetricsCmd_RequiresQuery(t *testing.T) {
	resetOpsMeshFlags()
	defer resetOpsMeshFlags()

	cmd := newOpsMeshMetricsCmd()
	err := runOpsMeshMetrics(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--query")
	assert.Contains(t, err.Error(), "[exit=2]")
}

func TestOpsMeshClientFromConfig_RequiresEnabledSection(t *testing.T) {
	resetOpsMeshFlags()
	defer resetOpsMeshFlags()

	prev := optConfigPath
	t.Cleanup(func() { optConfigPath = prev })
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("server:\n  data_dir: .\n"), 0o600))
	optConfigPath = cfgPath

	_, err := opsmeshClientFromConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base_url",
		"the refusal must name the missing config so the operator can fix it")
}
