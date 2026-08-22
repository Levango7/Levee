package state

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compile-time assertion that SQLiteStore also satisfies the restricted
// WORMStore interface used by audit/WORM contexts.
var _ WORMStore = (*SQLiteStore)(nil)

// newTestRun builds a minimally valid run with deterministic fields derived
// from id. Callers may override any field afterwards.
func newTestRun(id string, createdAt time.Time) *Run {
	return &Run{
		ID:             id,
		WorkflowName:   "wf-" + id,
		TemplateName:   "tpl-" + id,
		Params:         `{"k":"v"}`,
		PlanHash:       "plan-" + id,
		Status:         "pending",
		ApprovalStatus: "pending",
		ApprovalLevel:  "low",
		CreatedAt:      createdAt,
		UpdatedAt:      createdAt,
		Creator:        "tester",
	}
}

// mustCreateRun creates the run or fails the test immediately.
func mustCreateRun(t *testing.T, store *SQLiteStore, run *Run) {
	t.Helper()
	require.NoError(t, store.CreateRun(context.Background(), run))
}

// =========================================================================
// Constructor / migrate / raw exec error paths
// =========================================================================

func TestNewSQLiteStore_RejectsEmptyPath(t *testing.T) {
	_, err := NewSQLiteStore(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty db path")
}

func TestNewSQLiteStore_MemoryDSN(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	run := newTestRun("mem-run-1", now)
	require.NoError(t, store.CreateRun(ctx, run))

	got, err := store.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, run, got)
}

func TestMigrate_NilDBRejected(t *testing.T) {
	err := Migrate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil db")
}

func TestExecRaw_InvalidSQLReturnsError(t *testing.T) {
	store := newTestStore(t)
	err := store.ExecRaw(context.Background(), "THIS IS NOT VALID SQL")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exec raw")
}

// =========================================================================
// Nil-argument rejection for every Create*/Update* method
// =========================================================================

func TestCreate_NilRecordRejected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"run", func() error { return store.CreateRun(ctx, nil) }},
		{"batch", func() error { return store.CreateBatch(ctx, nil) }},
		{"step", func() error { return store.CreateStep(ctx, nil) }},
		{"trace", func() error { return store.CreateTrace(ctx, nil) }},
		{"approval", func() error { return store.CreateApproval(ctx, nil) }},
		{"lock", func() error { return store.CreateLock(ctx, nil) }},
		{"credential", func() error { return store.CreateCredential(ctx, nil) }},
		{"audit", func() error { return store.CreateAudit(ctx, nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "nil")
		})
	}
}

func TestUpdate_NilRecordRejected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"run", func() error { return store.UpdateRun(ctx, nil) }},
		{"batch", func() error { return store.UpdateBatch(ctx, nil) }},
		{"step", func() error { return store.UpdateStep(ctx, nil) }},
		{"trace", func() error { return store.UpdateTrace(ctx, nil) }},
		{"approval", func() error { return store.UpdateApproval(ctx, nil) }},
		{"lock", func() error { return store.UpdateLock(ctx, nil) }},
		{"credential", func() error { return store.UpdateCredential(ctx, nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "nil")
		})
	}
}

// =========================================================================
// Get*: missing rows return (nil, nil)
// =========================================================================

func TestGet_MissingRowReturnsNilNil(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() (any, error)
	}{
		{"run", func() (any, error) { return store.GetRun(ctx, "ghost") }},
		{"batch", func() (any, error) { return store.GetBatch(ctx, "ghost") }},
		{"step", func() (any, error) { return store.GetStep(ctx, "ghost") }},
		{"trace", func() (any, error) { return store.GetTrace(ctx, "ghost") }},
		{"approval", func() (any, error) { return store.GetApproval(ctx, "ghost") }},
		{"lock", func() (any, error) { return store.GetLock(ctx, "ghost") }},
		{"lock-by-scope", func() (any, error) { return store.GetLockByScope(ctx, "host:ghost") }},
		{"credential", func() (any, error) { return store.GetCredential(ctx, "ghost") }},
		{"credential-by-name", func() (any, error) { return store.GetCredentialByName(ctx, "ghost") }},
		{"audit", func() (any, error) { return store.GetAudit(ctx, "ghost") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.call()
			require.NoError(t, err)
			assert.Nil(t, got)
		})
	}
}

