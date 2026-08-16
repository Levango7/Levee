// pgstore_test.go exercises the PostgreSQL Store. Tests are skipped unless the
// environment variable LEVEE_PG_TEST_DSN is set to a reachable PostgreSQL DSN.
// This keeps `go test ./...` green in environments without a PostgreSQL
// instance (the common case for CI on Windows / macOS dev laptops).
//
// To run the tests locally:
//
//	LEVEE_PG_TEST_DSN="postgres://postgres:postgres@localhost:5432/levee_test?sslmode=disable" \
//	    go test ./internal/state/ -run PG -v

package state

import (
	"context"
	"os"
	"testing"
	"time"
)

// pgTestDSN returns the PostgreSQL DSN from the environment, or "" if unset.
func pgTestDSN() string {
	return os.Getenv("LEVEE_PG_TEST_DSN")
}

// newPGTestStore opens a fresh PGStore connected to the test database. The
// schema is applied by NewPGStore; we additionally truncate all tables between
// tests so each test starts from a clean slate. Returns (store, cleanup) or
// skips the test when no DSN is available.
func newPGTestStore(t *testing.T) (*PGStore, func()) {
	t.Helper()
	dsn := pgTestDSN()
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL store test")
	}
	ctx := context.Background()
	store, err := NewPGStore(ctx, dsn, PGPoolConfig{
		MaxOpenConns: 5,
		MaxIdleConns: 2,
	})
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}

	// Truncate all data tables so tests are independent. Order matters because
	// of FK constraints: children first, then parents.
	truncateTables := []string{
		"audit", "credentials", "locks", "approvals", "trace", "steps",
		"batches", "runs", "cluster_nodes",
	}
	for _, tbl := range truncateTables {
		if _, err := store.DB().ExecContext(ctx, "TRUNCATE TABLE "+tbl+" RESTART IDENTITY CASCADE"); err != nil {
			// Table may not exist in older schemas; ignore the error.
			t.Logf("truncate %s: %v (continuing)", tbl, err)
		}
	}

	cleanup := func() {
		_ = store.Close()
	}
	return store, cleanup
}

// TestPGStoreRunCRUD exercises the Run CRUD methods against a real PostgreSQL
// instance. Skipped when LEVEE_PG_TEST_DSN is not set.
func TestPGStoreRunCRUD(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	run := &Run{
		ID:             "run-001",
		WorkflowName:   "wf",
		TemplateName:   "tpl",
		Params:         "{}",
		PlanHash:       "deadbeef",
		Status:         "pending",
		ApprovalStatus: "pending",
		ApprovalLevel:  "",
		CreatedAt:      now,
		UpdatedAt:      now,
		Creator:        "tester",
		IncidentID:     "",
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got == nil {
		t.Fatal("GetRun returned nil")
	}
	if got.WorkflowName != run.WorkflowName {
		t.Errorf("WorkflowName = %q, want %q", got.WorkflowName, run.WorkflowName)
	}

	// Update.
	got.Status = "running"
	got.UpdatedAt = time.Now().UTC()
	if err := store.UpdateRun(ctx, got); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	got2, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after update: %v", err)
	}
	if got2.Status != "running" {
		t.Errorf("Status = %q, want running", got2.Status)
	}

	// List.
	runs, err := store.ListRuns(ctx, RunFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("ListRuns len = %d, want 1", len(runs))
	}

	// Delete.
	if err := store.DeleteRun(ctx, run.ID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	got3, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after delete: %v", err)
	}
	if got3 != nil {
		t.Error("GetRun after delete returned non-nil")
	}
}

