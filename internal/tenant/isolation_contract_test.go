package tenant

// Isolation-contract tests for IsolatedStore. These complement the
// per-method tests in isolation_test.go by pinning the guarantees that
// matter when the wrapper is eventually wired into the server:
//
//   - every method refuses to operate on a store without a base backend;
//   - a denied cross-tenant mutation leaves the stored record untouched;
//   - a failed base write rolls back the quota reservation and the
//     in-memory tenant tag;
//   - legacy records created before multi-tenancy are invisible to every
//     tenant rather than leaked to all of them;
//   - a successful self-tenant update preserves the tenant tag on the
//     stored row (otherwise the run would be orphaned from its owner).
import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func TestIsolatedStoreNilBase(t *testing.T) {
	ctx := context.Background()
	iso := NewIsolatedStore("t1", nil, nil)
	require.NotNil(t, iso)

	run := makeRun("run-x", "")
	cases := []struct {
		name string
		act  func() error
	}{
		{"create run", func() error { return iso.CreateRun(ctx, run) }},
		{"get run", func() error { _, err := iso.GetRun(ctx, "run-x"); return err }},
		{"list runs", func() error { _, err := iso.ListRuns(ctx, state.RunFilter{}); return err }},
		{"update run", func() error { return iso.UpdateRun(ctx, run) }},
		{"delete run", func() error { return iso.DeleteRun(ctx, "run-x") }},
		{"list traces", func() error { _, err := iso.ListTraces(ctx, state.TraceFilter{}); return err }},
		{"list audits", func() error { _, err := iso.ListAudits(ctx, state.AuditFilter{}); return err }},
		{"list batches", func() error { _, err := iso.ListBatches(ctx, state.BatchFilter{}); return err }},
		{"list steps", func() error { _, err := iso.ListSteps(ctx, state.StepFilter{}); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Error(t, tc.act())
		})
	}
}

func TestCrossTenantMutationDeniedLeavesBaseUntouched(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	isoA := NewIsolatedStore("tenantA", store, nil)
	isoB := NewIsolatedStore("tenantB", store, nil)
	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a", "INC-A")))

	// Snapshot of the stored record before the denied mutations.
	before, err := store.GetRun(ctx, "run-a")
	require.NoError(t, err)
	require.NotNil(t, before)

	t.Run("denied update does not write", func(t *testing.T) {
		hostile := makeRun("run-a", "INC-HIJACK")
		hostile.Status = "running"
		err := isoB.UpdateRun(ctx, hostile)
		require.ErrorIs(t, err, ErrCrossTenantAccess)

		got, err := store.GetRun(ctx, "run-a")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, *before, *got, "stored record must be unchanged after denied update")

		// The hostile object itself must not have been re-tagged either,
		// otherwise the caller would leak the tag into later writes.
		assert.Equal(t, "INC-HIJACK", hostile.IncidentID)

		// The legitimate owner still sees its own run unchanged.
		mine, err := isoA.GetRun(ctx, "run-a")
		require.NoError(t, err)
		require.NotNil(t, mine)
		assert.Equal(t, "pending", mine.Status)
		assert.Equal(t, "INC-A", mine.IncidentID)
	})

	t.Run("denied delete does not remove", func(t *testing.T) {
		err := isoB.DeleteRun(ctx, "run-a")
		require.ErrorIs(t, err, ErrCrossTenantAccess)

		got, err := store.GetRun(ctx, "run-a")
		require.NoError(t, err)
		require.NotNil(t, got, "run must survive a denied cross-tenant delete")

		mine, err := isoA.GetRun(ctx, "run-a")
		require.NoError(t, err)
		require.NotNil(t, mine)
	})
}

