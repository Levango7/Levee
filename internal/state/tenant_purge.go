package state

// tenant_purge.go — removing a DELETED tenant's rows, and only the ones the
// evidence chain permits.
//
// WHY THIS IS NOT `DELETE FROM … WHERE tenant_id = ?`. `trace` and `audit` are
// append-only by design: the WORM triggers in schema.sql / pgschema.sql abort
// any delete on them, and `internal/audit` seals rows into a hash chain. But
// `trace` also carries an `ON DELETE CASCADE` edge from `runs`, so a purge that
// deletes a run quietly tries to delete that run's evidence too. Measured
// against the store's own schema with `PRAGMA foreign_keys=1` (the driver turns
// it on — sqlite.go:84 and :110): deleting a run that has a trace row fails
// with `WORM violation: trace records cannot be deleted`, and SQLite rolls the
// WHOLE statement back, so nothing is purged and the operator sees what looks
// like a bug in the purge. Deleting a trace-free run succeeds and its
// batches/steps go with it via the same cascade.
//
// So a tenant whose changes were ever executed keeps those runs. Purging them
// would mean deleting evidence — a separate decision, not an implementation
// detail. This function therefore reports what it held back and why, instead of
// implying the tenant is now empty.
//
// WHAT IT REMOVES, for a tenant already deleted in the registry:
//
//   - terminal runs that produced NO trace row (created, planned, then
//     abandoned); their batches and steps go via the existing cascade edges;
//   - their approvals, which ride the same cascade — deliberately not purged by
//     tenant_id alone, because a run that stays must keep the approval its plan
//     hash was verified against (`verifyStoredPlanHash` reads it);
//   - their `run_assignment` rows, which have no FK to `runs` and would
//     otherwise outlive the run they point at.
//
// Non-terminal runs are never touched: a running or paused change under a
// deleted tenant is an incident to settle, not data to vacuum, and deleting its
// row mid-flight strands the lease and dispatch bookkeeping.
//
// TENANT-ID HYGIENE. `Tenant.ID` is immutable, and re-creating a tenant under
// the same name mints a NEW id (see tenant.TenantManager.Delete), so purging by
// id cannot reach a successor tenant's rows. An empty id is refused outright:
// with tenancy disabled every row carries `tenant_id = ''`, and purging that
// string would empty the whole single-tenant database behind a "delete one
// tenant" command.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/nexus/levee/internal/runstatus"
)

// TenantPurgeReport is what a purge did and what it refused to do. The held
// counts are the point: "deleted" must not read as "nothing left".
type TenantPurgeReport struct {
	// RunsRemoved counts terminal, trace-free runs deleted.
	RunsRemoved int
	// ApprovalsRemoved counts the approval rows deleted with those runs.
	ApprovalsRemoved int
	// AssignmentsRemoved counts run_assignment rows deleted (no FK edge).
	AssignmentsRemoved int
	// RunsHeldTrace counts terminal runs left because they carry trace rows:
	// purging them would delete evidence.
	RunsHeldTrace int
	// RunsHeldActive counts non-terminal runs left because they are still in
	// flight or paused.
	RunsHeldActive int
}

// NothingRemoved reports whether the purge changed no rows. Callers use it to
// say "this tenant's data is entirely evidence-backed" rather than suggesting a
// cleanup happened.
func (r TenantPurgeReport) NothingRemoved() bool {
	return r.RunsRemoved == 0 && r.ApprovalsRemoved == 0 && r.AssignmentsRemoved == 0
}

