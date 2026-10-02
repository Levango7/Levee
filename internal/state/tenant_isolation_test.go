package state

// tenant_isolation_test.go proves, at the SQL layer, that two tenants' rows
// cannot see each other. It is the test that makes the tenant_id columns worth
// anything: without it the columns are just documentation.
//
// Scope: this file tests the BASE store (the SQL predicates), not the tenant
// wrapper. The wrapper's job is to resolve WHICH tenant and to refuse
// cross-tenant access by id; that is tested in internal/tenant. Splitting them
// this way means a failure here points at a missing/misordered SQL predicate,
// and a failure there points at the resolver — rather than both landing in one
// test that is hard to read a failure out of.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tnow() time.Time { return time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC) }

func TestTenantIsolation_RunsAreSeparatedByFilter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for _, spec := range []struct {
		id     string
		tenant string
	}{{id: "r-acme", tenant: "acme"}, {id: "r-globex", tenant: "globex"}} {
		require.NoError(t, s.CreateRun(ctx, &Run{
			ID: spec.id, WorkflowName: "wf", TemplateName: "tpl",
			PlanHash: "h", Status: "draft",
			CreatedAt: tnow(), UpdatedAt: tnow(), Creator: "alice",
			TenantID: spec.tenant,
		}))
	}

	acme, err := s.ListRuns(ctx, RunFilter{TenantID: "acme"})
	require.NoError(t, err)
	require.Len(t, acme, 1, "tenant filter must return exactly its own rows")
	assert.Equal(t, "r-acme", acme[0].ID)
	assert.Equal(t, "acme", acme[0].TenantID, "tenant_id must round-trip on read")

	globex, err := s.ListRuns(ctx, RunFilter{TenantID: "globex"})
	require.NoError(t, err)
	require.Len(t, globex, 1)
	assert.Equal(t, "r-globex", globex[0].ID)

	// The dangerous case: a filter that omits TenantID must NOT be what
	// isolation relies on. Document the actual behaviour rather than assert a
	// wish — an empty TenantID means "no tenant predicate" (the single-tenant /
	// migration path), so it returns everything. This assertion exists to make
	// that explicit: if a future change made empty mean "no rows", this fails.
	unfiltered, err := s.ListRuns(ctx, RunFilter{})
	require.NoError(t, err)
	assert.Len(t, unfiltered, 2,
		"empty TenantID means no predicate; the wrapper, not the base store, is what must never pass one")

	// Unknown tenant sees nothing rather than erroring.
	none, err := s.ListRuns(ctx, RunFilter{TenantID: "does-not-exist"})
	require.NoError(t, err)
	assert.Empty(t, none)
}

