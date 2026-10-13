// rest_deeplink_settle_test.go pins the mobile deeplink settlement:
// ApproveViaDeepLink returns the runID, the gateway settles the run via
// ChangeService.SettleApproval, and a partial quorum is surfaced in the
// response instead of a fabricated "approved".
package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/push"
	"github.com/nexus/levee/internal/state"
)

// deeplinkSettleEnv bundles the pieces the endpoint needs: a real
// MobileApprovalService over a real approval service and a ChangeService
// sharing the store. The deeplink generator is kept on the env because
// tokens live in the generator's in-memory store — minting through a
// second generator would produce tokens the service cannot validate.
type deeplinkSettleEnv struct {
	gw       *Gateway
	store    state.Store
	change   *ChangeService
	deeplink *push.DeepLinkGenerator
}

func newDeeplinkSettleEnv(t *testing.T) *deeplinkSettleEnv {
	t.Helper()
	store := newTestStore(t)
	apprSvc := approval.NewService(&kickoffStoreAdapter{store: store})
	deeplink := push.NewDeepLinkGenerator("levee", "https://levee.local")
	mobile := approval.NewMobileApprovalService(apprSvc, nil, deeplink)
	change := NewChangeService(store, nil, apprSvc, nil)

	gw := NewGateway(ServeGatewayConfig{Addr: "127.0.0.1:0"})
	gw.SetMobileApproval(mobile)
	gw.SetServices(change, nil, nil, nil, nil, nil, nil, nil)
	return &deeplinkSettleEnv{gw: gw, store: store, change: change, deeplink: deeplink}
}

// deeplinkTokenFor creates the approval chain and mints a one-time
// approve token for user.
func deeplinkTokenFor(t *testing.T, env *deeplinkSettleEnv, runID, user string, approvers []string, min int) string {
	t.Helper()
	_, err := approval.NewService(&kickoffStoreAdapter{store: env.store}).Create(context.Background(),
		approval.CreateRequest{
			RunID: runID, Level: approval.LevelStandard,
			Approvers: approvers, MinApprovers: min,
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		})
	require.NoError(t, err)
	link, err := env.deeplink.GenerateApprovalLink(runID, user)
	require.NoError(t, err)
	return link.Token
}

func deeplinkPost(t *testing.T, env *deeplinkSettleEnv, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	env.gw.restRoute().ServeHTTP(rec, req)
	return rec
}

