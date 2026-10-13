package grpc

// rest_takeover_test.go pins GET /system/takeover-events.
//
// The gap it closes: internal/takeover settles an expired run (running →
// interrupted under a takeover lock) and records a trace row, but NOTHING read
// that row back. grep found no reader anywhere, and the audit RPC's action
// filter covers the audit table, not the trace table — so an operator could not
// tell "the cluster interrupted this run because its executor died" from "a
// human cancelled it". Those are different incidents.
//
// The tests pin the three properties the read path promises:
//
//   - the event name is the shared constant, so writer and reader cannot drift;
//   - a READ gate applies (view, default_env) — the same posture as the node
//     list it sits beside;
//   - the run's CURRENT status travels with the event, because an interrupted
//     run that has since been resumed is a different story from one still
//     sitting interrupted.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

// takeoverResponse is the endpoint's payload as the browser reads it.
type takeoverResponse struct {
	Events []struct {
		ID        string `json:"id"`
		RunID     string `json:"run_id"`
		Actor     string `json:"actor"`
		Timestamp int64  `json:"timestamp"`
		Status    string `json:"status"`
	} `json:"events"`
	Days       int   `json:"days"`
	WindowFrom int64 `json:"window_from"`
}

// getTakeover issues a GET and decodes the payload; the caller closes nothing
// because doReq's body is read to completion here.
func getTakeover(t *testing.T, url string) takeoverResponse {
	t.Helper()
	resp := doReq(t, http.MethodGet, url, "")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", url)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var out takeoverResponse
	require.NoError(t, json.Unmarshal(raw, &out), "payload: %s", raw)
	return out
}

func seedTakeoverTrace(t *testing.T, store state.Store, runID, id string, at time.Time) {
	t.Helper()
	require.NoError(t, store.CreateTrace(context.Background(), &state.Trace{
		ID:        id,
		RunID:     runID,
		Event:     state.EventTakeoverInterrupted,
		Actor:     "cluster-takeover",
		Timestamp: at,
	}))
}

func TestTakeoverEvents_ListsSettlementsWithCurrentStatus(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	ctx := context.Background()
	now := time.Now().UTC()

	// A run the takeover interrupted and nobody has touched since.
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-settled", WorkflowName: "wf", Status: runstatus.StatusInterrupted,
		Creator: "liwg", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
	}))
	seedTakeoverTrace(t, store, "run-settled", "trc-tk-1", now.Add(-30*time.Minute))

	payload := getTakeover(t, srv.URL+"/system/takeover-events")
	require.Len(t, payload.Events, 1)
	assert.Equal(t, "trc-tk-1", payload.Events[0].ID)
	assert.Equal(t, "run-settled", payload.Events[0].RunID)
	assert.Equal(t, "cluster-takeover", payload.Events[0].Actor)
	assert.Equal(t, runstatus.StatusInterrupted, payload.Events[0].Status,
		"the event carries the run's CURRENT status, not the moment of settlement")
	assert.Positive(t, payload.Days)
	assert.Positive(t, payload.WindowFrom)
}

func TestTakeoverEvents_ReadsCurrentStatusNotSettlementMoment(t *testing.T) {
	// A run interrupted by takeover and since RESUMED must read as running (or
	// whatever it is now), not as interrupted: a timeline reporting it as still
	// interrupted would send the operator chasing a problem that was already
	// fixed.
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-resumed", WorkflowName: "wf", Status: runstatus.StatusRunning,
		Creator: "liwg", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
	}))
	seedTakeoverTrace(t, store, "run-resumed", "trc-tk-2", now.Add(-20*time.Minute))

	payload := getTakeover(t, srv.URL+"/system/takeover-events")
	require.Len(t, payload.Events, 1)
	assert.Equal(t, runstatus.StatusRunning, payload.Events[0].Status)
}

func TestTakeoverEvents_EmptyWhenNoSettlements(t *testing.T) {
	// A deployment that has never had a takeover reads as an empty timeline,
	// not an error and not a 404.
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	payload := getTakeover(t, srv.URL+"/system/takeover-events")
	assert.Empty(t, payload.Events)
}

func TestTakeoverEvents_RequiresReadGate(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{
		AuthTokens: []TokenIdentity{{Token: "alice-secret", Subject: "alice"}},
	})
	gw.SetStore(store)

	// No matrix configured: nothing to judge against, so the endpoint admits.
	// That is the posture every other gate has (and serve announces it at
	// startup rather than deciding per request). The request PRESENTS alice's
	// token: without a named token every request carries no subject, and an
	// unattributable caller is admitted by design — which would make the gate
	// unobservable rather than absent.
	noPolicy := authedGet(t, srv.URL+"/system/takeover-events")
	defer func() { _ = noPolicy.Body.Close() }()
	require.Equal(t, http.StatusOK, noPolicy.StatusCode)

	// A matrix granting alice nothing in the default environment: refused.
	gw.WithAuthorizer(authorizerWith(t,
		"users:\n  - name: alice\n    team: sre\n    role: operator\n",
		"teams:\n  - name: sre\n    environments:\n      - name: staging\n        actions: [view]\n",
		"prod"))
	denied := authedGet(t, srv.URL+"/system/takeover-events")
	defer func() { _ = denied.Body.Close() }()
	require.Equal(t, http.StatusForbidden, denied.StatusCode)

	// The same subject with a grant in the default environment: admitted.
	gw.WithAuthorizer(authorizerWith(t,
		"users:\n  - name: alice\n    team: sre\n    role: operator\n",
		"teams:\n  - name: sre\n    environments:\n      - name: prod\n        actions: [view]\n",
		"prod"))
	granted := authedGet(t, srv.URL+"/system/takeover-events")
	defer func() { _ = granted.Body.Close() }()
	require.Equal(t, http.StatusOK, granted.StatusCode)
}

func TestTakeoverEvents_RejectsBadParameters(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	for _, q := range []string{"?days=0", "?days=-1", "?days=abc", "?limit=0", "?limit=-5", "?limit=abc"} {
		resp := doReq(t, http.MethodGet, srv.URL+"/system/takeover-events"+q, "")
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, q)
	}
}

func TestTakeoverEvents_ClampsAnAbsurdWindowInsteadOfRefusing(t *testing.T) {
	// "Recent events" stays true when clamped: a caller asking for 10 years of
	// takeover history gets the cap and the window it actually saw, rather than
	// an error that tells them nothing about what exists.
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	payload := getTakeover(t, srv.URL+"/system/takeover-events?days=100000")
	assert.Equal(t, maxTakeoverWindowDays, payload.Days)
}