// =========================================================================
// Update*: unknown IDs report "not found"
// =========================================================================

func TestUpdate_UnknownIDReturnsNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// A valid parent run so ghost child records reference an existing FK target.
	mustCreateRun(t, store, newTestRun("r1", now))

	tests := []struct {
		name string
		call func() error
	}{
		{"run", func() error {
			r := newTestRun("ghost", now)
			return store.UpdateRun(ctx, r)
		}},
		{"batch", func() error {
			return store.UpdateBatch(ctx, &Batch{ID: "ghost", RunID: "r1", BatchNo: 1, Status: "pending"})
		}},
		{"step", func() error {
			return store.UpdateStep(ctx, &Step{ID: "ghost", RunID: "r1", BatchID: "b1", Host: "h", StepName: "n", Action: "a", Status: "pending"})
		}},
		{"trace", func() error {
			return store.UpdateTrace(ctx, &Trace{ID: "ghost", RunID: "r1", Event: "e", Actor: "a", CurrHash: "h", Timestamp: now})
		}},
		{"approval", func() error {
			return store.UpdateApproval(ctx, &Approval{ID: "ghost", RunID: "r1", Level: "low", Approver: "u", Status: "pending"})
		}},
		{"lock", func() error {
			return store.UpdateLock(ctx, &Lock{ID: "ghost", Scope: "host:ghost", Owner: "o", TTLSeconds: 1, AcquiredAt: now, ExpiresAt: now})
		}},
		{"credential", func() error {
			return store.UpdateCredential(ctx, &Credential{ID: "ghost", Name: "n", Type: "t", EncryptedData: []byte{1}, CreatedAt: now})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not found")
		})
	}
}

// =========================================================================
// Delete*: deleting an unknown ID is a no-op (returns nil)
// =========================================================================

func TestDelete_UnknownIDIsNoop(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"run", func() error { return store.DeleteRun(ctx, "ghost") }},
		{"batch", func() error { return store.DeleteBatch(ctx, "ghost") }},
		{"step", func() error { return store.DeleteStep(ctx, "ghost") }},
		// The WORM delete trigger fires per deleted row; with no matching rows
		// it never fires, so deleting an unknown trace id must succeed.
		{"trace", func() error { return store.DeleteTrace(ctx, "ghost") }},
		{"approval", func() error { return store.DeleteApproval(ctx, "ghost") }},
		{"lock", func() error { return store.DeleteLock(ctx, "ghost") }},
		{"credential", func() error { return store.DeleteCredential(ctx, "ghost") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, tt.call())
		})
	}
}

// =========================================================================
// Run status transitions
// =========================================================================

func TestRun_StatusTransitions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Walk a single run through the full lifecycle.
	run := newTestRun("lifecycle", now)
	mustCreateRun(t, store, run)

	lifecycle := []string{"planning", "awaiting_approval", "running", "verifying", "completed"}
	for i, status := range lifecycle {
		run.Status = status
		run.UpdatedAt = now.Add(time.Duration(i+1) * time.Minute)
		run.ApprovalStatus = "approved"
		require.NoError(t, store.UpdateRun(ctx, run), "transition to %s", status)

		got, err := store.GetRun(ctx, run.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, status, got.Status, "persisted status after update")
		assert.Equal(t, run.UpdatedAt, got.UpdatedAt)
		// UpdateRun must not touch created_at even if the caller mutated it.
		assert.Equal(t, now, got.CreatedAt)
	}

	// Terminal failure states must be reachable from any prior state.
	terminal := []string{"failed", "rolled_back", "aborted"}
	for _, status := range terminal {
		id := "terminal-" + status
		r := newTestRun(id, now)
		r.Status = "running"
		mustCreateRun(t, store, r)

		r.Status = status
		r.UpdatedAt = now.Add(time.Minute)
		require.NoError(t, store.UpdateRun(ctx, r))

		got, err := store.GetRun(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, status, got.Status)
	}
}

