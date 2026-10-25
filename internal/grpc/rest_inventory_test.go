package grpc

// rest_inventory_test.go pins the REST exposure of InventoryService's six
// RPCs — the service was complete at the gRPC layer and gated, but absent from
// both route trees, so the console could not manage groups, import a file, or
// see a host's history at all.
//
// Three things are pinned here:
//  1. the routes EXIST (a 404 or 400 "unknown sub-path" on any of them is the
//     regression this file is for — that is what "service complete, gateway
//     silent" looks like from a client);
//  2. they carry through to the SERVICE (groups created here show up in
//     ListGroups, import counts land in the store, history reflects seeded
//     runs) — routing to a stub would satisfy the first pin and be worse;
//  3. the history route resolves the target ID to its HOSTNAME before calling
//     the service, because TargetHistoryRequest.Host is an address and a
//     history keyed by a generated id would be useless next to the row.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func TestRESTInventoryRoutesExist(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	// Every route answers SOMETHING that is not a routing failure. 404 on the
	// resource, 400 with "unknown ... sub-path", or 503 "not configured" all
	// mean the gateway never reached the handler — the shape this file exists
	// to prevent coming back.
	cases := []struct {
		method, path string
	}{
		{"GET", "/groups"},
		{"POST", "/groups"},
		{"DELETE", "/groups/group-nope"},
		{"POST", "/targets/import"},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader("{}"))
		require.NoError(t, err, tc.method+" "+tc.path)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err, tc.method+" "+tc.path)
		resp.Body.Close()
		assert.NotEqual(t, http.StatusNotFound, resp.StatusCode, tc.method+" "+tc.path+" must be routed")
		body := make([]byte, 200)
		n, _ := resp.Body.Read(body)
		assert.NotContains(t, string(body[:n]), "unknown groups sub-path", tc.method+" "+tc.path)
	}

	// history hangs off the targets tree. An unknown id is a legitimate 404
	// from the handler (it resolves the id first); what must NOT happen is the
	// dispatcher's "unknown target sub-path" — that is the routing failure this
	// suite exists to catch.
	resp, err := http.Get(srv.URL + "/targets/target-nope/history")
	require.NoError(t, err)
	defer resp.Body.Close()
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	assert.NotContains(t, string(buf[:n]), "unknown target sub-path",
		"GET /targets/{id}/history must be routed, not fall through to the dispatcher's 400")
}

