// exec_guard.go implements the execution lease/fencing primitive behind
// failover takeover (docs/design-cluster-failover.md §7.2-B2).
//
// Every cluster-mode execution registers a run_execution row before it
// dispatches its first step. The row carries a monotonically increasing
// epoch drawn from a dedicated sequence: re-registration by a different
// owner always mints a NEW epoch, which permanently invalidates the
// previous owner's writes. An executor keeps its lease alive by calling
// Owns (which renews — see the Owns doc for the TOCTOU argument) or
// Heartbeat; a takeover that observes an expired lease settles the run
// and deletes the row, after which every remaining write by the old
// owner is a no-op (0 rows affected) — fail-closed fencing.
//
// The SQL shape mirrors acquireLockSQL in distributed_lock.go (INSERT …
// ON CONFLICT with a WHERE-guarded DO UPDATE): a proven pattern in this
// package. Sequence gaps on conflict paths are expected and harmless;
// monotonicity is what matters.

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrFencedOut is the sentinel a guard consumer returns when it has lost
// ownership of an execution. Engine layers must treat it as "stop
// immediately, do NOT trigger the automatic rollback" (see
// engine.ErrFencedOut — the closure path maps this sentinel).
var ErrFencedOut = errors.New("cluster: execution fenced out")

// DefaultExecLeaseTTL bounds how long a run_execution lease stays valid
// without renewal. Its semantics are the UPPER BOUND on post-crash
// detection latency, not an execution-duration cap: a healthy executor
// renews at TTL/3 regardless of step progress, so slow steps never let
// the lease lapse.
const DefaultExecLeaseTTL = 30 * time.Second

// ExecLease is a held execution registration.
type ExecLease struct {
	RunID  string
	Owner  string // node ID of the executor
	Epoch  int64  // fencing token; every change of owner mints a new one
	Expiry time.Time
}

// registerExecSQL atomically claims the execution row for a run. The
// INSERT path covers a run never executed (or whose previous execution
// was settled and deleted). The ON CONFLICT update fires only when the
// caller already owns the row (lease refresh — epoch kept) or the lease
// has expired (takeover-style reclaim by a new executor — fresh epoch
// from the sequence). Otherwise no row is returned: another live owner
// holds the execution and the caller must refuse to execute (a run is
// single-flight cluster-wide).
const registerExecSQL = `
INSERT INTO run_execution (run_id, owner, epoch, registered_at, lease_expires)
VALUES ($1, $2, nextval('run_execution_epoch_seq'), NOW(), NOW() + ($3::bigint * INTERVAL '1 millisecond'))
ON CONFLICT (run_id) DO UPDATE
SET owner = EXCLUDED.owner,
    epoch = CASE WHEN run_execution.owner = EXCLUDED.owner
                 THEN run_execution.epoch
                 ELSE nextval('run_execution_epoch_seq') END,
    registered_at = NOW(),
    lease_expires = NOW() + ($3::bigint * INTERVAL '1 millisecond')
WHERE run_execution.owner = EXCLUDED.owner OR run_execution.lease_expires < NOW()
RETURNING owner, epoch, lease_expires`

// ExecutionGuard issues and validates execution leases for runs against
// the run_execution table. Safe for concurrent use; a single guard should
// be shared by all executions in a process.
type ExecutionGuard struct {
	db *sql.DB
}

// NewExecutionGuard returns a guard backed by the given *sql.DB (the
// cluster's PostgreSQL pool). A nil db yields a guard whose every method
// fails — wiring must treat a nil guard interface (not this) as
// "fencing disabled" (single-node mode).
func NewExecutionGuard(db *sql.DB) *ExecutionGuard {
	return &ExecutionGuard{db: db}
}

