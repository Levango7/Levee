package state

import (
	"context"
	"fmt"
	"strings"
)

// RunCounter is implemented by stores that can count matching runs without
// loading their rows. It is deliberately optional: Store keeps its existing
// contract, while production SQLite/PostgreSQL stores provide the native path
// and wrappers can fall back to batched ListRuns counting.
type RunCounter interface {
	CountRuns(ctx context.Context, filter RunFilter) (int, error)
}

// CountRuns returns the total number of rows matching filter. Limit and Offset
// are pagination controls and therefore do not affect the count.
func (s *SQLiteStore) CountRuns(ctx context.Context, filter RunFilter) (int, error) {
	var clauses []string
	var args []any
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
	if filter.TenantID != "" {
		clauses = append(clauses, "tenant_id = ?")
		args = append(args, filter.TenantID)
	}
	q := "SELECT COUNT(*) FROM runs"
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ") // #nosec G202 -- static fragments; every value is bound below
	}
	var count int
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("state: count runs: %w", err)
	}
	return count, nil
}

// CountRuns returns the total number of rows matching filter. Limit and Offset
// are pagination controls and therefore do not affect the count.
func (s *PGStore) CountRuns(ctx context.Context, filter RunFilter) (int, error) {
	var clauses []string
	var args []any
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
	if filter.TenantID != "" {
		clauses = append(clauses, "tenant_id = $%d")
		args = append(args, filter.TenantID)
	}
	q := "SELECT COUNT(*) FROM runs"
	if len(clauses) > 0 {
		q += " WHERE " + pgJoinPlaceholders(clauses)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("state: count runs: %w", err)
	}
	return count, nil
}
