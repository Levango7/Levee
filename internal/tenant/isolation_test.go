package tenant

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// newTestStore returns a fresh SQLite store backed by a temp file. It is
// a copy of the helper in internal/state to avoid importing the test
// package.
func newTestStore(t *testing.T) *state.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "levee-tenant-test.db")
	store, err := state.NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func makeRun(id, incident string) *state.Run {
	now := time.Now().UTC().Truncate(time.Second)
	return &state.Run{
		ID:             id,
		WorkflowName:   "test",
		TemplateName:   "test",
		Params:         "{}",
		PlanHash:       "hash",
		Status:         "pending",
		ApprovalStatus: "pending",
		CreatedAt:      now,
		UpdatedAt:      now,
		Creator:        "tester",
		IncidentID:     incident,
	}
}

func TestEncodeTenantTag(t *testing.T) {
	encoded := EncodeTenantTag("t1", "INC-1")
	assert.Equal(t, "tenant:t1|INC-1", encoded)

	encoded = EncodeTenantTag("t1", "")
	assert.Equal(t, "tenant:t1", encoded)
}

func TestDecodeTenantTag(t *testing.T) {
	tid, inc := DecodeTenantTag("tenant:t1|INC-1")
	assert.Equal(t, "t1", tid)
	assert.Equal(t, "INC-1", inc)

	tid, inc = DecodeTenantTag("tenant:t1")
	assert.Equal(t, "t1", tid)
	assert.Empty(t, inc)

	// Legacy record without tenant tag.
	tid, inc = DecodeTenantTag("INC-legacy")
	assert.Empty(t, tid)
	assert.Equal(t, "INC-legacy", inc)
}

func TestIsolatedStoreCreateRun(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxConcurrentChanges: 5}))

	iso := NewIsolatedStore("t1", store, qm)
	run := makeRun("run-1", "INC-1")
	require.NoError(t, iso.CreateRun(ctx, run))

	// The stored record should carry the tenant tag.
	got, err := store.GetRun(ctx, "run-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	tid, inc := DecodeTenantTag(got.IncidentID)
	assert.Equal(t, "t1", tid)
	assert.Equal(t, "INC-1", inc)

	// Usage should reflect the reservation.
	u, err := qm.GetUsage("t1")
	require.NoError(t, err)
	assert.Equal(t, 1, u.ActiveChanges)
}

func TestIsolatedStoreCreateRunQuotaExceeded(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxConcurrentChanges: 1}))

	iso := NewIsolatedStore("t1", store, qm)
	require.NoError(t, iso.CreateRun(ctx, makeRun("run-1", "")))
	err := iso.CreateRun(ctx, makeRun("run-2", ""))
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrQuotaExceeded)

	// The second run should not have been created.
	got, err := store.GetRun(ctx, "run-2")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestIsolatedStoreGetRun(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	isoA := NewIsolatedStore("tenantA", store, nil)
	isoB := NewIsolatedStore("tenantB", store, nil)

	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a", "INC-A")))
	require.NoError(t, isoB.CreateRun(ctx, makeRun("run-b", "INC-B")))

	// Each tenant can fetch its own run.
	got, err := isoA.GetRun(ctx, "run-a")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "run-a", got.ID)
	// The returned IncidentID should be the original, not the tenant tag.
	assert.Equal(t, "INC-A", got.IncidentID)

	// Tenant A cannot fetch tenant B's run.
	got, err = isoA.GetRun(ctx, "run-b")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Tenant B cannot fetch tenant A's run.
	got, err = isoB.GetRun(ctx, "run-a")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestIsolatedStoreListRuns(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	isoA := NewIsolatedStore("tenantA", store, nil)
	isoB := NewIsolatedStore("tenantB", store, nil)

	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a1", "")))
	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a2", "")))
	require.NoError(t, isoB.CreateRun(ctx, makeRun("run-b1", "")))
	require.NoError(t, isoB.CreateRun(ctx, makeRun("run-b2", "")))

	runsA, err := isoA.ListRuns(ctx, state.RunFilter{})
	require.NoError(t, err)
	assert.Len(t, runsA, 2)
	for _, r := range runsA {
		assert.Contains(t, []string{"run-a1", "run-a2"}, r.ID)
	}

	runsB, err := isoB.ListRuns(ctx, state.RunFilter{})
	require.NoError(t, err)
	assert.Len(t, runsB, 2)
	for _, r := range runsB {
		assert.Contains(t, []string{"run-b1", "run-b2"}, r.ID)
	}
}

func TestIsolatedStoreUpdateRun(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	isoA := NewIsolatedStore("tenantA", store, nil)
	isoB := NewIsolatedStore("tenantB", store, nil)

	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a", "INC-A")))

	// Tenant B cannot update tenant A's run.
	run := makeRun("run-a", "INC-X")
	run.Status = "running"
	err := isoB.UpdateRun(ctx, run)
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrCrossTenantAccess)

	// Tenant A can update its own run.
	require.NoError(t, isoA.UpdateRun(ctx, run))

	got, err := isoA.GetRun(ctx, "run-a")
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
	assert.Equal(t, "INC-X", got.IncidentID)
}

