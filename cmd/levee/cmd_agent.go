package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/agent"
	"github.com/nexus/levee/internal/log"
)

// Agent command option variables. They are populated by cobra from the
// flags defined on the agent sub-commands.
var (
	agentStartOptAddr      string
	agentStartOptMaster    string
	agentStartOptCaps      string
	agentStartOptMaxConc   int
	agentStartOptHeartbeat time.Duration
	agentStartOptID        string
)

func init() {
	RegisterCommand(newAgentCmd())
}

// newAgentCmd builds the `levee agent` sub-command with its children.
//
//	levee agent start --addr :9091 --master localhost:9090 --caps shell,file
//	levee agent status
//	levee agent list
//	levee agent show <agent-id>
//	levee agent remove <agent-id>
func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage LEVEE agents",
		Long: "Manage LEVEE distributed-execution agents: start a local " +
			"agent process, query the master registry, or remove an " +
			"offline agent.",
	}
	cmd.AddCommand(newAgentStartCmd())
	cmd.AddCommand(newAgentStatusCmd())
	cmd.AddCommand(newAgentListCmd())
	cmd.AddCommand(newAgentShowCmd())
	cmd.AddCommand(newAgentRemoveCmd())
	return cmd
}

// newAgentStartCmd builds the `levee agent start` sub-command.
func newAgentStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a local agent process",
		Long: "Start a常驻 LEVEE agent that registers with the master, " +
			"receives task assignments, executes them locally and " +
			"reports results back. Blocks until interrupted (Ctrl-C).",
		Args: cobra.NoArgs,
		RunE: runAgentStart,
	}
	cmd.Flags().StringVar(&agentStartOptAddr, "addr", ":9091",
		"Agent listen address (host:port)")
	cmd.Flags().StringVar(&agentStartOptMaster, "master", "localhost:9090",
		"Master node address (host:port)")
	cmd.Flags().StringVar(&agentStartOptCaps, "caps", "shell,file,pkg,svc,user",
		"Comma-separated list of module capabilities")
	cmd.Flags().IntVar(&agentStartOptMaxConc, "max-concurrent", 4,
		"Maximum concurrent task executions")
	cmd.Flags().DurationVar(&agentStartOptHeartbeat, "heartbeat", agent.DefaultHeartbeatInterval,
		"Heartbeat interval")
	cmd.Flags().StringVar(&agentStartOptID, "id", "",
		"Agent ID (default: auto-generated UUID)")
	return cmd
}

// newAgentStatusCmd builds the `levee agent status` sub-command.
func newAgentStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show local agent status",
		Long:  "Show the status of the agent process running in the current CLI.",
		Args:  cobra.NoArgs,
		RunE:  runAgentStatus,
	}
}

// newAgentListCmd builds the `levee agent list` sub-command.
func newAgentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered agents",
		Long:  "List all agents currently registered with the master.",
		Args:  cobra.NoArgs,
		RunE:  runAgentList,
	}
}

// newAgentShowCmd builds the `levee agent show <agent-id>` sub-command.
func newAgentShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <agent-id>",
		Short: "Show agent details",
		Long:  "Show detailed information about a single registered agent.",
		Args:  cobra.ExactArgs(1),
		RunE:  runAgentShow,
	}
}

// newAgentRemoveCmd builds the `levee agent remove <agent-id>` sub-command.
func newAgentRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <agent-id>",
		Short: "Remove an agent from the registry",
		Long: "Remove an agent from the master registry. Use this to " +
			"reclaim the slot of an agent that crashed without " +
			"deregistering.",
		Args: cobra.ExactArgs(1),
		RunE: runAgentRemove,
	}
}

// runAgentStart executes the `levee agent start` command.
func runAgentStart(cmd *cobra.Command, args []string) error {
	caps := parseCapabilities(agentStartOptCaps)

	a := agent.NewAgent(agentStartOptID, agentStartOptAddr, caps, agentStartOptMaxConc)
	a.HeartbeatInterval = agentStartOptHeartbeat

	// Build a master client. In the current MVP we only support the
	// in-process local dispatcher path; a real gRPC client will be
	// wired in once the master-side agent service is implemented.
	mc := newInProcessMasterClient(agentStartOptMaster)
	a.SetMasterClient(mc)

	// Honour OS signals for graceful shutdown.
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Info("agent: starting", "id", a.ID, "addr", a.Address,
		"master", agentStartOptMaster, "caps", caps)

	if optJSON {
		_ = PrintJSON(os.Stdout, map[string]any{
			"data": map[string]any{
				"agent_id": a.ID,
				"address":  a.Address,
				"master":   agentStartOptMaster,
				"caps":     caps,
				"status":   string(a.Status()),
			},
			"meta":  nil,
			"error": nil,
		})
	}

	if err := a.Start(ctx); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

// runAgentStatus executes the `levee agent status` command.
func runAgentStatus(cmd *cobra.Command, args []string) error {
	// In the current MVP the CLI does not keep a long-lived agent in
	// the process between commands, so status reports the local
	// executor's cumulative counters as a proxy.
	exec := agent.NewAgentExecutor(1, nil)
	st := exec.Stats()

	output := map[string]any{
		"status":          string(agent.StatusOffline),
		"active_tasks":    st.Active,
		"completed_tasks": st.Completed,
		"failed_tasks":    st.Failed,
		"max_concurrent":  exec.MaxConcurrent(),
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	printAgentStatusHuman(os.Stdout, output)
	return nil
}

// runAgentList executes the `levee agent list` command.
func runAgentList(cmd *cobra.Command, args []string) error {
	registry := getGlobalAgentRegistry()
	agents := registry.List()

	rows := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, agentInfoToMap(a))
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  rows,
			"meta":  map[string]any{"count": len(rows)},
			"error": nil,
		})
	}

	if optQuiet {
		for _, a := range agents {
			fmt.Fprintln(os.Stdout, a.ID)
		}
		return nil
	}

	printAgentListHuman(os.Stdout, rows)
	return nil
}