func TestRun_UpdatePreservesCreatedAt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	run := newTestRun("r-created", now)
	mustCreateRun(t, store, run)

	// Sabotage created_at before updating; the column is not mutable and the
	// stored value must survive.
	run.CreatedAt = now.Add(-24 * time.Hour)
	run.Status = "running"
	run.UpdatedAt = now.Add(time.Minute)
	require.NoError(t, store.UpdateRun(ctx, run))

	got, err := store.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, now, got.CreatedAt, "created_at must be immutable via UpdateRun")
}

// =========================================================================
// Run listing: filter combinations and Limit boundaries
// =========================================================================

func TestRun_ListFilters(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	// Distinct created_at values make the created_at DESC order deterministic:
	// newest first -> rD, rC, rB, rA.
	rA := newTestRun("rA", base)
	rA.Creator = "alice"
	rB := newTestRun("rB", base.Add(time.Second))
	rB.Status, rB.TemplateName, rB.Creator, rB.IncidentID = "running", "tpl-B", "bob", "INC-2"
	rC := newTestRun("rC", base.Add(2*time.Second))
	rC.WorkflowName, rC.Creator = "wf-other", "alice"
	rD := newTestRun("rD", base.Add(3*time.Second))
	rD.Status, rD.Creator, rD.IncidentID = "failed", "carol", "INC-2"

	for _, r := range []*Run{rA, rB, rC, rD} {
		mustCreateRun(t, store, r)
	}

	tests := []struct {
		name    string
		filter  RunFilter
		wantIDs []string
	}{
		{"no filter returns newest first", RunFilter{}, []string{"rD", "rC", "rB", "rA"}},
		{"by status", RunFilter{Status: "pending"}, []string{"rC", "rA"}},
		{"by workflow", RunFilter{WorkflowName: "wf-other"}, []string{"rC"}},
		{"by template", RunFilter{TemplateName: "tpl-B"}, []string{"rB"}},
		{"by creator", RunFilter{Creator: "alice"}, []string{"rC", "rA"}},
		{"by incident", RunFilter{IncidentID: "INC-2"}, []string{"rD", "rB"}},
		{"combined AND", RunFilter{Status: "pending", Creator: "alice"}, []string{"rC", "rA"}},
		{"combined AND narrows to one", RunFilter{Status: "pending", Creator: "alice", WorkflowName: "wf-other"}, []string{"rC"}},
		{"no match yields empty result", RunFilter{Status: "completed", Creator: "nobody"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list, err := store.ListRuns(ctx, tt.filter)
			require.NoError(t, err)
			ids := make([]string, 0, len(list))
			for _, r := range list {
				ids = append(ids, r.ID)
			}
			assert.Equal(t, tt.wantIDs, ids)
		})
	}
}

func TestRun_ListLimitBoundaries(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	// created_at DESC order: r3, r2, r1.
	for i := 1; i <= 3; i++ {
		mustCreateRun(t, store, newTestRun(fmt.Sprintf("r%d", i), base.Add(time.Duration(i)*time.Second)))
	}

	tests := []struct {
		name    string
		limit   int
		wantIDs []string
	}{
		{"zero means no cap", 0, []string{"r3", "r2", "r1"}},
		{"negative means no cap", -1, []string{"r3", "r2", "r1"}},
		{"limit smaller than result set", 2, []string{"r3", "r2"}},
		{"limit equal to result set", 3, []string{"r3", "r2", "r1"}},
		{"limit larger than result set", 99, []string{"r3", "r2", "r1"}},
		{"limit one", 1, []string{"r3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list, err := store.ListRuns(ctx, RunFilter{Limit: tt.limit})
			require.NoError(t, err)
			ids := make([]string, 0, len(list))
			for _, r := range list {
				ids = append(ids, r.ID)
			}
			assert.Equal(t, tt.wantIDs, ids)
		})
	}
}