func TestIsolatedStoreUpdateRunMissing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	iso := NewIsolatedStore("t1", store, nil)

	err := iso.UpdateRun(ctx, makeRun("missing", ""))
	assert.Error(t, err)
}

func TestIsolatedStoreDeleteRun(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("tenantA", Quota{MaxConcurrentChanges: 5}))

	isoA := NewIsolatedStore("tenantA", store, qm)
	isoB := NewIsolatedStore("tenantB", store, qm)

	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a", "")))

	// Tenant B cannot delete tenant A's run.
	err := isoB.DeleteRun(ctx, "run-a")
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrCrossTenantAccess)

	// Tenant A can delete its own run; usage should be released.
	require.NoError(t, isoA.DeleteRun(ctx, "run-a"))
	u, err := qm.GetUsage("tenantA")
	require.NoError(t, err)
	assert.Equal(t, 0, u.ActiveChanges)

	// The run is gone.
	got, err := store.GetRun(ctx, "run-a")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestIsolatedStoreDeleteRunMissing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	iso := NewIsolatedStore("t1", store, nil)

	// Deleting a non-existent run is a no-op.
	require.NoError(t, iso.DeleteRun(ctx, "missing"))
}

func TestIsolatedStoreListTraces(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	isoA := NewIsolatedStore("tenantA", store, nil)
	isoB := NewIsolatedStore("tenantB", store, nil)

	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a", "")))
	require.NoError(t, isoB.CreateRun(ctx, makeRun("run-b", "")))

	now := time.Now().UTC()
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID:        "trace-a",
		RunID:     "run-a",
		Event:     "started",
		Actor:     "alice",
		Timestamp: now,
	}))
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID:        "trace-b",
		RunID:     "run-b",
		Event:     "started",
		Actor:     "bob",
		Timestamp: now,
	}))

	tracesA, err := isoA.ListTraces(ctx, state.TraceFilter{})
	require.NoError(t, err)
	require.Len(t, tracesA, 1)
	assert.Equal(t, "trace-a", tracesA[0].ID)

	tracesB, err := isoB.ListTraces(ctx, state.TraceFilter{})
	require.NoError(t, err)
	require.Len(t, tracesB, 1)
	assert.Equal(t, "trace-b", tracesB[0].ID)
}

func TestIsolatedStoreListAudits(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	isoA := NewIsolatedStore("tenantA", store, nil)
	isoB := NewIsolatedStore("tenantB", store, nil)

	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a", "")))
	require.NoError(t, isoB.CreateRun(ctx, makeRun("run-b", "")))

	now := time.Now().UTC()
	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID:        "audit-a",
		RunID:     "run-a",
		Action:    "plan",
		Actor:     "alice",
		Target:    "host1",
		Result:    "ok",
		Timestamp: now,
	}))
	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID:        "audit-b",
		RunID:     "run-b",
		Action:    "plan",
		Actor:     "bob",
		Target:    "host2",
		Result:    "ok",
		Timestamp: now,
	}))

	auditsA, err := isoA.ListAudits(ctx, state.AuditFilter{})
	require.NoError(t, err)
	require.Len(t, auditsA, 1)
	assert.Equal(t, "audit-a", auditsA[0].ID)

	auditsB, err := isoB.ListAudits(ctx, state.AuditFilter{})
	require.NoError(t, err)
	require.Len(t, auditsB, 1)
	assert.Equal(t, "audit-b", auditsB[0].ID)
}

func TestIsolatedStoreListBatches(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	isoA := NewIsolatedStore("tenantA", store, nil)
	isoB := NewIsolatedStore("tenantB", store, nil)

	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a", "")))
	require.NoError(t, isoB.CreateRun(ctx, makeRun("run-b", "")))

	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID:      "batch-a",
		RunID:   "run-a",
		BatchNo: 1,
		Status:  "pending",
	}))
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID:      "batch-b",
		RunID:   "run-b",
		BatchNo: 1,
		Status:  "pending",
	}))

	// Filter by RunID: tenant A only sees its own batches.
	batchesA, err := isoA.ListBatches(ctx, state.BatchFilter{RunID: "run-a"})
	require.NoError(t, err)
	require.Len(t, batchesA, 1)
	assert.Equal(t, "batch-a", batchesA[0].ID)

	// Tenant A querying tenant B's run gets nothing.
	batchesA, err = isoA.ListBatches(ctx, state.BatchFilter{RunID: "run-b"})
	require.NoError(t, err)
	assert.Empty(t, batchesA)

	// Unfiltered list: tenant A only sees its own batches.
	batchesA, err = isoA.ListBatches(ctx, state.BatchFilter{})
	require.NoError(t, err)
	require.Len(t, batchesA, 1)
	assert.Equal(t, "batch-a", batchesA[0].ID)

	batchesB, err := isoB.ListBatches(ctx, state.BatchFilter{})
	require.NoError(t, err)
	require.Len(t, batchesB, 1)
	assert.Equal(t, "batch-b", batchesB[0].ID)
}

