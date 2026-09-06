// End-to-end tests for `levee pause`, `levee resume`, `levee pause-all` and
// `levee resume-all` (E-2): single-run transitions via the pause manager and
// the global path including the LEVEE_PERMISSIONS gate with its denial audit
// row (SA-007).
package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func TestPauseE2E_SingleRun(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	e.seedRun(t, "p1", "pending")

	// pending -> paused (human), then back to running (JSON).
	out := mustRun(t, append([]string{"pause", "p1"}, cfg...)...)
	assert.Contains(t, out, "Run p1 paused by")
	store := e.open(t)
	run, err := store.GetRun(context.Background(), "p1")
	require.NoError(t, err)
	assert.Equal(t, "paused", run.Status)
	_ = store.Close()

	res, _, err := freshJSON(t, append([]string{"resume", "p1", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "resumed", data["action"])

	// Quiet variant of pause.
	quiet := mustRun(t, append([]string{"--quiet", "pause", "p1"}, cfg...)...)
	assert.Contains(t, quiet, "p1")

	// Error mapping: unknown run and non-pausable state.
	err = runErr(t, append([]string{"pause", "p-ghost"}, cfg...)...)
	assert.Contains(t, err.Error(), "run not found")
	e.seedRun(t, "p2", "completed")
	err = runErr(t, append([]string{"pause", "p2"}, cfg...)...)
	assert.Contains(t, err.Error(), "cannot pause")
	err = runErr(t, append([]string{"resume", "p-ghost"}, cfg...)...)
	assert.Contains(t, err.Error(), "run not found")
	err = runErr(t, append([]string{"resume", "p2"}, cfg...)...)
	assert.Contains(t, err.Error(), "cannot resume")
}

func TestPauseE2E_AllRunsAndPermissions(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	// Human pause-all with a reason pauses the running/pending set;
	// resume-all (JSON) brings them back.
	e.seedRun(t, "a1", "running")
	e.seedRun(t, "a2", "pending")
	out := mustRun(t, append([]string{"pause-all", "--reason", "incident drill"}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))
	store := e.open(t)
	run, err := store.GetRun(context.Background(), "a1")
	require.NoError(t, err)
	assert.Equal(t, "paused", run.Status)
	_ = store.Close()

	res, _, err := freshJSON(t, append([]string{"resume-all", "--json",
		"--reason", "drill over"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)

	// Restricted permission set: pause-all is denied with exit=3 and a
	// permission.denied audit row is written (SA-007).
	t.Setenv("LEVEE_PERMISSIONS", "view:all")
	err = runErr(t, append([]string{"pause-all", "--reason", "should fail"}, cfg...)...)
	assert.Contains(t, err.Error(), "permission denied")
	assert.Contains(t, err.Error(), "[exit=3]")

	store = e.open(t)
	audits, err := store.ListAudits(context.Background(), state.AuditFilter{
		Action: "permission.denied",
	})
	require.NoError(t, err)
	require.NotEmpty(t, audits, "denied pause-all must leave an audit trail")
	assert.Equal(t, currentActor(), audits[0].Actor)
	_ = store.Close()

	// Asymmetric grant: pause:all only — pause-all passes, resume-all denied.
	t.Setenv("LEVEE_PERMISSIONS", "pause:all")
	mustRun(t, append([]string{"pause-all"}, cfg...)...)
	err = runErr(t, append([]string{"resume-all"}, cfg...)...)
	assert.Contains(t, err.Error(), "[exit=3]")

	// Admin default (unset) restores full access.
	t.Setenv("LEVEE_PERMISSIONS", "")
	mustRun(t, append([]string{"resume-all", "--reason", "all clear"}, cfg...)...)
}
