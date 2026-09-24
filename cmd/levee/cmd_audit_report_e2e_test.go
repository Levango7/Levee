// End-to-end tests for `levee audit report` (compliance deliverable): the
// command must aggregate runs, approvals, chain verdicts and rollback
// records over a time window and emit a self-contained HTML document.
package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// seedApprovalWithStatus writes an approval row for runID.
func seedApprovalWithStatus(t *testing.T, e *cliEnv, runID, level, approver, status string) {
	t.Helper()
	store := e.open(t)
	defer func() { _ = store.Close() }()
	now := time.Now().UTC()
	timeoutAt := now.Add(time.Hour)
	err := store.CreateApproval(context.Background(), &state.Approval{
		ID:        runID + "-apr-1",
		RunID:     runID,
		Level:     level,
		Approver:  approver,
		Status:    status,
		Comment:   "approved in window",
		TimeoutAt: &timeoutAt,
		ActedAt:   &now,
	})
	require.NoError(t, err)
}

// seedRollbackAudit writes an audit row whose action contains "rollback".
func seedRollbackAudit(t *testing.T, e *cliEnv, runID string) {
	t.Helper()
	store := e.open(t)
	defer func() { _ = store.Close() }()
	err := store.CreateAudit(context.Background(), &state.Audit{
		ID:        runID + "-aud-1",
		RunID:     runID,
		Action:    "rollback",
		Actor:     "system",
		Target:    "batch:1",
		Result:    "success",
		Timestamp: time.Now().UTC(),
	})
	require.NoError(t, err)
}

func TestAuditReportE2E_HTMLAggregatesWindow(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	// Run 1: completed, chained, approved, rolled back.
	seedTraces(t, e, "rep1", 2)
	require.Equal(t, 2, buildChain(t, e, "rep1"))
	seedApprovalWithStatus(t, e, "rep1", "high", "alice", "approved")
	seedRollbackAudit(t, e, "rep1")

	// Run 2: failed, chained (broken via direct mutation later in this
	// test we keep it intact — the tamper case is covered by verify E2E).
	seedTraces(t, e, "rep2", 1)
	require.Equal(t, 1, buildChain(t, e, "rep2"))
	seedApprovalWithStatus(t, e, "rep2", "standard", "bob", "rejected")

	// Run 3: failed, no traces (chain verdict empty), outside window.
	e.seedRun(t, "rep3", "failed")

	// Default window (no --since): everything created now, all included.
	out := mustRun(t, append([]string{"audit", "report"}, cfg...)...)
	lower := strings.ToLower(out)

	// Self-contained HTML document markers.
	assert.Contains(t, lower, "<!doctype html>")
	assert.Contains(t, lower, "levee compliance report")

	// Summary counters.
	assert.Contains(t, out, "Total change runs")
	assert.Contains(t, out, "Hash chains verified intact")

	// Both windowed runs with their data appear.
	assert.Contains(t, out, "rep1")
	assert.Contains(t, out, "rep2")
	assert.Contains(t, out, "rep3")
	assert.Contains(t, out, "alice")
	assert.Contains(t, out, "bob")
	assert.Contains(t, out, "rollback")
	assert.Contains(t, out, "intact")

	// The output has no external resources (self-contained artifact).
	assert.NotContains(t, lower, "src=")
	assert.NotContains(t, lower, "link rel=")

	// The store interface is untouched: ListRuns has no timestamp filter,
	// so window narrowing happens in-memory; a --since in the future
	// excludes everything created "now".
	future := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")
	out = mustRun(t, append([]string{"audit", "report", "--since", future}, cfg...)...)
	assert.Contains(t, out, "No change runs in the selected window")

	// --output writes the file.
	path := t.TempDir() + "\\report.html"
	_ = mustRun(t, append([]string{"audit", "report", "--output", path}, cfg...)...)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, strings.ToLower(string(content)), "levee compliance report")
}

func TestAuditReportE2E_TimeFlagsValidate(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	// Malformed --since.
	err := runErr(t, append([]string{"audit", "report", "--since", "yesterday-ish"}, cfg...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid time")

	// --until before --since.
	err = runErr(t, append([]string{"audit", "report",
		"--since", "2026-09-01", "--until", "2026-08-01"}, cfg...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be after")
}
