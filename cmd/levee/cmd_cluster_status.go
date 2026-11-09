// cmd_cluster_status.go implements `levee cluster status`: a read-only
// diagnostic view of the cluster's dispatch state — nodes (derived from
// assignment ownership), active assignments and per-worker load. It
// connects directly to the cluster's PostgreSQL via --pg-dsn and never
// mutates state, so it is safe to run against a live deployment.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/capability"
	"github.com/nexus/levee/internal/dispatch"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

var (
	clusterStatusDSN    string
	clusterStatusFormat string
)

func init() {
	RegisterCommand(newClusterStatusCmd())
}

func newClusterStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster status",
		Short: "Show cluster dispatch status: nodes, assignments and worker load",
		Long: "Read-only diagnostic view of the cluster's dispatch state. " +
			"Connects directly to the cluster's PostgreSQL via --pg-dsn. " +
			"Shows nodes (derived from assignment ownership), active run " +
			"assignments and per-worker load.",
		Args: cobra.NoArgs,
		RunE: runClusterStatus,
	}
	cmd.Flags().StringVar(&clusterStatusDSN, "pg-dsn", "", "PostgreSQL DSN of the cluster (required)")
	cmd.Flags().StringVar(&clusterStatusFormat, "format", "table", "Output format: table | json")
	return cmd
}

func runClusterStatus(cmd *cobra.Command, args []string) error {
	if clusterStatusDSN == "" {
		return fmt.Errorf("--pg-dsn is required (the cluster's PostgreSQL DSN)")
	}

	ctx := context.Background()
	store, err := state.NewPGStore(ctx, clusterStatusDSN, state.PGPoolConfig{})
	if err != nil {
		return fmt.Errorf("connect to cluster postgres: %w", err)
	}
	defer func() { _ = store.Close() }()

	assignments, err := store.ListAssignments(ctx, state.AssignmentFilter{Limit: 10000})
	if err != nil {
		return fmt.Errorf("list assignments: %w", err)
	}

	// Placement view: which data centres each registered node can reach, and
	// which approved runs the dispatcher is refusing to place. Without this an
	// operator staring at a run stuck in `approved` has only the leader's log
	// line — and only if they happen to be reading it.
	clusterNodes, err := store.ListClusterNodes(ctx)
	if err != nil {
		return fmt.Errorf("list cluster nodes: %w", err)
	}
	approved, err := store.ListRuns(ctx, state.RunFilter{Status: runstatus.StatusApproved})
	if err != nil {
		return fmt.Errorf("list approved runs: %w", err)
	}
	stuck, err := unplaceableRuns(ctx, store, clusterNodes, approved)
	if err != nil {
		return fmt.Errorf("resolve placement: %w", err)
	}

	// Derive the node set and per-node active load from assignments.
	nodes := map[string]bool{}
	load := map[string]int{}
	for _, a := range assignments {
		nodes[a.OwnerNode] = true
		if a.State == state.AssignStatePending || a.State == state.AssignmentStateExecuting {
			load[a.OwnerNode]++
		}
	}

	if clusterStatusFormat == "json" {
		return printClusterStatusJSON(assignments, load, clusterNodes, stuck)
	}
	return printClusterStatusTable(nodes, assignments, load, clusterNodes, stuck)
}

// unplaceableRun is one approved run the dispatcher will not assign, with the
// reason spelled as the dispatcher spells it.
type unplaceableRun struct {
	RunID     string `json:"run_id"`
	Placement string `json:"placement"`
	Reason    string `json:"reason"`
	// Detail is the operator-facing diagnosis: the requirement, which batch
	// needs which data centre, and the remedy for the reason. It is additive
	// to Placement (the compact requirement) so existing JSON consumers keep
	// reading the field they already parse.
	Detail string `json:"detail"`
}

// unplaceableRuns classifies every approved run against the registered node
// declarations. Capacity is deliberately ignored: a run waiting for a busy but
// eligible node will be picked up by a later sweep, while a run with no
// eligible node waits forever. Only the second kind belongs in this list.
func unplaceableRuns(ctx context.Context, store state.Store, nodes []state.ClusterNode, runs []*state.Run) ([]unplaceableRun, error) {
	decls := make([]string, 0, len(nodes))
	for _, n := range nodes {
		decls = append(decls, n.Capabilities)
	}
	resolver := dispatch.NewResolver(store)
	var out []unplaceableRun
	for _, run := range runs {
		place, err := resolver.ForRun(ctx, run)
		if err != nil {
			return nil, err
		}
		if reason := classifyPlacement(place, decls); reason != "" {
			out = append(out, unplaceableRun{
				RunID:     run.ID,
				Placement: place.String(),
				Reason:    reason,
				Detail:    place.Diagnosis(reason),
			})
		}
	}
	return out, nil
}

