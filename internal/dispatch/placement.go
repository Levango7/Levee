// placement.go answers the one question the dispatcher could not answer before:
// which data centre does this run touch, and may the node I am about to hand it
// to reach that data centre.
//
// The gap this closes was a documented promise without an implementation:
// docs/scenario-cross-region-ops.md says each region's worker executes locally,
// while dispatch picked the least-loaded live node and nothing else, so a run
// targeting hosts in one region could be handed to a worker in another. Both
// ends of the fact already existed — the plan names the resolved hosts and the
// inventory row carries the target's labels — so no schema change and no new
// DSL keyword are needed: locality is a property of the asset, not of the
// workflow.
//
// Two deliberate asymmetries:
//
//   - The constraint is opt-in per run: a run whose targets carry no `region`
//     label places exactly as it did before (any live worker). Deployments that
//     never label their inventory see no behaviour change at all.
//   - It is fail-closed once any target of a run is labelled: a run with some
//     labelled and some unlabelled (or missing) targets is refused instead of
//     being executed on a guess. Half-applied locality is the failure mode that
//     looks green in a demo and crosses a network boundary in production.
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nexus/levee/internal/capability"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
)

// TargetRegionLabel is the inventory label naming the data centre a target
// lives in. It is read from targets.labels, so no column and no migration.
const TargetRegionLabel = "region"

// Refusal reasons. These are metric label values and operator-facing text: an
// operator must be able to tell "nobody declares that region" from "no single
// node covers both regions" from "the plan does not say where these hosts are"
// without reading the source.
const (
	// ReasonNoEligibleNode: no live worker declares (or can be credited with)
	// every region the run's targets sit in.
	ReasonNoEligibleNode = "no_eligible_node"
	// ReasonNoSingleSpan: each region has its own worker, but this cluster
	// assigns a whole run to one node and no node covers all the regions in
	// this run. Splitting a run across nodes is deliberately out of scope (it
	// would multiply in-flight executors per run and is not what the approval
	// names), so this is reported rather than approximated.
	ReasonNoSingleSpan = "no_single_node_spans_regions"
	// ReasonRegionUnresolved: at least one target is labelled with a region and
	// at least one other target of the same run is not (or has no inventory
	// row), so the run's requirement cannot be computed without guessing.
	ReasonRegionUnresolved = "region_unresolved"
	// ReasonPlanUnreadable: the persisted plan exists but cannot be decoded, so
	// the run's targets are unknown. Apply would refuse this run anyway; the
	// dispatcher refuses it too, with a reason instead of a silent attempt.
	ReasonPlanUnreadable = "plan_unreadable"
)

// Placement is the dispatch-time locality requirement of one run.
type Placement struct {
	// Regions is the sorted, deduplicated set of data centres the run's
	// targets live in. Empty means unconstrained.
	Regions []string
	// Reason is empty when the requirement could be computed; otherwise it
	// names why the run cannot be placed (one of the Reason* constants).
	Reason string
	// Detail is a short human-readable addition to Reason (which targets are
	// unresolved, for example). It is for logs and diagnostics, never matched.
	Detail string
}

// Placeable reports whether the run's requirement could be computed.
func (p Placement) Placeable() bool { return p.Reason == "" }

// String renders the requirement for logs and `cluster status`.
func (p Placement) String() string {
	if !p.Placeable() {
		if p.Detail == "" {
			return p.Reason
		}
		return p.Reason + ": " + p.Detail
	}
	if len(p.Regions) == 0 {
		return "any"
	}
	return "region in [" + strings.Join(p.Regions, ",") + "]"
}

// placementResolver answers "which data centres does this run touch" for a
// whole dispatch sweep. Inventory is listed once per tenant per sweep: the
// alternative is a query per candidate run every ten seconds for a fact that
// changes only when an operator edits inventory.
type placementResolver struct {
	store    state.Store
	byTenant map[string]map[string]string // tenant -> hostname -> region label ("" = unlabelled)
}

func newPlacementResolver(store state.Store) *placementResolver {
	return &placementResolver{store: store, byTenant: map[string]map[string]string{}}
}