// TestTenantIsolation_ChildRowsAreSeparated covers the run-scoped child tables.
// Each is created with an explicit tenant that matches its parent run — the
// invariant the wrapper maintains — and then read back under each tenant.
func TestTenantIsolation_ChildRowsAreSeparated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	seed := func(runID, tenant string) {
		require.NoError(t, s.CreateRun(ctx, &Run{
			ID: runID, WorkflowName: "wf", TemplateName: "tpl", PlanHash: "h",
			Status: "draft", CreatedAt: tnow(), UpdatedAt: tnow(), Creator: "alice",
			TenantID: tenant,
		}))
	}
	seed("r-acme", "acme")
	seed("r-globex", "globex")

	require.NoError(t, s.CreateBatch(ctx, &Batch{
		ID: "b-acme", RunID: "r-acme", BatchNo: 1, Status: "pending", TenantID: "acme",
	}))
	require.NoError(t, s.CreateBatch(ctx, &Batch{
		ID: "b-globex", RunID: "r-globex", BatchNo: 1, Status: "pending", TenantID: "globex",
	}))
	require.NoError(t, s.CreateStep(ctx, &Step{
		ID: "s-acme", RunID: "r-acme", BatchID: "b-acme", Host: "h1",
		StepName: "reload", Action: "shell.exec", Status: "success", TenantID: "acme",
	}))
	require.NoError(t, s.CreateStep(ctx, &Step{
		ID: "s-globex", RunID: "r-globex", BatchID: "b-globex", Host: "h1",
		StepName: "reload", Action: "shell.exec", Status: "success", TenantID: "globex",
	}))
	require.NoError(t, s.CreateApproval(ctx, &Approval{
		ID: "a-acme", RunID: "r-acme", Level: "high", Approver: "bob",
		Status: "pending", TenantID: "acme",
	}))
	require.NoError(t, s.CreateApproval(ctx, &Approval{
		ID: "a-globex", RunID: "r-globex", Level: "high", Approver: "bob",
		Status: "pending", TenantID: "globex",
	}))

	t.Run("batches", func(t *testing.T) {
		got, err := s.ListBatches(ctx, BatchFilter{TenantID: "acme"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "b-acme", got[0].ID)
		assert.Equal(t, "acme", got[0].TenantID)
	})

	t.Run("steps", func(t *testing.T) {
		got, err := s.ListSteps(ctx, StepFilter{TenantID: "acme"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "s-acme", got[0].ID)
		assert.Equal(t, "acme", got[0].TenantID)
	})

	t.Run("approvals", func(t *testing.T) {
		got, err := s.ListApprovals(ctx, ApprovalFilter{TenantID: "acme"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "a-acme", got[0].ID)
		assert.Equal(t, "acme", got[0].TenantID)
	})

	// The combined case: tenant + run filter together must still agree. This is
	// the query shape the services actually issue.
	t.Run("tenant and run filter compose", func(t *testing.T) {
		got, err := s.ListApprovals(ctx, ApprovalFilter{RunID: "r-globex", TenantID: "acme"})
		require.NoError(t, err)
		assert.Empty(t, got, "a run filter must not override the tenant predicate")
	})
}

// TestTenantIsolation_AuditAndTraceAreSeparated covers the two evidence tables.
// These matter more than the rest: if a tenant can read another's audit trail
// it can learn what changes that tenant makes to shared infrastructure.
func TestTenantIsolation_AuditAndTraceAreSeparated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	require.NoError(t, s.CreateRun(ctx, &Run{
		ID: "r-acme", WorkflowName: "wf", TemplateName: "tpl", PlanHash: "h",
		Status: "draft", CreatedAt: tnow(), UpdatedAt: tnow(), Creator: "alice",
		TenantID: "acme",
	}))

	// A run-less audit entry: login/config/credential actions carry RunID "",
	// which is exactly why audit carries its own tenant_id rather than
	// deriving it from runs.
	require.NoError(t, s.CreateAudit(ctx, &Audit{
		ID: "au-acme-runless", Action: "login", Actor: "alice",
		Result: "success", TenantID: "acme", Timestamp: tnow(),
	}))
	require.NoError(t, s.CreateAudit(ctx, &Audit{
		ID: "au-globex", RunID: "other-run", Action: "apply", Actor: "bob",
		Result: "success", TenantID: "globex", Timestamp: tnow(),
	}))
	require.NoError(t, s.CreateTrace(ctx, &Trace{
		ID: "tr-acme", RunID: "r-acme", Event: "plan", Actor: "alice",
		Detail: "{}", CurrHash: "deadbeef", Timestamp: tnow(), TenantID: "acme",
	}))

	audits, err := s.ListAudits(ctx, AuditFilter{TenantID: "acme"})
	require.NoError(t, err)
	require.Len(t, audits, 1, "the run-less acme entry must still be visible to acme")
	assert.Equal(t, "au-acme-runless", audits[0].ID)
	assert.Equal(t, "acme", audits[0].TenantID)

	traces, err := s.ListTraces(ctx, TraceFilter{TenantID: "acme"})
	require.NoError(t, err)
	require.Len(t, traces, 1)
	assert.Equal(t, "tr-acme", traces[0].ID)
	assert.Equal(t, "acme", traces[0].TenantID)
}

// TestTenantIsolation_TraceTenantIsWORMProtected pins the security property
// that the v6 migration's trigger recreation exists for. Without tenant_id in
// the immutable column set, an UPDATE could move a trace row into (or out of)
// another tenant's scope — a silent isolation bypass that the hash chain
// would then happily verify, because the chain hashes content, not ownership.
func TestTenantIsolation_TraceTenantIsWORMProtected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	require.NoError(t, s.CreateRun(ctx, &Run{
		ID: "r-acme", WorkflowName: "wf", TemplateName: "tpl", PlanHash: "h",
		Status: "draft", CreatedAt: tnow(), UpdatedAt: tnow(), Creator: "alice",
		TenantID: "acme",
	}))
	require.NoError(t, s.CreateTrace(ctx, &Trace{
		ID: "tr-1", RunID: "r-acme", Event: "plan", Actor: "alice",
		Detail: "{}", CurrHash: "h1", Timestamp: tnow(), TenantID: "acme",
	}))

	// Direct SQL, bypassing every layer of application code — this is the
	// database-level defence, so the test has to attack at this level too.
	_, err := s.DB().ExecContext(ctx,
		`UPDATE trace SET tenant_id = 'globex' WHERE id = 'tr-1'`)
	require.Error(t, err, "trace.tenant_id must be immutable like the other content columns")
	assert.Contains(t, err.Error(), "WORM violation")

	// And the delete path is still closed.
	_, err = s.DB().ExecContext(ctx, `DELETE FROM trace WHERE id = 'tr-1'`)
	require.Error(t, err, "trace rows remain undeletable")
}