// classifyPlacement is the single place that decides whether a placement is a
// refusal and with which word. Internal errors in the requirement win over the
// node-coverage diagnosis: "the plan does not say where these hosts are" is
// actionable in a way that "no node spans the regions" is not.
func classifyPlacement(place dispatch.Placement, declarations []string) string {
	if !place.Placeable() {
		return place.Reason
	}
	return dispatch.Assignability(place.Regions, declarations)
}

// nodeRegions renders a node's declared data centres for the status view.
func nodeRegions(n state.ClusterNode) string {
	regions, err := capability.Regions(n.Capabilities)
	if err != nil {
		return "(unreadable declaration)"
	}
	if len(regions) == 0 {
		return "-"
	}
	return strings.Join(regions, ",")
}

func printClusterStatusTable(nodes map[string]bool, assignments []*state.Assignment, load map[string]int, clusterNodes []state.ClusterNode, stuck []unplaceableRun) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "=== Nodes (from cluster_nodes; REGIONS from capability tokens) ===")
	if len(clusterNodes) == 0 {
		fmt.Fprintln(w, "(no registered cluster nodes)")
	} else {
		fmt.Fprintln(w, "ID\tROLE\tSTATUS\tREGIONS")
		for _, n := range clusterNodes {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n.ID, n.Role, n.Status, nodeRegions(n))
		}
	}

	fmt.Fprintln(w, "\n=== Unplaceable Approved Runs ===")
	if len(stuck) == 0 {
		fmt.Fprintln(w, "(none: every approved run has an eligible node)")
	} else {
		fmt.Fprintln(w, "RUN_ID\tPLACEMENT\tREASON\tDETAIL")
		for _, s := range stuck {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.RunID, s.Placement, s.Reason, s.Detail)
		}
	}

	fmt.Fprintln(w, "\n=== Worker Load (active assignments per node) ===")
	// Sorted by load descending, then ID.
	sorted := make([]string, 0, len(nodes))
	for n := range nodes {
		sorted = append(sorted, n)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if load[sorted[i]] != load[sorted[j]] {
			return load[sorted[i]] > load[sorted[j]]
		}
		return sorted[i] < sorted[j]
	})
	for _, n := range sorted {
		fmt.Fprintf(w, "%s\t%d active\n", n, load[n])
	}

	fmt.Fprintln(w, "\n=== Active Assignments ===")
	fmt.Fprintln(w, "RUN_ID\tOWNER NODE\tEPOCH\tSTATE\tRESULT")
	activeCount := 0
	for _, a := range assignments {
		if a.State == state.AssignmentStateDone {
			continue
		}
		activeCount++
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
			a.RunID, a.OwnerNode, a.Epoch, a.State, a.Result)
	}
	if activeCount == 0 {
		fmt.Fprintln(w, "(no active assignments)")
	}

	return w.Flush()
}

func printClusterStatusJSON(assignments []*state.Assignment, load map[string]int, clusterNodes []state.ClusterNode, stuck []unplaceableRun) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	active := make([]*state.Assignment, 0, len(assignments))
	for _, a := range assignments {
		if a.State != state.AssignmentStateDone {
			active = append(active, a)
		}
	}
	type nodeView struct {
		ID      string   `json:"id"`
		Role    string   `json:"role"`
		Status  string   `json:"status"`
		Regions []string `json:"regions"`
	}
	views := make([]nodeView, 0, len(clusterNodes))
	for _, n := range clusterNodes {
		regions, err := capability.Regions(n.Capabilities)
		if err != nil {
			regions = nil // unreadable: reported as an empty set, not a guess
		}
		views = append(views, nodeView{ID: n.ID, Role: n.Role, Status: n.Status, Regions: regions})
	}
	return enc.Encode(map[string]any{
		"load":        load,
		"assignments": active,
		"nodes":       views,
		"unplaceable": stuck,
	})
}
