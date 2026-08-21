// cmd_alert.go implements the `levee alert` sub-command and its children.
//
// Sub-commands:
//
//	levee alert serve   [--addr :9095] [--dedup 5m] [--aggregate 30s]
//	    Run the alert gateway HTTP server in the current process. Registers
//	    the built-in Prometheus and custom adapters and blocks until SIGINT
//	    or SIGTERM.
//
//	levee alert list    [--server http://host:9095]
//	    List recently received alerts.
//
//	levee alert show <id> [--server http://host:9095]
//	    Show a single alert by ID (looked up in the recent buffer).
//
//	levee alert silence add    [--match k=v] [--duration 1h] [--reason ...]
//	levee alert silence list   [--server ...]
//	levee alert silence remove <id> [--server ...]
//	    Manage silence rules. When --server is omitted the commands operate
//	    against a fresh in-process silencer (useful for testing rule
//	    expressions); otherwise they call the gateway REST API.
//
//	levee alert history [--server ...] [--limit 50]
//	    Alias for `levee alert list` with a limit flag (same source data).
//
// All commands honour the global --json flag for machine-readable output.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nexus/levee/internal/alert"
	"github.com/nexus/levee/internal/log"

	"github.com/spf13/cobra"
)

// alertOpt* hold the values of the `levee alert` flags.
var (
	alertOptAddr      string
	alertOptDedup     time.Duration
	alertOptAggregate time.Duration
	alertOptServer    string
	alertOptLimit     int

	// silence add flags
	alertOptMatch    []string
	alertOptDuration time.Duration
	alertOptReason   string
	alertOptSource   string
)

func init() {
	RegisterCommand(newAlertCmd())
}

// newAlertCmd builds the `levee alert` sub-command tree.
func newAlertCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alert",
		Short: "Alert gateway: receive, dedup, silence and dispatch alerts",
		Long: "Manage the LEVEE alert gateway. The gateway receives alerts " +
			"from Prometheus Alertmanager and custom webhooks, normalises " +
			"them into a unified model, and applies deduplication, " +
			"aggregation and silencing before forwarding to a downstream " +
			"handler.",
	}
	cmd.AddCommand(newAlertServeCmd())
	cmd.AddCommand(newAlertListCmd())
	cmd.AddCommand(newAlertShowCmd())
	cmd.AddCommand(newAlertSilenceCmd())
	cmd.AddCommand(newAlertHistoryCmd())
	return cmd
}

// newAlertServeCmd builds `levee alert serve`.
func newAlertServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the alert gateway HTTP server",
		Long: "Run the alert gateway in the current process. Registers the " +
			"Prometheus and custom webhook adapters and blocks until " +
			"SIGINT / SIGTERM.",
		Args: cobra.NoArgs,
		RunE: runAlertServe,
	}
	cmd.Flags().StringVar(&alertOptAddr, "addr", ":9095", "HTTP listen address")
	cmd.Flags().DurationVar(&alertOptDedup, "dedup", 5*time.Minute, "deduplication window (0 to disable)")
	cmd.Flags().DurationVar(&alertOptAggregate, "aggregate", 30*time.Second, "aggregation window (0 to disable)")
	return cmd
}

// runAlertServe runs the gateway.
func runAlertServe(cmd *cobra.Command, args []string) error {
	handler := alert.AlertHandlerFunc(func(_ context.Context, a *alert.Alert) error {
		log.Info("alert dispatched", "id", a.ID, "source", a.Source, "title", a.Title, "severity", a.Severity.String())
		return nil
	})
	cfg := alert.GatewayConfig{
		Addr:      alertOptAddr,
		Dedup:     alertOptDedup,
		Aggregate: alertOptAggregate,
	}
	g := alert.NewAlertGateway(cfg, handler)
	g.RegisterAdapter(alert.NewPrometheusAdapter())
	g.RegisterAdapter(alert.NewCustomAdapter())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("received signal, shutting down", "signal", sig.String())
		cancel()
	}()

	log.Info("starting levee alert gateway",
		"addr", alertOptAddr,
		"dedup", alertOptDedup.String(),
		"aggregate", alertOptAggregate.String(),
	)
	if err := g.Start(ctx, alertOptAddr); err != nil {
		return fmt.Errorf("alert serve: %w", err)
	}
	return nil
}