// PurgeTenantData deletes the removable rows of a deleted tenant. `store` may be
// a decorated handle: the unwrapping happens here rather than at the call site,
// so a caller cannot forget it — with tenancy on, the handle in play is
// tenant.TenantStore, and a type switch that misses it reports "not a backend I
// recognise" for a perfectly purgeable database.
//
// One transaction: if any statement fails, nothing is deleted. A half-purged
// tenant is worse than an unpurged one.
func PurgeTenantData(ctx context.Context, store Store, tenantID string) (TenantPurgeReport, error) {
	var rep TenantPurgeReport
	if strings.TrimSpace(tenantID) == "" {
		return rep, fmt.Errorf("state: tenant purge refuses an empty tenant id: with tenancy disabled every row carries tenant_id = ''")
	}

	base := Underlying(store)
	db, postgres, err := purgeHandle(base)
	if err != nil {
		return rep, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return rep, fmt.Errorf("state: tenant purge begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// args order is shared by every statement below: tenant id, then the
	// terminal vocabulary read from runstatus rather than copied into SQL.
	args := make([]any, 0, 1+len(runstatus.Terminal))
	args = append(args, tenantID)
	for _, s := range runstatus.Terminal {
		args = append(args, s)
	}
	inTerms := "?" + strings.Repeat(",?", len(runstatus.Terminal)-1)

	// The predicate is reused verbatim for the count and the deletes, so what is
	// counted is what goes. Inside the subquery and the DELETE target, `runs.id`
	// is the correlation name both dialects accept.
	pred := "tenant_id = ? AND status IN (" + inTerms + ")" +
		" AND NOT EXISTS (SELECT 1 FROM trace t WHERE t.run_id = runs.id)"

	// Classify first: the buckets depend on IsTerminal, and encoding that in a
	// CASE expression would put a second copy of the vocabulary in SQL.
	classify := bind(postgres, "SELECT status, "+
		"(SELECT COUNT(*) FROM trace t WHERE t.run_id = r.id) "+
		"FROM runs r WHERE r.tenant_id = ?")
	rows, err := tx.QueryContext(ctx, classify, tenantID)
	if err != nil {
		return rep, fmt.Errorf("state: tenant purge classify: %w", err)
	}
	purgeable := 0
	for rows.Next() {
		var status string
		var traceCount int
		if err := rows.Scan(&status, &traceCount); err != nil {
			_ = rows.Close()
			return rep, fmt.Errorf("state: tenant purge scan: %w", err)
		}
		switch {
		case !runstatus.IsTerminal(status):
			rep.RunsHeldActive++
		case traceCount > 0:
			rep.RunsHeldTrace++
		default:
			purgeable++
		}
	}
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("state: tenant purge rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return rep, fmt.Errorf("state: tenant purge close: %w", err)
	}

	if purgeable == 0 {
		return rep, nil
	}

	// run_assignment has no FK edge to runs, so it is cleared explicitly, and
	// FIRST: a dispatcher reading an assignment whose run is already gone is the
	// failure mode this ordering avoids.
	assignRes, err := tx.ExecContext(ctx, bind(postgres,
		"DELETE FROM run_assignment WHERE run_id IN (SELECT id FROM runs WHERE "+pred+")"), args...)
	if err != nil {
		return rep, fmt.Errorf("state: tenant purge assignments: %w", err)
	}
	assignments, err := assignRes.RowsAffected()
	if err != nil {
		return rep, fmt.Errorf("state: tenant purge assignment count: %w", err)
	}
	rep.AssignmentsRemoved = int(assignments)

	// approvals ride the runs cascade, so they are counted here rather than
	// deleted — an operator told "runs kept, approvals deleted" needs that number
	// to notice anything is off, and silence would hide it.
	var appr int
	if err := tx.QueryRowContext(ctx, bind(postgres,
		"SELECT COUNT(*) FROM approvals WHERE run_id IN (SELECT id FROM runs WHERE "+pred+")"),
		args...).Scan(&appr); err != nil {
		return rep, fmt.Errorf("state: tenant purge approvals: %w", err)
	}

	// The cascade deletes approvals, batches and steps with the run; that is the
	// schema's behaviour, measured in the header note, not an assumption here.
	res, err := tx.ExecContext(ctx, bind(postgres, "DELETE FROM runs WHERE "+pred), args...)
	if err != nil {
		return rep, fmt.Errorf("state: tenant purge runs: %w", err)
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return rep, fmt.Errorf("state: tenant purge runs affected: %w", err)
	}
	rep.RunsRemoved = int(removed)
	rep.ApprovalsRemoved = appr

	if err := tx.Commit(); err != nil {
		return rep, fmt.Errorf("state: tenant purge commit: %w", err)
	}
	return rep, nil
}

// bind converts `?` placeholders to PostgreSQL's `$n` form. Every statement
// passed here is a literal in this file, so counting `?` in order is
// well-defined; the helpers below keep that true.
func bind(postgres bool, query string) string {
	if !postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteString("$")
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

// purgeHandle returns the raw handle the purge speaks, and whether it is
// PostgreSQL. It is deliberately not part of the Store interface: adding a
// method there would force every decorator and test double to carry it, while
// the purge is a capability of the two real backends only.
func purgeHandle(base Store) (*sql.DB, bool, error) {
	switch h := base.(type) {
	case *SQLiteStore:
		return h.DB(), false, nil
	case *PGStore:
		return h.DB(), true, nil
	case nil:
		return nil, false, fmt.Errorf("state: tenant purge: nil store")
	default:
		return nil, false, fmt.Errorf("state: tenant purge: store %T exposes no SQL handle", h)
	}
}
