// End-to-end tests for `levee audit` (E-2): verify/list/show/export over a
// hash chain built with the real HashChainBuilder, plus a tamper probe that
// mutates a stored trace and must be caught by the chain (exit=6).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // same pure-Go driver the state store uses

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/state"
)

// seedTraces writes n traces for runID and returns them in creation order.
// traces carry an FK on run_id, so the run row is seeded first.
func seedTraces(t *testing.T, e *cliEnv, runID string, n int) []*state.Trace {
	t.Helper()
	e.seedRun(t, runID, "completed")
	store := e.open(t)
	defer func() { _ = store.Close() }()
	base := time.Now().UTC().Truncate(time.Second)
	out := make([]*state.Trace, 0, n)
	for i := range n {
		tr := &state.Trace{
			ID:        runID + "-tr" + string(rune('a'+i)),
			RunID:     runID,
			Event:     "step.done",
			Actor:     "cli-user",
			Detail:    `{"step":` + string(rune('0'+i)) + `}`,
			Timestamp: base.Add(time.Duration(i) * time.Second),
		}
		require.NoError(t, store.CreateTrace(context.Background(), tr))
		out = append(out, tr)
	}
	return out
}

// buildChain computes the WORM hashes for the run's traces.
func buildChain(t *testing.T, e *cliEnv, runID string) int {
	t.Helper()
	store := e.open(t)
	defer func() { _ = store.Close() }()
	b, err := audit.NewHashChainBuilder(store)
	require.NoError(t, err)
	count, tail, err := b.Build(context.Background(), runID)
	require.NoError(t, err)
	assert.NotEmpty(t, tail)
	return count
}

func TestAuditE2E_VerifyListShowExport(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	seedTraces(t, e, "chain1", 3)
	require.Equal(t, 3, buildChain(t, e, "chain1"))

	// verify: human, quiet, JSON.
	out := mustRun(t, append([]string{"audit", "verify", "chain1"}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))
	quiet := mustRun(t, append([]string{"--quiet", "audit", "verify", "chain1"}, cfg...)...)
	assert.Contains(t, quiet, "chain1")
	res, _, err := freshJSON(t, append([]string{"audit", "verify", "chain1", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, data["valid"])
	assert.Equal(t, float64(3), data["count"])

	// list: human table, quiet ids, JSON rows.
	out = mustRun(t, append([]string{"audit", "list", "chain1"}, cfg...)...)
	assert.Contains(t, out, "step.done")
	quiet = mustRun(t, append([]string{"--quiet", "audit", "list", "chain1"}, cfg...)...)
	assert.Contains(t, quiet, "chain1-tra")
	res, _, err = freshJSON(t, append([]string{"audit", "list", "chain1", "--json"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)

	// show a single trace by id (ids contain spaces only as separator
	// artifacts — the real id has none).
	show := mustRun(t, append([]string{"audit", "show", "chain1-tra"}, cfg...)...)
	assert.Contains(t, show, "step.done")
	err = runErr(t, append([]string{"audit", "show", "nope"}, cfg...)...)
	assert.Error(t, err)

	// export: json parses; csv has a header; unknown format falls back.
	out = mustRun(t, append([]string{"audit", "export", "chain1"}, cfg...)...)
	assert.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), new(any)))
	out = mustRun(t, append([]string{"audit", "export", "chain1", "--format", "csv"}, cfg...)...)
	assert.Contains(t, strings.ToLower(out), "id")
	out = mustRun(t, append([]string{"audit", "export", "chain1", "--format", "yaml"}, cfg...)...)
	assert.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), new(any)),
		"unknown format falls back to JSON export")
}

func TestAuditE2E_TamperDetected(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	traces := seedTraces(t, e, "chain2", 3)
	require.Equal(t, 3, buildChain(t, e, "chain2"))

	// Overwrite the middle trace's payload while keeping its hashes. The
	// store API refuses this (WORM guard), so tamper at the file level —
	// the threat model the chain exists for.
	tamperTraceDetail(t, e, traces[1].ID, `{"step":"forged"}`)

	err := runErr(t, append([]string{"audit", "verify", "chain2"}, cfg...)...)
	assert.Contains(t, err.Error(), "tampered [exit=6]")

	// JSON path fails with the same error (envelope is printed to stdout).
	err = runErr(t, append([]string{"audit", "verify", "chain2", "--json"}, cfg...)...)
	assert.Contains(t, err.Error(), "[exit=6]")
}

// tamperTraceDetail rewrites a trace's detail column bypassing BOTH the store
// API (WORM error path) and the schema's worm_prevent_trace_update trigger —
// i.e. the disk-level attacker the hash chain exists to catch.
func tamperTraceDetail(t *testing.T, e *cliEnv, traceID, detail string) {
	t.Helper()
	db, err := sql.Open("sqlite", e.dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`DROP TRIGGER IF EXISTS worm_prevent_trace_update`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE trace SET detail = ? WHERE id = ?`, detail, traceID)
	require.NoError(t, err)
}
