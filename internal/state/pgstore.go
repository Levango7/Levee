// pgstore.go implements the PostgreSQL-backed Store for LEVEE cluster mode.
// It mirrors SQLiteStore (sqlite.go) but talks to PostgreSQL via the pgx
// driver (github.com/jackc/pgx/v5/stdlib) and uses PostgreSQL-native DDL
// (pgschema.sql). All public behaviour matches the SQLite implementation so
// callers can swap stores without code changes; only the persistence backend
// differs.
//
// Design notes:
//   - IDs are TEXT (hex strings) — same as SQLite.
//   - Timestamps are TIMESTAMPTZ; pgx scans them into time.Time transparently.
//   - BYTEA is used for credentials.encrypted_data (vs BLOB in SQLite).
//   - WORM protection on the trace table is enforced by PostgreSQL triggers
//     (see pgschema.sql). UpdateTrace therefore only changes prev_hash/curr_hash
//     and relies on the trigger to reject tampering of immutable columns.
//   - The connection pool is configured via PoolConfig (max/min conns, idle
//     timeout, max lifetime) and exposed through *sql.DB so the same
//     database/sql surface is used across both stores.

package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"time"

	_ "embed" // required for go:embed

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/nexus/levee/internal/log"
)

// pgSchemaSQL holds the embedded pgschema.sql content. It is applied verbatim
// the first time a PostgreSQL store is opened.
//
//go:embed pgschema.sql
var pgSchemaSQL string

// pgCurrentSchemaVersion mirrors currentSchemaVersion for the PostgreSQL
// migration path. Bump whenever a forward PostgreSQL migration is added.
const pgCurrentSchemaVersion = 1

// PGPoolConfig tunes the PostgreSQL connection pool. Zero values fall back to
// sensible defaults derived from database/sql.
type PGPoolConfig struct {
	MaxOpenConns    int           // SetMaxOpenConns; <=0 means unlimited.
	MaxIdleConns    int           // SetMaxIdleConns; <=0 means 2.
	ConnMaxLifetime time.Duration // SetConnMaxLifetime; <=0 means unlimited.
	ConnMaxIdleTime time.Duration // SetConnMaxIdleTime; <=0 means unlimited.
}

// PGStore is the PostgreSQL-backed implementation of Store. It opens a
// connection pool backed by pgx and applies the embedded pgschema.sql. The
// underlying *sql.DB is safe for concurrent use.
type PGStore struct {
	db *sql.DB
}

// NewPGStore opens a PostgreSQL connection pool at dsn, applies migrations and
// configures pool sizing. The dsn must be a PostgreSQL connection string
// accepted by pgx (e.g. "postgres://user:pass@host:5432/dbname?sslmode=disable").
func NewPGStore(ctx context.Context, dsn string, cfg PGPoolConfig) (*PGStore, error) {
	if dsn == "" {
		return nil, fmt.Errorf("state: empty postgres dsn")
	}

	// Parse the DSN via pgx and register it as a database/sql driver. Using
	// stdlib.OpenDB avoids the global pgx driver name so multiple stores with
	// different DSNs can coexist in the same process (important for tests).
	pgxCfg, err := pgxParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("state: parse postgres dsn: %w", err)
	}
	db := stdlib.OpenDB(*pgxCfg)

	// Apply pool configuration.
	applyPGPoolConfig(db, cfg)

	// Verify connectivity so callers get an early error on bad DSN.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("state: ping postgres: %w", err)
	}

	if err := pgMigrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	log.Info("postgres store opened")
	return &PGStore{db: db}, nil
}

// DB exposes the underlying *sql.DB for advanced use cases (e.g. advisory
// locks in internal/cluster). Callers must not close it; use Close instead.
func (s *PGStore) DB() *sql.DB { return s.db }

// ExecRaw executes a raw SQL statement outside the Store CRUD methods. It is
// intended for testing scenarios that need to bypass WORM triggers or perform
// administrative operations not covered by the Store interface. Production
// code must not use this method.
func (s *PGStore) ExecRaw(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("state: exec raw: %w", err)
	}
	return nil
}

// Close releases the underlying database connection pool.
func (s *PGStore) Close() error {
	if s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("state: close postgres: %w", err)
	}
	return nil
}

// =========================================================================
// Run CRUD
// =========================================================================

