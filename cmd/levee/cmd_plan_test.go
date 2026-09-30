// cmd_plan_test.go drives `levee plan` and engine-mode `levee apply`
// against real SQLite persistence through the shared command harness.
// The happy execution path lives in internal/wiring's loopback suite;
// these tests cover the CLI surface: plan persistence artifacts, the
// inventory validation error path, and apply's honest refusals.

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

const planTestWorkflowYAML = `name: cli-plan-test
target:
  type: host
  query: "env=test"
steps:
  - name: restart-app
    action: svc.restart
    args:
      name: nginx
`

// seedPlanRun inserts a run whose WorkflowName is the inline workflow and
// registers the hosts as active inventory targets.
func seedPlanRun(t *testing.T, e *cliEnv, id, status string, hosts ...string) {
	t.Helper()
	store := e.open(t)
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID: id, WorkflowName: planTestWorkflowYAML, Params: "{}",
		Status: status, ApprovalStatus: "approved",
		CreatedAt: now, UpdatedAt: now, Creator: "cli-user",
	}))
	for _, h := range hosts {
		require.NoError(t, store.UpsertTarget(context.Background(), &state.Target{
			ID: "tgt-" + h, Hostname: h, Port: 22,
			ChannelType: "ssh", Status: "active", CreatedAt: now,
		}))
	}
}

func TestPlanCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plan")
	require.NotNil(t, cmd, "plan subcommand should be registered")
}

func TestPlanCmdPersistsArtifact(t *testing.T) {
	e := newCLIEnv(t)
	seedPlanRun(t, e, "run-cli-plan", "draft", "web-1", "web-2")

	out := mustRun(t, "--config", e.cfgPath, "plan", "run-cli-plan", "--targets", "web-1,web-2")
	assert.Contains(t, out, "1 batch")

	store := e.open(t)
	defer func() { _ = store.Close() }()
	run, err := store.GetRun(context.Background(), "run-cli-plan")
	require.NoError(t, err)
	assert.NotEmpty(t, run.PlanJSON, "plan must be persisted on the run")
	assert.NotEmpty(t, run.PlanHash)
}

func TestPlanCmdDryRunPersistsNothing(t *testing.T) {
	e := newCLIEnv(t)
	seedPlanRun(t, e, "run-cli-dry", "draft", "web-1")

	out := mustRun(t, "--config", e.cfgPath, "plan", "run-cli-dry", "--targets", "web-1", "--dry-run")
	assert.Contains(t, out, "NOT persisted")

	store := e.open(t)
	defer func() { _ = store.Close() }()
	run, err := store.GetRun(context.Background(), "run-cli-dry")
	require.NoError(t, err)
	assert.Empty(t, run.PlanJSON)
	assert.Empty(t, run.PlanHash)
}

func TestPlanCmdUnknownTargetFails(t *testing.T) {
	e := newCLIEnv(t)
	seedPlanRun(t, e, "run-cli-unk", "draft", "web-1")

	err := runErr(t, "--config", e.cfgPath, "plan", "run-cli-unk", "--targets", "ghost-1")
	assert.Contains(t, err.Error(), "not found in inventory")
	assert.Equal(t, 1, exitCodeFor(err))
}

func TestApplyEngineRefusesUnplannedRun(t *testing.T) {
	e := newCLIEnv(t)
	seedPlanRun(t, e, "run-cli-noapply", "approved", "web-1")

	err := runErr(t, "--config", e.cfgPath, "apply", "run-cli-noapply", "--engine-enabled")
	// Q2 semantics: no persisted plan → refuse with re-plan guidance, and
	// the refusal is a precondition failure (exit=4, run left untouched).
	assert.Contains(t, err.Error(), "no persisted plan")
	assert.Equal(t, 4, exitCodeFor(err))

	store := e.open(t)
	defer func() { _ = store.Close() }()
	run, err2 := store.GetRun(context.Background(), "run-cli-noapply")
	require.NoError(t, err2)
	assert.Equal(t, "approved", run.Status, "a refused apply must leave the run untouched")
}

func TestApplyEngineFlagDefaults(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("apply")
	require.NotNil(t, cmd)
	f := cmd.Flags().Lookup("engine-enabled")
	require.NotNil(t, f, "apply should expose --engine-enabled")
	assert.Equal(t, "false", f.DefValue, "engine mode must stay opt-in")
}

func TestPlanCmdRunNotFound(t *testing.T) {
	e := newCLIEnv(t)
	err := runErr(t, "--config", e.cfgPath, "plan", "run-missing", "--targets", "web-1")
	assert.Contains(t, err.Error(), "not found")
}