// Register claims the execution lease for run_id on behalf of owner,
// returning the lease with its fencing epoch. When another live owner
// holds the run it returns ErrFencedOut — the caller must refuse to
// execute (double execution is the worst outcome; refusing is safe and
// retryable after the other execution settles).
func (g *ExecutionGuard) Register(ctx context.Context, runID, owner string, ttl time.Duration) (*ExecLease, error) {
	if err := g.check(runID, owner, ttl); err != nil {
		return nil, err
	}
	var leaseOwner string
	var epoch int64
	var expiry time.Time
	err := g.db.QueryRowContext(ctx, registerExecSQL, runID, owner, ttl.Milliseconds()).
		Scan(&leaseOwner, &epoch, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("cluster: register execution for %q: %w", runID, ErrFencedOut)
	}
	if err != nil {
		return nil, fmt.Errorf("cluster: register execution for %q: %w", runID, err)
	}
	return &ExecLease{RunID: runID, Owner: leaseOwner, Epoch: epoch, Expiry: expiry}, nil
}

// Owns re-validates AND renews the lease in one statement. Renewal is the
// point, not a side effect: because the UPDATE both checks the epoch and
// extends lease_expires past NOW(), an Owns that returns true guarantees
// the lease cannot expire for another full ttl — so no takeover can
// legally observe this execution as expired between the check and the
// caller's subsequent write. A read-only check would leave a
// millisecond-scale window ("check passed → lease expires → takeover
// settles → caller writes") that renewal structurally closes.
//
// Lost ownership (row deleted by a takeover, reclaimed by a new owner,
// or the lease already expired) returns ErrFencedOut.
func (g *ExecutionGuard) Owns(ctx context.Context, lease *ExecLease, ttl time.Duration) error {
	if err := g.check(lease.RunID, lease.Owner, ttl); err != nil {
		return err
	}
	res, err := g.db.ExecContext(ctx, `
UPDATE run_execution
SET lease_expires = NOW() + ($3::bigint * INTERVAL '1 millisecond')
WHERE run_id = $1 AND owner = $2 AND epoch = $4 AND lease_expires >= NOW()`,
		lease.RunID, lease.Owner, ttl.Milliseconds(), lease.Epoch)
	if err != nil {
		return fmt.Errorf("cluster: own-check for %q: %w", lease.RunID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cluster: own-check for %q: rows: %w", lease.RunID, err)
	}
	if n == 0 {
		return fmt.Errorf("cluster: own-check for %q: %w", lease.RunID, ErrFencedOut)
	}
	return nil
}

// Heartbeat extends the lease without the epoch in the WHERE clause —
// it is the periodic keep-alive from the executor that still holds the
// row (same owner). Superseded by Owns for critical points; kept for the
// background renewal loop.
func (g *ExecutionGuard) Heartbeat(ctx context.Context, lease *ExecLease, ttl time.Duration) error {
	if err := g.check(lease.RunID, lease.Owner, ttl); err != nil {
		return err
	}
	res, err := g.db.ExecContext(ctx, `
UPDATE run_execution
SET lease_expires = NOW() + ($3::bigint * INTERVAL '1 millisecond')
WHERE run_id = $1 AND owner = $2 AND lease_expires >= NOW()`,
		lease.RunID, lease.Owner, ttl.Milliseconds())
	if err != nil {
		return fmt.Errorf("cluster: heartbeat for %q: %w", lease.RunID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cluster: heartbeat for %q: rows: %w", lease.RunID, err)
	}
	if n == 0 {
		return fmt.Errorf("cluster: heartbeat for %q: %w", lease.RunID, ErrFencedOut)
	}
	return nil
}

// End releases the lease when the execution settles normally. When the
// row is gone (a takeover already deleted it) or another owner holds it,
// End is a no-op — a zombie's End has no side effect by design.
func (g *ExecutionGuard) End(ctx context.Context, lease *ExecLease) error {
	if err := g.check(lease.RunID, lease.Owner, 0); err != nil {
		return err
	}
	_, err := g.db.ExecContext(ctx,
		`DELETE FROM run_execution WHERE run_id = $1 AND owner = $2 AND epoch = $3`,
		lease.RunID, lease.Owner, lease.Epoch)
	if err != nil {
		return fmt.Errorf("cluster: end execution for %q: %w", lease.RunID, err)
	}
	return nil
}

// LeaseGuardHandle pairs a lease with the guard that issued it, exposing
// the lease-bound operations (Owns/Heartbeat/End) without the caller
// needing to thread the guard and lease separately. Composition roots
// adapt this to their execution-layer interface.
type LeaseGuardHandle struct {
	g     *ExecutionGuard
	lease *ExecLease
	ttl   time.Duration
}

// LeaseGuard returns the lease-bound handle for a lease issued by this
// guard, renewing with the given ttl.
func (g *ExecutionGuard) LeaseGuard(lease *ExecLease, ttl time.Duration) *LeaseGuardHandle {
	return &LeaseGuardHandle{g: g, lease: lease, ttl: ttl}
}

func (l *LeaseGuardHandle) Owns(ctx context.Context) error {
	return l.g.Owns(ctx, l.lease, l.ttl)
}

func (l *LeaseGuardHandle) Heartbeat(ctx context.Context) error {
	return l.g.Heartbeat(ctx, l.lease, l.ttl)
}

func (l *LeaseGuardHandle) End(ctx context.Context) error {
	return l.g.End(ctx, l.lease)
}

// ExpiredExecutions returns the run IDs whose lease has expired — the
// takeover candidates. Row state is not modified here; the takeover loop
// settles each candidate under its own per-run lock and status CAS.
func (g *ExecutionGuard) ExpiredExecutions(ctx context.Context) ([]string, error) {
	if g.db == nil {
		return nil, fmt.Errorf("cluster: expired executions: nil db")
	}
	rows, err := g.db.QueryContext(ctx,
		`SELECT run_id FROM run_execution WHERE lease_expires < NOW() ORDER BY run_id`)
	if err != nil {
		return nil, fmt.Errorf("cluster: expired executions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("cluster: expired executions: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cluster: expired executions: rows: %w", err)
	}
	return ids, nil
}

// DeleteExecution removes the execution row unconditionally. This is
// the TAKEOVER's primitive (owner-agnostic by design: the takeover
// settles the run regardless of who the row claims); executors release
// through ExecLease-End, which is owner-checked.
func (g *ExecutionGuard) DeleteExecution(ctx context.Context, runID string) error {
	if g.db == nil {
		return fmt.Errorf("cluster: delete execution: nil db")
	}
	if runID == "" {
		return fmt.Errorf("cluster: delete execution: empty run id")
	}
	if _, err := g.db.ExecContext(ctx, `DELETE FROM run_execution WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("cluster: delete execution for %q: %w", runID, err)
	}
	return nil
}

// GetExecution returns the current execution row for a run, or (nil,
// nil) when the run has no row. Diagnostics and tests.
func (g *ExecutionGuard) GetExecution(ctx context.Context, runID string) (*ExecLease, error) {
	if g.db == nil {
		return nil, fmt.Errorf("cluster: get execution: nil db")
	}
	if runID == "" {
		return nil, fmt.Errorf("cluster: get execution: empty run id")
	}
	var lease ExecLease
	var expiry time.Time
	err := g.db.QueryRowContext(ctx,
		`SELECT run_id, owner, epoch, lease_expires FROM run_execution WHERE run_id = $1`, runID).
		Scan(&lease.RunID, &lease.Owner, &lease.Epoch, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cluster: get execution for %q: %w", runID, err)
	}
	lease.Expiry = expiry
	return &lease, nil
}

// check validates the shared preconditions of the guard operations.
func (g *ExecutionGuard) check(runID, owner string, ttl time.Duration) error {
	if g.db == nil {
		return fmt.Errorf("cluster: exec guard: nil db")
	}
	if runID == "" {
		return fmt.Errorf("cluster: exec guard: empty run id")
	}
	if owner == "" {
		return fmt.Errorf("cluster: exec guard: empty owner")
	}
	if ttl < 0 {
		return fmt.Errorf("cluster: exec guard: negative ttl")
	}
	return nil
}
