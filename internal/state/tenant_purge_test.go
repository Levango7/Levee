package state

// tenant_purge_test.go — the purge may only remove what the evidence chain
// permits, and it must say what it left.

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/runstatus"
)

// seedTenantRows writes, for tenant `tid`, one terminal trace-free run
// ("purgeable"), one terminal run that has trace evidence ("held-trace") and one
// running run ("held-active"), each with a batch, an approval and an assignment.
func seedTenantRows(t *testing.T, store *SQLiteStore, tid string) []string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	ids := []string{tid + "-purgeable", tid + "-held-trace", tid + "-held-active"}

	statuses := []string{runstatus.StatusCompleted, runstatus.StatusCompleted, runstatus.StatusRunning}
	for i, id := range ids {
		require.NoError(t, store.CreateRun(ctx, &Run{
			ID: id, WorkflowName: "wf", TemplateName: "tpl", Status: statuses[i],
			Creator: "op", TenantID: tid, CreatedAt: now, UpdatedAt: now,
		}))
		require.NoError(t, store.CreateBatch(ctx, &Batch{
			ID: id + "-b1", RunID: id, BatchNo: 1, Status: BatchStateCompleted,
			TotalHosts: 1, Succeeded: 1,
		}))
		require.NoError(t, store.CreateApproval(ctx, &Approval{
			ID: id + "-a1", RunID: id, Level: "standard", Approver: "alice",
			Status: "approved",
		}))
		require.NoError(t, store.CreateAssignment(ctx, &Assignment{
			RunID: id, OwnerNode: "node-a", Epoch: 1, State: AssignmentStateDone,
			Result: "completed", TenantID: tid,
		}))
	}
	// Only the second run gets evidence. This is the whole point of the test:
	// identical terminal runs, different right to be purged.
	require.NoError(t, store.CreateTrace(ctx, &Trace{
		ID: ids[1] + "-t1", RunID: ids[1], Event: "apply", Actor: "op",
		Timestamp: now, TenantID: tid,
	}))
	require.NoError(t, store.CreateAudit(ctx, &Audit{
		ID: ids[1] + "-au1", RunID: ids[1], Action: "apply", Actor: "op",
		Target: "10.0.0.1", Result: AuditResultSuccess, Timestamp: now, TenantID: tid,
		PrevHash: "prev-1", CurrHash: "curr-1",
	}))
	// The purgeable run gets one too: the deletion must not be able to reach it.
	require.NoError(t, store.CreateAudit(ctx, &Audit{
		ID: ids[0] + "-au1", RunID: ids[0], Action: "create", Actor: "op",
		Target: ids[0], Result: AuditResultSuccess, Timestamp: now, TenantID: tid,
		PrevHash: "prev-0", CurrHash: "curr-0",
	}))
	return ids
}

func scalar(t *testing.T, store *SQLiteStore, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, store.DB().QueryRowContext(context.Background(), q, args...).Scan(&n))
	return n
}

