package main

// cmd_tenant_purge_test.go — the three things only the COMMAND owns: the
// registry gate (a live tenant's data is never purgeable), the confirmation gate
// (an irreversible verb needs --yes, and --json has no terminal to ask in), and
// the store selection (it must be the one config.yaml names, per #47).
// The row-level guarantees live in internal/state/tenant_purge_test.go.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/tenant"
)

const purgeTenantID = "t-acme"

// setupTenantPurgeConfig points the CLI at a throwaway data dir AND a throwaway
// database. The database path has to be set explicitly: `server.data_dir` alone
// leaves database.path at its default, which is the developer's real ~/.levee.
func setupTenantPurgeConfig(t *testing.T, status string) (dataDir string, cleanup func()) {
	t.Helper()

	prevConfig, prevJSON, prevQuiet, prevYes := optConfigPath, optJSON, optQuiet, tenantPurgeOptYes

	dataDir = t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := "server:\n  data_dir: \"" + filepath.ToSlash(dataDir) + "\"\n" +
		"database:\n  driver: sqlite\n  path: \"" +
		filepath.ToSlash(filepath.Join(dataDir, "levee.db")) + "\"\n"
	require.NoError(t, os.WriteFile(configPath, []byte(configYAML), 0o644))

	optConfigPath = configPath
	optJSON = false
	optQuiet = false
	tenantPurgeOptYes = false

	reg := &tenantRegistry{Tenants: []tenantRecord{{
		ID: purgeTenantID, Name: "acme", Namespace: tenant.NamespaceFor("acme"), Status: status,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}}
	require.NoError(t, saveTenantRegistry(tenantsFilePath(dataDir), reg))

	return dataDir, func() {
		optConfigPath, optJSON, optQuiet, tenantPurgeOptYes = prevConfig, prevJSON, prevQuiet, prevYes
	}
}

// seedPurgeRows writes the three shapes a purge has to tell apart: a completed
// run with no trace, a completed run WITH trace, and a running run.
func seedPurgeRows(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	store, err := openStore(ctx)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	now := time.Now().UTC()
	for _, s := range []struct {
		id     string
		status string
		trace  bool
	}{
		{purgeTenantID + "-loose", runstatus.StatusCompleted, false},
		{purgeTenantID + "-sealed", runstatus.StatusCompleted, true},
		{purgeTenantID + "-live", runstatus.StatusRunning, false},
	} {
		require.NoError(t, store.CreateRun(ctx, &state.Run{
			ID: s.id, WorkflowName: "wf", Status: s.status, Creator: "op",
			TenantID: purgeTenantID, CreatedAt: now, UpdatedAt: now,
		}))
		require.NoError(t, store.CreateApproval(ctx, &state.Approval{
			ID: s.id + "-a1", RunID: s.id, Level: "standard", Approver: "alice", Status: "approved",
		}))
		require.NoError(t, store.CreateAssignment(ctx, &state.Assignment{
			RunID: s.id, OwnerNode: "node-a", Epoch: 1, State: state.AssignmentStateDone,
			Result: "completed", TenantID: purgeTenantID,
		}))
		if s.trace {
			require.NoError(t, store.CreateTrace(ctx, &state.Trace{
				ID: s.id + "-t1", RunID: s.id, Event: "apply", Actor: "op", Timestamp: now,
				TenantID: purgeTenantID,
			}))
		}
		// Every run has an audit row, because that is what the change service
		// writes. audit.Record is the production path — it also seals the chain,
		// so the seeded rows are honestly chained rather than carrying fake hashes.
		require.NoError(t, audit.Record(ctx, store, &state.Audit{
			ID: "audit-seed-" + s.id, RunID: s.id, Action: state.AuditActionCreate,
			Actor: "cli-test", Target: s.id, Result: state.AuditResultSuccess,
			Timestamp: now, TenantID: purgeTenantID,
		}))
	}
}

// runExists answers through a fresh handle, so the assertions below cannot be
// satisfied by rows a still-open transaction is holding in its own view. It goes
// through the Store interface rather than a raw handle: `openStore` hands back
// whatever driver the config names.
func runExists(t *testing.T, id string) bool {
	t.Helper()
	ctx := context.Background()
	store, err := openStore(ctx)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	r, err := store.GetRun(ctx, id)
	require.NoError(t, err)
	return r != nil
}

func TestTenantPurgeCmdIsRegistered(t *testing.T) {
	purge := findSubCmd(findSub("tenant"), "purge")
	require.NotNil(t, purge, "`tenant purge` must be on the command tree or the operator cannot reach it")
	require.NotNil(t, purge.Flags().Lookup("yes"))
	require.Error(t, purge.ValidateArgs([]string{}), "a bare purge must not act on anything")
	require.NoError(t, purge.ValidateArgs([]string{purgeTenantID}))
}

func TestTenantPurgeRefusesALiveTenant(t *testing.T) {
	_, cleanup := setupTenantPurgeConfig(t, "active")
	defer cleanup()
	seedPurgeRows(t)
	tenantPurgeOptYes = true // the registry gate, not the prompt, is under test

	err := runTenantPurge(nil, []string{purgeTenantID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only accepts a deleted tenant")
	for _, suffix := range []string{"-loose", "-sealed", "-live"} {
		assert.True(t, runExists(t, purgeTenantID+suffix), "a refused purge leaves every row alone")
	}
}

func TestTenantPurgeNeedsYesOutsideATerminal(t *testing.T) {
	_, cleanup := setupTenantPurgeConfig(t, "deleted")
	defer cleanup()
	seedPurgeRows(t)
	optJSON = true // no human to read a prompt

	err := runTenantPurge(nil, []string{purgeTenantID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--yes")
	assert.True(t, runExists(t, purgeTenantID+"-loose"), "a refused purge must have stopped before any delete")
}

func TestTenantPurgeRemovesTheHeldFreeSubsetAndReportsTheRest(t *testing.T) {
	dataDir, cleanup := setupTenantPurgeConfig(t, "deleted")
	defer cleanup()
	seedPurgeRows(t)
	tenantPurgeOptYes = true

	require.NoError(t, runTenantPurge(nil, []string{purgeTenantID}))

	// The configured file is the one that changed: a hardcoded SQLite open would
	// write next to ~/.levee and still print "purged".
	assert.FileExists(t, filepath.Join(dataDir, "levee.db"))

	assert.False(t, runExists(t, purgeTenantID+"-loose"), "trace-free terminal run is purged")
	assert.True(t, runExists(t, purgeTenantID+"-sealed"), "evidence-backed run is held")
	assert.True(t, runExists(t, purgeTenantID+"-live"), "running run is held")

	ctx := context.Background()
	store, err := openStore(ctx)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	traces, err := store.ListTraces(ctx, state.TraceFilter{TenantID: purgeTenantID})
	require.NoError(t, err)
	assert.Len(t, traces, 1, "the held run's evidence is untouched")

	looseApprovals, err := store.ListApprovals(ctx, state.ApprovalFilter{RunID: purgeTenantID + "-loose"})
	require.NoError(t, err)
	assert.Empty(t, looseApprovals, "the purged run's approval rode the cascade")
	sealedApprovals, err := store.ListApprovals(ctx, state.ApprovalFilter{RunID: purgeTenantID + "-sealed"})
	require.NoError(t, err)
	assert.Len(t, sealedApprovals, 1, "a run that stays must keep the approval its plan hash was verified against")

	// The purge leaves evidence of itself. Without this row, "the tenant's runs
	// were purged in March" and "this tenant never had runs" are the same
	// database — the exact hole the four P0s were about.
	purgeEntries, err := store.ListAudits(ctx, state.AuditFilter{Action: state.AuditActionTenantPurge})
	require.NoError(t, err)
	require.Len(t, purgeEntries, 1, "a destructive operator action must be attributable")
	assert.Equal(t, purgeTenantID, purgeEntries[0].Target)
	assert.Equal(t, state.AuditResultSuccess, purgeEntries[0].Result)
	assert.NotEmpty(t, purgeEntries[0].Actor)

	// And the deletion of a run must not have taken its audit rows with it:
	// `audit.run_id` has no foreign key, which is why a purge is possible at all.
	sealedAudits, err := store.ListAudits(ctx, state.AuditFilter{RunID: purgeTenantID + "-sealed"})
	require.NoError(t, err)
	assert.NotEmpty(t, sealedAudits, "audit rows outlive the runs they describe")

	// The hold-backs must be on the output surface, not only in the return value:
	// an operator who reads "purged" as "gone" has to be contradicted in words.
	var buf bytes.Buffer
	printTenantPurgeHuman(&buf, purgeTenantID, state.TenantPurgeReport{
		RunsRemoved: 1, ApprovalsRemoved: 1, AssignmentsRemoved: 1, RunsHeldTrace: 1, RunsHeldActive: 1,
	})
	assert.Contains(t, buf.String(), "1 run(s), 1 approval(s), 1 assignment(s) removed")
	assert.Contains(t, buf.String(), "Held back: 1 run(s) carrying trace evidence")
	assert.Contains(t, buf.String(), "1 non-terminal run(s)")
}

func TestTenantPurgeSaysSoWhenNothingWasRemovable(t *testing.T) {
	var buf bytes.Buffer
	printTenantPurgeHuman(&buf, purgeTenantID, state.TenantPurgeReport{RunsHeldTrace: 4})
	assert.Contains(t, buf.String(), "Nothing was removable")

	// And it does not claim a hold-back that did not happen.
	buf.Reset()
	printTenantPurgeHuman(&buf, purgeTenantID, state.TenantPurgeReport{RunsRemoved: 2, ApprovalsRemoved: 2})
	assert.NotContains(t, buf.String(), "Held back")
}
