// inventory_sqlite.go — SQLite persistence for managed target hosts and
// hierarchical inventory groups (part of state.Store).
//
// Labels are stored as a JSON object string and filtered with JSON1 path
// expressions; the PG implementation uses jsonb containment instead. Both
// expose the same AND-matching semantics through TargetFilter.

package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// upsertTargetSQL is shared shape: insert by id, or update every mutable
// column when the id already exists. The (hostname, port) UNIQUE constraint
// still fires for rows owned by a DIFFERENT id — surfaced as
// ErrDuplicateTarget.
const sqliteUpsertTarget = `INSERT INTO targets
	(id, hostname, port, channel_type, credential_ref, labels, group_id,
	 status, reachable, last_checked_at, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		hostname=excluded.hostname, port=excluded.port,
		channel_type=excluded.channel_type, credential_ref=excluded.credential_ref,
		labels=excluded.labels, group_id=excluded.group_id,
		status=excluded.status, reachable=excluded.reachable,
		last_checked_at=excluded.last_checked_at`

func encodeLabels(labels map[string]string) (string, error) {
	if labels == nil {
		labels = map[string]string{}
	}
	b, err := json.Marshal(labels)
	if err != nil {
		return "", fmt.Errorf("state: encode labels: %w", err)
	}
	return string(b), nil
}

func decodeLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	if s == "" || s == "{}" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("state: decode labels %q: %w", s, err)
	}
	return out, nil
}

func (s *SQLiteStore) UpsertInventoryGroup(ctx context.Context, g *InventoryGroup) error {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO inventory_groups (id, name, parent_id, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, parent_id=excluded.parent_id`,
		g.ID, g.Name, g.ParentID, g.CreatedAt)
	if err != nil {
		return fmt.Errorf("state: upsert inventory group: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetInventoryGroup(ctx context.Context, id string) (*InventoryGroup, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, parent_id, created_at FROM inventory_groups WHERE id = ?`, id)
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

func (s *SQLiteStore) GetInventoryGroupByName(ctx context.Context, name string) (*InventoryGroup, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, parent_id, created_at FROM inventory_groups WHERE name = ?`, name)
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

func (s *SQLiteStore) ListInventoryGroups(ctx context.Context) ([]*InventoryGroup, error) {
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

func (s *SQLiteStore) DeleteInventoryGroup(ctx context.Context, id string) error {
	// Targets referencing the group keep their rows: FK is ON DELETE SET NULL.
	_, err := s.db.ExecContext(ctx, `DELETE FROM inventory_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("state: delete inventory group: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UpsertTarget(ctx context.Context, t *Target) error {
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
	_, err = s.db.ExecContext(ctx, sqliteUpsertTarget,
		t.ID, t.Hostname, t.Port, t.ChannelType, t.CredentialRef, labels,
		groupID, t.Status, t.Reachable, lastChecked, t.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: targets.hostname") {
			return fmt.Errorf("state: upsert target %q: %w (%s:%d)", t.ID, ErrDuplicateTarget, t.Hostname, t.Port)
		}
		return fmt.Errorf("state: upsert target: %w", err)
	}
	return nil
}

func scanTarget(row interface{ Scan(...any) error }) (*Target, error) {
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

const sqliteTargetColumns = `id, hostname, port, channel_type, credential_ref,
	labels, group_id, status, reachable, last_checked_at, created_at`

func (s *SQLiteStore) GetTarget(ctx context.Context, id string) (*Target, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sqliteTargetColumns+` FROM targets WHERE id = ?`, id)
	t, err := scanTarget(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("state: get target: %w", err)
	}
	return t, nil
}

func (s *SQLiteStore) FindTargetByAddress(ctx context.Context, hostname string, port int) (*Target, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sqliteTargetColumns+` FROM targets WHERE hostname = ? AND port = ?`, hostname, port)
	t, err := scanTarget(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("state: find target by address: %w", err)
	}
	return t, nil
}

func (s *SQLiteStore) ListTargets(ctx context.Context, filter TargetFilter) ([]*Target, error) {
	var clauses []string
	var args []any
	if filter.GroupID != "" {
		clauses = append(clauses, "group_id = ?")
		args = append(args, filter.GroupID)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, filter.Status)
	}
	for k, v := range filter.Labels {
		clauses = append(clauses, fmt.Sprintf("json_extract(labels, '$.%s') = ?", k))
		args = append(args, v)
	}

	q := `SELECT ` + sqliteTargetColumns + ` FROM targets`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY hostname, port"
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
		return nil, fmt.Errorf("state: list targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, fmt.Errorf("state: list targets scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpdateTargetStatus(ctx context.Context, id string, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE targets SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("state: update target status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: update target status %q: not found", id)
	}
	return nil
}

func (s *SQLiteStore) SetTargetReachability(ctx context.Context, id string, reachable bool, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE targets SET reachable = ?, last_checked_at = ? WHERE id = ?`,
		reachable, at, id)
	if err != nil {
		return fmt.Errorf("state: set target reachability: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("state: set reachability %q: not found", id)
	}
	return nil
}

func (s *SQLiteStore) DeleteTarget(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM targets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("state: delete target: %w", err)
	}
	return nil
}

func (s *SQLiteStore) CountTargetsInGroup(ctx context.Context, groupID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM targets WHERE group_id = ?`, groupID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("state: count targets in group: %w", err)
	}
	return n, nil
}