// =========================================================================
// Batch listing: status filter, ordering, limit, unique constraint
// =========================================================================

func TestBatch_ListFilterOrderAndUniqueConstraint(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	mustCreateRun(t, store, newTestRun("r1", now))

	batches := []*Batch{
		{ID: "b1", RunID: "r1", BatchNo: 1, Status: "completed", TotalHosts: 2, Succeeded: 2},
		{ID: "b2", RunID: "r1", BatchNo: 2, Status: "running", TotalHosts: 2, Succeeded: 1, Failed: 1},
		{ID: "b3", RunID: "r1", BatchNo: 3, Status: "pending", TotalHosts: 2},
	}
	for _, b := range batches {
		require.NoError(t, store.CreateBatch(ctx, b))
	}

	// Ordered by batch_no ASC.
	list, err := store.ListBatches(ctx, BatchFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"b1", "b2", "b3"}, batchIDs(list))

	// Status filter.
	list, err = store.ListBatches(ctx, BatchFilter{RunID: "r1", Status: "running"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "b2", list[0].ID)

	// Limit truncates from the head of the ordered result.
	list, err = store.ListBatches(ctx, BatchFilter{RunID: "r1", Limit: 2})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, []string{"b1", "b2"}, batchIDs(list))

	// Empty result for a run without batches.
	list, err = store.ListBatches(ctx, BatchFilter{RunID: "no-such-run"})
	require.NoError(t, err)
	assert.Empty(t, list)

	// UNIQUE (run_id, batch_no): a second batch with the same number in the
	// same run must be rejected.
	dup := &Batch{ID: "b1-dup", RunID: "r1", BatchNo: 1, Status: "pending"}
	err = store.CreateBatch(ctx, dup)
	require.Error(t, err)
}

func batchIDs(batches []*Batch) []string {
	ids := make([]string, 0, len(batches))
	for _, b := range batches {
		ids = append(ids, b.ID)
	}
	return ids
}

// =========================================================================
// Step listing: filters, ordering (NULL started_at first), full round-trip
// =========================================================================

func TestStep_ListFiltersOrderingAndRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	mustCreateRun(t, store, newTestRun("r1", base))
	require.NoError(t, store.CreateBatch(ctx, &Batch{ID: "b1", RunID: "r1", BatchNo: 1, Status: "running"}))
	require.NoError(t, store.CreateBatch(ctx, &Batch{ID: "b2", RunID: "r1", BatchNo: 2, Status: "pending"}))

	t0 := base.Add(time.Second)
	t2 := base.Add(3 * time.Second)
	stdout := "line1\nline2"
	stderr := "warn: something"
	steps := []*Step{
		{ID: "s1", RunID: "r1", BatchID: "b1", Host: "h1", StepName: "n1", Action: "shell",
			Status: "success", ExitCode: ptrInt(0), Stdout: stdout, Stderr: stderr, DurationMs: 120,
			StartedAt: &t0, CompletedAt: &t0},
		{ID: "s2", RunID: "r1", BatchID: "b1", Host: "h2", StepName: "n2", Action: "shell",
			Status: "failed", ExitCode: ptrInt(2), DurationMs: 30, StartedAt: ptrTime(base.Add(2 * time.Second))},
		{ID: "s3", RunID: "r1", BatchID: "b2", Host: "h1", StepName: "n3", Action: "copy",
			Status: "running", DurationMs: 0, StartedAt: &t2},
		// s4 has no started_at yet (never dispatched); NULLs sort first in ASC order.
		{ID: "s4", RunID: "r1", BatchID: "b2", Host: "h1", StepName: "n4", Action: "shell",
			Status: "pending"},
	}
	for _, st := range steps {
		require.NoError(t, store.CreateStep(ctx, st))
	}

	// Full round-trip fidelity for a richly populated step.
	got, err := store.GetStep(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, steps[0], got)

	// By run: all four.
	list, err := store.ListSteps(ctx, StepFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, list, 4)

	// By batch.
	list, err = store.ListSteps(ctx, StepFilter{BatchID: "b1"})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "s1", list[0].ID) // ordered by started_at ASC
	assert.Equal(t, "s2", list[1].ID)

	// By host: s4 (NULL started_at) sorts before s1 and s3.
	list, err = store.ListSteps(ctx, StepFilter{Host: "h1"})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"s4", "s1", "s3"}, stepIDs(list))

	// Combined batch + host filter.
	list, err = store.ListSteps(ctx, StepFilter{BatchID: "b2", Host: "h1"})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, []string{"s4", "s3"}, stepIDs(list))

	// Status filter.
	list, err = store.ListSteps(ctx, StepFilter{Status: "success"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "s1", list[0].ID)

	// Limit truncates the ordered result.
	list, err = store.ListSteps(ctx, StepFilter{RunID: "r1", Limit: 2})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, []string{"s4", "s1"}, stepIDs(list))

	// UpdateStep persists a full transition including clearing nullable fields.
	s2 := steps[1]
	s2.Status = "success"
	s2.ExitCode = ptrInt(0)
	s2.CompletedAt = ptrTime(base.Add(2 * time.Second))
	require.NoError(t, store.UpdateStep(ctx, s2))
	got, err = store.GetStep(ctx, "s2")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "success", got.Status)
	require.NotNil(t, got.CompletedAt)
}

