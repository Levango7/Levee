package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// SQLiteStore is the SQLite-backed implementation of Store. It opens a single
// database file (or :memory:) and configures WAL mode plus foreign-key
// enforcement. The underlying *sql.DB is safe for concurrent use.
type SQLiteStore struct {
	db *sql.DB
}

// SQLiteOption customises NewSQLiteStore at open time. Options are applied
// in order; the zero option set reproduces the historical behaviour.
type SQLiteOption func(*sqliteOpenOptions)

type sqliteOpenOptions struct {
	synchronous string // "" | "normal" | "full" (case-insensitive)
}

// WithSynchronous sets the SQLite synchronous pragma: "normal" (default;
// WAL checkpoint-batched fsync) or "full" (fsync per commit, for deployments
// where audit durability outranks the small write amplification, SA-019).
// An empty string keeps the default. Any other value fails NewSQLiteStore.
func WithSynchronous(mode string) SQLiteOption {
	return func(o *sqliteOpenOptions) { o.synchronous = mode }
}

// NewSQLiteStore opens (or creates) the database at dbPath, applies migrations
// and configures connection-level pragmas. Use ":memory:" for an in-memory
// database (useful in tests); in that case a single shared connection is used
// so the in-memory database survives across calls.
func NewSQLiteStore(ctx context.Context, dbPath string, opts ...SQLiteOption) (*SQLiteStore, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("state: empty db path")
	}

	var o sqliteOpenOptions
	for _, opt := range opts {
		opt(&o)
	}
	syn := strings.ToLower(strings.TrimSpace(o.synchronous))
	if syn == "" {
		syn = "normal"
	}
	switch syn {
	case "normal", "full":
	default:
		return nil, fmt.Errorf("state: invalid synchronous mode %q: must be normal|full", o.synchronous)
	}

	// For :memory: databases we must keep a single connection alive for the
	// lifetime of the store, otherwise each pool connection sees a fresh
	// in-memory database. We achieve this by setting max open/inactive conns
	// to 1.
	dsn := dbPath
	if dbPath == ":memory:" {
		// modernc.org/sqlite supports the shared cache pragma via the DSN.
		// We use a unique shared name so concurrent tests do not collide.
		dsn = "file::memory:?cache=shared"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: open sqlite: %w", err)
	}

	if dbPath == ":memory:" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}

	// Apply pragmas on every pooled connection so they always hold.
	// recursive_triggers=ON makes FK ON DELETE CASCADE deletions fire
	// BEFORE DELETE triggers on the child table (trace), so a run delete
	// that would cascade into trace rows is aborted by
	// worm_prevent_trace_delete instead of silently bypassing WORM
	// protection.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=" + strings.ToUpper(syn),
		"PRAGMA recursive_triggers=ON",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("state: apply pragma %q: %w", p, err)
		}
	}

	if err := Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &SQLiteStore{db: db}, nil
}

// DB exposes the underlying *sql.DB for advanced use cases (e.g. backups).
// Callers must not close it; use Close instead.
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// Close releases the underlying database connection pool.
func (s *SQLiteStore) Close() error {
	if s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("state: close sqlite: %w", err)
	}
	return nil
}

// =========================================================================
// Run CRUD
// =========================================================================

