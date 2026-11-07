// placement_test.go covers the data-centre rule end to end at the dispatch
// seam: which worker a run lands on, which runs are refused and with what
// reason, and the two paths that would otherwise leak the rule (reclaim and
// tenant scoping of the inventory lookup).
//
// Every case is written so that deleting the rule makes it fail, not so that it
// passes for an unrelated reason. The discriminating condition in the
// affinity cases is that the *least loaded* worker is the wrong region: without
// placement the leader (or the idler node) wins the tie and the assertion on
// OwnerNode fails.
package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/nexus/levee/internal/capability"
	"github.com/nexus/levee/internal/cluster"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// regionCaps renders a capability declaration from region names.
func regionCaps(t *testing.T, regions ...string) (string, error) {
	t.Helper()
	tokens := make([]string, 0, len(regions))
	for _, r := range regions {
		tokens = append(tokens, capability.RegionPrefix+r)
	}
	return capability.Normalize(tokens)
}

// regionManager joins exactly the given nodes and makes the first one leader.
func regionManager(t *testing.T, nodes []cluster.Node) *cluster.ClusterManager {
	t.Helper()
	mgr := cluster.NewClusterManager(nil, cluster.ManagerConfig{
		SelfID:           nodes[0].ID,
		HeartbeatTimeout: time.Hour,
	})
	for _, n := range nodes {
		require.NoError(t, mgr.Join(n))
	}
	require.NoError(t, mgr.Start(context.Background()))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })
	mgr.HealthCheck()
	require.Eventually(t, func() bool {
		leader, ok := mgr.GetLeader()
		return ok && leader.ID == nodes[0].ID
	}, 2*time.Second, 10*time.Millisecond, "node %s must be elected leader", nodes[0].ID)
	return mgr
}

func regionNode(t *testing.T, id string, role cluster.NodeRole, regions ...string) cluster.Node {
	t.Helper()
	caps, err := regionCaps(t, regions...)
	require.NoError(t, err)
	return cluster.Node{ID: id, Address: id + ":9090", Role: role, Status: cluster.StatusActive, Capabilities: caps}
}

func seedTarget(t *testing.T, store state.Store, tenant, host, region string) {
	t.Helper()
	now := time.Now().UTC()
	labels := map[string]string{}
	if region != "" {
		labels[TargetRegionLabel] = region
	}
	require.NoError(t, store.UpsertTarget(context.Background(), &state.Target{
		ID: "tgt-" + tenant + "-" + host, Hostname: host, Port: 22, ChannelType: "ssh",
		Labels: labels, Status: "active", CreatedAt: now, TenantID: tenant,
	}))
}

// seedPlannedRun creates an approved run whose persisted plan names hosts.
func seedPlannedRun(t *testing.T, store state.Store, id, tenant string, hosts ...string) {
	t.Helper()
	now := time.Now().UTC()
	planJSON := `{"Batches":[{"Index":0,"Targets":[`
	for i, h := range hosts {
		if i > 0 {
			planJSON += ","
		}
		planJSON += `"` + h + `"`
	}
	planJSON += `]}]}`
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID: id, WorkflowName: "wf", TemplateName: "tpl", Params: "{}",
		PlanHash: "ph-" + id, PlanJSON: planJSON,
		Status: "approved", ApprovalStatus: "approved", Creator: "test",
		TenantID: tenant, CreatedAt: now, UpdatedAt: now,
	}))
}

func ownerOf(t *testing.T, store state.Store, runID string) string {
	t.Helper()
	a, err := store.GetAssignment(context.Background(), runID)
	require.NoError(t, err)
	require.NotNil(t, a, "run %s has no assignment", runID)
	return a.OwnerNode
}

// TestDispatchOnce_RegionAffinityBeatsLoad: the leader is the least loaded
// node and would win the old tie-break, but it is in the wrong data centre.
func TestDispatchOnce_RegionAffinityBeatsLoad(t *testing.T) {
	store := newDispatchStore(t)
	mgr := regionManager(t, []cluster.Node{
		regionNode(t, "node-a", cluster.RoleMaster, "dc-a"),
		regionNode(t, "node-b", cluster.RoleWorker, "dc-b"),
	})
	seedTarget(t, store, "", "h-b", "dc-b")
	seedPlannedRun(t, store, "run-affinity", "", "h-b")

	loop := NewLoop(mgr, store, "node-a", time.Second, 4, DefaultClaimTimeout)
	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, claimed)
	assert.Equal(t, "node-b", ownerOf(t, store, "run-affinity"),
		"a run whose only target lives in dc-b must not be handed to the dc-a leader")
}