// TestTenantIsolation_LocksStayGlobal pins the deliberate exception. If locks
// ever gained a tenant_id, two tenants could each hold host:db-1 and both be
// admitted — the exact concurrent-change collision the lock exists to stop.
func TestTenantIsolation_LocksStayGlobal(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	require.NoError(t, s.CreateLock(ctx, &Lock{
		ID: "l-1", Scope: "host:db-1", Owner: "run-acme",
		TTLSeconds: 60, AcquiredAt: tnow(), ExpiresAt: tnow().Add(time.Minute),
	}))

	// A second acquire of the same scope — standing in for tenant B — must
	// still collide, because the scope is global.
	err := s.CreateLock(ctx, &Lock{
		ID: "l-2", Scope: "host:db-1", Owner: "run-globex",
		TTLSeconds: 60, AcquiredAt: tnow(), ExpiresAt: tnow().Add(time.Minute),
	})
	require.Error(t, err, "host locks must remain cross-tenant: the host is one machine")

	all, err := s.ListLocks(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1, "the second acquire must not have created a second row")
}

// TestTenantIsolation_MigrationBackfillsFromIncidentIDTag pins the chosen
// backfill rule: a run that already carried a "tenant:<id>|..." tag keeps
// that tenant, and everything else lands in the default tenant. Getting this
// wrong would silently merge two tenants' histories into one.
func TestTenantIsolation_MigrationBackfillsFromIncidentIDTag(t *testing.T) {
	ctx := context.Background()

	legacyPath := t.TempDir() + "/tenant-backfill.db"
	db, err := sql.Open("sqlite", legacyPath)
	require.NoError(t, err)
	createLegacyV1DB(t, ctx, db)

	// Three v1 rows, three different ownership situations.
	for _, ddl := range []string{
		`INSERT INTO runs (id, workflow_name, template_name, plan_hash, status, created_at, updated_at, creator, incident_id)
			VALUES ('r-tagged', 'wf', 'tpl', 'h', 'draft', '2026-01-01 00:00:00', '2026-01-01 00:00:00', 'alice', 'tenant:acme|INC-42')`,
		`INSERT INTO runs (id, workflow_name, template_name, plan_hash, status, created_at, updated_at, creator, incident_id)
			VALUES ('r-tagonly', 'wf', 'tpl', 'h', 'draft', '2026-01-01 00:00:00', '2026-01-01 00:00:00', 'alice', 'tenant:globex')`,
		`INSERT INTO runs (id, workflow_name, template_name, plan_hash, status, created_at, updated_at, creator, incident_id)
			VALUES ('r-plain', 'wf', 'tpl', 'h', 'draft', '2026-01-01 00:00:00', '2026-01-01 00:00:00', 'alice', 'INC-7')`,
		// A child of the tagged run, to prove rule 2 inherits.
		`INSERT INTO batches (id, run_id, batch_no, status)
			VALUES ('b-tagged', 'r-tagged', 1, 'pending')`,
		// A run-less credential, to prove rule 3 defaults.
		`INSERT INTO credentials (id, name, type, encrypted_data, created_at)
			VALUES ('c-1', 'prod-ssh', 'ssh_key', x'00', '2026-01-01 00:00:00')`,
	} {
		_, err := db.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())

	s, err := NewSQLiteStore(ctx, legacyPath)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	for _, tc := range []struct{ runID, want string }{
		{"r-tagged", "acme"},         // rule 1: parsed from the tag
		{"r-tagonly", "globex"},      // rule 1: tag with no incident part
		{"r-plain", DefaultTenantID}, // rule 1 remainder
	} {
		got, err := s.GetRun(ctx, tc.runID)
		require.NoError(t, err)
		require.NotNil(t, got, tc.runID)
		assert.Equal(t, tc.want, got.TenantID, "backfill for %s", tc.runID)
	}

	// The incident id itself must be preserved verbatim: the backfill reads
	// the tag, it does not strip it. Rewriting the column would destroy an
	// operator's incident reference.
	tagged, err := s.GetRun(ctx, "r-tagged")
	require.NoError(t, err)
	assert.Equal(t, "tenant:acme|INC-42", tagged.IncidentID,
		"the legacy tag must survive the backfill untouched")

	// Rule 2: the child inherited its parent run's tenant.
	batches, err := s.ListBatches(ctx, BatchFilter{TenantID: "acme"})
	require.NoError(t, err)
	require.Len(t, batches, 1, "a batch must inherit its parent run's tenant")
	assert.Equal(t, "b-tagged", batches[0].ID)

	// Rule 3: no run parent, so default.
	cred, err := s.GetCredential(ctx, "c-1")
	require.NoError(t, err)
	require.NotNil(t, cred)
	assert.Equal(t, DefaultTenantID, cred.TenantID)
}
