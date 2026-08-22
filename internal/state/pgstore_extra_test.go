// pgstore_extra_test.go exercises PGStore paths not covered by
// pgstore_test.go. Like pgstore_test.go, every test is skipped unless the
// environment variable LEVEE_PG_TEST_DSN points at a reachable PostgreSQL
// instance.

package state

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireTimeEqual asserts that two timestamps denote the same instant,
// tolerating location differences between the driver and the database.
func requireTimeEqual(t *testing.T, name string, want, got time.Time) {
	t.Helper()
	require.True(t, want.Equal(got), "%s: want %v, got %v", name, want, got)
}

// requireTimePtrEqual asserts equality of nullable timestamps.
func requireTimePtrEqual(t *testing.T, name string, want, got *time.Time) {
	t.Helper()
	if want == nil {
		assert.Nil(t, got, name)
		return
	}
	require.NotNil(t, got, name)
	require.True(t, want.Equal(*got), "%s: want %v, got %v", name, *want, *got)
}

func TestPGStore_NilRecordRejection(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"run create", func() error { return store.CreateRun(ctx, nil) }},
		{"run update", func() error { return store.UpdateRun(ctx, nil) }},
		{"batch create", func() error { return store.CreateBatch(ctx, nil) }},
		{"batch update", func() error { return store.UpdateBatch(ctx, nil) }},
		{"step create", func() error { return store.CreateStep(ctx, nil) }},
		{"step update", func() error { return store.UpdateStep(ctx, nil) }},
		{"trace create", func() error { return store.CreateTrace(ctx, nil) }},
		{"trace update", func() error { return store.UpdateTrace(ctx, nil) }},
		{"approval create", func() error { return store.CreateApproval(ctx, nil) }},
		{"approval update", func() error { return store.UpdateApproval(ctx, nil) }},
		{"lock create", func() error { return store.CreateLock(ctx, nil) }},
		{"lock update", func() error { return store.UpdateLock(ctx, nil) }},
		{"credential create", func() error { return store.CreateCredential(ctx, nil) }},
		{"credential update", func() error { return store.UpdateCredential(ctx, nil) }},
		{"audit create", func() error { return store.CreateAudit(ctx, nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "nil")
		})
	}
}