func TestDeeplinkApproveSettlesRun(t *testing.T) {
	env := newDeeplinkSettleEnv(t)
	require.NoError(t, env.store.CreateRun(context.Background(), &state.Run{
		ID: "run-dl", WorkflowName: "wf", Status: "draft", Creator: "alice",
	}))
	token := deeplinkTokenFor(t, env, "run-dl", "alice", []string{"alice"}, 1)

	rec := deeplinkPost(t, env, "/changes/deeplink/approve", `{"token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "approved", out["status"])

	run, err := env.store.GetRun(context.Background(), "run-dl")
	require.NoError(t, err)
	assert.Equal(t, "approved", run.Status, "deeplink approve must settle the run")
}

func TestDeeplinkApprovePartialQuorumSurfaces(t *testing.T) {
	env := newDeeplinkSettleEnv(t)
	require.NoError(t, env.store.CreateRun(context.Background(), &state.Run{
		ID: "run-dl2", WorkflowName: "wf", Status: "draft", Creator: "alice",
	}))
	token := deeplinkTokenFor(t, env, "run-dl2", "alice", []string{"alice", "bob"}, 2)

	rec := deeplinkPost(t, env, "/changes/deeplink/approve", `{"token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "recorded; quorum pending", out["status"],
		"a 1/2 mobile vote must not claim approval")

	run, err := env.store.GetRun(context.Background(), "run-dl2")
	require.NoError(t, err)
	assert.NotEqual(t, "approved", run.Status)
}

func TestSettleApproval_PendingChainLeavesRunUntouched(t *testing.T) {
	// A pending chain with no decisions must not move the run — the
	// companion pin for the deeplink reject settle path (one-vote veto
	// is covered by TestApproveChange_RejectMirrorsImmediately).
	env := newDeeplinkSettleEnv(t)
	require.NoError(t, env.store.CreateRun(context.Background(), &state.Run{
		ID: "run-dl3", WorkflowName: "wf", Status: "draft", Creator: "alice",
	}))
	deeplinkTokenFor(t, env, "run-dl3", "alice", []string{"alice"}, 1)

	settled, err := env.change.SettleApproval(context.Background(), "run-dl3")
	require.NoError(t, err)
	assert.Equal(t, SettlePending, settled)
	run, err := env.store.GetRun(context.Background(), "run-dl3")
	require.NoError(t, err)
	assert.Equal(t, "draft", run.Status, "pending chain must leave the run untouched")
}

// --- second tap: evidence instead of a flat 401 ------------------------------

func TestDeeplinkApprove_TwiceSaysAlreadyApproved(t *testing.T) {
	env := newDeeplinkSettleEnv(t)
	require.NoError(t, env.store.CreateRun(context.Background(), &state.Run{
		ID: "run-dl-twice", WorkflowName: "wf", Status: "draft", Creator: "alice",
	}))
	token := deeplinkTokenFor(t, env, "run-dl-twice", "alice", []string{"alice"}, 1)

	first := deeplinkPost(t, env, "/changes/deeplink/approve", `{"token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())

	// Second tap of the same spent link: 409 with who decided and when, so
	// the operator is told not to click again instead of guessing.
	again := deeplinkPost(t, env, "/changes/deeplink/approve", `{"token":"`+token+`"}`)
	require.Equal(t, http.StatusConflict, again.Code, "body: %s", again.Body.String())
	var out struct {
		Error     string `json:"error"`
		Decided   bool   `json:"decided"`
		Decision  string `json:"decision"`
		RunID     string `json:"run_id"`
		DecidedBy string `json:"decided_by"`
		DecidedAt int64  `json:"decided_at"`
	}
	require.NoError(t, json.Unmarshal(again.Body.Bytes(), &out))
	assert.True(t, out.Decided)
	assert.Equal(t, "approved", out.Decision)
	assert.Equal(t, "run-dl-twice", out.RunID)
	assert.Equal(t, "alice", out.DecidedBy)
	assert.Positive(t, out.DecidedAt, "decided_at is Unix seconds, like every REST timestamp")
}

func TestDeeplinkApprove_ForgedTokenStaysFlat401(t *testing.T) {
	env := newDeeplinkSettleEnv(t)

	rec := deeplinkPost(t, env, "/changes/deeplink/approve", `{"token":"never-issued"}`)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	// Nothing beyond the flat error: no run id, no decision, no approver.
	assert.Equal(t, "invalid or expired deeplink token", out["error"])
	assert.Len(t, out, 1, "a forged token must not leak any evidence fields")
}

func TestDeeplinkApprove_AfterConsoleApprovalNamesApprover(t *testing.T) {
	env := newDeeplinkSettleEnv(t)
	require.NoError(t, env.store.CreateRun(context.Background(), &state.Run{
		ID: "run-dl-console", WorkflowName: "wf", Status: "draft", Creator: "alice",
	}))
	// The change gets settled through the console before the link is opened.
	chain := approval.NewService(&kickoffStoreAdapter{store: env.store})
	created, err := chain.Create(context.Background(), approval.CreateRequest{
		RunID: "run-dl-console", Level: approval.LevelStandard,
		Approvers: []string{"alice"}, MinApprovers: 1,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, chain.Approve(context.Background(), created.ID, "alice"))

	// One record only: the console already decided it, so the tap must find
	// nothing pending and answer from that record.
	link, err := env.deeplink.GenerateApprovalLink("run-dl-console", "alice")
	require.NoError(t, err)
	rec := deeplinkPost(t, env, "/changes/deeplink/approve", `{"token":"`+link.Token+`"}`)
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	var out struct {
		Decided   bool   `json:"decided"`
		Decision  string `json:"decision"`
		DecidedBy string `json:"decided_by"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.True(t, out.Decided)
	assert.Equal(t, "approved", out.Decision)
	assert.Equal(t, "alice", out.DecidedBy)
}

func TestDeeplinkReject_TwiceSaysAlreadyRejected(t *testing.T) {
	env := newDeeplinkSettleEnv(t)
	require.NoError(t, env.store.CreateRun(context.Background(), &state.Run{
		ID: "run-dl-rej", WorkflowName: "wf", Status: "draft", Creator: "alice",
	}))
	_, err := approval.NewService(&kickoffStoreAdapter{store: env.store}).Create(context.Background(),
		approval.CreateRequest{
			RunID: "run-dl-rej", Level: approval.LevelStandard,
			Approvers: []string{"alice"}, MinApprovers: 1,
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		})
	require.NoError(t, err)
	link, err := env.deeplink.GenerateRejectLink("run-dl-rej", "alice")
	require.NoError(t, err)

	first := deeplinkPost(t, env, "/changes/deeplink/reject", `{"token":"`+link.Token+`"}`)
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())

	again := deeplinkPost(t, env, "/changes/deeplink/reject", `{"token":"`+link.Token+`"}`)
	require.Equal(t, http.StatusConflict, again.Code, "body: %s", again.Body.String())
	var out struct {
		Decided  bool   `json:"decided"`
		Decision string `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(again.Body.Bytes(), &out))
	assert.True(t, out.Decided)
	assert.Equal(t, "rejected", out.Decision)
}

func TestDeeplinkApprove_SpentTokenWithoutRecordStaysFlat(t *testing.T) {
	env := newDeeplinkSettleEnv(t)
	require.NoError(t, env.store.CreateRun(context.Background(), &state.Run{
		ID: "run-dl-norec", WorkflowName: "wf", Status: "draft", Creator: "alice",
	}))
	// A link for a run that never had an approval record: the binding
	// resolves (the server issued the token), but nothing was ever decided,
	// so the response must stay the flat 401 — no invented evidence.
	link, err := env.deeplink.GenerateApprovalLink("run-dl-norec", "alice")
	require.NoError(t, err)

	rec := deeplinkPost(t, env, "/changes/deeplink/approve", `{"token":"`+link.Token+`"}`)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "invalid or expired deeplink token", out["error"])
	assert.Len(t, out, 1)
}

// TestDeeplinkApprove_EvidenceOnlyFromIssuedToken pins the disclosure rule at
// the route level: the 409 body exists because the spent token still binds to
// the run. A gateway whose handler reports no evidence must not be coaxed
// into inventing any.
func TestDeeplinkApprove_EvidenceOnlyFromIssuedToken(t *testing.T) {
	gw := &Gateway{cfg: ServeGatewayConfig{Addr: "127.0.0.1:0"}}
	stub := &stubMobileApproval{
		err: errors.New("token spent"),
		state: approval.DeeplinkState{
			RunID: "run-stub", Action: "approve", Decided: true,
			Status: "approved", By: "dave",
		},
		stateOK: true,
	}
	gw.SetMobileApproval(stub)
	stubSrv := httptest.NewServer(gw.restRoute())
	defer stubSrv.Close()

	resp, err := http.Post(stubSrv.URL+"/changes/deeplink/approve", "application/json",
		strings.NewReader(`{"token":"spent-token"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, true, out["decided"])
	assert.Equal(t, "dave", out["decided_by"])

	// Without the evidence the same failure is the flat 401.
	gwNoState := &Gateway{cfg: ServeGatewayConfig{Addr: "127.0.0.1:0"}}
	gwNoState.SetMobileApproval(&stubMobileApproval{err: errors.New("token spent")})
	noStateSrv := httptest.NewServer(gwNoState.restRoute())
	defer noStateSrv.Close()
	resp2, err := http.Post(noStateSrv.URL+"/changes/deeplink/approve", "application/json",
		strings.NewReader(`{"token":"spent-token"}`))
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp2.StatusCode)
}