// TestWormTriggerIsWhatForcesTheTraceGuard pins the PREMISE that PurgeTenantData
// is written around, not its behaviour. Deleting a trace-backed run directly is
// refused by the schema's own trigger, and SQLite rolls the whole statement back
// — so a "purge this tenant" that left the NOT EXISTS guard out would not delete
// evidence, it would delete nothing and look like a broken purge. If the trigger
// or the cascade edge is ever renamed, this test goes red and the guard above has
// to be re-derived rather than quietly left in as decoration.
func TestWormTriggerIsWhatForcesTheTraceGuard(t *testing.T) {
	store := newTestStore(t)
	ids := seedTenantRows(t, store, "acme")

	_, err := store.DB().ExecContext(context.Background(),
		`DELETE FROM runs WHERE id = ?`, ids[1])
	require.Error(t, err, "the schema, not the purge, is what protects the evidence")
	assert.Contains(t, err.Error(), "WORM violation")

	// Rolled back whole: the trace row, its run, and the batch the CASCADE would
	// have taken with it are all still there.
	assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM trace`))
	assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM runs WHERE id = ?`, ids[1]))
	assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM batches WHERE run_id = ?`, ids[1]))
}

// TestPurgeGuardIsLoadBearing is the other half of the same fact: drop the NOT
// EXISTS clause and the same purge errors out. A test that only checks the
// current behaviour cannot tell a guard from an accident.
func TestPurgeGuardIsLoadBearing(t *testing.T) {
	store := newTestStore(t)
	seedTenantRows(t, store, "acme")

	_, err := store.DB().ExecContext(context.Background(),
		`DELETE FROM runs WHERE tenant_id = ? AND status IN (?)`, "acme", runstatus.StatusCompleted)
	require.Error(t, err, "the same predicate without the trace guard reaches the trigger")
}

// TestAssignmentTableHasNoCascadeEdgeFromRuns is why the purge deletes
// run_assignment explicitly: run_id is that table's PRIMARY KEY, not a foreign
// key, so no run deletion takes its assignment row with it.
func TestAssignmentTableHasNoCascadeEdgeFromRuns(t *testing.T) {
	store := newTestStore(t)
	ids := seedTenantRows(t, store, "acme")

	var fkEdges int
	require.NoError(t, store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pragma_foreign_key_list('run_assignment')`).Scan(&fkEdges))
	assert.Equal(t, 0, fkEdges, "run_assignment has no FK edge, so it cannot ride a cascade")

	_, err := store.DB().ExecContext(context.Background(), `DELETE FROM runs WHERE id = ?`, ids[0])
	require.NoError(t, err, "a trace-free run is deletable")
	assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM run_assignment WHERE run_id = ?`, ids[0]),
		"and its assignment survives unless the purge clears it first")
}

// TestPurgeTenantDataOnPostgreSQL is the live-dialect half. `bind()` rewrites
// every `?` into `$n` in statement order, and a mis-numbered rewrite is exactly
// the kind of defect that stays invisible on SQLite — so the claim "the same
// predicate partitions the same three ways on both backends" is only evidence
// once a real PostgreSQL has answered it. The CI postgres job runs
// `go test -race ./internal/state/...` with LEVEE_PG_TEST_DSN set
// (.github/workflows/ci.yml:258), so this executes there rather than only on a
// developer machine.
func TestPurgeTenantDataOnPostgreSQL(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// newPGTestStore truncates everything except cluster_nodes, and other
	// packages' binaries share this database, so the ids below are unique per
	// run and every assertion is scoped to them.
	tid := "purge-pg-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	now := time.Now().UTC()
	ids := []string{tid + "-loose", tid + "-sealed", tid + "-live"}
	statuses := []string{runstatus.StatusCompleted, runstatus.StatusCompleted, runstatus.StatusRunning}

	for i, id := range ids {
		require.NoError(t, store.CreateRun(ctx, &Run{
			ID: id, WorkflowName: "wf", Status: statuses[i], Creator: "op",
			TenantID: tid, CreatedAt: now, UpdatedAt: now,
		}))
		require.NoError(t, store.CreateApproval(ctx, &Approval{
			ID: id + "-a1", RunID: id, Level: "standard", Approver: "alice", Status: "approved",
		}))
		require.NoError(t, store.CreateAssignment(ctx, &Assignment{
			RunID: id, OwnerNode: "node-a", Epoch: 1, State: AssignmentStateDone,
			Result: "completed", TenantID: tid,
		}))
	}
	require.NoError(t, store.CreateTrace(ctx, &Trace{
		ID: ids[1] + "-t1", RunID: ids[1], Event: "apply", Actor: "op", Timestamp: now, TenantID: tid,
	}))

	// The premise, on this backend: the WORM trigger is what forbids deleting an
	// evidence-backed run. PostgreSQL aborts the transaction on RAISE, so the
	// attempt is wrapped and rolled back rather than left half-applied.
	wormErr := withRollback(store, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE id = $1`, ids[1])
		return err
	})
	require.Error(t, wormErr, "deleting a trace-backed run must be refused by the schema, not by the purge")
	assert.Contains(t, wormErr.Error(), "WORM violation")

	rep, err := PurgeTenantData(ctx, store, tid)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.RunsRemoved)
	assert.Equal(t, 1, rep.RunsHeldTrace)
	assert.Equal(t, 1, rep.RunsHeldActive)
	assert.Equal(t, 1, rep.ApprovalsRemoved)
	assert.Equal(t, 1, rep.AssignmentsRemoved)

	for i, id := range ids {
		run, err := store.GetRun(ctx, id)
		require.NoError(t, err)
		if i == 0 {
			assert.Nil(t, run, "the trace-free terminal run is gone")
			// No cascade edge reaches run_assignment, so its disappearance is
			// evidence the explicit delete ran — on this dialect too.
			a, err := store.GetAssignment(ctx, id)
			require.NoError(t, err)
			assert.Nil(t, a, "and its assignment row went with it")
			continue
		}
		assert.NotNil(t, run, "%s must be held", id)
	}
}

