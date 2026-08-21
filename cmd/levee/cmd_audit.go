package main

import (
	"context"
	"encoding/csv"

	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/state"
)

// Audit command option variables.
var (
	auditVerifyOptRunID  string
	auditExportOptRunID  string
	auditExportOptFormat string
	auditListOptRunID    string
	auditShowOptTraceID  string
)

func init() {
	RegisterCommand(newAuditCmd())
}

// newAuditCmd builds the `levee audit` sub-command with its children.
func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Audit verification and export",
		Long: "Verify hash chain integrity, export audit traces, and " +
			"query trace records for change runs.",
	}

	cmd.AddCommand(newAuditVerifyCmd())
	cmd.AddCommand(newAuditExportCmd())
	cmd.AddCommand(newAuditListCmd())
	cmd.AddCommand(newAuditShowCmd())

	return cmd
}

// newAuditVerifyCmd builds the `levee audit verify <run-id>` sub-command.
func newAuditVerifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify <run-id>",
		Short: "Verify hash chain integrity for a run",
		Long: "Verify the hash chain integrity of all trace records " +
			"for the given run. Exit code 6 indicates verification failure.",
		Args: cobra.ExactArgs(1),
		RunE: runAuditVerify,
	}
	return cmd
}

// newAuditExportCmd builds the `levee audit export <run-id>` sub-command.
func newAuditExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export <run-id>",
		Short: "Export audit traces for a run",
		Long:  "Export all trace records for the given run as JSON or CSV.",
		Args:  cobra.ExactArgs(1),
		RunE:  runAuditExport,
	}
	cmd.Flags().StringVar(&auditExportOptFormat, "format", "json", "Export format: json or csv")
	return cmd
}

// newAuditListCmd builds the `levee audit list <run-id>` sub-command.
func newAuditListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <run-id>",
		Short: "List all traces for a run",
		Long:  "List all trace records for the given run.",
		Args:  cobra.ExactArgs(1),
		RunE:  runAuditList,
	}
	return cmd
}

// newAuditShowCmd builds the `levee audit show <trace-id>` sub-command.
func newAuditShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <trace-id>",
		Short: "Show details of a single trace record",
		Long:  "Display the full details of a single audit trace record.",
		Args:  cobra.ExactArgs(1),
		RunE:  runAuditShow,
	}
	return cmd
}

// runAuditVerify executes the `levee audit verify <run-id>` command.
func runAuditVerify(cmd *cobra.Command, args []string) error {
	auditVerifyOptRunID = args[0]
	ctx := context.Background()

	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	verifier, err := audit.NewChainVerifier(store)
	if err != nil {
		return fmt.Errorf("create chain verifier: %w", err)
	}

	result, err := verifier.Verify(ctx, auditVerifyOptRunID)
	if err != nil {
		return fmt.Errorf("verify chain: %w", err)
	}

	output := map[string]any{
		"run_id":   result.RunID,
		"valid":    result.Valid,
		"count":    result.Count,
		"failures": result.Failures,
	}

	if !result.Valid {
		if optJSON {
			_ = PrintJSON(os.Stdout, map[string]any{
				"data":  output,
				"meta":  nil,
				"error": map[string]any{"code": 6, "message": "chain verification failed"},
			})
		} else {
			printAuditVerifyHuman(os.Stdout, output)
			fmt.Fprintln(os.Stderr, "ERROR: hash chain verification failed")
		}
		return fmt.Errorf("chain verification failed for run %q: %d of %d records tampered [exit=6]",
			auditVerifyOptRunID, len(result.Failures), result.Count)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, auditVerifyOptRunID)
		return nil
	}

	printAuditVerifyHuman(os.Stdout, output)
	return nil
}

// runAuditExport executes the `levee audit export <run-id>` command.
func runAuditExport(cmd *cobra.Command, args []string) error {
	auditExportOptRunID = args[0]
	ctx := context.Background()

	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: auditExportOptRunID})
	if err != nil {
		return fmt.Errorf("list traces: %w", err)
	}

	switch strings.ToLower(auditExportOptFormat) {
	case "csv":
		return exportTracesCSV(os.Stdout, traces)
	default:
		return exportTracesJSON(os.Stdout, traces)
	}
}