// TestDispatchOnce_RefusesRunNoNodeCanReach: nobody declares the region, so the
// run stays approved and unassigned instead of being executed anywhere.
func TestDispatchOnce_RefusesRunNoNodeCanReach(t *testing.T) {
	store := newDispatchStore(t)
	mgr := regionManager(t, []cluster.Node{
		regionNode(t, "node-a", cluster.RoleMaster, "dc-a"),
		regionNode(t, "node-b", cluster.RoleWorker, "dc-b"),
	})
	seedTarget(t, store, "", "h-z", "dc-z")
	seedPlannedRun(t, store, "run-nowhere", "", "h-z")

	loop := NewLoop(mgr, store, "node-a", time.Second, 4, DefaultClaimTimeout)
	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, claimed, "no node reaches dc-z, so nothing may be claimed")
	a, err := store.GetAssignment(context.Background(), "run-nowhere")
	require.NoError(t, err)
	assert.Nil(t, a, "a refused run must not get an assignment row")
	run, err := store.GetRun(context.Background(), "run-nowhere")
	require.NoError(t, err)
	assert.Equal(t, "approved", run.Status, "the run must stay approved for a later sweep")
	assert.Contains(t, loop.lastRefusal, ReasonNoEligibleNode)
	assert.NotContains(t, loop.lastRefusal, ReasonNoSingleSpan,
		"one unreachable region is not a spanning problem")
}

// TestDispatchOnce_RefusesRunSpanningRegions: both regions have a worker, but a
// run goes to one node, so no worker may take it. The reason must say so rather
// than read like "nobody declared that region".
func TestDispatchOnce_RefusesRunSpanningRegions(t *testing.T) {
	store := newDispatchStore(t)
	mgr := regionManager(t, []cluster.Node{
		regionNode(t, "node-a", cluster.RoleMaster, "dc-a"),
		regionNode(t, "node-b", cluster.RoleWorker, "dc-b"),
	})
	seedTarget(t, store, "", "h-a", "dc-a")
	seedTarget(t, store, "", "h-b", "dc-b")
	seedPlannedRun(t, store, "run-span", "", "h-a", "h-b")

	loop := NewLoop(mgr, store, "node-a", time.Second, 4, DefaultClaimTimeout)
	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, claimed)
	assert.Nil(t, func() *state.Assignment {
		a, err := store.GetAssignment(context.Background(), "run-span")
		require.NoError(t, err)
		return a
	}())
	assert.Contains(t, loop.lastRefusal, ReasonNoSingleSpan)
}

// TestDispatchOnce_UnlabelledRunIsUnconstrained: the rule is opt-in. With
// region declarations present but no region labels on the targets, the run
// places exactly as before — the least-loaded node, which is the leader here.
func TestDispatchOnce_UnlabelledRunIsUnconstrained(t *testing.T) {
	store := newDispatchStore(t)
	mgr := regionManager(t, []cluster.Node{
		regionNode(t, "node-a", cluster.RoleMaster, "dc-a"),
		regionNode(t, "node-b", cluster.RoleWorker, "dc-b"),
	})
	seedTarget(t, store, "", "h-plain", "")
	seedPlannedRun(t, store, "run-plain", "", "h-plain")

	loop := NewLoop(mgr, store, "node-a", time.Second, 4, DefaultClaimTimeout)
	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, claimed)
	assert.Equal(t, "node-a", ownerOf(t, store, "run-plain"),
		"an unlabelled run must keep the pre-region placement (least loaded wins)")
	assert.Empty(t, loop.lastRefusal)
}

// TestDispatchOnce_PartiallyLabelledRunIsRefused: half-applied locality is the
// failure mode that looks green, so it is refused with its own reason.
func TestDispatchOnce_PartiallyLabelledRunIsRefused(t *testing.T) {
	store := newDispatchStore(t)
	mgr := regionManager(t, []cluster.Node{
		regionNode(t, "node-a", cluster.RoleMaster, "dc-a"),
		regionNode(t, "node-b", cluster.RoleWorker, "dc-b"),
	})
	seedTarget(t, store, "", "h-a", "dc-a")
	seedTarget(t, store, "", "h-plain", "")
	seedPlannedRun(t, store, "run-half", "", "h-a", "h-plain")

	loop := NewLoop(mgr, store, "node-a", time.Second, 4, DefaultClaimTimeout)
	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, claimed)
	assert.Contains(t, loop.lastRefusal, ReasonRegionUnresolved)
}

// TestDispatchOnce_BusyEligibleWorkerIsBackpressure: capacity exhaustion is not
// a placement failure and must not be reported as one.
func TestDispatchOnce_BusyEligibleWorkerIsBackpressure(t *testing.T) {
	store := newDispatchStore(t)
	mgr := regionManager(t, []cluster.Node{
		regionNode(t, "node-a", cluster.RoleMaster, "dc-a"),
	})
	seedTarget(t, store, "", "h-a", "dc-a")
	seedPlannedRun(t, store, "run-1", "", "h-a")
	seedPlannedRun(t, store, "run-2", "", "h-a")

	loop := NewLoop(mgr, store, "node-a", time.Second, 1, DefaultClaimTimeout)
	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, claimed, "capacity is one run")
	assert.Empty(t, loop.lastRefusal,
		"a saturated-but-eligible cluster is back-pressure, not an unplaceable run")
}

