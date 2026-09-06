// End-to-end tests for `levee retry` and `levee retry-host` (E-2): state gate
// (failed|paused), the 3-attempt budget counted from retry/retry_host audit
// rows, and the per-host budget keyed by Target "<run>/<host>".
package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// setRunStatus flips a seeded run's status so a fresh retry attempt passes
// the state gate after the previous attempt moved it to "running".
func setRunStatus(t *testing.T, e *cliEnv, id, status string) {
	t.Helper()
	store := e.open(t)
	defer func() { _ = store.Close() }()
	run, err := store.GetRun(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, run)
	run.Status = status
	run.UpdatedAt = time.Now().UTC()
	require.NoError(t, store.UpdateRun(context.Background(), run))
}

// seedAuditRows writes action-tagged audit rows to exhaust a retry budget.
func seedAuditRows(t *testing.T, e *cliEnv, runID, action, target string, n int) {
	t.Helper()
	store := e.open(t)
	defer func() { _ = store.Close() }()
	now := time.Now().UTC()
	for i := range n {
		require.NoError(t, store.CreateAudit(context.Background(), &state.Audit{
			ID:    fmt.Sprintf("audit-seed-%s-%s-%d", runID, target, i),
			RunID: runID, Action: action, Actor: "cli-user",
			Target: target, Result: "success", Timestamp: now,
		}))
	}
}

func TestRetryE2E_Run(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	e.seedRun(t, "r1", "failed")

	// Attempt 1 (human): run flips to running and the count increments.
	out := mustRun(t, append([]string{"retry", "r1"}, cfg...)...)
	assert.Contains(t, out, "retry #1")
	store := e.open(t)
	run, err := store.GetRun(context.Background(), "r1")
	require.NoError(t, err)
	assert.Equal(t, "running", run.Status)
	audits, err := store.ListAudits(context.Background(), state.AuditFilter{
		RunID: "r1", Action: "retry",
	})
	require.NoError(t, err)
	assert.Len(t, audits, 1)
	_ = store.Close()

	// Attempt 2 (JSON) and 3 (quiet), flipping back to failed each time.
	setRunStatus(t, e, "r1", "failed")
	res, _, err := freshJSON(t, append([]string{"retry", "r1", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(2), data["retry_count"])
	assert.Equal(t, float64(3), data["max_retries"])

	setRunStatus(t, e, "r1", "failed")
	quiet := mustRun(t, append([]string{"--quiet", "retry", "r1"}, cfg...)...)
	assert.Contains(t, quiet, "r1")

	// Budget exhausted: 3/3 -> exit=5.
	setRunStatus(t, e, "r1", "failed")
	err = runErr(t, append([]string{"retry", "r1"}, cfg...)...)
	assert.Contains(t, err.Error(), "retry limit (3/3) [exit=5]")

	// State gate and not-found mapping.
	e.seedRun(t, "r2", "completed")
	err = runErr(t, append([]string{"retry", "r2"}, cfg...)...)
	assert.Contains(t, err.Error(), `in "completed" state, cannot retry [exit=1]`)
	err = runErr(t, append([]string{"retry", "r-ghost"}, cfg...)...)
	assert.Contains(t, err.Error(), "not found [exit=1]")

	// A paused run is retryable too.
	e.seedRun(t, "r3", "paused")
	mustRun(t, append([]string{"retry", "r3"}, cfg...)...)
}

func TestRetryE2E_Host(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	e.seedRun(t, "rh1", "failed")

	out := mustRun(t, append([]string{"retry-host", "rh1", "web-01"}, cfg...)...)
	assert.Contains(t, out, "host web-01 retry #1")

	// Exhaust the host budget: the successful retry above already recorded
	// one retry_host row, so two seeded rows bring the Target count to 3.
	seedAuditRows(t, e, "rh1", "retry_host", "rh1/web-01", 2)
	setRunStatus(t, e, "rh1", "failed")
	err := runErr(t, append([]string{"retry-host", "rh1", "web-01"}, cfg...)...)
	assert.Contains(t, err.Error(), "retry limit (3/3) [exit=5]")

	// A different host keeps its own budget.
	quiet := mustRun(t, append([]string{"--quiet", "retry-host", "rh1", "web-02"}, cfg...)...)
	assert.Contains(t, quiet, "rh1/web-02")

	// Ghost run: state gate first.
	err = runErr(t, append([]string{"retry-host", "rh-ghost", "web-01"}, cfg...)...)
	assert.Contains(t, err.Error(), "not found [exit=1]")
}
