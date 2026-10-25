package grpc

// rest_agents_test.go pins the REST exposure of the agent registry's
// operator-facing RPCs (List/Get/Remove) — the service was complete at the
// gRPC layer (schema v8 persistence, policy gating, dual-process E2E) while the
// console had no agent view at all.
//
// Two behaviours are pinned beyond routing:
//   - the RemoveAgent REFUSAL is a reply field rendered as 200 with
//     removed=false — the same contract the CLI prints, deliberately not a 409:
//     the operator chooses --force in the CLI, and a browser URL that deletes
//     anyway is the footgun that refusal design exists to avoid;
//   - the filters (capability/status) pass through to the service.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func seedAgent(t *testing.T, store state.Store, id, address string, capabilities []string) {
	t.Helper()
	// AgentStore's write is UpsertAgent (re-register updates, never
	// duplicates); the RPC's RegisterAgent wraps it. The assertion mirrors
	// serveAgentRegistry: a store that cannot persist agents is the
	// Unimplemented path, and this suite's harness must not be on it.
	agentStore, ok := store.(state.AgentStore)
	require.True(t, ok, "the test store must support the agent registry; the suite exercises the served path, not Unimplemented")
	a := &state.Agent{ID: id, Address: address, Capabilities: capabilities}
	require.NoError(t, agentStore.UpsertAgent(context.Background(), a))
}

func TestRESTAgentsRoutesExist(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	resp, err := http.Get(srv.URL + "/agents")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "GET /agents must be routed and return an empty registry")
	var listed []struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listed))
	assert.Empty(t, listed, "the fresh store registers nothing")

	getResp, err := http.Get(srv.URL + "/agents/agent-nope")
	require.NoError(t, err)
	defer getResp.Body.Close()
	assert.Equal(t, http.StatusNotFound, getResp.StatusCode, "an unknown agent id is a 404, not a routing failure")

	delReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/agents/agent-nope", nil)
	require.NoError(t, err)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	defer delResp.Body.Close()
	// A missing record is not an error for RemoveAgent: removed=false with the
	// refusal text, still HTTP 200 (see the CLI's non-zero-exit contract).
	assert.Equal(t, http.StatusOK, delResp.StatusCode)
	var removed struct {
		Removed bool   `json:"removed"`
		Refusal string `json:"refusal"`
	}
	require.NoError(t, json.NewDecoder(delResp.Body).Decode(&removed))
	assert.False(t, removed.Removed)
	assert.NotEmpty(t, removed.Refusal, "the refusal must say why nothing was removed")
}

func TestRESTAgentsListCarriesSeededRecordsAndFilters(t *testing.T) {
	_, srv, store := startTestGateway(t, ServeGatewayConfig{})
	seedAgent(t, store, "agent-shell", "10.0.0.1:7000", []string{"shell"})
	seedAgent(t, store, "agent-pkg", "10.0.0.2:7000", []string{"pkg", "shell"})

	listResp, err := http.Get(srv.URL + "/agents")
	require.NoError(t, err)
	defer listResp.Body.Close()
	require.Equal(t, http.StatusOK, listResp.StatusCode)
	var listed []struct {
		ID           string   `json:"id"`
		Capabilities []string `json:"capabilities"`
	}
	require.NoError(t, json.NewDecoder(listResp.Body).Decode(&listed))
	ids := make([]string, 0, len(listed))
	for _, a := range listed {
		ids = append(ids, a.ID)
	}
	assert.ElementsMatch(t, []string{"agent-shell", "agent-pkg"}, ids)

	// capability filter passes through to the service's server-side narrowing.
	filtered, err := http.Get(srv.URL + "/agents?capability=pkg")
	require.NoError(t, err)
	defer filtered.Body.Close()
	require.Equal(t, http.StatusOK, filtered.StatusCode)
	var pkgOnly []struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(filtered.Body).Decode(&pkgOnly))
	require.Len(t, pkgOnly, 1)
	assert.Equal(t, "agent-pkg", pkgOnly[0].ID)
}

func TestRESTAgentsGetAndRemoveRefusalField(t *testing.T) {
	_, srv, store := startTestGateway(t, ServeGatewayConfig{})
	seedAgent(t, store, "agent-live", "10.0.0.3:7000", []string{"shell"})

	getResp, err := http.Get(srv.URL + "/agents/agent-live")
	require.NoError(t, err)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	var rec struct {
		ID      string `json:"id"`
		Address string `json:"address"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&rec))
	assert.Equal(t, "agent-live", rec.ID)
	assert.Equal(t, "10.0.0.3:7000", rec.Address)

	// Remove over REST: the agent has no in-flight tasks in this store, so the
	// removal succeeds — the refusal-field path is pinned by the routes test
	// above (unknown id).
	delReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/agents/agent-live", nil)
	require.NoError(t, err)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	defer delResp.Body.Close()
	require.Equal(t, http.StatusOK, delResp.StatusCode)
	var removed struct {
		Removed bool   `json:"removed"`
		Refusal string `json:"refusal"`
	}
	require.NoError(t, json.NewDecoder(delResp.Body).Decode(&removed))
	assert.True(t, removed.Removed, "refusal: %s", removed.Refusal)
}
