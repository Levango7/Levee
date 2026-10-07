// agents_pg.go — PostgreSQL persistence for the agent registry (AgentStore).
//
// The cluster-mode backend. A 1:1 mirror of agents_sqlite.go: same columns,
// same column order, same two write rules (registered_at keeps its original
// value, last_heartbeat belongs to the heartbeats). Only the placeholder
// markers and the upsert clause's casing differ.

package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// pgUpsertAgent is agents_sqlite.go's sqliteUpsertAgent in PostgreSQL syntax.
// registered_at and last_heartbeat are absent from the DO UPDATE SET list for
// the same reason: a re-register must not reset the agent's age or overwrite
// the liveness a heartbeat already stamped.
const pgUpsertAgent = `INSERT INTO agents
	(id, address, capabilities, status, last_heartbeat, registered_at,
	 active_tasks, completed_tasks, failed_tasks, max_concurrent)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	ON CONFLICT(id) DO UPDATE SET
		address = EXCLUDED.address, capabilities = EXCLUDED.capabilities,
		status = EXCLUDED.status,
		active_tasks = EXCLUDED.active_tasks,
		completed_tasks = EXCLUDED.completed_tasks,
		failed_tasks = EXCLUDED.failed_tasks,
		max_concurrent = EXCLUDED.max_concurrent`

func (s *PGStore) UpsertAgent(ctx context.Context, a *Agent) error {
	if a == nil {
		return fmt.Errorf("state: upsert agent: nil agent")
	}
	if a.ID == "" {
		return fmt.Errorf("state: upsert agent: empty id")
	}
	if a.RegisteredAt.IsZero() {
		a.RegisteredAt = time.Now().UTC()
	}
	caps, err := encodeAgentCapabilities(a.Capabilities)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, pgUpsertAgent,
		a.ID, a.Address, caps, a.Status, heartbeatArg(a.LastHeartbeat), a.RegisteredAt,
		a.ActiveTasks, a.CompletedTasks, a.FailedTasks, a.MaxConcurrent)
	if err != nil {
		return fmt.Errorf("state: upsert agent %q: %w", a.ID, err)
	}
	return nil
}

func (s *PGStore) TouchAgentHeartbeat(ctx context.Context, id string, status string,
	activeTasks int, completedTasks, failedTasks int64, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET status = $1, active_tasks = $2, completed_tasks = $3,
			failed_tasks = $4, last_heartbeat = $5 WHERE id = $6`,
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

func (s *PGStore) GetAgent(ctx context.Context, id string) (*Agent, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE id = $1`, id)
	a, err := scanAgent(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: get agent %q: %w", id, err)
	}
	return a, nil
}

func (s *PGStore) ListAgents(ctx context.Context) ([]*Agent, error) {
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

func (s *PGStore) DeleteAgent(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("state: delete agent %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("state: delete agent %q: rows affected: %w", id, err)
	}
	return n > 0, nil
}