// newAlertListCmd builds `levee alert list`.
func newAlertListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recently received alerts",
		Args:  cobra.NoArgs,
		RunE:  runAlertList,
	}
	cmd.Flags().StringVar(&alertOptServer, "server", "", "gateway URL (empty = error)")
	return cmd
}

// runAlertList calls GET /alerts on the gateway.
func runAlertList(cmd *cobra.Command, args []string) error {
	if alertOptServer == "" {
		return fmt.Errorf("alert list: --server is required [exit=2]")
	}
	return alertGetAndPrint(alertOptServer + "/alerts")
}

// newAlertShowCmd builds `levee alert show <id>`.
func newAlertShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a single alert by ID",
		Args:  cobra.ExactArgs(1),
		RunE:  runAlertShow,
	}
	cmd.Flags().StringVar(&alertOptServer, "server", "", "gateway URL (empty = error)")
	return cmd
}

// runAlertShow fetches /alerts and finds the alert with the given ID.
func runAlertShow(cmd *cobra.Command, args []string) error {
	if alertOptServer == "" {
		return fmt.Errorf("alert show: --server is required [exit=2]")
	}
	id := args[0]
	resp, err := http.Get(alertOptServer + "/alerts")
	if err != nil {
		return fmt.Errorf("alert show: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("alert show: read body: %w", err)
	}
	var out struct {
		Alerts []*alert.Alert `json:"alerts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("alert show: unmarshal: %w", err)
	}
	for _, a := range out.Alerts {
		if a.ID == id {
			return PrintJSON(os.Stdout, map[string]any{"data": a})
		}
	}
	return fmt.Errorf("alert show: %w: %q [exit=6]", alert.ErrAlertNotFound, id)
}

// newAlertSilenceCmd builds `levee alert silence` with its children.
func newAlertSilenceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "silence",
		Short: "Manage silence rules",
	}
	cmd.AddCommand(newAlertSilenceAddCmd())
	cmd.AddCommand(newAlertSilenceListCmd())
	cmd.AddCommand(newAlertSilenceRemoveCmd())
	return cmd
}

// newAlertSilenceAddCmd builds `levee alert silence add`.
func newAlertSilenceAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a silence rule",
		Args:  cobra.NoArgs,
		RunE:  runAlertSilenceAdd,
	}
	cmd.Flags().StringSliceVar(&alertOptMatch, "match", nil, "label match k=v (repeatable)")
	cmd.Flags().DurationVar(&alertOptDuration, "duration", time.Hour, "silence duration")
	cmd.Flags().StringVar(&alertOptReason, "reason", "", "human-readable reason")
	cmd.Flags().StringVar(&alertOptSource, "source", "", "restrict to adapter source (empty = all)")
	cmd.Flags().StringVar(&alertOptServer, "server", "", "gateway URL (empty = in-process)")
	return cmd
}

// runAlertSilenceAdd adds a silence rule either via HTTP or in-process.
func runAlertSilenceAdd(cmd *cobra.Command, args []string) error {
	match := parseLabelFlags(alertOptMatch)
	rule := alert.SilenceRule{
		Match:    match,
		Duration: alertOptDuration,
		Reason:   alertOptReason,
		Source:   alertOptSource,
	}

	if alertOptServer == "" {
		// In-process: just print the rule that would be added.
		s := alert.NewSilencer()
		id := s.AddRule(rule)
		return PrintJSON(os.Stdout, map[string]any{"data": map[string]any{"id": id, "rule": rule}})
	}

	body, err := json.Marshal(rule)
	if err != nil {
		return fmt.Errorf("alert silence add: marshal: %w", err)
	}
	resp, err := http.Post(alertOptServer+"/silences", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("alert silence add: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("alert silence add: http %d: %s [exit=1]", resp.StatusCode, string(respBody))
	}
	var out map[string]any
	_ = json.Unmarshal(respBody, &out)
	return PrintJSON(os.Stdout, map[string]any{"data": out})
}

// newAlertSilenceListCmd builds `levee alert silence list`.
func newAlertSilenceListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List silence rules",
		Args:  cobra.NoArgs,
		RunE:  runAlertSilenceList,
	}
	cmd.Flags().StringVar(&alertOptServer, "server", "", "gateway URL (empty = error)")
	return cmd
}

// runAlertSilenceList calls GET /silences.
func runAlertSilenceList(cmd *cobra.Command, args []string) error {
	if alertOptServer == "" {
		return fmt.Errorf("alert silence list: --server is required [exit=2]")
	}
	return alertGetAndPrint(alertOptServer + "/silences")
}

// newAlertSilenceRemoveCmd builds `levee alert silence remove <id>`.
func newAlertSilenceRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a silence rule",
		Args:  cobra.ExactArgs(1),
		RunE:  runAlertSilenceRemove,
	}
	cmd.Flags().StringVar(&alertOptServer, "server", "", "gateway URL (empty = error)")
	return cmd
}

// runAlertSilenceRemove calls DELETE /silences/{id}.
func runAlertSilenceRemove(cmd *cobra.Command, args []string) error {
	if alertOptServer == "" {
		return fmt.Errorf("alert silence remove: --server is required [exit=2]")
	}
	id := args[0]
	req, _ := http.NewRequest(http.MethodDelete, alertOptServer+"/silences/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("alert silence remove: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("alert silence remove: %w: %q [exit=6]", alert.ErrAlertNotFound, id)
	}
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("alert silence remove: http %d: %s [exit=1]", resp.StatusCode, string(body))
	}
	return PrintJSON(os.Stdout, map[string]any{"data": map[string]any{"removed": id}})
}

// newAlertHistoryCmd builds `levee alert history`.
func newAlertHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show alert history (alias for list with --limit)",
		Args:  cobra.NoArgs,
		RunE:  runAlertHistory,
	}
	cmd.Flags().StringVar(&alertOptServer, "server", "", "gateway URL (empty = error)")
	cmd.Flags().IntVar(&alertOptLimit, "limit", 50, "maximum number of alerts to return")
	return cmd
}

// runAlertHistory is the same as list for now; the limit is enforced
// client-side because the gateway exposes a single recent buffer.
func runAlertHistory(cmd *cobra.Command, args []string) error {
	if alertOptServer == "" {
		return fmt.Errorf("alert history: --server is required [exit=2]")
	}
	resp, err := http.Get(alertOptServer + "/alerts")
	if err != nil {
		return fmt.Errorf("alert history: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("alert history: read body: %w", err)
	}
	var out struct {
		Count  int            `json:"count"`
		Alerts []*alert.Alert `json:"alerts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("alert history: unmarshal: %w", err)
	}
	if alertOptLimit > 0 && len(out.Alerts) > alertOptLimit {
		out.Alerts = out.Alerts[len(out.Alerts)-alertOptLimit:]
		out.Count = len(out.Alerts)
	}
	return PrintJSON(os.Stdout, map[string]any{"data": out})
}

// alertGetAndPrint fetches url and forwards the JSON body to PrintJSON.
func alertGetAndPrint(url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("alert http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("alert http: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("alert http: status %d: %s [exit=1]", resp.StatusCode, string(body))
	}
	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return fmt.Errorf("alert http: unmarshal: %w", err)
	}
	return PrintJSON(os.Stdout, map[string]any{"data": data})
}

// (parseLabelFlags is defined in cmd_rbac.go and reused here.)