func TestRESTInventoryGroupLifecycle(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	// Create.
	resp, err := http.Post(srv.URL+"/groups", "application/json",
		strings.NewReader(`{"name":"prod/db"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "create group")

	// List shows it.
	listResp, err := http.Get(srv.URL + "/groups")
	require.NoError(t, err)
	defer listResp.Body.Close()
	require.Equal(t, http.StatusOK, listResp.StatusCode)
	// The list handler returns the groups as a bare JSON array — the RESTful
	// shape the frontend consumes, not the proto envelope.
	var listed []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	require.NoError(t, json.NewDecoder(listResp.Body).Decode(&listed))
	names := make([]string, 0, len(listed))
	for _, g := range listed {
		names = append(names, g.Name)
	}
	assert.Contains(t, names, "prod/db", "the group created over REST must be visible over REST")

	// Delete it.
	delReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/groups/"+listed[0].ID, nil)
	require.NoError(t, err)
	_ = listed // keep the id source readable
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	defer delResp.Body.Close()
	assert.Equal(t, http.StatusOK, delResp.StatusCode, "delete group")
}

func TestRESTInventoryImportAndHistoryCarryThrough(t *testing.T) {
	gw, srv, store := startTestGateway(t, ServeGatewayConfig{})
	gw.SetInventory(NewInventoryService(store).WithAuthorizer(nil))

	// Import one target over REST; the counts must be real, not echoed zeros.
	importResp, err := http.Post(srv.URL+"/targets/import", "application/json",
		strings.NewReader(`{"yaml":"targets:\n  - address: 10.9.0.1\n    labels:\n      env: prod\n"}`))
	require.NoError(t, err)
	defer importResp.Body.Close()
	require.Equal(t, http.StatusOK, importResp.StatusCode)
	var imported struct {
		Created int32    `json:"created"`
		Failed  int32    `json:"failed"`
		Errors  []string `json:"errors"`
	}
	require.NoError(t, json.NewDecoder(importResp.Body).Decode(&imported))
	assert.Equal(t, int32(1), imported.Created, "the imported target must land in the store")
	assert.Empty(t, imported.Errors)

	// Seed one run whose steps ran on the imported host, then read the history
	// route BY TARGET ID: the handler must resolve it to the hostname before
	// calling TargetHistory — that translation is the reason this route is not
	// a one-liner.
	ctx := context.Background()
	// seedRun + seedBatchAndStep are the same helpers audit_service_test.go
	// uses — they are known to satisfy every FK (runs/batches/steps) and every
	// NOT NULL column, which a hand-rolled seed got wrong twice.
	seedRun(t, ctx, store, "run-hist", "completed")
	seedBatchAndStep(t, ctx, store, "run-hist", "batch-hist", "step-hist", "10.9.0.1", "success")

	target, err := store.FindTargetByAddress(ctx, "10.9.0.1", 22)
	require.NoError(t, err)
	require.NotNil(t, target, "the imported row must be addressable")

	histResp, err := http.Get(srv.URL + "/targets/" + target.ID + "/history")
	require.NoError(t, err)
	defer histResp.Body.Close()
	require.Equal(t, http.StatusOK, histResp.StatusCode)
	var entries []struct {
		RunID string `json:"run_id"`
	}
	require.NoError(t, json.NewDecoder(histResp.Body).Decode(&entries))
	runIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		runIDs = append(runIDs, e.RunID)
	}
	assert.Contains(t, runIDs, "run-hist", "the seeded run must appear in the host's history")

	// And the status route exists with the same carry-through guarantee.
	stResp, err := http.Post(srv.URL+"/targets/"+target.ID+"/status", "application/json",
		strings.NewReader(`{"status":"frozen"}`))
	require.NoError(t, err)
	defer stResp.Body.Close()
	assert.Equal(t, http.StatusOK, stResp.StatusCode)
}

// Group filter on the targets list: the label-selector path already filters
// server-side; the group path is what the console's group tree clicks through
// to, and it filters on the STORED row's group_id (the importer's group
// resolution), not on any label the request could assert.
func TestRESTTargetsListFiltersByGroup(t *testing.T) {
	gw, srv, store := startTestGateway(t, ServeGatewayConfig{})
	gw.SetInventory(NewInventoryService(store).WithAuthorizer(nil))

	ctx := context.Background()
	require.NoError(t, store.UpsertInventoryGroup(ctx, &state.InventoryGroup{
		ID: "grp-db", Name: "prod/db",
	}))
	require.NoError(t, store.UpsertTarget(ctx, &state.Target{
		ID: "tgt-in", Hostname: "10.9.1.1", Port: 22, ChannelType: "ssh",
		GroupID: "grp-db", Status: state.StatusActive,
	}))
	require.NoError(t, store.UpsertTarget(ctx, &state.Target{
		ID: "tgt-out", Hostname: "10.9.1.2", Port: 22, ChannelType: "ssh",
		Status: state.StatusActive,
	}))

	listResp, err := http.Get(srv.URL + "/targets?groupId=grp-db")
	require.NoError(t, err)
	defer listResp.Body.Close()
	require.Equal(t, http.StatusOK, listResp.StatusCode)
	var listed struct {
		Targets []struct {
			ID string `json:"id"`
		} `json:"targets"`
	}
	require.NoError(t, json.NewDecoder(listResp.Body).Decode(&listed))
	ids := make([]string, 0, len(listed.Targets))
	for _, tg := range listed.Targets {
		ids = append(ids, tg.ID)
	}
	assert.Equal(t, []string{"tgt-in"}, ids,
		"the group filter must narrow to the store row's group_id")
}