func stepIDs(steps []*Step) []string {
	ids := make([]string, 0, len(steps))
	for _, st := range steps {
		ids = append(ids, st.ID)
	}
	return ids
}

// =========================================================================
// Trace: hash chain updates, WORM enforcement, deterministic ordering, FK
// =========================================================================

func TestTrace_HashChainHashesUpdatable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	mustCreateRun(t, store, newTestRun("r1", base))

	// Records are appended without hashes first, like the recorder does before
	// the hash chain builder fills them in.
	tr1 := &Trace{ID: "tr1", RunID: "r1", Event: "step_start", Actor: "executor",
		Detail: `{"host":"h1"}`, Timestamp: base}
	tr2 := &Trace{ID: "tr2", RunID: "r1", Event: "step_end", Actor: "executor",
		Detail: `{"host":"h1"}`, Timestamp: base.Add(time.Second)}
	require.NoError(t, store.CreateTrace(ctx, tr1))
	require.NoError(t, store.CreateTrace(ctx, tr2))

	// Simulate HashChainBuilder: update only prev_hash/curr_hash. The WORM
	// trigger allows this as long as content columns stay unchanged.
	got, err := store.GetTrace(ctx, "tr1")
	require.NoError(t, err)
	got.CurrHash = "hash-1"
	require.NoError(t, store.UpdateTrace(ctx, got))

	got, err = store.GetTrace(ctx, "tr2")
	require.NoError(t, err)
	got.PrevHash = "hash-1"
	got.CurrHash = "hash-2"
	require.NoError(t, store.UpdateTrace(ctx, got))

	list, err := store.ListTraces(ctx, TraceFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Chain linkage: each record's prev hash equals its predecessor's curr hash.
	assert.Empty(t, list[0].PrevHash)
	assert.Equal(t, "hash-1", list[0].CurrHash)
	assert.Equal(t, list[0].CurrHash, list[1].PrevHash)
	assert.Equal(t, "hash-2", list[1].CurrHash)

	// Content fields survived untouched.
	assert.Equal(t, "step_start", list[0].Event)
	assert.Equal(t, "executor", list[0].Actor)
	assert.Equal(t, `{"host":"h1"}`, list[0].Detail)
	assert.Equal(t, base, list[0].Timestamp)
}