func TestCreateRunRollsBackQuotaOnBaseFailure(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxConcurrentChanges: 5}))
	iso := NewIsolatedStore("t1", store, qm)

	// First create succeeds and reserves one unit.
	require.NoError(t, iso.CreateRun(ctx, makeRun("run-1", "INC-1")))
	u, err := qm.GetUsage("t1")
	require.NoError(t, err)
	require.Equal(t, 1, u.ActiveChanges)

	// A second create with the same ID fails at the base store (PK
	// conflict); the reservation must be rolled back and the caller's
	// IncidentID restored.
	dup := makeRun("run-1", "INC-DUP")
	err = iso.CreateRun(ctx, dup)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrQuotaExceeded, "failure must come from the base store, not the quota")

	u, err = qm.GetUsage("t1")
	require.NoError(t, err)
	assert.Equal(t, 1, u.ActiveChanges, "failed create must not leak a reservation")
	assert.Equal(t, "INC-DUP", dup.IncidentID, "caller's incident id must be restored on failure")

	// The duplicate must not exist under any other tenant either.
	for _, tid := range []string{"t1", "other"} {
		got, err := NewIsolatedStore(tid, store, nil).GetRun(ctx, "run-1")
		if tid == "t1" {
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, "INC-1", got.IncidentID)
		} else {
			require.NoError(t, err)
			assert.Nil(t, got)
		}
	}

	// Capacity was not leaked: another create is still possible.
	require.NoError(t, iso.CreateRun(ctx, makeRun("run-2", "")))
	u, err = qm.GetUsage("t1")
	require.NoError(t, err)
	assert.Equal(t, 2, u.ActiveChanges)
}

func TestLegacyRecordsInvisibleToEveryTenant(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// A pre-multi-tenancy run with no tenant tag, plus child records,
	// created directly on the base store.
	require.NoError(t, store.CreateRun(ctx, makeRun("run-legacy", "INC-LEGACY")))
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "batch-legacy", RunID: "run-legacy", BatchNo: 1, Status: "pending",
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "step-legacy", RunID: "run-legacy", BatchID: "batch-legacy",
		Host: "host0", StepName: "patch", Action: "shell", Status: "pending",
	}))
	now := time.Now().UTC()
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID: "trace-legacy", RunID: "run-legacy", Event: "started",
		Actor: "ghost", Timestamp: now,
	}))
	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID: "audit-legacy", RunID: "run-legacy", Action: "plan",
		Actor: "ghost", Target: "host0", Result: "ok", Timestamp: now,
	}))

	// One properly tagged run per tenant so each tenant has exactly one
	// visible run of its own.
	isoA := NewIsolatedStore("tenantA", store, nil)
	isoB := NewIsolatedStore("tenantB", store, nil)
	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a", "")))
	require.NoError(t, isoB.CreateRun(ctx, makeRun("run-b", "")))

	for _, tc := range []struct {
		tenant string
		ownID  string
	}{
		{tenant: "tenantA", ownID: "run-a"},
		{tenant: "tenantB", ownID: "run-b"},
	} {
		t.Run(tc.tenant, func(t *testing.T) {
			iso := NewIsolatedStore(tc.tenant, store, nil)

			// Untagged records are neither readable nor listed.
			got, err := iso.GetRun(ctx, "run-legacy")
			require.NoError(t, err)
			assert.Nil(t, got, "legacy run must not be readable")

			runs, err := iso.ListRuns(ctx, state.RunFilter{})
			require.NoError(t, err)
			require.Len(t, runs, 1)
			assert.Equal(t, tc.ownID, runs[0].ID)

			traces, err := iso.ListTraces(ctx, state.TraceFilter{})
			require.NoError(t, err)
			assert.Empty(t, traces, "trace of legacy run must be filtered out")

			traces, err = iso.ListTraces(ctx, state.TraceFilter{RunID: "run-legacy"})
			require.NoError(t, err)
			assert.Empty(t, traces, "trace filtered by legacy run id must be filtered out")

			audits, err := iso.ListAudits(ctx, state.AuditFilter{})
			require.NoError(t, err)
			assert.Empty(t, audits, "audit of legacy run must be filtered out")

			batches, err := iso.ListBatches(ctx, state.BatchFilter{})
			require.NoError(t, err)
			assert.Empty(t, batches, "batch of legacy run must be filtered out")

			steps, err := iso.ListSteps(ctx, state.StepFilter{})
			require.NoError(t, err)
			assert.Empty(t, steps, "step of legacy run must be filtered out")

			// Mutating a legacy record is refused like any foreign run:
			// nobody owns it, so nobody may touch it.
			err = iso.UpdateRun(ctx, makeRun("run-legacy", "INC-X"))
			assert.ErrorIs(t, err, ErrCrossTenantAccess)
			assert.Error(t, iso.DeleteRun(ctx, "run-legacy"))
		})
	}

	// The base store still holds every legacy record untouched.
	got, err := store.GetRun(ctx, "run-legacy")
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestSelfUpdatePreservesTenantTagInBase(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	iso := NewIsolatedStore("tenantA", store, nil)
	require.NoError(t, iso.CreateRun(ctx, makeRun("run-a", "INC-A")))

	// Realistic flow: fetch (GetRun restores the plain incident id),
	// mutate, write back.
	fetched, err := iso.GetRun(ctx, "run-a")
	require.NoError(t, err)
	require.NotNil(t, fetched)
	fetched.Status = "running"
	fetched.Creator = "alice"
	require.NoError(t, iso.UpdateRun(ctx, fetched))

	// The stored row must still carry the tenant tag; losing it would
	// orphan the run from its owner.
	raw, err := store.GetRun(ctx, "run-a")
	require.NoError(t, err)
	require.NotNil(t, raw)
	tid, original := DecodeTenantTag(raw.IncidentID)
	assert.Equal(t, "tenantA", tid, "tenant tag must survive a self-update")
	assert.Equal(t, "INC-A", original)
	assert.Equal(t, "running", raw.Status)

	// The owner can still read the run back with the stripped tag, and
	// no other tenant gained access through the update.
	got, err := iso.GetRun(ctx, "run-a")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "INC-A", got.IncidentID)

	other, err := NewIsolatedStore("tenantB", store, nil).GetRun(ctx, "run-a")
	require.NoError(t, err)
	assert.Nil(t, other)
}

