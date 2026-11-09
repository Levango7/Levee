// cluster_status_placement_test.go covers the two pieces of `levee cluster
// status` that decide what an operator is told: how a node's declarations are
// rendered, and which reason a stuck run gets. The rendering and the
// classification are separate functions precisely so both can be asserted
// without a PostgreSQL instance.
package main

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/nexus/levee/internal/capability"
	"github.com/nexus/levee/internal/dispatch"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustCaps(t *testing.T, tokens ...string) string {
	t.Helper()
	stored, err := capability.Normalize(tokens)
	require.NoError(t, err)
	return stored
}

func TestNodeRegions(t *testing.T) {
	assert.Equal(t, "-", nodeRegions(state.ClusterNode{Capabilities: capability.Empty}),
		"a node that declared nothing must not look like a parse failure")
	assert.Equal(t, "dc-a,dc-b", nodeRegions(state.ClusterNode{
		Capabilities: mustCaps(t, "region=dc-a", "region=dc-b", "shell"),
	}), "module capabilities are not regions")
	assert.Equal(t, "(unreadable declaration)", nodeRegions(state.ClusterNode{Capabilities: `{}`}),
		"a row that cannot be parsed must say so instead of rendering as '-'")
}

// TestClassifyPlacement pins the precedence an operator relies on: a run whose
// plan does not identify its hosts is not described as a spanning problem, and
// a region nobody declares is reported as such even when another required
// region is covered.
func TestClassifyPlacement(t *testing.T) {
	dcA := mustCaps(t, "region=dc-a")
	dcB := mustCaps(t, "region=dc-b")
	decls := []string{dcA, dcB}

	assert.Equal(t, "", classifyPlacement(dispatch.Placement{Regions: []string{"dc-a"}}, decls))
	assert.Equal(t, "", classifyPlacement(dispatch.Placement{}, decls),
		"an unconstrained run is never listed as stuck")

	assert.Equal(t, dispatch.ReasonNoSingleSpan,
		classifyPlacement(dispatch.Placement{Regions: []string{"dc-a", "dc-b"}}, decls))
	assert.Equal(t, dispatch.ReasonNoEligibleNode,
		classifyPlacement(dispatch.Placement{Regions: []string{"dc-a", "dc-z"}}, decls),
		"dc-z is declared by nobody; that is the fix the operator needs, not 'span'")
	assert.Equal(t, dispatch.ReasonRegionUnresolved,
		classifyPlacement(dispatch.Placement{Regions: []string{"dc-a"}, Reason: dispatch.ReasonRegionUnresolved}, decls),
		"a requirement that could not be computed must keep its own reason")
	assert.Equal(t, dispatch.ReasonPlanUnreadable,
		classifyPlacement(dispatch.Placement{Reason: dispatch.ReasonPlanUnreadable}, decls))
}

// seedSpanningRun puts one approved run whose two batches sit in different
// data centres into the store, the shape the refusal diagnostics describe.
func seedSpanningRun(t *testing.T, st state.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, tc := range []struct{ id, host, region string }{
		{"t-a", "h-a", "dc-a"},
		{"t-b", "h-b", "dc-b"},
	} {
		require.NoError(t, st.UpsertTarget(ctx, &state.Target{
			ID: tc.id, Hostname: tc.host, Port: 22, ChannelType: "ssh",
			Labels: map[string]string{"region": tc.region}, Status: "active",
			CreatedAt: now, TenantID: "",
		}))
	}
	require.NoError(t, st.CreateRun(ctx, &state.Run{
		ID: "run-span", WorkflowName: "wf", TemplateName: "tpl", Params: "{}",
		PlanHash: "ph",
		PlanJSON: `{"Batches":[{"Index":0,"Targets":["h-a"]},{"Index":1,"Targets":["h-b"]}]}`,
		Status:   "approved", ApprovalStatus: "approved", Creator: "test",
		TenantID: "", CreatedAt: now, UpdatedAt: now,
	}))
}

// TestUnplaceableRunsCarryDiagnosis pins the CLI half of the refusal: the
// stuck-run list must carry the batch-level footprint and the remedy, not
// just the reason word, or an operator reading `levee cluster status` still
// cannot act on the refusal.
func TestUnplaceableRunsCarryDiagnosis(t *testing.T) {
	st := newTestStore(t)
	seedSpanningRun(t, st)

	nodes := []state.ClusterNode{
		{ID: "node-a", Capabilities: mustCaps(t, "region=dc-a")},
		{ID: "node-b", Capabilities: mustCaps(t, "region=dc-b")},
	}
	runs, err := st.ListRuns(context.Background(), state.RunFilter{Status: "approved", Limit: 10})
	require.NoError(t, err)
	stuck, err := unplaceableRuns(context.Background(), st, nodes, runs)
	require.NoError(t, err)
	require.Len(t, stuck, 1)
	assert.Equal(t, dispatch.ReasonNoSingleSpan, stuck[0].Reason)
	assert.Contains(t, stuck[0].Detail, "batch 0: dc-a")
	assert.Contains(t, stuck[0].Detail, "batch 1: dc-b")
	assert.Contains(t, stuck[0].Detail, "fix: split the run into one run per region")
}

// TestPrintClusterStatusTableShowsDiagnosis pins the table surface: the
// DETAIL column must actually render what unplaceableRuns collected, or the
// operator-facing half of the upgrade is missing.
func TestPrintClusterStatusTableShowsDiagnosis(t *testing.T) {
	st := newTestStore(t)
	seedSpanningRun(t, st)

	nodes := []state.ClusterNode{
		{ID: "node-a", Capabilities: mustCaps(t, "region=dc-a")},
		{ID: "node-b", Capabilities: mustCaps(t, "region=dc-b")},
	}
	runs, err := st.ListRuns(context.Background(), state.RunFilter{Status: "approved", Limit: 10})
	require.NoError(t, err)
	stuck, err := unplaceableRuns(context.Background(), st, nodes, runs)
	require.NoError(t, err)
	require.Len(t, stuck, 1)

	old := os.Stdout
	r, w, perr := os.Pipe()
	require.NoError(t, perr)
	os.Stdout = w
	printerr := printClusterStatusTable(map[string]bool{"node-a": true}, nil, map[string]int{}, nil, stuck)
	require.NoError(t, w.Close())
	os.Stdout = old
	out, ioErr := io.ReadAll(r)
	require.NoError(t, ioErr)
	require.NoError(t, printerr)

	assert.Contains(t, string(out), "DETAIL")
	assert.Contains(t, string(out), "batch 0: dc-a")
	assert.Contains(t, string(out), "fix: split the run into one run per region")
}