func TestTrace_WORMRejectsContentUpdates(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	mustCreateRun(t, store, newTestRun("r1", base))
	tr := &Trace{ID: "tr1", RunID: "r1", Event: "plan", Actor: "planner",
		Detail: `{"ok":true}`, CurrHash: "hash-1", Timestamp: base}
	require.NoError(t, store.CreateTrace(ctx, tr))

	tests := []struct {
		name   string
		mutate func(tr *Trace)
	}{
		// Note: mutating only the ID is intentionally absent — the WHERE clause
		// keys on id, so that path yields "not found" rather than a WORM abort
		// (covered by TestUpdate_UnknownIDReturnsNotFound).
		{"run_id", func(tr *Trace) { tr.RunID = "r-evil" }},
		{"event", func(tr *Trace) { tr.Event = "evil" }},
		{"actor", func(tr *Trace) { tr.Actor = "evil" }},
		{"detail", func(tr *Trace) { tr.Detail = `{"ok":false}` }},
		{"timestamp", func(tr *Trace) { tr.Timestamp = tr.Timestamp.Add(time.Minute) }},
		// Changing a hash column must not be a Trojan horse for content changes.
		{"event-and-hash", func(tr *Trace) { tr.Event = "evil"; tr.CurrHash = "new-hash" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.GetTrace(ctx, "tr1")
			require.NoError(t, err)
			require.NotNil(t, got)

			tt.mutate(got)
			err = store.UpdateTrace(ctx, got)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "WORM violation")

			// The stored record is unchanged.
			after, err := store.GetTrace(ctx, "tr1")
			require.NoError(t, err)
			require.NotNil(t, after)
			assert.Equal(t, tr, after)
		})
	}
}

func TestTrace_WORMRejectsDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	mustCreateRun(t, store, newTestRun("r1", base))
	tr := &Trace{ID: "tr1", RunID: "r1", Event: "plan", Actor: "planner",
		CurrHash: "h", Timestamp: base}
	require.NoError(t, store.CreateTrace(ctx, tr))

	err := store.DeleteTrace(ctx, tr.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WORM violation")

	// The record must still be readable after the rejected delete.
	got, err := store.GetTrace(ctx, tr.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestTrace_TimestampOrderWithIDTieBreaker(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	mustCreateRun(t, store, newTestRun("r1", base))

	// Insert out of order: two records share a timestamp, one is earlier.
	// Expected read order is timestamp ASC with id ASC as tie-breaker.
	traces := []*Trace{
		{ID: "zz-later", RunID: "r1", Event: "e", Actor: "a", CurrHash: "h3", Timestamp: base.Add(2 * time.Second)},
		{ID: "zz-same", RunID: "r1", Event: "e", Actor: "a", CurrHash: "h2", Timestamp: base.Add(time.Second)},
		{ID: "aa-same", RunID: "r1", Event: "e", Actor: "a", CurrHash: "h1", Timestamp: base.Add(time.Second)},
	}
	for _, tr := range traces {
		require.NoError(t, store.CreateTrace(ctx, tr))
	}

	list, err := store.ListTraces(ctx, TraceFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"aa-same", "zz-same", "zz-later"}, traceIDs(list))

	// The deterministic order is what makes the hash chain reproducible:
	// re-reading yields the identical sequence.
	list2, err := store.ListTraces(ctx, TraceFilter{RunID: "r1"})
	require.NoError(t, err)
	assert.Equal(t, traceIDs(list), traceIDs(list2))
}

func TestTrace_FKViolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	tr := &Trace{ID: "tr-orphan", RunID: "no-such-run", Event: "plan", Actor: "a", CurrHash: "h", Timestamp: time.Now().UTC()}
	err := store.CreateTrace(ctx, tr)
	require.Error(t, err)
}

func traceIDs(traces []*Trace) []string {
	ids := make([]string, 0, len(traces))
	for _, tr := range traces {
		ids = append(ids, tr.ID)
	}
	return ids
}

// =========================================================================
// Approvals: filters, ordering with NULL timeout, cascade delete
// =========================================================================