func TestDeleteRunMissingWithQuotaIsNoOp(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxConcurrentChanges: 3}))
	iso := NewIsolatedStore("t1", store, qm)
	require.NoError(t, iso.CreateRun(ctx, makeRun("run-1", "")))
	u, err := qm.GetUsage("t1")
	require.NoError(t, err)
	require.Equal(t, 1, u.ActiveChanges)

	// Deleting an unknown run succeeds without releasing anyone's slot.
	require.NoError(t, iso.DeleteRun(ctx, "missing"))
	u, err = qm.GetUsage("t1")
	require.NoError(t, err)
	assert.Equal(t, 1, u.ActiveChanges, "quota must not be released for a non-existent run")

	// Cross-check: deleting the real run does release exactly one unit.
	require.NoError(t, iso.DeleteRun(ctx, "run-1"))
	u, err = qm.GetUsage("t1")
	require.NoError(t, err)
	assert.Equal(t, 0, u.ActiveChanges)
}

func TestListRunsEmptyResultIsUsable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	iso := NewIsolatedStore("empty-tenant", store, nil)
	runs, err := iso.ListRuns(ctx, state.RunFilter{})
	require.NoError(t, err)
	assert.NotNil(t, runs, "an empty result should be an empty slice, not nil")
	assert.Empty(t, runs)

	// A filter that matches nothing on a non-empty store behaves the same.
	require.NoError(t, NewIsolatedStore("t1", store, nil).CreateRun(ctx, makeRun("run-1", "")))
	runs, err = iso.ListRuns(ctx, state.RunFilter{})
	require.NoError(t, err)
	assert.NotNil(t, runs)
	assert.Empty(t, runs)

	// Status filters compose with the tenant filter.
	runs, err = NewIsolatedStore("t1", store, nil).ListRuns(ctx, state.RunFilter{Status: "nonexistent"})
	require.NoError(t, err)
	assert.Empty(t, runs)
}