// TestReclaimOnce_ObeysRegionAffinity pins the hole that a first-assignment-only
// rule would leave: reclaim is the path a partitioned region's runs take.
func TestReclaimOnce_ObeysRegionAffinity(t *testing.T) {
	store := newDispatchStore(t)
	mgr := regionManager(t, []cluster.Node{
		regionNode(t, "node-a", cluster.RoleMaster, "dc-a"), // idle, wrong region
		regionNode(t, "node-b", cluster.RoleWorker, "dc-b"), // stale owner, right region
	})
	seedTarget(t, store, "", "h-b", "dc-b")
	seedPlannedRun(t, store, "run-reclaim", "", "h-b")
	// The stale owner is the *more* loaded node (its own pending row counts),
	// so the least-loaded pick without the rule is node-a — the wrong region.
	seedPending(t, store, "run-reclaim", "node-b", 20*time.Minute)

	loop := NewLoop(mgr, store, "node-a", 50*time.Millisecond, 4, time.Minute)
	n, err := loop.ReclaimOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the stale assignment must be reclaimed")
	assert.Equal(t, "node-b", ownerOf(t, store, "run-reclaim"),
		"reclaim must not re-point a dc-b run at the idle dc-a node")
}

// TestDispatchOnce_PlacementIsPerRunNotPerSweep: the resolver lists inventory
// once per tenant for the whole sweep, so a bug that hoisted the requirement out
// of the per-run loop would send both runs to whichever node the first one
// needed. Two runs, two regions, two nodes — each must get its own.
func TestDispatchOnce_PlacementIsPerRunNotPerSweep(t *testing.T) {
	store := newDispatchStore(t)
	mgr := regionManager(t, []cluster.Node{
		regionNode(t, "node-a", cluster.RoleMaster, "dc-a"),
		regionNode(t, "node-b", cluster.RoleWorker, "dc-b"),
	})
	seedTarget(t, store, "", "h-a", "dc-a")
	seedTarget(t, store, "", "h-b", "dc-b")
	seedPlannedRun(t, store, "run-a", "", "h-a")
	seedPlannedRun(t, store, "run-b", "", "h-b")

	loop := NewLoop(mgr, store, "node-a", time.Second, 4, DefaultClaimTimeout)
	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, claimed)
	assert.Equal(t, "node-a", ownerOf(t, store, "run-a"))
	assert.Equal(t, "node-b", ownerOf(t, store, "run-b"),
		"the second run's requirement must not be inherited from the first")
}

// TestDispatchOnce_UnreadablePlanIsRefusedNotDispatched: a corrupt plan must
// not be read as "targets nothing" and sent to an arbitrary node.
func TestDispatchOnce_UnreadablePlanIsRefusedNotDispatched(t *testing.T) {
	store := newDispatchStore(t)
	mgr := regionManager(t, []cluster.Node{
		regionNode(t, "node-a", cluster.RoleMaster, "dc-a"),
	})
	now := time.Now().UTC()
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID: "run-corrupt", WorkflowName: "wf", TemplateName: "tpl", Params: "{}",
		PlanHash: "ph", PlanJSON: `{"Batches": "not-an-array"}`,
		Status: "approved", ApprovalStatus: "approved", Creator: "test",
		CreatedAt: now, UpdatedAt: now,
	}))

	loop := NewLoop(mgr, store, "node-a", time.Second, 4, DefaultClaimTimeout)
	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, claimed)
	assert.Contains(t, loop.lastRefusal, ReasonPlanUnreadable)
}

// TestAssignability pins the one classification both the dispatcher and
// `cluster status` use. If these two ever disagree, an operator sees a run
// described as "nobody declared that region" in one place and "no node spans
// them" in another, and learns to trust neither.
func TestAssignability(t *testing.T) {
	caps := func(regions ...string) string {
		s, err := regionCaps(t, regions...)
		require.NoError(t, err)
		return s
	}
	dcA, dcB := caps("dc-a"), caps("dc-b")

	assert.Equal(t, "", Assignability(nil, []string{dcA}),
		"an unconstrained run is assignable by definition")
	assert.Equal(t, "", Assignability([]string{"dc-a"}, []string{dcB, dcA}))
	assert.Equal(t, ReasonNoSingleSpan, Assignability([]string{"dc-a", "dc-b"}, []string{dcA, dcB}),
		"each region is covered by a different node, which is not enough for one run")
	assert.Equal(t, ReasonNoEligibleNode, Assignability([]string{"dc-a", "dc-z"}, []string{dcA, dcB}),
		"one region is declared by nobody at all")
	assert.Equal(t, ReasonNoEligibleNode, Assignability([]string{"dc-a"}, []string{`{}`}),
		"an unreadable declaration must not be credited with reachability")
}

// TestSortByLoadTiesByID pins the determinism CI caught: registry.List()
// iterates a map, so without the ID tie-break two equally loaded nodes are
// ordered arbitrarily and the same cluster state places a run differently on
// every sweep. The fixture is deliberately not alphabetical input order.
func TestSortByLoadTiesByID(t *testing.T) {
	workers := []workerLoad{
		{id: "node-c"},
		{id: "node-a2", active: 1},
		{id: "node-b"},
	}
	sortByLoad(workers)
	assert.Equal(t, []string{"node-b", "node-c", "node-a2"},
		[]string{workers[0].id, workers[1].id, workers[2].id},
		"zero-load ties must be ordered by ID, and load still dominates the ID")
}