// runAgentShow executes the `levee agent show <agent-id>` command.
func runAgentShow(cmd *cobra.Command, args []string) error {
	agentID := args[0]
	registry := getGlobalAgentRegistry()
	info, err := registry.Get(agentID)
	if err != nil {
		return fmt.Errorf("agent show: %w", err)
	}

	output := agentInfoToMap(info)
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	printAgentShowHuman(os.Stdout, output)
	return nil
}

// runAgentRemove executes the `levee agent remove <agent-id>` command.
func runAgentRemove(cmd *cobra.Command, args []string) error {
	agentID := args[0]
	registry := getGlobalAgentRegistry()
	if err := registry.Deregister(agentID); err != nil {
		return fmt.Errorf("agent remove: %w", err)
	}

	output := map[string]any{
		"removed": agentID,
		"at":      time.Now().Format(time.RFC3339),
	}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}
	fmt.Fprintf(os.Stdout, "Removed agent %s\n", agentID)
	return nil
}

// parseCapabilities splits a comma-separated capability list into a
// slice, trimming whitespace and dropping empty entries.
func parseCapabilities(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// agentInfoToMap converts an AgentInfo to a map for JSON / human output.
func agentInfoToMap(a agent.AgentInfo) map[string]any {
	return map[string]any{
		"id":              a.ID,
		"address":         a.Address,
		"capabilities":    a.Capabilities,
		"status":          string(a.Status),
		"last_heartbeat":  a.LastHeartbeat.Format(time.RFC3339),
		"active_tasks":    a.ActiveTasks,
		"completed_tasks": a.CompletedTasks,
		"failed_tasks":    a.FailedTasks,
		"max_concurrent":  a.MaxConcurrent,
		"spare_capacity":  a.SpareCapacity(),
	}
}

// printAgentListHuman renders the agent list as a table.
func printAgentListHuman(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No agents registered.")
		return
	}
	fmt.Fprintf(w, "%-36s %-20s %-10s %-6s %-6s\n",
		"ID", "ADDRESS", "STATUS", "ACTIVE", "SPARE")
	for _, r := range rows {
		fmt.Fprintf(w, "%-36v %-20v %-10v %-6v %-6v\n",
			r["id"], r["address"], r["status"],
			r["active_tasks"], r["spare_capacity"])
	}
}

// printAgentShowHuman renders a single agent's details.
func printAgentShowHuman(w io.Writer, m map[string]any) {
	fmt.Fprintf(w, "Agent: %v\n", m["id"])
	fmt.Fprintf(w, "  Address:        %v\n", m["address"])
	fmt.Fprintf(w, "  Status:         %v\n", m["status"])
	fmt.Fprintf(w, "  Capabilities:   %v\n", m["capabilities"])
	fmt.Fprintf(w, "  Last heartbeat: %v\n", m["last_heartbeat"])
	fmt.Fprintf(w, "  Active tasks:   %v\n", m["active_tasks"])
	fmt.Fprintf(w, "  Completed:      %v\n", m["completed_tasks"])
	fmt.Fprintf(w, "  Failed:         %v\n", m["failed_tasks"])
	fmt.Fprintf(w, "  Max concurrent: %v\n", m["max_concurrent"])
	fmt.Fprintf(w, "  Spare capacity: %v\n", m["spare_capacity"])
}

// printAgentStatusHuman renders the local agent status.
func printAgentStatusHuman(w io.Writer, m map[string]any) {
	fmt.Fprintf(w, "Status:         %v\n", m["status"])
	fmt.Fprintf(w, "  Active tasks:   %v\n", m["active_tasks"])
	fmt.Fprintf(w, "  Completed:      %v\n", m["completed_tasks"])
	fmt.Fprintf(w, "  Failed:         %v\n", m["failed_tasks"])
	fmt.Fprintf(w, "  Max concurrent: %v\n", m["max_concurrent"])
}
