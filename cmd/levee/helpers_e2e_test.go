// Unit tests for helpers.go (E-2): actor resolution, template dir derivation,
// run ID generation, openStore against the cliEnv config, and the
// approval.Store adapter round-trip including malformed-record tolerance.
package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/state"
)

func TestCurrentActor(t *testing.T) {
	t.Setenv("LEVEE_ACTOR", "")
	assert.Equal(t, "cli-user", currentActor())
	t.Setenv("LEVEE_ACTOR", "ops.alice")
	assert.Equal(t, "ops.alice", currentActor())
}

func TestTemplateDir(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.DataDir = filepath.Join(string(filepath.Separator), "home", "u", ".levee", "data")
	want := filepath.Join(string(filepath.Separator), "home", "u", ".levee", "templates")
	assert.Equal(t, want, templateDir(cfg))
}

func TestGenerateRunID(t *testing.T) {
	a, err := generateRunID()
	require.NoError(t, err)
	b, err := generateRunID()
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	assert.Len(t, a, len("run-")+16)
	assert.Contains(t, a, "run-")
}

func TestOpenStoreUsesConfiguredPath(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	optConfigPath = e.cfgPath
	store, err := openStore(context.Background())
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	// The store is the same file the cliEnv advertises.
	e.seedRun(t, "helper-run", "completed")
	run, err := store.GetRun(context.Background(), "helper-run")
	require.NoError(t, err)
	require.NotNil(t, run)

	// Bad config path fails cleanly.
	optConfigPath = e.cfgPath + ".missing"
	_, err = openStore(context.Background())
	assert.Error(t, err)
}

func TestApplySecurityConfigNilSafe(t *testing.T) {
	applySecurityConfig(nil) // must not panic
	applySecurityConfig(&config.Config{})
}

// --- approval store adapter -------------------------------------------------

func TestApprovalAdapter_RoundTrip(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	e.seedRun(t, "run-rt", "pending") // approvals carry an FK on run_id
	store := e.open(t)
	defer func() { _ = store.Close() }()

	adapter := newApprovalStoreAdapter(store)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	ap := &approval.Approval{
		ID: "ap-rt", RunID: "run-rt", Level: "L2",
		Status:       approval.StatusPending,
		Approvers:    []string{"alice", "bob"},
		MinApprovers: 2,
		CreatedAt:    now,
		ExpiresAt:    now.Add(time.Hour),
	}
	require.NoError(t, adapter.Create(ctx, ap))

	got, err := adapter.Get(ctx, "ap-rt")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "run-rt", got.RunID)
	assert.Equal(t, []string{"alice", "bob"}, got.Approvers)
	assert.Equal(t, 2, got.MinApprovers)
	assert.True(t, got.ExpiresAt.Equal(now.Add(time.Hour)))

	// Missing record yields a nil approval and no error (adapter contract).
	gotNone, err := adapter.Get(ctx, "ap-none")
	require.NoError(t, err)
	assert.Nil(t, gotNone)

	// Decision updates flow through Update and map onto the state row's
	// Approver/ActedAt columns (approvalToState's decision mapping).
	got.Status = approval.StatusApproved
	got.Decisions = []approval.Decision{{Approver: "alice", At: now}}
	require.NoError(t, adapter.Update(ctx, got))
	sa, err := store.GetApproval(ctx, "ap-rt")
	require.NoError(t, err)
	require.NotNil(t, sa)
	assert.Equal(t, "alice", sa.Approver)

	// CAS: the second conditional update must lose once status is approved.
	ok, err := adapter.UpdateIfPending(ctx, got)
	require.NoError(t, err)
	assert.False(t, ok, "approved row must not accept a pending CAS")
}

func TestApprovalAdapter_ListPendingSkipsMalformed(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	e.seedRun(t, "run-x", "pending") // approvals carry an FK on run_id
	store := e.open(t)
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	// One well-formed pending record via the adapter...
	adapter := newApprovalStoreAdapter(store)
	require.NoError(t, adapter.Create(ctx, &approval.Approval{
		ID: "ap-ok", RunID: "run-x", Level: "L1",
		Status: approval.StatusPending, MinApprovers: 1,
	}))
	// ...and one raw row whose Comment is not JSON at all.
	require.NoError(t, store.CreateApproval(ctx, &state.Approval{
		ID: "ap-broken", RunID: "run-x", Level: "L1",
		Status: "pending", Comment: "not json",
	}))

	pending, err := adapter.ListPending(ctx)
	require.NoError(t, err)
	// stateToApproval leaves zero-value extras on broken JSON instead of
	// failing, so both rows must surface (defensive path, never an error).
	require.Len(t, pending, 2)
	byID := map[string]*approval.Approval{}
	for _, a := range pending {
		byID[a.ID] = a
	}
	assert.NotNil(t, byID["ap-ok"])
	require.NotNil(t, byID["ap-broken"])
	assert.Equal(t, approval.Status("pending"), byID["ap-broken"].Status)
}

func TestStateToApproval_ZeroTimeoutCopy(t *testing.T) {
	ts := time.Now().UTC()
	ap, err := stateToApproval(&state.Approval{
		ID: "z", RunID: "r", Level: "L1", Status: "pending", TimeoutAt: &ts,
	})
	require.NoError(t, err)
	assert.True(t, ap.ExpiresAt.Equal(ts), "TimeoutAt must backfill ExpiresAt")
}