func TestApproval_ListFiltersOrderingAndCascade(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	mustCreateRun(t, store, newTestRun("r1", base))
	mustCreateRun(t, store, newTestRun("r2", base))

	acted := base.Add(30 * time.Minute)
	approvals := []*Approval{
		// NULL timeout_at sorts first under ORDER BY timeout_at ASC.
		{ID: "a3", RunID: "r1", Level: "critical", Approver: "carol", Status: "rejected", Comment: "too risky"},
		{ID: "a2", RunID: "r1", Level: "low", Approver: "bob", Status: "approved",
			Comment: "ok", TimeoutAt: ptrTime(base.Add(time.Hour)), ActedAt: &acted},
		{ID: "a1", RunID: "r1", Level: "high", Approver: "alice", Status: "pending",
			TimeoutAt: ptrTime(base.Add(2 * time.Hour))},
		{ID: "a4", RunID: "r2", Level: "high", Approver: "alice", Status: "pending",
			TimeoutAt: ptrTime(base.Add(3 * time.Hour))},
	}
	for _, a := range approvals {
		require.NoError(t, store.CreateApproval(ctx, a))
	}

	// Full round-trip of a nullable-heavy record.
	got, err := store.GetApproval(ctx, "a3")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, approvals[0], got)

	// Ordering: NULL timeout first, then ascending timeouts.
	list, err := store.ListApprovals(ctx, ApprovalFilter{RunID: "r1"})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"a3", "a2", "a1"}, approvalIDs(list))

	// Level filter.
	list, err = store.ListApprovals(ctx, ApprovalFilter{RunID: "r1", Level: "high"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "a1", list[0].ID)

	// Status filter across runs.
	list, err = store.ListApprovals(ctx, ApprovalFilter{Status: "pending"})
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Limit truncates the ordered result.
	list, err = store.ListApprovals(ctx, ApprovalFilter{RunID: "r1", Limit: 2})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, []string{"a3", "a2"}, approvalIDs(list))

	// Deleting the run cascades to its approvals.
	require.NoError(t, store.DeleteRun(ctx, "r1"))
	for _, id := range []string{"a1", "a2", "a3"} {
		got, err := store.GetApproval(ctx, id)
		require.NoError(t, err)
		assert.Nil(t, got, "approval %s must be cascade-deleted", id)
	}
	// Approvals of other runs survive.
	got, err = store.GetApproval(ctx, "a4")
	require.NoError(t, err)
	require.NotNil(t, got)
}

func approvalIDs(approvals []*Approval) []string {
	ids := make([]string, 0, len(approvals))
	for _, a := range approvals {
		ids = append(ids, a.ID)
	}
	return ids
}

// =========================================================================
// Locks: UpdateLockOwnedBy conditional update, TTL expiry semantics
// =========================================================================

func TestLock_UpdateLockOwnedBy(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	mkLock := func(id, scope, owner string, acquired, expires time.Time) *Lock {
		return &Lock{
			ID: id, Scope: scope, Owner: owner, TTLSeconds: 3600,
			AcquiredAt: acquired, ExpiresAt: expires,
		}
	}

	tests := []struct {
		name     string
		lock     *Lock // nil means the target lock does not exist
		newOwner string
		newTTL   int
		at       time.Time
		wantRows int64
		verify   func(t *testing.T, got *Lock)
	}{
		{
			name:     "active lock cannot be taken over",
			lock:     mkLock("l-active", "host:a", "run-orig", now, now.Add(time.Hour)),
			newOwner: "run-thief", newTTL: 60, at: now.Add(time.Minute),
			wantRows: 0,
			verify: func(t *testing.T, got *Lock) {
				assert.Equal(t, "run-orig", got.Owner)
				assert.Equal(t, 3600, got.TTLSeconds)
				assert.Equal(t, now.Add(time.Hour), got.ExpiresAt)
			},
		},
		{
			name:     "expired lock is taken over",
			lock:     mkLock("l-expired", "host:b", "run-orig", now.Add(-2*time.Hour), now.Add(-time.Hour)),
			newOwner: "run-new", newTTL: 120, at: now,
			wantRows: 1,
			verify: func(t *testing.T, got *Lock) {
				assert.Equal(t, "run-new", got.Owner)
				assert.Equal(t, 120, got.TTLSeconds)
				assert.Equal(t, now, got.AcquiredAt)
				assert.Equal(t, now.Add(120*time.Second), got.ExpiresAt)
			},
		},
		{
			name:     "boundary expiry equal to now is takeable (expires_at <= now)",
			lock:     mkLock("l-boundary", "host:c", "run-orig", now.Add(-time.Hour), now),
			newOwner: "run-boundary", newTTL: 30, at: now,
			wantRows: 1,
			verify: func(t *testing.T, got *Lock) {
				assert.Equal(t, "run-boundary", got.Owner)
				assert.Equal(t, now.Add(30*time.Second), got.ExpiresAt)
			},
		},
		{
			name:     "unknown lock id affects no rows",
			lock:     nil,
			newOwner: "run-x", newTTL: 60, at: now,
			wantRows: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetID := "does-not-exist"
			if tt.lock != nil {
				require.NoError(t, store.CreateLock(ctx, tt.lock))
				targetID = tt.lock.ID
			}

			n, err := store.UpdateLockOwnedBy(ctx, targetID, tt.newOwner, tt.newTTL, tt.at)
			require.NoError(t, err)
			assert.Equal(t, tt.wantRows, n)

			if tt.lock == nil {
				return
			}
			got, err := store.GetLock(ctx, tt.lock.ID)
			require.NoError(t, err)
			require.NotNil(t, got)
			tt.verify(t, got)
		})
	}
}

