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
	q := `SELECT status, created_at FROM runs WHERE created_at >= $1`
	args := []any{start}
	next := func() string {
		args = append(args, nil)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Env != "" {
		// incident_id carries the change's environment (see envOf in the
		// change service); the column is indexed on both dialects.
		q += ` AND incident_id = ` + next()
		args[len(args)-1] = f.Env
	}
	if f.TenantID != "" {
		q += ` AND tenant_id = ` + next()
		args[len(args)-1] = f.TenantID
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultRunTrendLimit
	}
	q += ` ORDER BY created_at DESC LIMIT ` + next()
	args[len(args)-1] = limit + 1

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