// withRollback runs fn inside a transaction and returns fn's error after
// rolling back, whatever happened. PostgreSQL's RAISE aborts the whole
// transaction rather than one statement, so an unforgiving probe needs a place
// to land; SQLite keeps its data either way.
func withRollback(store *PGStore, fn func(*sql.Tx) error) error {
	tx, err := store.DB().Begin()
	if err != nil {
		return err
	}
	fnErr := fn(tx)
	rollbackErr := tx.Rollback()
	if fnErr != nil {
		return fnErr
	}
	return rollbackErr
}

func TestPurgeTenantDataRemovesOnlyWhatTheChainPermits(t *testing.T) {
	store := newTestStore(t)
	ids := seedTenantRows(t, store, "acme")

	rep, err := PurgeTenantData(context.Background(), store, "acme")
	require.NoError(t, err)

	assert.Equal(t, 1, rep.RunsRemoved, "only the terminal trace-free run is removable")
	assert.Equal(t, 1, rep.RunsHeldTrace, "the run with trace rows is held, not deleted")
	assert.Equal(t, 1, rep.RunsHeldActive, "a running run is never purged")
	assert.Equal(t, 1, rep.ApprovalsRemoved)
	assert.Equal(t, 1, rep.AssignmentsRemoved)
	assert.False(t, rep.NothingRemoved())

	// The rows themselves, not just the report.
	assert.Equal(t, 0, scalar(t, store, `SELECT COUNT(*) FROM runs WHERE id = ?`, ids[0]))
	assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM runs WHERE id = ?`, ids[1]))
	assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM runs WHERE id = ?`, ids[2]))

	// batches/steps went with the deleted run by cascade; the held run keeps its.
	assert.Equal(t, 0, scalar(t, store, `SELECT COUNT(*) FROM batches WHERE run_id = ?`, ids[0]))
	assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM batches WHERE run_id = ?`, ids[1]))

	// Evidence survives untouched — this is the guarantee the purge must not cost.
	assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM trace`))
	// Both seeded audit rows are still there, including the one describing the
	// run that was just deleted: `audit.run_id` carries no foreign key, so no
	// cascade can take it. Purging a run must never purge the record that it
	// happened.
	assert.Equal(t, 2, scalar(t, store, `SELECT COUNT(*) FROM audit`))
	assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM audit WHERE run_id = ?`, ids[0]),
		"the purged run's audit row outlives it")

	// The approval deleted is the one belonging to the removed run only.
	assert.Equal(t, 0, scalar(t, store, `SELECT COUNT(*) FROM approvals WHERE run_id = ?`, ids[0]))
	assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM approvals WHERE run_id = ?`, ids[1]))

	// And the assignment table lost only that run's row.
	assert.Equal(t, 0, scalar(t, store, `SELECT COUNT(*) FROM run_assignment WHERE run_id = ?`, ids[0]))
	assert.Equal(t, 2, scalar(t, store, `SELECT COUNT(*) FROM run_assignment`))
}