func TestLock_DeleteExpiredLocks_StrictlyBefore(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// A lock that expires exactly at now, one already expired, one still valid.
	exact := &Lock{ID: "l-exact", Scope: "host:exact", Owner: "run-1", TTLSeconds: 60,
		AcquiredAt: now.Add(-time.Hour), ExpiresAt: now}
	expired := &Lock{ID: "l-expired", Scope: "host:expired", Owner: "run-2", TTLSeconds: 60,
		AcquiredAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	valid := &Lock{ID: "l-valid", Scope: "host:valid", Owner: "run-3", TTLSeconds: 3600,
		AcquiredAt: now, ExpiresAt: now.Add(time.Hour)}
	for _, l := range []*Lock{exact, expired, valid} {
		require.NoError(t, store.CreateLock(ctx, l))
	}

	// DeleteExpiredLocks uses expires_at < now: the boundary lock survives.
	n, err := store.DeleteExpiredLocks(ctx, now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	got, err := store.GetLock(ctx, "l-exact")
	require.NoError(t, err)
	assert.NotNil(t, got, "lock expiring exactly now must survive expires_at < now")

	got, err = store.GetLock(ctx, "l-valid")
	require.NoError(t, err)
	assert.NotNil(t, got)

	// Advancing the clock past the boundary removes it.
	n, err = store.DeleteExpiredLocks(ctx, now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	got, err = store.GetLock(ctx, "l-exact")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestLock_GetByScopeAndListOrdering(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	locks := []*Lock{
		{ID: "l-far", Scope: "host:far", Owner: "run-1", TTLSeconds: 3600,
			AcquiredAt: now, ExpiresAt: now.Add(3 * time.Hour)},
		{ID: "l-near", Scope: "host:near", Owner: "run-2", TTLSeconds: 60,
			AcquiredAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "l-mid", Scope: "host:mid", Owner: "run-3", TTLSeconds: 600,
			AcquiredAt: now, ExpiresAt: now.Add(2 * time.Hour)},
	}
	for _, l := range locks {
		require.NoError(t, store.CreateLock(ctx, l))
	}

	got, err := store.GetLockByScope(ctx, "host:mid")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "l-mid", got.ID)
	assert.Equal(t, locks[2], got)

	// ListLocks orders by expires_at ascending (soonest expiry first).
	list, err := store.ListLocks(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"l-near", "l-mid", "l-far"}, lockIDs(list))
}

func lockIDs(locks []*Lock) []string {
	ids := make([]string, 0, len(locks))
	for _, l := range locks {
		ids = append(ids, l.ID)
	}
	return ids
}
