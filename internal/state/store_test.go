package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore returns a fresh in-memory store for each test. Each test gets
// its own shared-cache database name so concurrent tests do not interfere.
func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	ctx := context.Background()
	// Use a temp file instead of :memory: so WAL pragmas apply and we get
	// behaviour closer to production. The temp file is cleaned up via Close.
	dir := t.TempDir()
	path := filepath.Join(dir, "levee-test.db")
	store, err := NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func ptrTime(t time.Time) *time.Time { return &t }
func ptrInt(i int) *int              { return &i }

// =========================================================================
// Migrate / pragma tests
// =========================================================================

func TestMigrate_Idempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Running migrate again on the same DB must succeed and be a no-op.
	err := Migrate(ctx, store.DB())
	require.NoError(t, err)

	// schema_version must record the current version.
	var version int
	err = store.DB().QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version)
	require.NoError(t, err)
	assert.Equal(t, currentSchemaVersion, version)
}

func TestPragma_WALMode(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	var mode string
	err := store.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
	require.NoError(t, err)
	// SQLite returns the journal mode in lowercase.
	assert.Equal(t, "wal", mode)
}

func TestPragma_ForeignKeysOn(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	var fk int
	err := store.DB().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk)
	require.NoError(t, err)
	assert.Equal(t, 1, fk)
}

// =========================================================================
// Run CRUD
// =========================================================================

func TestRun_CRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	run := &Run{
		ID:             "run-1",
		WorkflowName:   "patch-linux",
		TemplateName:   "kernel-patch",
		Params:         `{"version":"5.15"}`,
		PlanHash:       "abc123",
		Status:         "pending",
		ApprovalStatus: "pending",
		ApprovalLevel:  "high",
		CreatedAt:      now,
		UpdatedAt:      now,
		Creator:        "alice",
		IncidentID:     "INC-42",
	}

	// Create.
	require.NoError(t, store.CreateRun(ctx, run))

	// Get.
	got, err := store.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, run, got)

	// Get non-existent returns (nil, nil).
	got, err = store.GetRun(ctx, "nope")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Update.
	run.Status = "running"
	run.UpdatedAt = now.Add(time.Minute)
	require.NoError(t, store.UpdateRun(ctx, run))
	got, err = store.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "running", got.Status)

	// List with filter.
	run2 := *run
	run2.ID = "run-2"
	run2.Status = "pending"
	run2.CreatedAt = now.Add(time.Second)
	require.NoError(t, store.CreateRun(ctx, &run2))

	list, err := store.ListRuns(ctx, RunFilter{Status: "running"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "run-1", list[0].ID)

	// List all (ordered by created_at DESC -> run-2 first).
	list, err = store.ListRuns(ctx, RunFilter{})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "run-2", list[0].ID)

	// Limit.
	list, err = store.ListRuns(ctx, RunFilter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Delete.
	require.NoError(t, store.DeleteRun(ctx, run.ID))
	got, err = store.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestRun_DuplicateIDRejected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	run := &Run{
		ID: "dup", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "pending", ApprovalStatus: "pending", ApprovalLevel: "low",
		CreatedAt: now, UpdatedAt: now, Creator: "u",
	}
	require.NoError(t, store.CreateRun(ctx, run))
	err := store.CreateRun(ctx, run)
	assert.Error(t, err)
}

// =========================================================================
// Batch CRUD + FK
// =========================================================================

func TestBatch_CRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Parent run must exist.
	run := &Run{
		ID: "r1", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "running", ApprovalStatus: "approved", ApprovalLevel: "low",
		CreatedAt: now, UpdatedAt: now, Creator: "u",
	}
	require.NoError(t, store.CreateRun(ctx, run))

	started := now.Add(time.Second)
	batch := &Batch{
		ID: "b1", RunID: "r1", BatchNo: 1, Status: "running",
		TotalHosts: 10, Succeeded: 3, Failed: 0, StartedAt: &started,
	}
	require.NoError(t, store.CreateBatch(ctx, batch))

	got, err := store.GetBatch(ctx, batch.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, batch, got)

	// Update.
	batch.Status = "completed"
	batch.Succeeded = 10
	completed := now.Add(time.Minute)
	batch.CompletedAt = &completed
	require.NoError(t, store.UpdateBatch(ctx, batch))
	got, err = store.GetBatch(ctx, batch.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, 10, got.Succeeded)

	// List by run.
	batch2 := &Batch{ID: "b2", RunID: "r1", BatchNo: 2, Status: "pending"}
	require.NoError(t, store.CreateBatch(ctx, batch2))
	list, err := store.ListBatches(ctx, BatchFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "b1", list[0].ID) // ordered by batch_no ASC

	// Delete.
	require.NoError(t, store.DeleteBatch(ctx, batch.ID))
	got, err = store.GetBatch(ctx, batch.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestBatch_FKViolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// No parent run exists; creating a batch referencing a non-existent run
	// must fail because of the foreign key constraint.
	batch := &Batch{ID: "orphan", RunID: "no-such-run", BatchNo: 1, Status: "pending"}
	err := store.CreateBatch(ctx, batch)
	require.Error(t, err)
}

