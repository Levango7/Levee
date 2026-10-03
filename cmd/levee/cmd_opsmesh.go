package main

// cmd_opsmesh.go — `levee opsmesh topology|metrics`: direct operator access
// to the OpsMesh platform's pull APIs (the same client the diagnosis
// topology stage uses through provider "opsmesh").
//
// Both subcommands are READ-ONLY against the platform: they fetch and render
// (human or --json), and never touch the local store or the change pipeline.
// Connection settings come from the opsmesh.* config section; there is no
// separate credential surface to get out of sync.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/diagnosis/topology"
	"github.com/nexus/levee/internal/opsmesh"
)

var (
	opsmeshOptTenant  string
	opsmeshOptService string
	opsmeshOptQuery   string
)

// resetOpsMeshFlags restores defaults; called from tests so repeated
// invocations do not leak flag state.
func resetOpsMeshFlags() {
	opsmeshOptTenant = ""
	opsmeshOptService = ""
	opsmeshOptQuery = ""
}

func init() {
	RegisterCommand(newOpsMeshCmd())
}

func newOpsMeshCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "opsmesh",
		Short: "Query the OpsMesh platform (catalog topology / PromQL metrics)",
		Long: "Read-only access to the OpsMesh platform through the same client the\n" +
			"diagnosis topology stage uses. Connection settings come from the\n" +
			"opsmesh.* config section (enabled + base_url + api_key).",
	}
	parent.AddCommand(newOpsMeshTopologyCmd(), newOpsMeshMetricsCmd())
	return parent
}

func newOpsMeshTopologyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "topology",
		Short: "Fetch the platform's asset/service catalog graph",
		Long: "Fetch the platform's catalog graph (GET /api/v1/catalog/topology).\n" +
			"With --service, also analyses that node's impact radius (upstream /\n" +
			"downstream / statuses) — the same analysis the diagnosis engine runs.",
		Args: cobra.NoArgs,
		RunE: runOpsMeshTopology,
	}
	cmd.Flags().StringVar(&opsmeshOptTenant, "tenant", "", "tenant scope (empty = platform default)")
	cmd.Flags().StringVar(&opsmeshOptService, "service", "", "analyse this node's impact radius after fetching")
	return cmd
}

func newOpsMeshMetricsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metrics --query <promql>",
		Short: "Run a PromQL query through the platform's Prometheus proxy",
		Args:  cobra.NoArgs,
		RunE:  runOpsMeshMetrics,
	}
	cmd.Flags().StringVar(&opsmeshOptQuery, "query", "", "PromQL expression (required)")
	return cmd
}

// opsmeshClientFromConfig builds the client from the opsmesh.* section,
// failing with a config-naming error when the section is not usable.
func opsmeshClientFromConfig() (*opsmesh.OpsMeshClient, error) {
	cfg, err := loadConfigForCmd()
	if err != nil {
		return nil, fmt.Errorf("opsmesh: load config: %w", err)
	}
	if !cfg.OpsMesh.Enabled || cfg.OpsMesh.BaseURL == "" {
		return nil, fmt.Errorf("opsmesh: opsmesh.enabled=true and base_url are required (see config.example.yaml)")
	}
	return opsmesh.NewOpsMeshClient(opsmesh.OpsMeshClientConfig{
		BaseURL: cfg.OpsMesh.BaseURL,
		APIKey:  cfg.OpsMesh.APIKey,
	}), nil
}

func runOpsMeshTopology(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		// Direct invocation in tests leaves Context unset; never pass nil on.
		ctx = context.Background()
	}
	client, err := opsmeshClientFromConfig()
	if err != nil {
		return err
	}
	graph, err := client.GetTopology(ctx, opsmeshOptTenant)
	if err != nil {
		return fmt.Errorf("opsmesh topology: %w", err)
	}

	if optJSON {
		return PrintJSON(cmd.OutOrStdout(), graph)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "catalog topology (tenant %q): %d node(s), %d edge(s)\n",
		graph.TenantID, len(graph.Nodes), len(graph.Edges))
	for _, n := range graph.Nodes {
		status := n.Status
		if status == "" {
			status = "-"
		}
		fmt.Fprintf(out, "  %-10s %-24s %s\n", n.Type, n.Name, status)
	}

	if opsmeshOptService != "" {
		topo := opsmesh.ConvertCatalogGraph(graph)
		impact := topology.ImpactRadius(topo, opsmeshOptService, 0)
		if impact == nil {
			fmt.Fprintf(out, "\nservice %q is not present in the catalog graph\n", opsmeshOptService)
			return nil
		}
		fmt.Fprintf(out, "\nimpact radius of %q [%s]:\n", impact.Pivot.Name, impact.Pivot.Metadata["status"])
		fmt.Fprintf(out, "  upstream   (%d): %s\n", len(impact.Upstream), topology.NodeNames(impact.Upstream, 20))
		fmt.Fprintf(out, "  downstream (%d): %s\n", len(impact.Downstream), topology.NodeNames(impact.Downstream, 20))
	}
	return nil
}

func runOpsMeshMetrics(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(opsmeshOptQuery) == "" {
		return fmt.Errorf("opsmesh metrics: --query <promql> is required [exit=2]")
	}
	ctx := cmd.Context()
	if ctx == nil {
		// Direct invocation in tests leaves Context unset; never pass nil on.
		ctx = context.Background()
	}
	client, err := opsmeshClientFromConfig()
	if err != nil {
		return err
	}
	metrics, err := client.GetMetrics(ctx, opsmeshOptQuery)
	if err != nil {
		return fmt.Errorf("opsmesh metrics: %w", err)
	}

	if optJSON {
		return PrintJSON(cmd.OutOrStdout(), metrics)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "query: %s\n%d series\n", metrics.Query, len(metrics.Series))
	for _, s := range metrics.Series {
		keys := make([]string, 0, len(s.Labels))
		for k := range s.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+s.Labels[k])
		}
		fmt.Fprintf(out, "  {%s} = %g @ %s\n", strings.Join(parts, ", "), s.Value, s.Timestamp.Format("15:04:05"))
	}
	return nil
}
