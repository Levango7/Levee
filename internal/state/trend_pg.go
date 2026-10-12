package state

import (
	"context"
	"fmt"
	"time"
)

// PG twin of trend_sqlite.go. Same window, same cap, same truncation signal —
// the day bucketing lives in BucketRuns so the two backends cannot disagree
// about where a day starts.
func (s *PGStore) runTrendQuery(ctx context.Context, f RunTrendFilter, start time.Time) ([]*Run, bool, error) {
	// Placeholders are numbered by POSITION in args, and every value binds: the
	// only thing ever concatenated into the statement is a literal "$N"
	// computed from len(args). The two lines that do it carry the same
	// justification the rest of the package uses (see pgstore.go's LIMIT/OFFSET
	// builders).
	q := `SELECT status, created_at FROM runs WHERE created_at >= $1`
	args := []any{start}
	if f.Env != "" {
		// incident_id carries the change's environment (envOf in the change
		// service reads the same column); it is indexed on both dialects.
		q += ` AND incident_id = $2`
		args = append(args, f.Env)
	}
	if f.TenantID != "" {
		q += fmt.Sprintf(" AND tenant_id = $%d", len(args)+1) // #nosec G202 -- placeholder index computed from len(args); the tenant value binds via args
		args = append(args, f.TenantID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultRunTrendLimit
	}
	q += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args)+1) // #nosec G202 -- placeholder index computed from len(args); the cap binds via args
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
func (s *PGStore) RunTrend(ctx context.Context, f RunTrendFilter) ([]RunTrendBucket, bool, error) {
	return buildTrend(ctx, f, time.Now(), s.runTrendQuery)
}