func TestBatch_CascadeDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	run := &Run{
		ID: "r1", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "running", ApprovalStatus: "approved", ApprovalLevel: "low",
		CreatedAt: now, UpdatedAt: now, Creator: "u",
	}
	require.NoError(t, store.CreateRun(ctx, run))
	require.NoError(t, store.CreateBatch(ctx, &Batch{ID: "b1", RunID: "r1", BatchNo: 1, Status: "running"}))

	// Deleting the run must cascade-delete the batch.
	require.NoError(t, store.DeleteRun(ctx, "r1"))
	got, err := store.GetBatch(ctx, "b1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// =========================================================================
// Step CRUD + FK
// =========================================================================

func TestStep_CRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, store.CreateRun(ctx, &Run{
		ID: "r1", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "running", ApprovalStatus: "approved", ApprovalLevel: "low",
		CreatedAt: now, UpdatedAt: now, Creator: "u",
	}))
	require.NoError(t, store.CreateBatch(ctx, &Batch{ID: "b1", RunID: "r1", BatchNo: 1, Status: "running"}))

	started := now.Add(time.Second)
	completed := started.Add(500 * time.Millisecond)
	step := &Step{
		ID: "s1", RunID: "r1", BatchID: "b1", Host: "host-01",
		StepName: "apply-patch", Action: "shell", Status: "success",
		ExitCode: ptrInt(0), Stdout: "ok", Stderr: "", DurationMs: 500,
		StartedAt: &started, CompletedAt: &completed,
	}
	require.NoError(t, store.CreateStep(ctx, step))

	got, err := store.GetStep(ctx, step.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, step, got)

	// Update.
	step.Status = "failed"
	step.ExitCode = ptrInt(1)
	step.Stderr = "boom"
	require.NoError(t, store.UpdateStep(ctx, step))
	got, err = store.GetStep(ctx, step.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "failed", got.Status)
	require.NotNil(t, got.ExitCode)
	assert.Equal(t, 1, *got.ExitCode)

	// List by run.
	list, err := store.ListSteps(ctx, StepFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, list, 1)

	// List by host.
	list, err = store.ListSteps(ctx, StepFilter{Host: "host-01"})
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Delete.
	require.NoError(t, store.DeleteStep(ctx, step.ID))
	got, err = store.GetStep(ctx, step.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStep_FKViolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	step := &Step{ID: "s1", RunID: "no-run", BatchID: "no-batch", Host: "h", StepName: "n", Action: "a", Status: "pending"}
	err := store.CreateStep(ctx, step)
	require.Error(t, err)
}

// =========================================================================
// Trace CRUD
// =========================================================================

