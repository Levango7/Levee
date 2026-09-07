// wiring_test.go covers plan generation: workflow source resolution (inline
// YAML vs file path), inventory validation of requested hosts, and the
// StoredPlan artifact contract (JSON round-trips to a plan whose canonical
// hash equals the stored hash — the invariant ApplyChange verifies).

package wiring

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testWorkflowYAML = `name: wiring-test
target:
  type: host
  query: "env=test"
steps:
  - name: restart-app
    action: svc.restart
    args:
      name: nginx
`

func newTestEngine(t *testing.T, hosts ...string) (*Engine, state.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	for i, h := range hosts {
		require.NoError(t, store.UpsertTarget(ctx, &state.Target{
			ID:          "tgt-" + h,
			Hostname:    h,
			Port:        22,
			ChannelType: "ssh",
			Status:      "active",
			CreatedAt:   time.Now().UTC(),
		}), "seed target %d", i)
	}
	return NewEngine(store), store
}

func seedRun(t *testing.T, store state.Store, id, workflowSource string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID:           id,
		WorkflowName: workflowSource,
		Status:       "draft",
		CreatedAt:    now,
		UpdatedAt:    now,
	}))
}

func TestGeneratePlan_InlineYAML(t *testing.T) {
	e, store := newTestEngine(t, "web-1", "web-2")
	seedRun(t, store, "run-1", testWorkflowYAML)

	msg, stored, err := e.GeneratePlan(context.Background(), "run-1", []string{"web-2", "web-1"})
	require.NoError(t, err)
	require.NotNil(t, stored)

	// Artifact contract: the JSON round-trips to a plan whose canonical
	// hash equals the stored hash (ApplyChange recomputes exactly this).
	var p plan.Plan
	require.NoError(t, json.Unmarshal([]byte(stored.JSON), &p))
	assert.Equal(t, plan.ComputeHash(&p), stored.Hash)
	assert.Equal(t, "wiring-test", p.WorkflowName)
	require.Len(t, p.Batches, 1)
	assert.Equal(t, []string{"web-2", "web-1"}, p.Batches[0].Targets)

	assert.Equal(t, "run-1", msg.GetChangeId())
	require.Len(t, msg.GetBatches(), 1)
	assert.Equal(t, int32(0), msg.GetBatches()[0].GetIndex())
	assert.Equal(t, []string{"web-2", "web-1"}, msg.GetBatches()[0].GetHosts())
	assert.Contains(t, msg.GetImpactSummary(), "2 direct")
	assert.Len(t, msg.GetTargetHosts(), 2)
}

func TestGeneratePlan_WorkflowFile(t *testing.T) {
	e, store := newTestEngine(t, "web-1")
	path := filepath.Join(t.TempDir(), "app.levee.yaml")
	require.NoError(t, os.WriteFile(path, []byte(testWorkflowYAML), 0o600))
	seedRun(t, store, "run-file", path)

	msg, stored, err := e.GeneratePlan(context.Background(), "run-file", []string{"web-1"})
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "run-file", msg.GetChangeId())
}

func TestGeneratePlan_UnknownAndRetiredTargetsRejected(t *testing.T) {
	ctx := context.Background()
	e, store := newTestEngine(t, "web-1", "db-old")
	require.NoError(t, store.UpsertTarget(ctx, &state.Target{
		ID: "tgt-retired", Hostname: "gone-1", ChannelType: "ssh",
		Status: "retired", CreatedAt: time.Now().UTC(),
	}))
	seedRun(t, store, "run-2", testWorkflowYAML)

	_, _, err := e.GeneratePlan(ctx, "run-2", []string{"web-1", "nope-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in inventory")
	assert.Contains(t, err.Error(), "nope-1")

	_, _, err = e.GeneratePlan(ctx, "run-2", []string{"gone-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retired")
}

func TestGeneratePlan_EmptyHostsRejected(t *testing.T) {
	e, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-3", testWorkflowYAML)
	_, _, err := e.GeneratePlan(context.Background(), "run-3", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target_hosts is required")
}

func TestGeneratePlan_UnresolvableWorkflowSource(t *testing.T) {
	e, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-4", "not-yaml-not-a-path")
	_, _, err := e.GeneratePlan(context.Background(), "run-4", []string{"web-1"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "inline") || strings.Contains(err.Error(), "workflow"),
		"error must explain both resolution attempts: %v", err)
}

func TestGeneratePlan_RunNotFound(t *testing.T) {
	e, _ := newTestEngine(t, "web-1")
	_, _, err := e.GeneratePlan(context.Background(), "run-missing", []string{"web-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestLooksLikeWorkflowPath(t *testing.T) {
	cases := map[string]bool{
		"name: x\nsteps:\n": false, // multiline → inline
		"steps: []":         false, // colon-space mapping, not a path
		"deploy/app.levee":  true,
		`C:\work\app.levee`: true,
		"app.levee.yaml":    true,
		"nginx":             false, // bare word: neither YAML nor path — reported as inline failure
		"weird name.levee":  false, // extension with a space is not path-like
	}
	for src, want := range cases {
		assert.Equal(t, want, looksLikeWorkflowPath(src), "src=%q", src)
	}
}