// CreateRun inserts a new run row.
func (s *PGStore) CreateRun(ctx context.Context, run *Run) error {
	if run == nil {
		return fmt.Errorf("state: create run: nil run")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs
		(id, workflow_name, template_name, params, plan_hash, status,
		 approval_status, approval_level, created_at, updated_at, creator, incident_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		run.ID, run.WorkflowName, run.TemplateName, run.Params, run.PlanHash, run.Status,
		run.ApprovalStatus, run.ApprovalLevel, run.CreatedAt, run.UpdatedAt, run.Creator, run.IncidentID,
	)
	if err != nil {
		return fmt.Errorf("state: create run: %w", err)
	}
	return nil
}

// GetRun returns the run with the given id, or (nil, nil) if not found.
func (s *PGStore) GetRun(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, workflow_name, template_name, params, plan_hash, status,
		approval_status, approval_level, created_at, updated_at, creator, incident_id
		FROM runs WHERE id = $1`, id)
	r := &Run{}
	err := row.Scan(
		&r.ID, &r.WorkflowName, &r.TemplateName, &r.Params, &r.PlanHash, &r.Status,
		&r.ApprovalStatus, &r.ApprovalLevel, &r.CreatedAt, &r.UpdatedAt, &r.Creator, &r.IncidentID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get run %q: %w", id, err)
	}
	return r, nil
}

// UpdateRun overwrites all mutable columns of an existing run.
func (s *PGStore) UpdateRun(ctx context.Context, run *Run) error {
	if run == nil {
		return fmt.Errorf("state: update run: nil run")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET
		workflow_name=$1, template_name=$2, params=$3, plan_hash=$4, status=$5,
		approval_status=$6, approval_level=$7, updated_at=$8, creator=$9, incident_id=$10
		WHERE id=$11`,
		run.WorkflowName, run.TemplateName, run.Params, run.PlanHash, run.Status,
		run.ApprovalStatus, run.ApprovalLevel, run.UpdatedAt, run.Creator, run.IncidentID,
		run.ID,
	)
	if err != nil {
		return fmt.Errorf("state: update run %q: %w", run.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: update run %q: not found", run.ID)
	}
	return nil
}

// ListRuns returns runs matching the filter, ordered by created_at descending.
func (s *PGStore) ListRuns(ctx context.Context, filter RunFilter) ([]*Run, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.Status != "" {
		clauses = append(clauses, "status = $%d")
		args = append(args, filter.Status)
	}
	if filter.WorkflowName != "" {
		clauses = append(clauses, "workflow_name = $%d")
		args = append(args, filter.WorkflowName)
	}
	if filter.TemplateName != "" {
		clauses = append(clauses, "template_name = $%d")
		args = append(args, filter.TemplateName)
	}
	if filter.Creator != "" {
		clauses = append(clauses, "creator = $%d")
		args = append(args, filter.Creator)
	}
	if filter.IncidentID != "" {
		clauses = append(clauses, "incident_id = $%d")
		args = append(args, filter.IncidentID)
	}

	q := `SELECT id, workflow_name, template_name, params, plan_hash, status,
		approval_status, approval_level, created_at, updated_at, creator, incident_id
		FROM runs`
	if len(clauses) > 0 {
		q += " WHERE " + pgJoinPlaceholders(clauses)
	}
	q += " ORDER BY created_at DESC"
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Run
	for rows.Next() {
		r := &Run{}
		if err := rows.Scan(
			&r.ID, &r.WorkflowName, &r.TemplateName, &r.Params, &r.PlanHash, &r.Status,
			&r.ApprovalStatus, &r.ApprovalLevel, &r.CreatedAt, &r.UpdatedAt, &r.Creator, &r.IncidentID,
		); err != nil {
			return nil, fmt.Errorf("state: list runs scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list runs rows: %w", err)
	}
	return out, nil
}

// DeleteRun removes a run and all dependent rows (cascading via FK).
func (s *PGStore) DeleteRun(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("state: delete run %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Batch CRUD
// =========================================================================

// CreateBatch inserts a new batch row.
func (s *PGStore) CreateBatch(ctx context.Context, batch *Batch) error {
	if batch == nil {
		return fmt.Errorf("state: create batch: nil batch")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO batches
		(id, run_id, batch_no, status, total_hosts, succeeded, failed, started_at, completed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		batch.ID, batch.RunID, batch.BatchNo, batch.Status, batch.TotalHosts, batch.Succeeded, batch.Failed,
		batch.StartedAt, batch.CompletedAt,
	)
	if err != nil {
		return fmt.Errorf("state: create batch: %w", err)
	}
	return nil
}

// GetBatch returns the batch with the given id, or (nil, nil) if not found.
func (s *PGStore) GetBatch(ctx context.Context, id string) (*Batch, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, run_id, batch_no, status, total_hosts, succeeded, failed, started_at, completed_at
		FROM batches WHERE id = $1`, id)
	b := &Batch{}
	err := row.Scan(
		&b.ID, &b.RunID, &b.BatchNo, &b.Status, &b.TotalHosts, &b.Succeeded, &b.Failed,
		&b.StartedAt, &b.CompletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get batch %q: %w", id, err)
	}
	return b, nil
}

// UpdateBatch overwrites all mutable columns of an existing batch.
func (s *PGStore) UpdateBatch(ctx context.Context, batch *Batch) error {
	if batch == nil {
		return fmt.Errorf("state: update batch: nil batch")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE batches SET
		run_id=$1, batch_no=$2, status=$3, total_hosts=$4, succeeded=$5, failed=$6,
		started_at=$7, completed_at=$8
		WHERE id=$9`,
		batch.RunID, batch.BatchNo, batch.Status, batch.TotalHosts, batch.Succeeded, batch.Failed,
		batch.StartedAt, batch.CompletedAt, batch.ID,
	)
	if err != nil {
		return fmt.Errorf("state: update batch %q: %w", batch.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: update batch %q: not found", batch.ID)
	}
	return nil
}

// ListBatches returns batches matching the filter, ordered by batch_no ascending.
func (s *PGStore) ListBatches(ctx context.Context, filter BatchFilter) ([]*Batch, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = $%d")
		args = append(args, filter.RunID)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = $%d")
		args = append(args, filter.Status)
	}

	q := `SELECT id, run_id, batch_no, status, total_hosts, succeeded, failed, started_at, completed_at
		FROM batches`
	if len(clauses) > 0 {
		q += " WHERE " + pgJoinPlaceholders(clauses)
	}
	q += " ORDER BY batch_no ASC"
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list batches: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Batch
	for rows.Next() {
		b := &Batch{}
		if err := rows.Scan(
			&b.ID, &b.RunID, &b.BatchNo, &b.Status, &b.TotalHosts, &b.Succeeded, &b.Failed,
			&b.StartedAt, &b.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("state: list batches scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list batches rows: %w", err)
	}
	return out, nil
}

// DeleteBatch removes a batch by id.
func (s *PGStore) DeleteBatch(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM batches WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("state: delete batch %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Step CRUD
// =========================================================================

// CreateStep inserts a new step row.
func (s *PGStore) CreateStep(ctx context.Context, step *Step) error {
	if step == nil {
		return fmt.Errorf("state: create step: nil step")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO steps
		(id, run_id, batch_id, host, step_name, action, status, exit_code,
		 stdout, stderr, duration_ms, started_at, completed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		step.ID, step.RunID, step.BatchID, step.Host, step.StepName, step.Action, step.Status, step.ExitCode,
		step.Stdout, step.Stderr, step.DurationMs, step.StartedAt, step.CompletedAt,
	)
	if err != nil {
		return fmt.Errorf("state: create step: %w", err)
	}
	return nil
}

// GetStep returns the step with the given id, or (nil, nil) if not found.
func (s *PGStore) GetStep(ctx context.Context, id string) (*Step, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, run_id, batch_id, host, step_name, action, status, exit_code,
		stdout, stderr, duration_ms, started_at, completed_at
		FROM steps WHERE id = $1`, id)
	st := &Step{}
	err := row.Scan(
		&st.ID, &st.RunID, &st.BatchID, &st.Host, &st.StepName, &st.Action, &st.Status, &st.ExitCode,
		&st.Stdout, &st.Stderr, &st.DurationMs, &st.StartedAt, &st.CompletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get step %q: %w", id, err)
	}
	return st, nil
}

// UpdateStep overwrites all mutable columns of an existing step.
func (s *PGStore) UpdateStep(ctx context.Context, step *Step) error {
	if step == nil {
		return fmt.Errorf("state: update step: nil step")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE steps SET
		run_id=$1, batch_id=$2, host=$3, step_name=$4, action=$5, status=$6, exit_code=$7,
		stdout=$8, stderr=$9, duration_ms=$10, started_at=$11, completed_at=$12
		WHERE id=$13`,
		step.RunID, step.BatchID, step.Host, step.StepName, step.Action, step.Status, step.ExitCode,
		step.Stdout, step.Stderr, step.DurationMs, step.StartedAt, step.CompletedAt, step.ID,
	)
	if err != nil {
		return fmt.Errorf("state: update step %q: %w", step.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: update step %q: not found", step.ID)
	}
	return nil
}

// ListSteps returns steps matching the filter, ordered by started_at ascending.
func (s *PGStore) ListSteps(ctx context.Context, filter StepFilter) ([]*Step, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = $%d")
		args = append(args, filter.RunID)
	}
	if filter.BatchID != "" {
		clauses = append(clauses, "batch_id = $%d")
		args = append(args, filter.BatchID)
	}
	if filter.Host != "" {
		clauses = append(clauses, "host = $%d")
		args = append(args, filter.Host)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = $%d")
		args = append(args, filter.Status)
	}

	q := `SELECT id, run_id, batch_id, host, step_name, action, status, exit_code,
		stdout, stderr, duration_ms, started_at, completed_at
		FROM steps`
	if len(clauses) > 0 {
		q += " WHERE " + pgJoinPlaceholders(clauses)
	}
	q += " ORDER BY started_at ASC"
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list steps: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Step
	for rows.Next() {
		st := &Step{}
		if err := rows.Scan(
			&st.ID, &st.RunID, &st.BatchID, &st.Host, &st.StepName, &st.Action, &st.Status, &st.ExitCode,
			&st.Stdout, &st.Stderr, &st.DurationMs, &st.StartedAt, &st.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("state: list steps scan: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list steps rows: %w", err)
	}
	return out, nil
}

// DeleteStep removes a step by id.
func (s *PGStore) DeleteStep(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM steps WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("state: delete step %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Trace CRUD
// =========================================================================

// CreateTrace inserts a new trace record.
func (s *PGStore) CreateTrace(ctx context.Context, trace *Trace) error {
	if trace == nil {
		return fmt.Errorf("state: create trace: nil trace")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO trace
		(id, run_id, event, actor, detail, prev_hash, curr_hash, timestamp)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		trace.ID, trace.RunID, trace.Event, trace.Actor, trace.Detail, trace.PrevHash, trace.CurrHash, trace.Timestamp,
	)
	if err != nil {
		return fmt.Errorf("state: create trace: %w", err)
	}
	return nil
}

// GetTrace returns the trace with the given id, or (nil, nil) if not found.
func (s *PGStore) GetTrace(ctx context.Context, id string) (*Trace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, run_id, event, actor, detail, prev_hash, curr_hash, timestamp
		FROM trace WHERE id = $1`, id)
	t := &Trace{}
	err := row.Scan(&t.ID, &t.RunID, &t.Event, &t.Actor, &t.Detail, &t.PrevHash, &t.CurrHash, &t.Timestamp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get trace %q: %w", id, err)
	}
	return t, nil
}

// UpdateTrace overwrites all mutable columns of an existing trace record. It is
// used by the audit hash-chain builder (T044) to fill in prev_hash/curr_hash
// after the record has been inserted. The PostgreSQL WORM trigger
// (worm_prevent_trace_update) will reject changes to immutable content columns
// (id, run_id, event, actor, detail, timestamp).
func (s *PGStore) UpdateTrace(ctx context.Context, trace *Trace) error {
	if trace == nil {
		return fmt.Errorf("state: update trace: nil trace")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE trace SET
		run_id=$1, event=$2, actor=$3, detail=$4, prev_hash=$5, curr_hash=$6, timestamp=$7
		WHERE id=$8`,
		trace.RunID, trace.Event, trace.Actor, trace.Detail, trace.PrevHash, trace.CurrHash, trace.Timestamp, trace.ID,
	)
	if err != nil {
		return fmt.Errorf("state: update trace %q: %w", trace.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: update trace %q: not found", trace.ID)
	}
	return nil
}

// ListTraces returns trace records matching the filter, ordered by timestamp
// ascending with id as a deterministic tie-breaker. See SQLiteStore.ListTraces
// for the rationale.
func (s *PGStore) ListTraces(ctx context.Context, filter TraceFilter) ([]*Trace, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = $%d")
		args = append(args, filter.RunID)
	}
	if filter.Event != "" {
		clauses = append(clauses, "event = $%d")
		args = append(args, filter.Event)
	}

	q := `SELECT id, run_id, event, actor, detail, prev_hash, curr_hash, timestamp FROM trace`
	if len(clauses) > 0 {
		q += " WHERE " + pgJoinPlaceholders(clauses)
	}
	q += " ORDER BY timestamp ASC, id ASC"
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list traces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Trace
	for rows.Next() {
		t := &Trace{}
		if err := rows.Scan(&t.ID, &t.RunID, &t.Event, &t.Actor, &t.Detail, &t.PrevHash, &t.CurrHash, &t.Timestamp); err != nil {
			return nil, fmt.Errorf("state: list traces scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list traces rows: %w", err)
	}
	return out, nil
}

// DeleteTrace removes a trace record by id. The PostgreSQL WORM trigger
// (worm_prevent_trace_delete) will reject this operation; the error is wrapped
// and returned to the caller.
func (s *PGStore) DeleteTrace(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM trace WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("state: delete trace %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Approval CRUD
// =========================================================================

// CreateApproval inserts a new approval record.
func (s *PGStore) CreateApproval(ctx context.Context, approval *Approval) error {
	if approval == nil {
		return fmt.Errorf("state: create approval: nil approval")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals
		(id, run_id, level, approver, status, comment, timeout_at, acted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		approval.ID, approval.RunID, approval.Level, approval.Approver, approval.Status,
		approval.Comment, approval.TimeoutAt, approval.ActedAt,
	)
	if err != nil {
		return fmt.Errorf("state: create approval: %w", err)
	}
	return nil
}

// GetApproval returns the approval with the given id, or (nil, nil) if not found.
func (s *PGStore) GetApproval(ctx context.Context, id string) (*Approval, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, run_id, level, approver, status, comment, timeout_at, acted_at
		FROM approvals WHERE id = $1`, id)
	a := &Approval{}
	err := row.Scan(&a.ID, &a.RunID, &a.Level, &a.Approver, &a.Status, &a.Comment, &a.TimeoutAt, &a.ActedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get approval %q: %w", id, err)
	}
	return a, nil
}

// UpdateApproval overwrites all mutable columns of an existing approval.
func (s *PGStore) UpdateApproval(ctx context.Context, approval *Approval) error {
	if approval == nil {
		return fmt.Errorf("state: update approval: nil approval")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET
		run_id=$1, level=$2, approver=$3, status=$4, comment=$5, timeout_at=$6, acted_at=$7
		WHERE id=$8`,
		approval.RunID, approval.Level, approval.Approver, approval.Status, approval.Comment,
		approval.TimeoutAt, approval.ActedAt, approval.ID,
	)
	if err != nil {
		return fmt.Errorf("state: update approval %q: %w", approval.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: update approval %q: not found", approval.ID)
	}
	return nil
}

// ListApprovals returns approvals matching the filter, ordered by timeout_at ascending.
func (s *PGStore) ListApprovals(ctx context.Context, filter ApprovalFilter) ([]*Approval, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = $%d")
		args = append(args, filter.RunID)
	}
	if filter.Level != "" {
		clauses = append(clauses, "level = $%d")
		args = append(args, filter.Level)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = $%d")
		args = append(args, filter.Status)
	}

	q := `SELECT id, run_id, level, approver, status, comment, timeout_at, acted_at FROM approvals`
	if len(clauses) > 0 {
		q += " WHERE " + pgJoinPlaceholders(clauses)
	}
	q += " ORDER BY timeout_at ASC"
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list approvals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Approval
	for rows.Next() {
		a := &Approval{}
		if err := rows.Scan(&a.ID, &a.RunID, &a.Level, &a.Approver, &a.Status, &a.Comment, &a.TimeoutAt, &a.ActedAt); err != nil {
			return nil, fmt.Errorf("state: list approvals scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list approvals rows: %w", err)
	}
	return out, nil
}

// DeleteApproval removes an approval by id.
func (s *PGStore) DeleteApproval(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM approvals WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("state: delete approval %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Lock CRUD
// =========================================================================

// CreateLock inserts a new lock record. The scope has a UNIQUE constraint,
// so attempting to create a second lock on the same scope will fail with a
// constraint violation.
func (s *PGStore) CreateLock(ctx context.Context, lock *Lock) error {
	if lock == nil {
		return fmt.Errorf("state: create lock: nil lock")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO locks
		(id, scope, owner, ttl_seconds, acquired_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		lock.ID, lock.Scope, lock.Owner, lock.TTLSeconds, lock.AcquiredAt, lock.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("state: create lock: %w", err)
	}
	return nil
}

// GetLock returns the lock with the given id, or (nil, nil) if not found.
func (s *PGStore) GetLock(ctx context.Context, id string) (*Lock, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, scope, owner, ttl_seconds, acquired_at, expires_at
		FROM locks WHERE id = $1`, id)
	l := &Lock{}
	err := row.Scan(&l.ID, &l.Scope, &l.Owner, &l.TTLSeconds, &l.AcquiredAt, &l.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get lock %q: %w", id, err)
	}
	return l, nil
}

// GetLockByScope returns the lock for the given scope, or (nil, nil) if none.
func (s *PGStore) GetLockByScope(ctx context.Context, scope string) (*Lock, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, scope, owner, ttl_seconds, acquired_at, expires_at
		FROM locks WHERE scope = $1`, scope)
	l := &Lock{}
	err := row.Scan(&l.ID, &l.Scope, &l.Owner, &l.TTLSeconds, &l.AcquiredAt, &l.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get lock by scope %q: %w", scope, err)
	}
	return l, nil
}

// UpdateLock overwrites all mutable columns of an existing lock.
func (s *PGStore) UpdateLock(ctx context.Context, lock *Lock) error {
	if lock == nil {
		return fmt.Errorf("state: update lock: nil lock")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE locks SET
		scope=$1, owner=$2, ttl_seconds=$3, acquired_at=$4, expires_at=$5
		WHERE id=$6`,
		lock.Scope, lock.Owner, lock.TTLSeconds, lock.AcquiredAt, lock.ExpiresAt, lock.ID,
	)
	if err != nil {
		return fmt.Errorf("state: update lock %q: %w", lock.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: update lock %q: not found", lock.ID)
	}
	return nil
}

// UpdateLockOwnedBy atomically updates the owner (and ttl/acquired/expires)
// of the lock with the given id, but only when the lock has expired
// (expires_at <= now). It returns the number of rows affected so callers
// can detect concurrent races: 0 means another actor already performed the
// update and the caller should retry.
func (s *PGStore) UpdateLockOwnedBy(ctx context.Context, id string, owner string, ttlSeconds int, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE locks SET
		owner=$1, ttl_seconds=$2, acquired_at=$3, expires_at=$4
		WHERE id=$5 AND expires_at <= $6`,
		owner, ttlSeconds, now, now.Add(time.Duration(ttlSeconds)*time.Second), id, now,
	)
	if err != nil {
		return 0, fmt.Errorf("state: update lock owned by %q: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListLocks returns all locks, ordered by expires_at ascending.
func (s *PGStore) ListLocks(ctx context.Context) ([]*Lock, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, scope, owner, ttl_seconds, acquired_at, expires_at
		FROM locks ORDER BY expires_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("state: list locks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Lock
	for rows.Next() {
		l := &Lock{}
		if err := rows.Scan(&l.ID, &l.Scope, &l.Owner, &l.TTLSeconds, &l.AcquiredAt, &l.ExpiresAt); err != nil {
			return nil, fmt.Errorf("state: list locks scan: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list locks rows: %w", err)
	}
	return out, nil
}

// DeleteLock removes a lock by id.
func (s *PGStore) DeleteLock(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM locks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("state: delete lock %q: %w", id, err)
	}
	return nil
}

// DeleteExpiredLocks removes all locks whose expires_at is before the given
// timestamp and returns the number of rows removed.
func (s *PGStore) DeleteExpiredLocks(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM locks WHERE expires_at < $1`, now)
	if err != nil {
		return 0, fmt.Errorf("state: delete expired locks: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// =========================================================================
// Credential CRUD
// =========================================================================

// CreateCredential inserts a new credential record.
func (s *PGStore) CreateCredential(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("state: create credential: nil credential")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO credentials
		(id, name, type, encrypted_data, created_at, rotated_at)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		cred.ID, cred.Name, cred.Type, cred.EncryptedData, cred.CreatedAt, cred.RotatedAt,
	)
	if err != nil {
		return fmt.Errorf("state: create credential: %w", err)
	}
	return nil
}

// GetCredential returns the credential with the given id, or (nil, nil) if not found.
func (s *PGStore) GetCredential(ctx context.Context, id string) (*Credential, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, name, type, encrypted_data, created_at, rotated_at
		FROM credentials WHERE id = $1`, id)
	c := &Credential{}
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.EncryptedData, &c.CreatedAt, &c.RotatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get credential %q: %w", id, err)
	}
	return c, nil
}

// GetCredentialByName returns the credential with the given unique name, or (nil, nil).
func (s *PGStore) GetCredentialByName(ctx context.Context, name string) (*Credential, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, name, type, encrypted_data, created_at, rotated_at
		FROM credentials WHERE name = $1`, name)
	c := &Credential{}
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.EncryptedData, &c.CreatedAt, &c.RotatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get credential by name %q: %w", name, err)
	}
	return c, nil
}

// UpdateCredential overwrites all mutable columns of an existing credential.
func (s *PGStore) UpdateCredential(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("state: update credential: nil credential")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE credentials SET
		name=$1, type=$2, encrypted_data=$3, created_at=$4, rotated_at=$5
		WHERE id=$6`,
		cred.Name, cred.Type, cred.EncryptedData, cred.CreatedAt, cred.RotatedAt, cred.ID,
	)
	if err != nil {
		return fmt.Errorf("state: update credential %q: %w", cred.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: update credential %q: not found", cred.ID)
	}
	return nil
}

// ListCredentials returns all credentials, ordered by name ascending.
func (s *PGStore) ListCredentials(ctx context.Context) ([]*Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, name, type, encrypted_data, created_at, rotated_at
		FROM credentials ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("state: list credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Credential
	for rows.Next() {
		c := &Credential{}
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &c.EncryptedData, &c.CreatedAt, &c.RotatedAt); err != nil {
			return nil, fmt.Errorf("state: list credentials scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list credentials rows: %w", err)
	}
	return out, nil
}

// DeleteCredential removes a credential by id.
func (s *PGStore) DeleteCredential(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM credentials WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("state: delete credential %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Audit CRUD
// =========================================================================

// CreateAudit inserts a new audit log entry.
func (s *PGStore) CreateAudit(ctx context.Context, audit *Audit) error {
	if audit == nil {
		return fmt.Errorf("state: create audit: nil audit")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit
		(id, run_id, action, actor, target, result, timestamp)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		audit.ID, audit.RunID, audit.Action, audit.Actor, audit.Target, audit.Result, audit.Timestamp,
	)
	if err != nil {
		return fmt.Errorf("state: create audit: %w", err)
	}
	return nil
}

// GetAudit returns the audit entry with the given id, or (nil, nil) if not found.
func (s *PGStore) GetAudit(ctx context.Context, id string) (*Audit, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, run_id, action, actor, target, result, timestamp
		FROM audit WHERE id = $1`, id)
	a := &Audit{}
	err := row.Scan(&a.ID, &a.RunID, &a.Action, &a.Actor, &a.Target, &a.Result, &a.Timestamp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get audit %q: %w", id, err)
	}
	return a, nil
}

// ListAudits returns audit entries matching the filter, ordered by timestamp descending.
func (s *PGStore) ListAudits(ctx context.Context, filter AuditFilter) ([]*Audit, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = $%d")
		args = append(args, filter.RunID)
	}
	if filter.Action != "" {
		clauses = append(clauses, "action = $%d")
		args = append(args, filter.Action)
	}
	if filter.Actor != "" {
		clauses = append(clauses, "actor = $%d")
		args = append(args, filter.Actor)
	}

	q := `SELECT id, run_id, action, actor, target, result, timestamp FROM audit`
	if len(clauses) > 0 {
		q += " WHERE " + pgJoinPlaceholders(clauses)
	}
	q += " ORDER BY timestamp DESC"
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list audits: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Audit
	for rows.Next() {
		a := &Audit{}
		if err := rows.Scan(&a.ID, &a.RunID, &a.Action, &a.Actor, &a.Target, &a.Result, &a.Timestamp); err != nil {
			return nil, fmt.Errorf("state: list audits scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list audits rows: %w", err)
	}
	return out, nil
}