func TestTrace_CRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, store.CreateRun(ctx, &Run{
		ID: "r1", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "running", ApprovalStatus: "approved", ApprovalLevel: "low",
		CreatedAt: now, UpdatedAt: now, Creator: "u",
	}))

	trace := &Trace{
		ID: "t1", RunID: "r1", Event: "step_end", Actor: "executor",
		Detail: `{"host":"h1"}`, PrevHash: "", CurrHash: "h1", Timestamp: now,
	}
	require.NoError(t, store.CreateTrace(ctx, trace))

	trace2 := &Trace{
		ID: "t2", RunID: "r1", Event: "step_end", Actor: "executor",
		Detail: `{"host":"h2"}`, PrevHash: "h1", CurrHash: "h2", Timestamp: now.Add(time.Second),
	}
	require.NoError(t, store.CreateTrace(ctx, trace2))

	got, err := store.GetTrace(ctx, trace.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, trace, got)

	// List ordered by timestamp ASC.
	list, err := store.ListTraces(ctx, TraceFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "t1", list[0].ID)
	assert.Equal(t, "t2", list[1].ID)

	// Filter by event.
	list, err = store.ListTraces(ctx, TraceFilter{RunID: "r1", Event: "step_end"})
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Delete.
	require.NoError(t, store.DeleteTrace(ctx, trace.ID))
	got, err = store.GetTrace(ctx, trace.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// =========================================================================
// Approval CRUD
// =========================================================================

func TestApproval_CRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, store.CreateRun(ctx, &Run{
		ID: "r1", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "running", ApprovalStatus: "pending", ApprovalLevel: "high",
		CreatedAt: now, UpdatedAt: now, Creator: "u",
	}))

	timeout := now.Add(time.Hour)
	approval := &Approval{
		ID: "a1", RunID: "r1", Level: "high", Approver: "bob",
		Status: "pending", Comment: "", TimeoutAt: &timeout,
	}
	require.NoError(t, store.CreateApproval(ctx, approval))

	got, err := store.GetApproval(ctx, approval.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, approval, got)

	// Update (approve).
	acted := now.Add(time.Minute)
	approval.Status = "approved"
	approval.Comment = "lgtm"
	approval.ActedAt = &acted
	require.NoError(t, store.UpdateApproval(ctx, approval))
	got, err = store.GetApproval(ctx, approval.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "approved", got.Status)
	assert.Equal(t, "lgtm", got.Comment)

	// List.
	list, err := store.ListApprovals(ctx, ApprovalFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Delete.
	require.NoError(t, store.DeleteApproval(ctx, approval.ID))
	got, err = store.GetApproval(ctx, approval.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// =========================================================================
// Lock CRUD
// =========================================================================

func TestLock_CRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	lock := &Lock{
		ID: "l1", Scope: "host:host-01", Owner: "run-1", TTLSeconds: 3600,
		AcquiredAt: now, ExpiresAt: now.Add(time.Hour),
	}
	require.NoError(t, store.CreateLock(ctx, lock))

	got, err := store.GetLock(ctx, lock.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, lock, got)

	// Get by scope.
	got, err = store.GetLockByScope(ctx, lock.Scope)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, lock.ID, got.ID)

	// Duplicate scope rejected.
	dup := *lock
	dup.ID = "l2"
	err = store.CreateLock(ctx, &dup)
	require.Error(t, err)

	// Update.
	lock.Owner = "run-2"
	require.NoError(t, store.UpdateLock(ctx, lock))
	got, err = store.GetLock(ctx, lock.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "run-2", got.Owner)

	// List.
	list, err := store.ListLocks(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Delete expired.
	expired := now.Add(-time.Hour)
	expLock := &Lock{
		ID: "l3", Scope: "host:host-02", Owner: "run-3", TTLSeconds: 60,
		AcquiredAt: expired, ExpiresAt: expired.Add(time.Minute),
	}
	require.NoError(t, store.CreateLock(ctx, expLock))
	n, err := store.DeleteExpiredLocks(ctx, now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	// Delete.
	require.NoError(t, store.DeleteLock(ctx, lock.ID))
	got, err = store.GetLock(ctx, lock.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// =========================================================================
// Credential CRUD
// =========================================================================

func TestCredential_CRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	cred := &Credential{
		ID: "c1", Name: "ssh-prod-key", Type: "ssh_key",
		EncryptedData: []byte{0xDE, 0xAD, 0xBE, 0xEF}, CreatedAt: now,
	}
	require.NoError(t, store.CreateCredential(ctx, cred))

	got, err := store.GetCredential(ctx, cred.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, cred, got)

	// Get by name.
	got, err = store.GetCredentialByName(ctx, cred.Name)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, cred.ID, got.ID)

	// Duplicate name rejected.
	dup := *cred
	dup.ID = "c2"
	err = store.CreateCredential(ctx, &dup)
	require.Error(t, err)

	// Update (rotate).
	rotated := now.Add(time.Hour)
	cred.EncryptedData = []byte{0x01, 0x02, 0x03, 0x04}
	cred.RotatedAt = &rotated
	require.NoError(t, store.UpdateCredential(ctx, cred))
	got, err = store.GetCredential(ctx, cred.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, got.EncryptedData)
	require.NotNil(t, got.RotatedAt)

	// List.
	list, err := store.ListCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Delete.
	require.NoError(t, store.DeleteCredential(ctx, cred.ID))
	got, err = store.GetCredential(ctx, cred.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// =========================================================================
// Audit CRUD
// =========================================================================

func TestAudit_CRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	audit := &Audit{
		ID: "a1", RunID: "r1", Action: "apply", Actor: "alice",
		Target: "host-01", Result: "success", Timestamp: now,
	}
	require.NoError(t, store.CreateAudit(ctx, audit))

	audit2 := &Audit{
		ID: "a2", RunID: "r1", Action: "verify", Actor: "alice",
		Target: "host-01", Result: "success", Timestamp: now.Add(time.Second),
	}
	require.NoError(t, store.CreateAudit(ctx, audit2))

	got, err := store.GetAudit(ctx, audit.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, audit, got)

	// List ordered by timestamp DESC.
	list, err := store.ListAudits(ctx, AuditFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "a2", list[0].ID)
	assert.Equal(t, "a1", list[1].ID)

	// Filter by action.
	list, err = store.ListAudits(ctx, AuditFilter{Action: "apply"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "a1", list[0].ID)

	// Filter by actor.
	list, err = store.ListAudits(ctx, AuditFilter{Actor: "alice"})
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Limit.
	list, err = store.ListAudits(ctx, AuditFilter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, list, 1)
}

// =========================================================================
// Store interface compile-time check
// =========================================================================

// Compile-time assertion that SQLiteStore implements Store.
var _ Store = (*SQLiteStore)(nil)
