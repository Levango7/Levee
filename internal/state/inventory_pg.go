// inventory_pg.go — PostgreSQL persistence for managed target hosts and
// hierarchical inventory groups (part of state.Store).
//
// Labels are a jsonb column; label filters use containment (@>) with the
// AND semantics shared with the SQLite JSON1 implementation.

package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (s *PGStore) UpsertInventoryGroup(ctx context.Context, g *InventoryGroup) error {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO inventory_groups (id, name, parent_id, created_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT(id) DO UPDATE SET name = EXCLUDED.name, parent_id = EXCLUDED.parent_id`,
		g.ID, g.Name, g.ParentID, g.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: upsert inventory group: %w", err)
	}
	return nil
}

func (s *PGStore) GetInventoryGroup(ctx context.Context, id string) (*InventoryGroup, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, parent_id, created_at FROM inventory_groups WHERE id = $1`, id)
	g := &InventoryGroup{}
	var parent sql.NullString
	if err := row.Scan(&g.ID, &g.Name, &parent, &g.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("state: get inventory group: %w", err)
	}
	g.ParentID = parent.String
	return g, nil
}

func (s *PGStore) GetInventoryGroupByName(ctx context.Context, name string) (*InventoryGroup, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, parent_id, created_at FROM inventory_groups WHERE name = $1`, name)
	g := &InventoryGroup{}
	var parent sql.NullString
	if err := row.Scan(&g.ID, &g.Name, &parent, &g.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("state: get inventory group by name: %w", err)
	}
	g.ParentID = parent.String
	return g, nil
}

func (s *PGStore) ListInventoryGroups(ctx context.Context) ([]*InventoryGroup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, parent_id, created_at FROM inventory_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("state: list inventory groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*InventoryGroup
	for rows.Next() {
		g := &InventoryGroup{}
		var parent sql.NullString
		if err := rows.Scan(&g.ID, &g.Name, &parent, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("state: list inventory groups scan: %w", err)
		}
		g.ParentID = parent.String
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *PGStore) DeleteInventoryGroup(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM inventory_groups WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("state: delete inventory group: %w", err)
	}
	return nil
}

const pgUpsertTarget = `INSERT INTO targets
	(id, hostname, port, channel_type, credential_ref, labels, group_id,
	 status, reachable, last_checked_at, created_at)
	VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11)
	ON CONFLICT(id) DO UPDATE SET
		hostname = EXCLUDED.hostname, port = EXCLUDED.port,
		channel_type = EXCLUDED.channel_type, credential_ref = EXCLUDED.credential_ref,
		labels = EXCLUDED.labels, group_id = EXCLUDED.group_id,
		status = EXCLUDED.status, reachable = EXCLUDED.reachable,
		last_checked_at = EXCLUDED.last_checked_at`

func (s *PGStore) UpsertTarget(ctx context.Context, t *Target) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	labels, err := encodeLabels(t.Labels)
	if err != nil {
		return err
	}
	var lastChecked any
	if t.LastCheckedAt != nil {
		lastChecked = *t.LastCheckedAt
	}
	// Empty GroupID must bind as SQL NULL, not '' — the FK would reject ''.
	var groupID any
	if t.GroupID != "" {
		groupID = t.GroupID
	}
	_, err = s.db.ExecContext(ctx, pgUpsertTarget,
		t.ID, t.Hostname, t.Port, t.ChannelType, t.CredentialRef, labels,
		groupID, t.Status, t.Reachable, lastChecked, t.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "targets_hostname_port_key") ||
			strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
			return fmt.Errorf("state: upsert target %q: %w (%s:%d)", t.ID, ErrDuplicateTarget, t.Hostname, t.Port)
		}
		return fmt.Errorf("state: upsert target: %w", err)
	}
	return nil
}

func scanTargetPG(row interface{ Scan(...any) error }) (*Target, error) {
	t := &Target{}
	var labels string
	var groupID sql.NullString
	var lastChecked sql.NullTime
	if err := row.Scan(&t.ID, &t.Hostname, &t.Port, &t.ChannelType, &t.CredentialRef,
		&labels, &groupID, &t.Status, &t.Reachable, &lastChecked, &t.CreatedAt); err != nil {
		return nil, err
	}
	var derr error
	if t.Labels, derr = decodeLabels(labels); derr != nil {
		return nil, derr
	}
	t.GroupID = groupID.String
	if lastChecked.Valid {
		v := lastChecked.Time
		t.LastCheckedAt = &v
	}
	return t, nil
}

const pgTargetColumns = `id, hostname, port, channel_type, credential_ref,
	labels::text, group_id, status, reachable, last_checked_at, created_at`

func (s *PGStore) GetTarget(ctx context.Context, id string) (*Target, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+pgTargetColumns+` FROM targets WHERE id = $1`, id)
	t, err := scanTargetPG(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("state: get target: %w", err)
	}
	return t, nil
}

func (s *PGStore) FindTargetByAddress(ctx context.Context, hostname string, port int) (*Target, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+pgTargetColumns+` FROM targets WHERE hostname = $1 AND port = $2`, hostname, port)
	t, err := scanTargetPG(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("state: find target by address: %w", err)
	}
	return t, nil
}

func (s *PGStore) ListTargets(ctx context.Context, filter TargetFilter) ([]*Target, error) {
	var clauses []string
	var args []any
	argN := 0
	nextArg := func(v any) string {
		argN++
		args = append(args, v)
		return fmt.Sprintf("$%d", argN)
	}
	if filter.GroupID != "" {
		clauses = append(clauses, "group_id = "+nextArg(filter.GroupID))
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = "+nextArg(filter.Status))
	}
	for k, v := range filter.Labels {
		pair, _ := json.Marshal(map[string]string{k: v})
		clauses = append(clauses, "labels @> "+nextArg(string(pair))+"::jsonb")
	}

	q := `SELECT ` + pgTargetColumns + ` FROM targets`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY hostname, port"
	if filter.Limit > 0 {
		q += " LIMIT " + nextArg(filter.Limit)
	}
	if filter.Offset > 0 {
		q += " OFFSET " + nextArg(filter.Offset)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Target
	for rows.Next() {
		t, err := scanTargetPG(rows)
		if err != nil {
			return nil, fmt.Errorf("state: list targets scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PGStore) UpdateTargetStatus(ctx context.Context, id string, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE targets SET status = $1 WHERE id = $2`, status, id)
	if err != nil {
		return fmt.Errorf("state: update target status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: update target status %q: not found", id)
	}
	return nil
}

func (s *PGStore) SetTargetReachability(ctx context.Context, id string, reachable bool, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE targets SET reachable = $1, last_checked_at = $2 WHERE id = $3`,
		reachable, at, id)
	if err != nil {
		return fmt.Errorf("state: set target reachability: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: set reachability %q: not found", id)
	}
	return nil
}

func (s *PGStore) DeleteTarget(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM targets WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("state: delete target: %w", err)
	}
	return nil
}

func (s *PGStore) CountTargetsInGroup(ctx context.Context, groupID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM targets WHERE group_id = $1`, groupID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("state: count targets in group: %w", err)
	}
	return n, nil
}
