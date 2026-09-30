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
	"text/tabwriter"

	"github.com/spf13/cobra"

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
		return printClusterStatusJSON(assignments, load)
	}
	return printClusterStatusTable(nodes, assignments, load)
}

func printClusterStatusTable(nodes map[string]bool, assignments []*state.Assignment, load map[string]int) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "=== Worker Load (active assignments per node) ===")
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

func printClusterStatusJSON(assignments []*state.Assignment, load map[string]int) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	active := make([]*state.Assignment, 0, len(assignments))
	for _, a := range assignments {
		if a.State != state.AssignmentStateDone {
			active = append(active, a)
		}
	}
	return enc.Encode(map[string]any{
		"load":        load,
		"assignments": active,
	})
}