func TestIsolatedStoreListSteps(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	isoA := NewIsolatedStore("tenantA", store, nil)
	isoB := NewIsolatedStore("tenantB", store, nil)

	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a", "")))
	require.NoError(t, isoB.CreateRun(ctx, makeRun("run-b", "")))

	// Batches are required because steps reference them via FK.
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID:      "batch-a",
		RunID:   "run-a",
		BatchNo: 1,
		Status:  "pending",
	}))
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID:      "batch-b",
		RunID:   "run-b",
		BatchNo: 1,
		Status:  "pending",
	}))

	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID:       "step-a",
		RunID:    "run-a",
		BatchID:  "batch-a",
		Host:     "host1",
		StepName: "patch",
		Action:   "shell",
		Status:   "pending",
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID:       "step-b",
		RunID:    "run-b",
		BatchID:  "batch-b",
		Host:     "host2",
		StepName: "patch",
		Action:   "shell",
		Status:   "pending",
	}))

	stepsA, err := isoA.ListSteps(ctx, state.StepFilter{RunID: "run-a"})
	require.NoError(t, err)
	require.Len(t, stepsA, 1)
	assert.Equal(t, "step-a", stepsA[0].ID)

	stepsA, err = isoA.ListSteps(ctx, state.StepFilter{RunID: "run-b"})
	require.NoError(t, err)
	assert.Empty(t, stepsA)

	stepsA, err = isoA.ListSteps(ctx, state.StepFilter{})
	require.NoError(t, err)
	require.Len(t, stepsA, 1)

	stepsB, err := isoB.ListSteps(ctx, state.StepFilter{})
	require.NoError(t, err)
	require.Len(t, stepsB, 1)
	assert.Equal(t, "step-b", stepsB[0].ID)
}

func TestVerifyIsolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	isoA := NewIsolatedStore("tenantA", store, nil)
	isoB := NewIsolatedStore("tenantB", store, nil)

	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a1", "")))
	require.NoError(t, isoA.CreateRun(ctx, makeRun("run-a2", "")))
	require.NoError(t, isoB.CreateRun(ctx, makeRun("run-b1", "")))

	now := time.Now().UTC()
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID:        "trace-a",
		RunID:     "run-a1",
		Event:     "started",
		Actor:     "alice",
		Timestamp: now,
	}))
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID:        "trace-b",
		RunID:     "run-b1",
		Event:     "started",
		Actor:     "bob",
		Timestamp: now,
	}))
	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID:        "audit-a",
		RunID:     "run-a1",
		Action:    "plan",
		Actor:     "alice",
		Target:    "host1",
		Result:    "ok",
		Timestamp: now,
	}))
	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID:        "audit-b",
		RunID:     "run-b1",
		Action:    "plan",
		Actor:     "bob",
		Target:    "host2",
		Result:    "ok",
		Timestamp: now,
	}))

	require.NoError(t, VerifyIsolation(ctx, store, "tenantA", "tenantB"))
}

func TestVerifyIsolationViolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Manually create a run with no tenant tag (legacy record). It will
	// be invisible to both tenants, so VerifyIsolation should still pass.
	require.NoError(t, store.CreateRun(ctx, makeRun("run-legacy", "INC-1")))
	require.NoError(t, VerifyIsolation(ctx, store, "tenantA", "tenantB"))
}

func TestVerifyIsolationErrors(t *testing.T) {
	ctx := context.Background()

	// Nil store.
	err := VerifyIsolation(ctx, nil, "a", "b")
	assert.Error(t, err)

	store := newTestStore(t)

	// Empty tenant ids.
	err = VerifyIsolation(ctx, store, "", "b")
	assert.Error(t, err)

	// Same tenant ids.
	err = VerifyIsolation(ctx, store, "a", "a")
	assert.Error(t, err)
}

func TestIsolatedStoreNilGuards(t *testing.T) {
	var iso *IsolatedStore
	err := iso.CreateRun(context.Background(), makeRun("r", ""))
	assert.Error(t, err)

	_, err = iso.GetRun(context.Background(), "r")
	assert.Error(t, err)

	_, err = iso.ListRuns(context.Background(), state.RunFilter{})
	assert.Error(t, err)
}

func TestIsolatedStoreNilRun(t *testing.T) {
	store := newTestStore(t)
	iso := NewIsolatedStore("t1", store, nil)

	err := iso.CreateRun(context.Background(), nil)
	assert.Error(t, err)

	err = iso.UpdateRun(context.Background(), nil)
	assert.Error(t, err)
}

func TestIsolatedStoreTouchUpdatedAt(t *testing.T) {
	store := newTestStore(t)
	iso := NewIsolatedStore("t1", store, nil)
	// Should not panic.
	iso.TouchUpdatedAt(time.Now())
}

func TestIsolatedStoreTenantID(t *testing.T) {
	store := newTestStore(t)
	iso := NewIsolatedStore("t1", store, nil)
	assert.Equal(t, "t1", iso.TenantID())
}