// CreateRun inserts a new run row.
func (s *SQLiteStore) CreateRun(ctx context.Context, run *Run) error {
	if run == nil {
		return fmt.Errorf("state: create run: nil run")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs
		(id, workflow_name, template_name, params, plan_hash, plan_json, status,
		 approval_status, approval_level, created_at, updated_at, creator, incident_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.WorkflowName, run.TemplateName, run.Params, run.PlanHash, run.PlanJSON, run.Status,
		run.ApprovalStatus, run.ApprovalLevel, run.CreatedAt, run.UpdatedAt, run.Creator, run.IncidentID,
	)
	if err != nil {
		return fmt.Errorf("state: create run: %w", err)
	}
	return nil
}

// GetRun returns the run with the given id, or (nil, nil) if not found.
func (s *SQLiteStore) GetRun(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, workflow_name, template_name, params, plan_hash, plan_json, status,
		approval_status, approval_level, created_at, updated_at, creator, incident_id
		FROM runs WHERE id = ?`, id)
	r := &Run{}
	err := row.Scan(
		&r.ID, &r.WorkflowName, &r.TemplateName, &r.Params, &r.PlanHash, &r.PlanJSON, &r.Status,
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
func (s *SQLiteStore) UpdateRun(ctx context.Context, run *Run) error {
	if run == nil {
		return fmt.Errorf("state: update run: nil run")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET
		workflow_name=?, template_name=?, params=?, plan_hash=?, plan_json=?, status=?,
		approval_status=?, approval_level=?, updated_at=?, creator=?, incident_id=?
		WHERE id=?`,
		run.WorkflowName, run.TemplateName, run.Params, run.PlanHash, run.PlanJSON, run.Status,
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

// UpdateRunStatusIf atomically transitions a run's status from `from` to
// `to`, stamping updated_at. It returns (false, nil) when the run does not
// exist or its current status is not `from`, leaving the row untouched.
func (s *SQLiteStore) UpdateRunStatusIf(ctx context.Context, id string, from string, to string, updatedAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status=?, updated_at=? WHERE id=? AND status=?`,
		to, updatedAt, id, from,
	)
	if err != nil {
		return false, fmt.Errorf("state: update run status %q %s->%s: %w", id, from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("state: update run status %q: rows affected: %w", id, err)
	}
	return n > 0, nil
}

// MarkNonTerminalSteps flips the run's non-terminal step rows
// (status IN running/pending) to marker; see the Store interface for the
// takeover rationale.
func (s *SQLiteStore) MarkNonTerminalSteps(ctx context.Context, runID string, marker string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE steps SET status=? WHERE run_id=? AND status IN ('running','pending')`,
		marker, runID,
	)
	if err != nil {
		return 0, fmt.Errorf("state: mark non-terminal steps for %q: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("state: mark non-terminal steps for %q: rows: %w", runID, err)
	}
	return n, nil
}

// ListRuns returns runs matching the filter, ordered by created_at descending.
func (s *SQLiteStore) ListRuns(ctx context.Context, filter RunFilter) ([]*Run, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.WorkflowName != "" {
		clauses = append(clauses, "workflow_name = ?")
		args = append(args, filter.WorkflowName)
	}
	if filter.TemplateName != "" {
		clauses = append(clauses, "template_name = ?")
		args = append(args, filter.TemplateName)
	}
	if filter.Creator != "" {
		clauses = append(clauses, "creator = ?")
		args = append(args, filter.Creator)
	}
	if filter.IncidentID != "" {
		clauses = append(clauses, "incident_id = ?")
		args = append(args, filter.IncidentID)
	}

	q := `SELECT id, workflow_name, template_name, params, plan_hash, plan_json, status,
		approval_status, approval_level, created_at, updated_at, creator, incident_id
		FROM runs`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ") // #nosec G202 -- clause fragments are static; all values bind via placeholders
	}
	q += " ORDER BY created_at DESC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	if filter.Offset > 0 {
		q += " OFFSET ?"
		args = append(args, filter.Offset)
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
			&r.ID, &r.WorkflowName, &r.TemplateName, &r.Params, &r.PlanHash, &r.PlanJSON, &r.Status,
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

// DeleteRun removes a run and all dependent rows (cascading via FK). WORM
// protection is preserved: with PRAGMA recursive_triggers=ON the cascade
// into the trace table fires the worm_prevent_trace_delete trigger, so
// deleting a run that still has trace records fails with the WORM error
// instead of silently removing audit history. Runs without traces delete
// normally.
func (s *SQLiteStore) DeleteRun(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("state: delete run %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Batch CRUD
// =========================================================================

// CreateBatch inserts a new batch row.
func (s *SQLiteStore) CreateBatch(ctx context.Context, batch *Batch) error {
	if batch == nil {
		return fmt.Errorf("state: create batch: nil batch")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO batches
		(id, run_id, batch_no, status, total_hosts, succeeded, failed, started_at, completed_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		batch.ID, batch.RunID, batch.BatchNo, batch.Status, batch.TotalHosts, batch.Succeeded, batch.Failed,
		batch.StartedAt, batch.CompletedAt,
	)
	if err != nil {
		return fmt.Errorf("state: create batch: %w", err)
	}
	return nil
}

// GetBatch returns the batch with the given id, or (nil, nil) if not found.
func (s *SQLiteStore) GetBatch(ctx context.Context, id string) (*Batch, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, run_id, batch_no, status, total_hosts, succeeded, failed, started_at, completed_at
		FROM batches WHERE id = ?`, id)
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
func (s *SQLiteStore) UpdateBatch(ctx context.Context, batch *Batch) error {
	if batch == nil {
		return fmt.Errorf("state: update batch: nil batch")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE batches SET
		run_id=?, batch_no=?, status=?, total_hosts=?, succeeded=?, failed=?,
		started_at=?, completed_at=?
		WHERE id=?`,
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
func (s *SQLiteStore) ListBatches(ctx context.Context, filter BatchFilter) ([]*Batch, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = ?")
		args = append(args, filter.RunID)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, filter.Status)
	}

	q := `SELECT id, run_id, batch_no, status, total_hosts, succeeded, failed, started_at, completed_at
		FROM batches`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ") // #nosec G202 -- clause fragments are static; all values bind via placeholders
	}
	q += " ORDER BY batch_no ASC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
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
func (s *SQLiteStore) DeleteBatch(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM batches WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("state: delete batch %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Step CRUD
// =========================================================================

// CreateStep inserts a new step row.
func (s *SQLiteStore) CreateStep(ctx context.Context, step *Step) error {
	if step == nil {
		return fmt.Errorf("state: create step: nil step")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO steps
		(id, run_id, batch_id, host, step_name, action, status, exit_code,
		 stdout, stderr, duration_ms, started_at, completed_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		step.ID, step.RunID, step.BatchID, step.Host, step.StepName, step.Action, step.Status, step.ExitCode,
		step.Stdout, step.Stderr, step.DurationMs, step.StartedAt, step.CompletedAt,
	)
	if err != nil {
		return fmt.Errorf("state: create step: %w", err)
	}
	return nil
}

// GetStep returns the step with the given id, or (nil, nil) if not found.
func (s *SQLiteStore) GetStep(ctx context.Context, id string) (*Step, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, run_id, batch_id, host, step_name, action, status, exit_code,
		stdout, stderr, duration_ms, started_at, completed_at
		FROM steps WHERE id = ?`, id)
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
func (s *SQLiteStore) UpdateStep(ctx context.Context, step *Step) error {
	if step == nil {
		return fmt.Errorf("state: update step: nil step")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE steps SET
		run_id=?, batch_id=?, host=?, step_name=?, action=?, status=?, exit_code=?,
		stdout=?, stderr=?, duration_ms=?, started_at=?, completed_at=?
		WHERE id=?`,
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
func (s *SQLiteStore) ListSteps(ctx context.Context, filter StepFilter) ([]*Step, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = ?")
		args = append(args, filter.RunID)
	}
	if filter.BatchID != "" {
		clauses = append(clauses, "batch_id = ?")
		args = append(args, filter.BatchID)
	}
	if filter.Host != "" {
		clauses = append(clauses, "host = ?")
		args = append(args, filter.Host)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, filter.Status)
	}

	q := `SELECT id, run_id, batch_id, host, step_name, action, status, exit_code,
		stdout, stderr, duration_ms, started_at, completed_at
		FROM steps`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ") // #nosec G202 -- clause fragments are static; all values bind via placeholders
	}
	q += " ORDER BY started_at ASC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
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
func (s *SQLiteStore) DeleteStep(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM steps WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("state: delete step %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Trace CRUD
// =========================================================================

// CreateTrace inserts a new trace record.
func (s *SQLiteStore) CreateTrace(ctx context.Context, trace *Trace) error {
	if trace == nil {
		return fmt.Errorf("state: create trace: nil trace")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO trace
		(id, run_id, event, actor, detail, prev_hash, curr_hash, timestamp)
		VALUES (?,?,?,?,?,?,?,?)`,
		trace.ID, trace.RunID, trace.Event, trace.Actor, trace.Detail, trace.PrevHash, trace.CurrHash, trace.Timestamp,
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return fmt.Errorf("state: create trace %q: %w", trace.ID, ErrTraceExists)
		}
		return fmt.Errorf("state: create trace: %w", err)
	}
	return nil
}

// GetTrace returns the trace with the given id, or (nil, nil) if not found.
func (s *SQLiteStore) GetTrace(ctx context.Context, id string) (*Trace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, run_id, event, actor, detail, prev_hash, curr_hash, timestamp
		FROM trace WHERE id = ?`, id)
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
// after the record has been inserted.
func (s *SQLiteStore) UpdateTrace(ctx context.Context, trace *Trace) error {
	if trace == nil {
		return fmt.Errorf("state: update trace: nil trace")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE trace SET
		run_id=?, event=?, actor=?, detail=?, prev_hash=?, curr_hash=?, timestamp=?
		WHERE id=?`,
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

// UpdateTraceChecksum stamps the WORM content checksum into curr_hash, but
// only when curr_hash is still empty. The conditional WHERE clause guarantees
// an already-computed hash (content checksum or hash-chain link) is never
// overwritten, complementing the worm_prevent_trace_update trigger which
// permits curr_hash writes. An empty checksum or unknown trace id yields an
// error.
func (s *SQLiteStore) UpdateTraceChecksum(ctx context.Context, id string, checksum string) error {
	if id == "" {
		return fmt.Errorf("state: update trace checksum: empty id")
	}
	if checksum == "" {
		return fmt.Errorf("state: update trace checksum %q: empty checksum", id)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE trace SET curr_hash=? WHERE id=? AND curr_hash=''`,
		checksum, id)
	if err != nil {
		return fmt.Errorf("state: update trace checksum %q: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: update trace checksum %q: not found or checksum already present", id)
	}
	return nil
}

// ListTraces returns trace records matching the filter, ordered by timestamp
// ascending. When multiple records share the same timestamp, the secondary sort
// key id ASC guarantees a deterministic order. This is essential for the audit
// hash-chain builder (HashChainBuilder.Build), which must produce an identical
// chain for the same set of records on every invocation; without a tie-breaker
// the row order would be unspecified and the resulting hash chain unpredictable.
// The id column is the TEXT primary key set by the recorder and immutable after
// insertion, so it is a stable tie-breaker even though it is not an autoincrement
// integer.
func (s *SQLiteStore) ListTraces(ctx context.Context, filter TraceFilter) ([]*Trace, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = ?")
		args = append(args, filter.RunID)
	}
	if filter.Event != "" {
		clauses = append(clauses, "event = ?")
		args = append(args, filter.Event)
	}

	q := `SELECT id, run_id, event, actor, detail, prev_hash, curr_hash, timestamp FROM trace`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ") // #nosec G202 -- clause fragments are static; all values bind via placeholders
	}
	q += " ORDER BY timestamp ASC, id ASC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
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

// DeleteTrace removes a trace record by id.
func (s *SQLiteStore) DeleteTrace(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM trace WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("state: delete trace %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Approval CRUD
// =========================================================================

// CreateApproval inserts a new approval record.
func (s *SQLiteStore) CreateApproval(ctx context.Context, approval *Approval) error {
	if approval == nil {
		return fmt.Errorf("state: create approval: nil approval")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals
		(id, run_id, level, approver, status, comment, timeout_at, acted_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		approval.ID, approval.RunID, approval.Level, approval.Approver, approval.Status,
		approval.Comment, approval.TimeoutAt, approval.ActedAt,
	)
	if err != nil {
		return fmt.Errorf("state: create approval: %w", err)
	}
	return nil
}

// GetApproval returns the approval with the given id, or (nil, nil) if not found.
func (s *SQLiteStore) GetApproval(ctx context.Context, id string) (*Approval, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, run_id, level, approver, status, comment, timeout_at, acted_at
		FROM approvals WHERE id = ?`, id)
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
func (s *SQLiteStore) UpdateApproval(ctx context.Context, approval *Approval) error {
	if approval == nil {
		return fmt.Errorf("state: update approval: nil approval")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET
		run_id=?, level=?, approver=?, status=?, comment=?, timeout_at=?, acted_at=?
		WHERE id=?`,
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

// UpdateApprovalIfPending is the compare-and-set variant of UpdateApproval:
// it applies the update only when the stored row is still in status
// "pending". It returns true when the update was applied and false when the
// row was concurrently decided (or does not exist), so callers never
// overwrite a terminal decision.
func (s *SQLiteStore) UpdateApprovalIfPending(ctx context.Context, approval *Approval) (bool, error) {
	if approval == nil {
		return false, fmt.Errorf("state: update approval if pending: nil approval")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET
		run_id=?, level=?, approver=?, status=?, comment=?, timeout_at=?, acted_at=?
		WHERE id=? AND status='pending'`,
		approval.RunID, approval.Level, approval.Approver, approval.Status, approval.Comment,
		approval.TimeoutAt, approval.ActedAt, approval.ID,
	)
	if err != nil {
		return false, fmt.Errorf("state: update approval %q if pending: %w", approval.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListApprovals returns approvals matching the filter, ordered by timeout_at ascending.
func (s *SQLiteStore) ListApprovals(ctx context.Context, filter ApprovalFilter) ([]*Approval, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = ?")
		args = append(args, filter.RunID)
	}
	if filter.Level != "" {
		clauses = append(clauses, "level = ?")
		args = append(args, filter.Level)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, filter.Status)
	}

	q := `SELECT id, run_id, level, approver, status, comment, timeout_at, acted_at FROM approvals`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ") // #nosec G202 -- clause fragments are static; all values bind via placeholders
	}
	q += " ORDER BY timeout_at ASC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
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
func (s *SQLiteStore) DeleteApproval(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM approvals WHERE id = ?`, id)
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
func (s *SQLiteStore) CreateLock(ctx context.Context, lock *Lock) error {
	if lock == nil {
		return fmt.Errorf("state: create lock: nil lock")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO locks
		(id, scope, owner, ttl_seconds, acquired_at, expires_at)
		VALUES (?,?,?,?,?,?)`,
		lock.ID, lock.Scope, lock.Owner, lock.TTLSeconds, lock.AcquiredAt, lock.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("state: create lock: %w", err)
	}
	return nil
}

// GetLock returns the lock with the given id, or (nil, nil) if not found.
func (s *SQLiteStore) GetLock(ctx context.Context, id string) (*Lock, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, scope, owner, ttl_seconds, acquired_at, expires_at
		FROM locks WHERE id = ?`, id)
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
func (s *SQLiteStore) GetLockByScope(ctx context.Context, scope string) (*Lock, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, scope, owner, ttl_seconds, acquired_at, expires_at
		FROM locks WHERE scope = ?`, scope)
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
func (s *SQLiteStore) UpdateLock(ctx context.Context, lock *Lock) error {
	if lock == nil {
		return fmt.Errorf("state: update lock: nil lock")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE locks SET
		scope=?, owner=?, ttl_seconds=?, acquired_at=?, expires_at=?
		WHERE id=?`,
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
func (s *SQLiteStore) UpdateLockOwnedBy(ctx context.Context, id string, owner string, ttlSeconds int, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE locks SET
		owner=?, ttl_seconds=?, acquired_at=?, expires_at=?
		WHERE id=? AND expires_at <= ?`,
		owner, ttlSeconds, now, now.Add(time.Duration(ttlSeconds)*time.Second), id, now,
	)
	if err != nil {
		return 0, fmt.Errorf("state: update lock owned by %q: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListLocks returns all locks, ordered by expires_at ascending.
func (s *SQLiteStore) ListLocks(ctx context.Context) ([]*Lock, error) {
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
func (s *SQLiteStore) DeleteLock(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM locks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("state: delete lock %q: %w", id, err)
	}
	return nil
}

// DeleteLockByIDAndOwner deletes the lock identified by id but only when it
// is still owned by owner. Ownership check and delete happen inside a single
// statement so a concurrent ForceAcquire takeover (which reuses the same row
// id with a new owner) can never be deleted by the stale owner. It returns
// false when no row matched (lock taken over by another owner or already
// gone).
func (s *SQLiteStore) DeleteLockByIDAndOwner(ctx context.Context, id string, owner string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM locks WHERE id = ? AND owner = ?`, id, owner)
	if err != nil {
		return false, fmt.Errorf("state: delete lock %q owned by %q: %w", id, owner, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteExpiredLocks removes all locks whose expires_at is before the given
// timestamp and returns the number of rows removed.
func (s *SQLiteStore) DeleteExpiredLocks(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM locks WHERE expires_at < ?`, now)
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
func (s *SQLiteStore) CreateCredential(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("state: create credential: nil credential")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO credentials
		(id, name, type, encrypted_data, created_at, rotated_at, tags)
		VALUES (?,?,?,?,?,?,?)`,
		cred.ID, cred.Name, cred.Type, cred.EncryptedData, cred.CreatedAt, cred.RotatedAt, cred.Tags,
	)
	if err != nil {
		return fmt.Errorf("state: create credential: %w", err)
	}
	return nil
}

// GetCredential returns the credential with the given id, or (nil, nil) if not found.
func (s *SQLiteStore) GetCredential(ctx context.Context, id string) (*Credential, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, name, type, encrypted_data, created_at, rotated_at, tags
		FROM credentials WHERE id = ?`, id)
	c := &Credential{}
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.EncryptedData, &c.CreatedAt, &c.RotatedAt, &c.Tags)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get credential %q: %w", id, err)
	}
	return c, nil
}

// GetCredentialByName returns the credential with the given unique name, or (nil, nil).
func (s *SQLiteStore) GetCredentialByName(ctx context.Context, name string) (*Credential, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, name, type, encrypted_data, created_at, rotated_at, tags
		FROM credentials WHERE name = ?`, name)
	c := &Credential{}
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.EncryptedData, &c.CreatedAt, &c.RotatedAt, &c.Tags)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: get credential by name %q: %w", name, err)
	}
	return c, nil
}

// UpdateCredential overwrites all mutable columns of an existing credential.
func (s *SQLiteStore) UpdateCredential(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("state: update credential: nil credential")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE credentials SET
		name=?, type=?, encrypted_data=?, created_at=?, rotated_at=?, tags=?
		WHERE id=?`,
		cred.Name, cred.Type, cred.EncryptedData, cred.CreatedAt, cred.RotatedAt, cred.Tags, cred.ID,
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
func (s *SQLiteStore) ListCredentials(ctx context.Context) ([]*Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, name, type, encrypted_data, created_at, rotated_at, tags
		FROM credentials ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("state: list credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Credential
	for rows.Next() {
		c := &Credential{}
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &c.EncryptedData, &c.CreatedAt, &c.RotatedAt, &c.Tags); err != nil {
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
func (s *SQLiteStore) DeleteCredential(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM credentials WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("state: delete credential %q: %w", id, err)
	}
	return nil
}

// =========================================================================
// Dispatch assignment CRUD
// =========================================================================

// CreateAssignment inserts a new run_assignment row.
func (s *SQLiteStore) CreateAssignment(ctx context.Context, a *Assignment) error {
	if a == nil {
		return fmt.Errorf("state: create assignment: nil assignment")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_assignment
		(run_id, owner_node, epoch, state, result, assigned_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`,
		a.RunID, a.OwnerNode, a.Epoch, a.State, a.Result, now, now,
	)
	if err != nil {
		return fmt.Errorf("state: create assignment for %q: %w", a.RunID, err)
	}
	return nil
}

// GetAssignment returns the assignment for runID, or (nil, nil) if none.
func (s *SQLiteStore) GetAssignment(ctx context.Context, runID string) (*Assignment, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		run_id, owner_node, epoch, state, result, assigned_at, updated_at
		FROM run_assignment WHERE run_id = ?`, runID)
	a := &Assignment{}
	if err := row.Scan(&a.RunID, &a.OwnerNode, &a.Epoch, &a.State, &a.Result, &a.AssignedAt, &a.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: get assignment %q: %w", runID, err)
	}
	return a, nil
}

// UpdateAssignmentStateIf is a compare-and-set on (run_id, epoch, state): it
// advances expected→next only while the row still matches (runID, epoch,
// expected). It reports (true, nil) when applied and (false, nil) when the row
// is missing or no longer matches (a concurrent actor won the race).
func (s *SQLiteStore) UpdateAssignmentStateIf(ctx context.Context, runID string, epoch int64, expected, next string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE run_assignment
		SET state=?, result=CASE WHEN ?='done' THEN COALESCE(NULLIF(result,''), ?) ELSE result END, updated_at=?
		WHERE run_id=? AND epoch=? AND state=?`,
		next, next, next, time.Now().UTC(), runID, epoch, expected,
	)
	if err != nil {
		return false, fmt.Errorf("state: update assignment %q: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("state: update assignment %q rows: %w", runID, err)
	}
	return n > 0, nil
}

// Reassign bumps the epoch and resets state to pending for a run, giving it to
// a fresh worker. It succeeds only when the existing row matches (runID,
// prevEpoch) — a stale scheduler that no longer owns the assignment is ignored.
func (s *SQLiteStore) Reassign(ctx context.Context, runID string, prevEpoch int64, newNode string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE run_assignment
		SET owner_node=?, epoch=epoch+1, state=?, result='', updated_at=?
		WHERE run_id=? AND epoch=?`,
		newNode, AssignStatePending, time.Now().UTC(), runID, prevEpoch,
	)
	if err != nil {
		return false, fmt.Errorf("state: reassign %q: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("state: reassign %q rows: %w", runID, err)
	}
	return n > 0, nil
}

// ListAssignments returns assignments matching the filter, ordered by
// assigned_at ascending. The ExcludeStates clause lists assignments whose
// state is NOT in the given set (i.e. "active" assignments).
func (s *SQLiteStore) ListAssignments(ctx context.Context, filter AssignmentFilter) ([]*Assignment, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = ?")
		args = append(args, filter.RunID)
	}
	if filter.OwnerNode != "" {
		clauses = append(clauses, "owner_node = ?")
		args = append(args, filter.OwnerNode)
	}
	if filter.State != "" {
		clauses = append(clauses, "state = ?")
		args = append(args, filter.State)
	}
	if len(filter.ExcludeStates) > 0 {
		placeholders := make([]string, len(filter.ExcludeStates))
		for i := range filter.ExcludeStates {
			placeholders[i] = "?"
			args = append(args, filter.ExcludeStates[i])
		}
		clauses = append(clauses, "state NOT IN ("+strings.Join(placeholders, ",")+")") // #nosec G202 -- fragments static, values bound
	}

	q := `SELECT run_id, owner_node, epoch, state, result, assigned_at, updated_at
		FROM run_assignment`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ") // #nosec G202 -- clause fragments are static; all values bind via placeholders
	}
	q += " ORDER BY assigned_at ASC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list assignments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Assignment
	for rows.Next() {
		a := &Assignment{}
		if err := rows.Scan(&a.RunID, &a.OwnerNode, &a.Epoch, &a.State, &a.Result, &a.AssignedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("state: list assignments scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list assignments rows: %w", err)
	}
	return out, nil
}

// DeleteAssignment removes the assignment for a run. Idempotent.
func (s *SQLiteStore) DeleteAssignment(ctx context.Context, runID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM run_assignment WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("state: delete assignment %q: %w", runID, err)
	}
	return nil
}

// SetAssignmentResult writes the terminal result onto an assignment row.
func (s *SQLiteStore) SetAssignmentResult(ctx context.Context, runID string, epoch int64, result string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE run_assignment SET result=?, updated_at=? WHERE run_id=? AND epoch=?`,
		result, time.Now().UTC(), runID, epoch)
	if err != nil {
		return fmt.Errorf("state: set result for %q: %w", runID, err)
	}
	return nil
}

// isMissingTableError reports whether err is a "no such table" failure from
// the SQLite driver. The cluster_nodes and run_assignment tables exist only
// in PostgreSQL; single-node SQLite must treat their absence as empty.
func isMissingTableError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// ListClusterNodes returns every registered cluster node, ordered by ID.
// Single-node (SQLite) deployments have no cluster_nodes table — return
// (nil, nil) so callers uniformly treat "no rows" as an empty cluster.
func (s *SQLiteStore) ListClusterNodes(ctx context.Context) ([]ClusterNode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, address, role, status, last_heartbeat, joined_at FROM cluster_nodes ORDER BY id`)
	if err != nil {
		// The cluster_nodes table is PostgreSQL-only. Treat a missing table
		// as an empty cluster rather than an error so the same Store
		// interface works for both backends.
		if isMissingTableError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: list cluster nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var nodes []ClusterNode
	for rows.Next() {
		var n ClusterNode
		if err := rows.Scan(&n.ID, &n.Address, &n.Role, &n.Status, &n.LastHeartbeat, &n.JoinedAt); err != nil {
			return nil, fmt.Errorf("state: list cluster nodes scan: %w", err)
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list cluster nodes rows: %w", err)
	}
	return nodes, nil
}

// AssignmentSummary aggregates run_assignment rows across both backends.
func (s *SQLiteStore) AssignmentSummary(ctx context.Context) (*AssignmentSummary, error) {
	summary := &AssignmentSummary{Counts: map[string]int{}, NodeLoad: map[string]int{}}

	rows, err := s.db.QueryContext(ctx, `SELECT state, owner_node, COUNT(*) FROM run_assignment GROUP BY state, owner_node`)
	if err != nil {
		if isMissingTableError(err) {
			return summary, nil
		}
		return nil, fmt.Errorf("state: assignment summary: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var state, node string
		var count int
		if err := rows.Scan(&state, &node, &count); err != nil {
			return nil, fmt.Errorf("state: assignment summary scan: %w", err)
		}
		summary.Counts[state] += count
		if state == AssignStatePending || state == AssignmentStateExecuting {
			summary.NodeLoad[node] += count
			summary.TotalActive += count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: assignment summary rows: %w", err)
	}
	return summary, nil
}

// =========================================================================

// =========================================================================
// Audit CRUD
// =========================================================================

// CreateAudit inserts a new audit log entry.
func (s *SQLiteStore) CreateAudit(ctx context.Context, audit *Audit) error {
	if audit == nil {
		return fmt.Errorf("state: create audit: nil audit")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit
		(id, run_id, action, actor, target, result, timestamp)
		VALUES (?,?,?,?,?,?,?)`,
		audit.ID, audit.RunID, audit.Action, audit.Actor, audit.Target, audit.Result, audit.Timestamp,
	)
	if err != nil {
		return fmt.Errorf("state: create audit: %w", err)
	}
	return nil
}

// GetAudit returns the audit entry with the given id, or (nil, nil) if not found.
func (s *SQLiteStore) GetAudit(ctx context.Context, id string) (*Audit, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, run_id, action, actor, target, result, timestamp
		FROM audit WHERE id = ?`, id)
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
func (s *SQLiteStore) ListAudits(ctx context.Context, filter AuditFilter) ([]*Audit, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = ?")
		args = append(args, filter.RunID)
	}
	if filter.Action != "" {
		clauses = append(clauses, "action = ?")
		args = append(args, filter.Action)
	}
	if filter.Actor != "" {
		clauses = append(clauses, "actor = ?")
		args = append(args, filter.Actor)
	}

	q := `SELECT id, run_id, action, actor, target, result, timestamp FROM audit`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ") // #nosec G202 -- clause fragments are static; all values bind via placeholders
	}
	q += " ORDER BY timestamp DESC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	if filter.Offset > 0 {
		q += " OFFSET ?"
		args = append(args, filter.Offset)
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
