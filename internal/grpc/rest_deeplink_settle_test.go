// rest_deeplink_settle_test.go pins the mobile deeplink settlement:
// ApproveViaDeepLink returns the runID, the gateway settles the run via
// ChangeService.SettleApproval, and a partial quorum is surfaced in the
// response instead of a fabricated "approved".
package grpc

import (
	"context"
	"encoding/json"
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