// targetsFor returns the tenant's hostname → region map for this sweep.
func (r *placementResolver) targetsFor(ctx context.Context, tenantID string) (map[string]string, error) {
	if got, ok := r.byTenant[tenantID]; ok {
		return got, nil
	}
	rows, err := r.store.ListTargets(ctx, state.TargetFilter{TenantID: tenantID})
	if err != nil {
		return nil, fmt.Errorf("dispatch: list targets for placement: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, t := range rows {
		out[t.Hostname] = strings.TrimSpace(t.Labels[TargetRegionLabel])
	}
	r.byTenant[tenantID] = out
	return out, nil
}

// forRun computes one run's requirement. A run that cannot be decoded or whose
// targets are only partially labelled is returned as not placeable with the
// reason set; the caller refuses it and reports that reason.
func (r *placementResolver) forRun(ctx context.Context, run *state.Run) (Placement, error) {
	hosts, err := planHosts(run.PlanJSON)
	if err != nil {
		return Placement{Reason: ReasonPlanUnreadable, Detail: err.Error()}, nil
	}
	if len(hosts) == 0 {
		return Placement{}, nil
	}
	known, err := r.targetsFor(ctx, run.TenantID)
	if err != nil {
		return Placement{}, err
	}
	var (
		regions    []string
		seen       = map[string]bool{}
		unlabelled []string
		missing    []string
	)
	for _, h := range hosts {
		reg, ok := known[h]
		switch {
		case !ok:
			missing = append(missing, h)
		case reg == "":
			unlabelled = append(unlabelled, h)
		case !seen[reg]:
			seen[reg] = true
			regions = append(regions, reg)
		}
	}
	sort.Strings(regions)
	if len(regions) == 0 {
		// Nothing in this run says where it lives: place it the way every
		// deployment placed runs before regions existed.
		return Placement{}, nil
	}
	if len(unlabelled) > 0 || len(missing) > 0 {
		var parts []string
		if len(unlabelled) > 0 {
			parts = append(parts, "no region label: "+summarise(unlabelled))
		}
		if len(missing) > 0 {
			parts = append(parts, "not in inventory: "+summarise(missing))
		}
		return Placement{Regions: regions, Reason: ReasonRegionUnresolved, Detail: strings.Join(parts, "; ")}, nil
	}
	return Placement{Regions: regions}, nil
}

// summarise caps a list so one misconfigured batch cannot write a thousand
// entries into a log line or a metric label.
func summarise(hosts []string) string {
	const max = 5
	if len(hosts) <= max {
		return strings.Join(hosts, ",")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(hosts[:max], ","), len(hosts)-max)
}

// planHosts extracts the resolved target hostnames from the persisted plan.
// An empty plan column yields no hosts (legacy rows behave as before); a
// non-empty column that cannot be decoded is an error, because "the plan is
// corrupt" must not be read as "the run targets nothing".
func planHosts(planJSON string) ([]string, error) {
	if strings.TrimSpace(planJSON) == "" {
		return nil, nil
	}
	var p plan.Plan
	if err := json.Unmarshal([]byte(planJSON), &p); err != nil {
		return nil, fmt.Errorf("persisted plan is not decodable: %w", err)
	}
	var (
		out  []string
		seen = map[string]bool{}
	)
	for _, b := range p.Batches {
		for _, h := range b.Targets {
			if h == "" || seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	return out, nil
}

// pickWorker returns the index of the least-loaded worker that (a) has spare
// capacity and (b) can reach every required region. workers must be sorted by
// load, which workerLoads and sortByLoad keep true, so the first match is the
// least loaded eligible node rather than merely the least loaded node.
func pickWorker(workers []workerLoad, want []string, capacity int) (int, bool) {
	for i, w := range workers {
		if w.active >= capacity {
			continue
		}
		if !capability.Reaches(w.capabilities, want) {
			continue
		}
		return i, true
	}
	return 0, false
}

// Assignability classifies a requirement against raw node declarations,
// ignoring load: "" when some node covers every required region,
// ReasonNoSingleSpan when the regions are covered only piecemeal, and
// ReasonNoEligibleNode when some region is declared by no node at all.
//
// It is exported so `levee cluster status` explains a stuck run with exactly
// the word the dispatcher logs. Two surfaces answering "why is this not
// running" differently is how an operator learns to distrust both.
func Assignability(want []string, declarations []string) string {
	if len(want) == 0 {
		return ""
	}
	coveredBy := make(map[string]int, len(want))
	for _, w := range want {
		coveredBy[w] = 0
	}
	for _, d := range declarations {
		have, err := capability.Regions(d)
		if err != nil {
			// An unreadable declaration covers nothing (see capability.Reaches):
			// it must not be credited with access it may not have.
			continue
		}
		for _, r := range have {
			if _, required := coveredBy[r]; required {
				coveredBy[r]++
			}
		}
	}
	for _, w := range want {
		if coveredBy[w] == 0 {
			// Some region is declared by no node at all. That is the actionable
			// fact ("nothing here can reach dc-z"), and it takes precedence over
			// the spanning diagnosis: telling an operator "no single node spans
			// these" about a region nobody has would send them to the wrong fix.
			return ReasonNoEligibleNode
		}
	}
	for _, d := range declarations {
		if capability.Reaches(d, want) {
			return ""
		}
	}
	return ReasonNoSingleSpan
}

// capsOf projects the per-sweep worker view onto raw declarations.
func capsOf(workers []workerLoad) []string {
	out := make([]string, 0, len(workers))
	for _, w := range workers {
		out = append(out, w.capabilities)
	}
	return out
}

// Resolver is the read-only placement view for surfaces outside the dispatch
// loop (`levee cluster status`). It caches inventory per tenant for its own
// lifetime, so a caller that walks every approved run issues one target query
// per tenant rather than one per run.
type Resolver struct{ inner *placementResolver }

// NewResolver returns a resolver backed by store.
func NewResolver(store state.Store) *Resolver {
	return &Resolver{inner: newPlacementResolver(store)}
}

// ForRun computes one run's requirement.
func (r *Resolver) ForRun(ctx context.Context, run *state.Run) (Placement, error) {
	return r.inner.forRun(ctx, run)
}