func TestPurgeTenantDataRefusesTheEmptyTenantID(t *testing.T) {
	store := newTestStore(t)
	ids := seedTenantRows(t, store, "acme")

	// With tenancy off, EVERY row carries tenant_id = ''. Purging '' would delete
	// the whole single-tenant database behind a "delete one tenant" command, so
	// the call must be refused before any statement runs.
	_, err := PurgeTenantData(context.Background(), store, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty tenant id")

	_, err = PurgeTenantData(context.Background(), store, "   ")
	require.Error(t, err, "whitespace is not a tenant either")

	for _, id := range ids {
		assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM runs WHERE id = ?`, id),
			"a refused purge must not have touched anything")
	}
}

func TestPurgeTenantDataIsScopedToItsTenant(t *testing.T) {
	store := newTestStore(t)
	mine := seedTenantRows(t, store, "acme")
	theirs := seedTenantRows(t, store, "globex")

	rep, err := PurgeTenantData(context.Background(), store, "acme")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.RunsRemoved)

	assert.Equal(t, 0, scalar(t, store, `SELECT COUNT(*) FROM runs WHERE id = ?`, mine[0]))
	for _, id := range theirs {
		assert.Equal(t, 1, scalar(t, store, `SELECT COUNT(*) FROM runs WHERE id = ?`, id),
			"another tenant's rows are never in scope")
	}
}

// TestPurgeTenantDataUnwrapsDecoratedStores is the #47 lesson encoded: with
// tenancy on the handle every service holds is tenant.TenantStore, so a type
// switch that does not unwrap reports "not a backend I recognise" for a
// perfectly purgeable database. The unwrap lives inside PurgeTenantData, so the
// caller cannot forget it.
func TestPurgeTenantDataUnwrapsDecoratedStores(t *testing.T) {
	store := newTestStore(t)
	ids := seedTenantRows(t, store, "acme")

	rep, err := PurgeTenantData(context.Background(), &fakeDecorator{base: store}, "acme")
	require.NoError(t, err, "a wrapped store must purge like the store inside it")
	assert.Equal(t, 1, rep.RunsRemoved)
	assert.Equal(t, 0, scalar(t, store, `SELECT COUNT(*) FROM runs WHERE id = ?`, ids[0]))
}

func TestPurgeTenantDataRejectsStoresWithNoHandle(t *testing.T) {
	_, err := PurgeTenantData(context.Background(), nil, "acme")
	require.Error(t, err)

	_, err = PurgeTenantData(context.Background(), notARealStore{}, "acme")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no SQL handle")
}

func TestPurgeTenantDataOnAnUnknownTenantIsQuietlyEmpty(t *testing.T) {
	store := newTestStore(t)
	seedTenantRows(t, store, "acme")

	rep, err := PurgeTenantData(context.Background(), store, "nobody-here")
	require.NoError(t, err)
	assert.True(t, rep.NothingRemoved())
	assert.Zero(t, rep.RunsHeldActive+rep.RunsHeldTrace)
	assert.Equal(t, 3, scalar(t, store, `SELECT COUNT(*) FROM runs`))
}

// --- doubles ---------------------------------------------------------------

// fakeDecorator mimics a store decorator the way tenant.TenantStore decorates
// one: it embeds Store so it satisfies the surface (importing internal/tenant
// from this package would be an import cycle) and implements Unwrapper, which
// is the only seam PurgeTenantData is allowed to use. Only Underlying is ever
// called, so the embedded nil interface is never reached.
type fakeDecorator struct {
	Store
	base Store
}

func (f fakeDecorator) Underlying() Store { return f.base }

// notARealStore satisfies Store through an embedded interface but is neither
// *SQLiteStore nor *PGStore and unwraps to nothing: it stands for any future
// decorator that forgets to expose a handle.
type notARealStore struct{ Store }
