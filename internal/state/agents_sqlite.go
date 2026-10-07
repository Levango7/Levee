// agents_sqlite.go — SQLite persistence for the agent registry (AgentStore).
//
// The single-node default: a daemon and a `levee agent list` in another
// process share one file, which is the whole point of persisting the registry.
// Mirrors inventory_sqlite.go; pgstore.go's note on why the two files are
// kept diffable column-by-column applies here too.

package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// sqliteUpsertAgent inserts an agent or updates the existing row by ID.
//
// registered_at and last_heartbeat are deliberately absent from the DO UPDATE
// SET list. Bindings still feed the INSERT branch, so a first registration
// records its age and, when the caller brought one along, its heartbeat —
// but a re-register can neither reset the age nor overwrite liveness that a
// heartbeat already stamped. Same shape as sqliteUpsertTarget leaving
// created_at out.
const sqliteUpsertAgent = `INSERT INTO agents
	(id, address, capabilities, status, last_heartbeat, registered_at,
	 active_tasks, completed_tasks, failed_tasks, max_concurrent)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		address=excluded.address, capabilities=excluded.capabilities,
		status=excluded.status,
		active_tasks=excluded.active_tasks,
		completed_tasks=excluded.completed_tasks,
		failed_tasks=excluded.failed_tasks,
		max_concurrent=excluded.max_concurrent`

func (s *SQLiteStore) UpsertAgent(ctx context.Context, a *Agent) error {
	if a == nil {
		return fmt.Errorf("state: upsert agent: nil agent")
	}
	if a.ID == "" {
		return fmt.Errorf("state: upsert agent: empty id")
	}
	if a.RegisteredAt.IsZero() {
		// Insert-side stamp, as UpsertInventoryGroup/UpsertTarget do for
		// CreatedAt. Never reaches an UPDATE: registered_at is not in the SET
		// list, so a re-register keeps the stored value whatever this is.
		a.RegisteredAt = time.Now().UTC()
	}
	caps, err := encodeAgentCapabilities(a.Capabilities)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, sqliteUpsertAgent,
		a.ID, a.Address, caps, a.Status, heartbeatArg(a.LastHeartbeat), a.RegisteredAt,
		a.ActiveTasks, a.CompletedTasks, a.FailedTasks, a.MaxConcurrent)
	if err != nil {
		return fmt.Errorf("state: upsert agent %q: %w", a.ID, err)
	}
	return nil
}

func (s *SQLiteStore) TouchAgentHeartbeat(ctx context.Context, id string, status string,
	activeTasks int, completedTasks, failedTasks int64, at time.Time) (bool, error) {
	// UPDATE, not upsert: a heartbeat for an unknown agent is the caller's
	// signal to re-register, so this must not resurrect a deleted record.
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET status = ?, active_tasks = ?, completed_tasks = ?,
			failed_tasks = ?, last_heartbeat = ? WHERE id = ?`,
		status, activeTasks, completedTasks, failedTasks, heartbeatArg(at), id)
	if err != nil {
		return false, fmt.Errorf("state: touch agent heartbeat %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("state: touch agent heartbeat %q: rows affected: %w", id, err)
	}
	return n > 0, nil
}

func (s *SQLiteStore) GetAgent(ctx context.Context, id string) (*Agent, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE id = ?`, id)
	a, err := scanAgent(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: get agent %q: %w", id, err)
	}
	return a, nil
}

func (s *SQLiteStore) ListAgents(ctx context.Context) ([]*Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+agentColumns+` FROM agents ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("state: list agents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("state: list agents scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteAgent(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("state: delete agent %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("state: delete agent %q: rows affected: %w", id, err)
	}
	return n > 0, nil
}