func TestPGStore_RunFiltersLimitAndTransitions(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)

	rA := newTestRun("pg-rA", base)
	rB := newTestRun("pg-rB", base.Add(time.Second))
	rB.Status, rB.Creator = "running", "bob"
	rC := newTestRun("pg-rC", base.Add(2*time.Second))
	rC.Status, rC.IncidentID = "failed", "INC-9"
	for _, r := range []*Run{rA, rB, rC} {
		require.NoError(t, store.CreateRun(ctx, r))
	}

	// Get missing row returns (nil, nil).
	got, err := store.GetRun(ctx, "pg-ghost")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Newest first without a filter.
	list, err := store.ListRuns(ctx, RunFilter{})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "pg-rC", list[0].ID)

	list, err = store.ListRuns(ctx, RunFilter{Status: "pending"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "pg-rA", list[0].ID)

	list, err = store.ListRuns(ctx, RunFilter{IncidentID: "INC-9"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "pg-rC", list[0].ID)

	// Limit boundaries.
	list, err = store.ListRuns(ctx, RunFilter{Limit: 0})
	require.NoError(t, err)
	assert.Len(t, list, 3)
	list, err = store.ListRuns(ctx, RunFilter{Limit: -1})
	require.NoError(t, err)
	assert.Len(t, list, 3)
	list, err = store.ListRuns(ctx, RunFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, []string{"pg-rC", "pg-rB"}, []string{list[0].ID, list[1].ID})

	// Status transition persists and leaves created_at untouched.
	rA.Status = "completed"
	rA.UpdatedAt = base.Add(time.Minute)
	require.NoError(t, store.UpdateRun(ctx, rA))
	got, err = store.GetRun(ctx, rA.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "completed", got.Status)
	requireTimeEqual(t, "created_at", base, got.CreatedAt)

	// Updating an unknown id reports not found.
	err = store.UpdateRun(ctx, newTestRun("pg-ghost", base))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	// Deleting the run cascades; deleting an unknown id is a no-op.
	require.NoError(t, store.DeleteRun(ctx, "pg-rB"))
	got, err = store.GetRun(ctx, "pg-rB")
	require.NoError(t, err)
	assert.Nil(t, got)
	require.NoError(t, store.DeleteRun(ctx, "pg-ghost"))
}

func TestPGStore_BatchUpdateUniqueAndFK(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	run := newTestRun("pg-run-b", now)
	require.NoError(t, store.CreateRun(ctx, run))

	batch := &Batch{ID: "pg-b1", RunID: run.ID, BatchNo: 1, Status: "running",
		TotalHosts: 4, Succeeded: 1, Failed: 0}
	require.NoError(t, store.CreateBatch(ctx, batch))

	// UNIQUE (run_id, batch_no).
	dup := &Batch{ID: "pg-b1-dup", RunID: run.ID, BatchNo: 1, Status: "pending"}
	err := store.CreateBatch(ctx, dup)
	require.Error(t, err)

	// FK violation against a missing run.
	err = store.CreateBatch(ctx, &Batch{ID: "pg-orphan", RunID: "nope", BatchNo: 1, Status: "pending"})
	require.Error(t, err)

	// Update completion counters.
	batch.Status = "completed"
	batch.Succeeded = 4
	done := now.Add(time.Minute)
	batch.CompletedAt = &done
	require.NoError(t, store.UpdateBatch(ctx, batch))
	got, err := store.GetBatch(ctx, batch.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, 4, got.Succeeded)
	requireTimePtrEqual(t, "completed_at", &done, got.CompletedAt)

	// Unknown id update reports not found; unknown id delete is a no-op.
	err = store.UpdateBatch(ctx, &Batch{ID: "pg-ghost", RunID: run.ID, BatchNo: 9, Status: "pending"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	require.NoError(t, store.DeleteBatch(ctx, "pg-ghost"))

	// Missing row returns (nil, nil).
	got, err = store.GetBatch(ctx, "pg-ghost")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPGStore_StepCRUDAndFilters(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	run := newTestRun("pg-run-s", now)
	require.NoError(t, store.CreateRun(ctx, run))
	require.NoError(t, store.CreateBatch(ctx, &Batch{ID: "pg-sb1", RunID: run.ID, BatchNo: 1, Status: "running"}))
	require.NoError(t, store.CreateBatch(ctx, &Batch{ID: "pg-sb2", RunID: run.ID, BatchNo: 2, Status: "running"}))

	t0 := now.Add(time.Second)
	t1 := now.Add(2 * time.Second)
	steps := []*Step{
		{ID: "pg-st1", RunID: run.ID, BatchID: "pg-sb1", Host: "h1", StepName: "n1",
			Action: "shell", Status: "success", ExitCode: ptrInt(0),
			Stdout: "out", Stderr: "err", DurationMs: 42, StartedAt: &t0},
		{ID: "pg-st2", RunID: run.ID, BatchID: "pg-sb1", Host: "h2", StepName: "n2",
			Action: "shell", Status: "failed", ExitCode: ptrInt(3), DurationMs: 7, StartedAt: &t1},
		{ID: "pg-st3", RunID: run.ID, BatchID: "pg-sb2", Host: "h1", StepName: "n3",
			Action: "copy", Status: "pending"},
	}
	for _, st := range steps {
		require.NoError(t, store.CreateStep(ctx, st))
	}

	// FK violation for an unknown batch.
	err := store.CreateStep(ctx, &Step{ID: "pg-orphan", RunID: run.ID, BatchID: "nope",
		Host: "h", StepName: "n", Action: "a", Status: "pending"})
	require.Error(t, err)

	// Field-level round trip. pgx returns timestamps with a concrete location,
	// so compare times via .Equal instead of whole-struct equality.
	got, err := store.GetStep(ctx, "pg-st1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, steps[0].ID, got.ID)
	assert.Equal(t, steps[0].RunID, got.RunID)
	assert.Equal(t, steps[0].BatchID, got.BatchID)
	assert.Equal(t, steps[0].Host, got.Host)
	assert.Equal(t, steps[0].Status, got.Status)
	require.NotNil(t, got.ExitCode)
	assert.Equal(t, *steps[0].ExitCode, *got.ExitCode)
	assert.Equal(t, steps[0].Stdout, got.Stdout)
	assert.Equal(t, steps[0].Stderr, got.Stderr)
	assert.Equal(t, steps[0].DurationMs, got.DurationMs)
	requireTimePtrEqual(t, "started_at", steps[0].StartedAt, got.StartedAt)

	// By batch / host / status.
	list, err := store.ListSteps(ctx, StepFilter{BatchID: "pg-sb1"})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "pg-st1", list[0].ID)

	// PostgreSQL ASC orders NULLs last (unlike SQLite where NULLs come first),
	// so the never-started step trails its started peers.
	list, err = store.ListSteps(ctx, StepFilter{Host: "h1"})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "pg-st1", list[0].ID)
	assert.Equal(t, "pg-st3", list[1].ID)

	list, err = store.ListSteps(ctx, StepFilter{Status: "failed"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "pg-st2", list[0].ID)

	list, err = store.ListSteps(ctx, StepFilter{RunID: run.ID, Limit: 1})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "pg-st1", list[0].ID)

	// Transition to terminal state.
	st2 := steps[1]
	st2.Status = "success"
	st2.ExitCode = ptrInt(0)
	finished := now.Add(3 * time.Second)
	st2.CompletedAt = &finished
	require.NoError(t, store.UpdateStep(ctx, st2))
	got, err = store.GetStep(ctx, "pg-st2")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "success", got.Status)
	requireTimePtrEqual(t, "completed_at", &finished, got.CompletedAt)

	// Unknown id update fails; unknown id delete is a no-op.
	err = store.UpdateStep(ctx, &Step{ID: "pg-ghost", RunID: run.ID, BatchID: "pg-sb1",
		Host: "h", StepName: "n", Action: "a", Status: "pending"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	require.NoError(t, store.DeleteStep(ctx, "pg-ghost"))
	require.NoError(t, store.DeleteStep(ctx, "pg-st3"))
	got, err = store.GetStep(ctx, "pg-st3")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPGStore_TraceHashChainAndWORM(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)

	run := newTestRun("pg-run-t", base)
	require.NoError(t, store.CreateRun(ctx, run))

	// Insert out of order with a shared timestamp to exercise the tie-breaker.
	traces := []*Trace{
		{ID: "pg-zz", RunID: run.ID, Event: "step_end", Actor: "executor",
			Detail: `{"host":"h1"}`, Timestamp: base.Add(time.Second)},
		{ID: "pg-aa", RunID: run.ID, Event: "step_end", Actor: "executor",
			Detail: `{"host":"h1"}`, Timestamp: base.Add(time.Second)},
		{ID: "pg-early", RunID: run.ID, Event: "plan", Actor: "planner",
			CurrHash: "seed", Timestamp: base},
	}
	for _, tr := range traces {
		require.NoError(t, store.CreateTrace(ctx, tr))
	}

	// FK violation for an unknown run.
	err := store.CreateTrace(ctx, &Trace{ID: "pg-orphan", RunID: "nope", Event: "plan",
		Actor: "a", CurrHash: "h", Timestamp: base})
	require.Error(t, err)

	// Order: timestamp ASC, id ASC tie-breaker.
	list, err := store.ListTraces(ctx, TraceFilter{RunID: run.ID})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"pg-early", "pg-aa", "pg-zz"}, []string{list[0].ID, list[1].ID, list[2].ID})

	// Fill in hash chain values on records appended without hashes; content
	// columns stay untouched so the WORM trigger allows the update.
	got, err := store.GetTrace(ctx, "pg-early")
	require.NoError(t, err)
	got.CurrHash = "hash-a"
	require.NoError(t, store.UpdateTrace(ctx, got))

	got, err = store.GetTrace(ctx, "pg-aa")
	require.NoError(t, err)
	got.PrevHash = "hash-a"
	got.CurrHash = "hash-b"
	require.NoError(t, store.UpdateTrace(ctx, got))

	list, err = store.ListTraces(ctx, TraceFilter{RunID: run.ID})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "hash-a", list[0].CurrHash)
	assert.Equal(t, list[0].CurrHash, list[1].PrevHash)
	assert.Equal(t, "hash-b", list[1].CurrHash)

	// Tampering with a content column is rejected by the WORM trigger even
	// when a hash column changes at the same time.
	got, err = store.GetTrace(ctx, "pg-zz")
	require.NoError(t, err)
	got.Event = "tampered"
	got.CurrHash = "evil"
	err = store.UpdateTrace(ctx, got)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WORM violation")

	// Filter by event.
	list, err = store.ListTraces(ctx, TraceFilter{RunID: run.ID, Event: "plan"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "pg-early", list[0].ID)
}

func TestPGStore_ApprovalCRUDFiltersCascade(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)

	run := newTestRun("pg-run-a", base)
	require.NoError(t, store.CreateRun(ctx, run))
	other := newTestRun("pg-run-a2", base)
	require.NoError(t, store.CreateRun(ctx, other))

	acted := base.Add(30 * time.Minute)
	approvals := []*Approval{
		{ID: "pg-ap3", RunID: run.ID, Level: "critical", Approver: "carol",
			Status: "rejected", Comment: "too risky"}, // NULL timeout_at sorts first
		{ID: "pg-ap2", RunID: run.ID, Level: "low", Approver: "bob", Status: "approved",
			Comment: "ok", TimeoutAt: ptrTime(base.Add(time.Hour)), ActedAt: &acted},
		{ID: "pg-ap1", RunID: run.ID, Level: "high", Approver: "alice", Status: "pending",
			TimeoutAt: ptrTime(base.Add(2 * time.Hour))},
		{ID: "pg-ap4", RunID: other.ID, Level: "high", Approver: "alice", Status: "pending",
			TimeoutAt: ptrTime(base.Add(3 * time.Hour))},
	}
	for _, a := range approvals {
		require.NoError(t, store.CreateApproval(ctx, a))
	}

	// FK violation for an unknown run.
	err := store.CreateApproval(ctx, &Approval{ID: "pg-orphan", RunID: "nope",
		Level: "low", Approver: "u", Status: "pending"})
	require.Error(t, err)

	// Round-trip of the nullable-heavy record.
	got, err := store.GetApproval(ctx, "pg-ap3")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, approvals[0], got)

	// Ordering: PostgreSQL ASC places the NULL timeout_at last (SQLite sorts
	// NULLs first), so the rejected approval trails the timed-out peers.
	list, err := store.ListApprovals(ctx, ApprovalFilter{RunID: run.ID})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"pg-ap2", "pg-ap1", "pg-ap3"}, []string{list[0].ID, list[1].ID, list[2].ID})

	// Filters.
	list, err = store.ListApprovals(ctx, ApprovalFilter{Level: "high"})
	require.NoError(t, err)
	require.Len(t, list, 2)
	list, err = store.ListApprovals(ctx, ApprovalFilter{Status: "approved"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "pg-ap2", list[0].ID)
	list, err = store.ListApprovals(ctx, ApprovalFilter{RunID: run.ID, Limit: 2})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "pg-ap2", list[0].ID)

	// Approve flow.
	ap1 := approvals[2]
	ap1.Status = "approved"
	ap1.Comment = "lgtm"
	decided := base.Add(time.Minute)
	ap1.ActedAt = &decided
	require.NoError(t, store.UpdateApproval(ctx, ap1))
	got, err = store.GetApproval(ctx, ap1.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "approved", got.Status)
	assert.Equal(t, "lgtm", got.Comment)
	requireTimePtrEqual(t, "acted_at", &decided, got.ActedAt)

	// Unknown id update fails; unknown id delete is a no-op.
	err = store.UpdateApproval(ctx, &Approval{ID: "pg-ghost", RunID: run.ID,
		Level: "low", Approver: "u", Status: "pending"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	require.NoError(t, store.DeleteApproval(ctx, "pg-ghost"))

	// Cascade delete through the parent run spares other runs' approvals.
	require.NoError(t, store.DeleteRun(ctx, run.ID))
	got, err = store.GetApproval(ctx, "pg-ap1")
	require.NoError(t, err)
	assert.Nil(t, got)
	got, err = store.GetApproval(ctx, "pg-ap4")
	require.NoError(t, err)
	require.NotNil(t, got)

	// Missing rows return (nil, nil).
	got, err = store.GetApproval(ctx, "pg-ghost")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPGStore_LockLifecycle(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	active := &Lock{ID: "pg-l-active", Scope: "host:pg-a", Owner: "run-orig", TTLSeconds: 3600,
		AcquiredAt: now, ExpiresAt: now.Add(time.Hour)}
	expired := &Lock{ID: "pg-l-expired", Scope: "host:pg-b", Owner: "run-orig", TTLSeconds: 60,
		AcquiredAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	boundary := &Lock{ID: "pg-l-boundary", Scope: "host:pg-c", Owner: "run-orig", TTLSeconds: 60,
		AcquiredAt: now.Add(-time.Hour), ExpiresAt: now}
	for _, l := range []*Lock{active, expired, boundary} {
		require.NoError(t, store.CreateLock(ctx, l))
	}

	// Duplicate scope rejected.
	dup := *expired
	dup.ID = "pg-l-dup"
	require.Error(t, store.CreateLock(ctx, &dup))

	// Active lock cannot be taken over.
	n, err := store.UpdateLockOwnedBy(ctx, active.ID, "run-thief", 30, now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// Expired lock is taken over with fresh ownership window.
	n, err = store.UpdateLockOwnedBy(ctx, expired.ID, "run-new", 120, now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	got, err := store.GetLock(ctx, expired.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "run-new", got.Owner)
	assert.Equal(t, 120, got.TTLSeconds)
	requireTimeEqual(t, "acquired_at", now, got.AcquiredAt)
	requireTimeEqual(t, "expires_at", now.Add(120*time.Second), got.ExpiresAt)

	// Boundary expiry equal to now counts as expired (expires_at <= now).
	n, err = store.UpdateLockOwnedBy(ctx, boundary.ID, "run-boundary", 45, now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	// Active lock survives DeleteExpiredLocks (strictly-before semantics);
	// both renewed locks are gone already.
	n, err = store.DeleteExpiredLocks(ctx, now)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
	survivor, err := store.GetLock(ctx, active.ID)
	require.NoError(t, err)
	require.NotNil(t, survivor)

	// UpdateLock overwrites mutable columns; unknown id reports not found.
	active.Owner = "run-next"
	renewed := now.Add(2 * time.Hour)
	active.ExpiresAt = renewed
	require.NoError(t, store.UpdateLock(ctx, active))
	got, err = store.GetLock(ctx, active.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "run-next", got.Owner)
	requireTimeEqual(t, "renewed expiry", renewed, got.ExpiresAt)

	err = store.UpdateLock(ctx, &Lock{ID: "pg-ghost", Scope: "host:x", Owner: "o",
		TTLSeconds: 1, AcquiredAt: now, ExpiresAt: now})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	// Scope lookup hit and miss.
	got, err = store.GetLockByScope(ctx, "host:pg-a")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, active.ID, got.ID)
	got, err = store.GetLockByScope(ctx, "host:none")
	require.NoError(t, err)
	assert.Nil(t, got)

	// ListLocks orders by expires_at ascending. All three locks are live at
	// this point: the two taken-over ones expire soonest, the renewed active
	// lock much later.
	list, err := store.ListLocks(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{boundary.ID, expired.ID, active.ID},
		[]string{list[0].ID, list[1].ID, list[2].ID})

	// Delete hit and miss.
	require.NoError(t, store.DeleteLock(ctx, active.ID))
	got, err = store.GetLock(ctx, active.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
	require.NoError(t, store.DeleteLock(ctx, active.ID))
}

func TestPGStore_CredentialCRUDComplete(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	cred := &Credential{ID: "pg-c1", Name: "ssh-pg-key", Type: "ssh_key",
		EncryptedData: []byte{0xDE, 0xAD}, CreatedAt: now}
	other := &Credential{ID: "pg-c2", Name: "vault-ref-pg", Type: "vault_ref",
		EncryptedData: []byte{0x01}, CreatedAt: now}
	require.NoError(t, store.CreateCredential(ctx, cred))
	require.NoError(t, store.CreateCredential(ctx, other))

	// Duplicate name rejected.
	dup := *cred
	dup.ID = "pg-c1-dup"
	require.Error(t, store.CreateCredential(ctx, &dup))

	got, err := store.GetCredential(ctx, cred.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, cred.EncryptedData, got.EncryptedData)

	got, err = store.GetCredentialByName(ctx, cred.Name)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, cred.ID, got.ID)

	got, err = store.GetCredentialByName(ctx, "no-such-name")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Rotation updates ciphertext and RotatedAt.
	rotated := now.Add(time.Hour)
	cred.EncryptedData = []byte{0x09, 0x09, 0x09}
	cred.RotatedAt = &rotated
	require.NoError(t, store.UpdateCredential(ctx, cred))
	got, err = store.GetCredential(ctx, cred.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []byte{0x09, 0x09, 0x09}, got.EncryptedData)
	require.NotNil(t, got.RotatedAt)
	requireTimeEqual(t, "rotated_at", rotated, *got.RotatedAt)

	// List ordered by name ascending.
	list, err := store.ListCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "ssh-pg-key", list[0].Name)

	// Unknown id update fails; delete hit and miss.
	err = store.UpdateCredential(ctx, &Credential{ID: "pg-ghost", Name: "n", Type: "t",
		EncryptedData: []byte{1}, CreatedAt: now})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	require.NoError(t, store.DeleteCredential(ctx, cred.ID))
	got, err = store.GetCredential(ctx, cred.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
	require.NoError(t, store.DeleteCredential(ctx, cred.ID))
}

func TestPGStore_AuditFiltersOrdering(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)

	audits := []*Audit{
		{ID: "pg-au1", RunID: "pg-run-x", Action: "apply", Actor: "alice",
			Target: "h1", Result: "success", Timestamp: base},
		{ID: "pg-au2", RunID: "pg-run-x", Action: "verify", Actor: "alice",
			Target: "h1", Result: "success", Timestamp: base.Add(time.Second)},
		{ID: "pg-au3", RunID: "pg-run-y", Action: "apply", Actor: "bob",
			Target: "h2", Result: "failure", Timestamp: base.Add(2 * time.Second)},
	}
	for _, a := range audits {
		require.NoError(t, store.CreateAudit(ctx, a))
	}

	got, err := store.GetAudit(ctx, "pg-au1")
	require.NoError(t, err)
	require.NotNil(t, got)
	// Field-wise comparison: pgx returns timestamps with a concrete location,
	// so struct equality would spuriously fail on time.Time internals.
	assert.Equal(t, audits[0].ID, got.ID)
	assert.Equal(t, audits[0].RunID, got.RunID)
	assert.Equal(t, audits[0].Action, got.Action)
	assert.Equal(t, audits[0].Actor, got.Actor)
	assert.Equal(t, audits[0].Target, got.Target)
	assert.Equal(t, audits[0].Result, got.Result)
	requireTimeEqual(t, "timestamp", audits[0].Timestamp, got.Timestamp)

	got, err = store.GetAudit(ctx, "pg-ghost")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Newest first across runs.
	list, err := store.ListAudits(ctx, AuditFilter{})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "pg-au3", list[0].ID)

	// Filters combine.
	list, err = store.ListAudits(ctx, AuditFilter{Action: "apply", Actor: "alice"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "pg-au1", list[0].ID)

	list, err = store.ListAudits(ctx, AuditFilter{RunID: "pg-run-x", Limit: 1})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "pg-au2", list[0].ID)
}
