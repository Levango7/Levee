// cluster_status_placement_test.go covers the two pieces of `levee cluster
// status` that decide what an operator is told: how a node's declarations are
// rendered, and which reason a stuck run gets. The rendering and the
// classification are separate functions precisely so both can be asserted
// without a PostgreSQL instance.
package main

import (
	"testing"

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
