// End-to-end tests for `levee chatops` (E-2): approve/reject drive the local
// approval service against a seeded pending approval; send posts through a
// httptest webhook captured via FEISHU_WEBHOOK_URL. The chatops family owns a
// local -c/--config (bot config) that shadows the root persistent --config,
// so these tests inject the store config via execKeepCfg instead of a flag.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// seedPendingApproval writes one pending approval row for the change.
// Approvals carry an FK on run_id, so the run row is seeded first.
func seedPendingApproval(t *testing.T, e *cliEnv, id, changeID string) {
	t.Helper()
	e.seedRun(t, changeID, "pending")
	store := e.open(t)
	defer func() { _ = store.Close() }()
	require.NoError(t, store.CreateApproval(context.Background(), &state.Approval{
		ID: id, RunID: changeID, Level: "L1", Status: "pending",
	}))
}

func TestChatopsE2E_Flows(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	t.Setenv("LEVEE_CHATOPS_APPROVER", "oncall.bob")

	// approve: human flow against a seeded pending approval.
	seedPendingApproval(t, e, "ap-1", "chg1")
	out := mustRunKeepCfg(t, e.cfgPath, "chatops", "approve", "chg1")
	assert.Contains(t, out, "chg1")
	assert.Contains(t, out, "oncall.bob")

	// Second approve of the same change: no longer pending -> error.
	err := runErrKeepCfg(t, e.cfgPath, "chatops", "approve", "chg1")
	assert.Error(t, err)

	// reject with --id and reason (JSON variant).
	seedPendingApproval(t, e, "ap-2", "chg2")
	res, raw, err := execKeepCfgJSON(t, e.cfgPath, "chatops", "reject",
		"--id", "chg2", "--reason", "schema mismatch", "--json")
	require.NoError(t, err, raw)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "rejected", data["action"])
	assert.Equal(t, "schema mismatch", data["reason"])
	assert.Equal(t, "chatops", data["source"])

	// reject without reason: usage guard.
	seedPendingApproval(t, e, "ap-3", "chg3")
	err = runErrKeepCfg(t, e.cfgPath, "chatops", "reject", "chg3")
	assert.Contains(t, err.Error(), "--reason is required for reject [exit=2]")

	// Unknown change id: no pending approval found.
	err = runErrKeepCfg(t, e.cfgPath, "chatops", "approve", "chg-none")
	assert.Error(t, err)

	// No change id at all.
	err = runErrKeepCfg(t, e.cfgPath, "chatops", "approve")
	assert.Contains(t, err.Error(), "change id is required (positional arg or --id) [exit=2]")
}

func TestChatopsE2E_SendAndStart(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)

	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	defer srv.Close()
	t.Setenv("FEISHU_WEBHOOK_URL", srv.URL)

	// send delivers through the webhook (human + JSON).
	out := mustRunKeepCfg(t, e.cfgPath, "chatops", "send",
		"--channel", "ops-room", "--message", "deploy approved")
	assert.Contains(t, out, "ops-room")
	assert.NotEmpty(t, gotPath)
	assert.Contains(t, string(gotBody), "deploy approved")

	res, _, err := execKeepCfgJSON(t, e.cfgPath, "chatops", "send",
		"--channel", "ops-room", "--message", "second", "--json")
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "sent", data["status"])

	// Missing channel/message usage guards.
	err = runErrKeepCfg(t, e.cfgPath, "chatops", "send", "--message", "x")
	assert.Contains(t, err.Error(), "--channel is required [exit=2]")
	err = runErrKeepCfg(t, e.cfgPath, "chatops", "send", "--channel", "c")
	assert.Contains(t, err.Error(), "--message is required [exit=2]")

	// start with a timeout runs briefly and exits cleanly.
	out = mustRunKeepCfg(t, e.cfgPath, "chatops", "start", "--timeout", "120ms")
	assert.Contains(t, out, "started")

	// Without a webhook URL the bot cannot be built.
	t.Setenv("FEISHU_WEBHOOK_URL", "")
	err = runErrKeepCfg(t, e.cfgPath, "chatops", "send",
		"--channel", "c", "--message", "x")
	assert.Contains(t, err.Error(), "webhook")
}