// TestPGStoreBatchCRUD exercises Batch CRUD.
func TestPGStoreBatchCRUD(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	run := &Run{
		ID: "run-batch", WorkflowName: "wf", TemplateName: "tpl", Params: "{}",
		PlanHash: "h", Status: "running", ApprovalStatus: "approved",
		CreatedAt: now, UpdatedAt: now, Creator: "tester",
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	batch := &Batch{
		ID: "batch-001", RunID: run.ID, BatchNo: 1, Status: "pending",
		TotalHosts: 3, Succeeded: 0, Failed: 0,
	}
	if err := store.CreateBatch(ctx, batch); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	got, err := store.GetBatch(ctx, batch.ID)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if got == nil || got.TotalHosts != 3 {
		t.Fatalf("GetBatch = %+v, err %v", got, err)
	}

	batches, err := store.ListBatches(ctx, BatchFilter{RunID: run.ID})
	if err != nil {
		t.Fatalf("ListBatches: %v", err)
	}
	if len(batches) != 1 {
		t.Errorf("ListBatches len = %d, want 1", len(batches))
	}

	if err := store.DeleteBatch(ctx, batch.ID); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
}

// TestPGStoreLockCRUD exercises Lock CRUD including the unique-scope constraint.
func TestPGStoreLockCRUD(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	lock := &Lock{
		ID: "lock-001", Scope: "host:node1", Owner: "run-1",
		TTLSeconds: 60, AcquiredAt: now, ExpiresAt: now.Add(60 * time.Second),
	}
	if err := store.CreateLock(ctx, lock); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}

	// Duplicate scope must fail.
	dup := *lock
	dup.ID = "lock-002"
	if err := store.CreateLock(ctx, &dup); err == nil {
		t.Fatal("CreateLock duplicate scope: expected error, got nil")
	}

	got, err := store.GetLockByScope(ctx, lock.Scope)
	if err != nil {
		t.Fatalf("GetLockByScope: %v", err)
	}
	if got == nil || got.Owner != lock.Owner {
		t.Fatalf("GetLockByScope = %+v", got)
	}

	// Expired lock deletion.
	expired := now.Add(-time.Second)
	n, err := store.DeleteExpiredLocks(ctx, expired)
	if err != nil {
		t.Fatalf("DeleteExpiredLocks: %v", err)
	}
	if n != 1 {
		t.Errorf("DeleteExpiredLocks n = %d, want 1", n)
	}
}

// TestPGStoreCredentialCRUD exercises Credential CRUD with BYTEA encryption.
func TestPGStoreCredentialCRUD(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	cred := &Credential{
		ID: "cred-001", Name: "ssh-prod", Type: "ssh_key",
		EncryptedData: []byte{0x01, 0x02, 0x03, 0x04, 0x05},
		CreatedAt:     now,
	}
	if err := store.CreateCredential(ctx, cred); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	got, err := store.GetCredentialByName(ctx, cred.Name)
	if err != nil {
		t.Fatalf("GetCredentialByName: %v", err)
	}
	if got == nil || string(got.EncryptedData) != string(cred.EncryptedData) {
		t.Fatalf("GetCredentialByName = %+v", got)
	}
}

// TestPGStoreAuditCRUD exercises Audit CRUD.
func TestPGStoreAuditCRUD(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	audit := &Audit{
		ID: "audit-001", RunID: "run-x", Action: "apply", Actor: "tester",
		Target: "host1", Result: "success", Timestamp: now,
	}
	if err := store.CreateAudit(ctx, audit); err != nil {
		t.Fatalf("CreateAudit: %v", err)
	}
	audits, err := store.ListAudits(ctx, AuditFilter{RunID: "run-x"})
	if err != nil {
		t.Fatalf("ListAudits: %v", err)
	}
	if len(audits) != 1 {
		t.Errorf("ListAudits len = %d, want 1", len(audits))
	}
}

// TestPGStoreWORMTraceDelete verifies that the PostgreSQL WORM trigger rejects
// deletion of trace records.
func TestPGStoreWORMTraceDelete(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	run := &Run{
		ID: "run-worm", WorkflowName: "wf", TemplateName: "tpl", Params: "{}",
		PlanHash: "h", Status: "running", ApprovalStatus: "approved",
		CreatedAt: now, UpdatedAt: now, Creator: "tester",
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	tr := &Trace{
		ID: "trace-001", RunID: run.ID, Event: "plan", Actor: "tester",
		Detail: "{}", PrevHash: "", CurrHash: "abc", Timestamp: now,
	}
	if err := store.CreateTrace(ctx, tr); err != nil {
		t.Fatalf("CreateTrace: %v", err)
	}

	// Delete must be rejected by the trigger.
	if err := store.DeleteTrace(ctx, tr.ID); err == nil {
		t.Fatal("DeleteTrace: expected WORM violation error, got nil")
	}
}

// TestPGStoreInterfaceConformance is a compile-time check that PGStore
// implements the Store interface. It is always run (no DSN required).
func TestPGStoreInterfaceConformance(t *testing.T) {
	var _ Store = (*PGStore)(nil)
}