// runAuditList executes the `levee audit list <run-id>` command.
func runAuditList(cmd *cobra.Command, args []string) error {
	auditListOptRunID = args[0]
	ctx := context.Background()

	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: auditListOptRunID})
	if err != nil {
		return fmt.Errorf("list traces: %w", err)
	}

	rows := make([]map[string]any, 0, len(traces))
	for _, t := range traces {
		rows = append(rows, map[string]any{
			"id":        t.ID,
			"event":     t.Event,
			"actor":     t.Actor,
			"timestamp": t.Timestamp,
		})
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  rows,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		for _, t := range traces {
			fmt.Fprintln(os.Stdout, t.ID)
		}
		return nil
	}

	printAuditListHuman(os.Stdout, rows)
	return nil
}

// runAuditShow executes the `levee audit show <trace-id>` command.
func runAuditShow(cmd *cobra.Command, args []string) error {
	auditShowOptTraceID = args[0]
	ctx := context.Background()

	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	trace, err := store.GetTrace(ctx, auditShowOptTraceID)
	if err != nil {
		return fmt.Errorf("get trace: %w", err)
	}
	if trace == nil {
		return fmt.Errorf("trace %q not found", auditShowOptTraceID)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  trace,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, trace.ID)
		return nil
	}

	printAuditShowHuman(os.Stdout, trace)
	return nil
}

// exportTracesJSON exports traces as a JSON document.
func exportTracesJSON(w io.Writer, traces []*state.Trace) error {
	output := map[string]any{
		"data":  traces,
		"meta":  nil,
		"error": nil,
	}
	return PrintJSON(w, output)
}

// exportTracesCSV exports traces as CSV.
func exportTracesCSV(w io.Writer, traces []*state.Trace) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	header := []string{"id", "run_id", "event", "actor", "detail", "prev_hash", "curr_hash", "timestamp"}
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}

	for _, t := range traces {
		record := []string{
			t.ID,
			t.RunID,
			t.Event,
			t.Actor,
			t.Detail,
			t.PrevHash,
			t.CurrHash,
			t.Timestamp.Format("2006-01-02T15:04:05Z"),
		}
		if err := cw.Write(record); err != nil {
			return fmt.Errorf("write csv record: %w", err)
		}
	}

	return nil
}

// printAuditVerifyHuman renders the audit verify output.
func printAuditVerifyHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Run: %v\n", output["run_id"])
	fmt.Fprintf(w, "  Valid: %v\n", output["valid"])
	fmt.Fprintf(w, "  Records checked: %v\n", output["count"])
	if failures, ok := output["failures"].([]audit.ChainFailure); ok && len(failures) > 0 {
		fmt.Fprintf(w, "  Failures (%d):\n", len(failures))
		for _, f := range failures {
			fmt.Fprintf(w, "    trace=%s  index=%d  type=%s\n", f.TraceID, f.Index, f.Type)
		}
	}
}

// printAuditListHuman renders the audit list output.
func printAuditListHuman(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No traces found.")
		return
	}
	fmt.Fprintf(w, "%-36s %-20s %-16s %s\n", "ID", "EVENT", "ACTOR", "TIMESTAMP")
	for _, row := range rows {
		id, _ := row["id"].(string)
		event, _ := row["event"].(string)
		actor, _ := row["actor"].(string)
		timestamp := fmt.Sprintf("%v", row["timestamp"])
		fmt.Fprintf(w, "%-36s %-20s %-16s %s\n", id, event, actor, timestamp)
	}
}

// printAuditShowHuman renders the audit show output.
func printAuditShowHuman(w io.Writer, trace *state.Trace) {
	fmt.Fprintf(w, "Trace: %s\n", trace.ID)
	fmt.Fprintf(w, "  Run ID:   %s\n", trace.RunID)
	fmt.Fprintf(w, "  Event:    %s\n", trace.Event)
	fmt.Fprintf(w, "  Actor:    %s\n", trace.Actor)
	fmt.Fprintf(w, "  Detail:   %s\n", trace.Detail)
	fmt.Fprintf(w, "  PrevHash: %s\n", trace.PrevHash)
	fmt.Fprintf(w, "  CurrHash: %s\n", trace.CurrHash)
	fmt.Fprintf(w, "  Timestamp: %s\n", trace.Timestamp.Format("2006-01-02T15:04:05Z"))
}
