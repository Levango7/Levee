package state

import (
	"context"
	"fmt"
	"time"
)

// trend window. The SQL is an indexed range scan plus a LIMIT; the bucketing
// happens in Go (see trend.go for why it cannot happen in SQL on SQLite).
//
// The LIMIT is deliberately one MORE than the filter's cap: hitting the cap is
// detectable as "returned exactly Limit rows", which is what lets the gateway
// report truncation instead of drawing a chart that silently stops early.
const trendWindowSQL = `SELECT status, created_at FROM runs WHERE created_at >= ?`

func (s *SQLiteStore) runTrendQuery(ctx context.Context, f RunTrendFilter, start time.Time) ([]*Run, bool, error) {
	q := trendWindowSQL
	args := []any{start}
	if f.Env != "" {
		// The change's environment is carried in the incident_id column —
		// envOf() in the change service reads the same field. Renaming that
		// column is a schema migration of its own and out of scope here.
		q += ` AND incident_id = ?`
		args = append(args, f.Env)
	}
	if f.TenantID != "" {
		q += ` AND tenant_id = ?`
		args = append(args, f.TenantID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultRunTrendLimit
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("state: run trend: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Run
	for rows.Next() {
		var status string
		var createdAt time.Time
		if err := rows.Scan(&status, &createdAt); err != nil {
			return nil, false, fmt.Errorf("state: run trend scan: %w", err)
		}
		out = append(out, &Run{Status: status, CreatedAt: createdAt})
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("state: run trend rows: %w", err)
	}
	truncated := f.Limit > 0 && len(out) > f.Limit
	if truncated {
		out = out[:f.Limit]
	}
	return out, truncated, nil
}

// RunTrend returns the change-outcome trend for the window described by f,
// bucketed by the caller's local day. The second return value reports whether
// the row scan hit the cap — the caller is expected to say so rather than draw
// a chart that stops early.
func (s *SQLiteStore) RunTrend(ctx context.Context, f RunTrendFilter) ([]RunTrendBucket, bool, error) {
	return buildTrend(ctx, f, time.Now(), s.runTrendQuery)
}
