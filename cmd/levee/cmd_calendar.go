package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/calendar"
)

// Calendar command option variables. Kept as package-level vars to mirror
// the cobra/viper idiom used by the rest of the CLI.
var (
	calOptName    string
	calOptStart   string
	calOptEnd     string
	calOptTargets string
	calOptFrozen  bool
	calOptCron    string
	calOptRepeat  string
	calOptLimit   int
)

func init() {
	RegisterCommand(newCalendarCmd())
}

// newCalendarCmd builds the `levee calendar` parent command with its children.
func newCalendarCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "calendar",
		Short: "Manage change calendar windows and freeze periods",
		Long: "Manage change calendar windows and freeze periods: list, show, " +
			"create, update, delete, and check current status for a target set.",
	}
	cmd.AddCommand(newCalendarListCmd())
	cmd.AddCommand(newCalendarShowCmd())
	cmd.AddCommand(newCalendarCreateCmd())
	cmd.AddCommand(newCalendarUpdateCmd())
	cmd.AddCommand(newCalendarDeleteCmd())
	cmd.AddCommand(newCalendarCheckCmd())
	return cmd
}

// newCalendarListCmd builds the `levee calendar list` sub-command.
func newCalendarListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all change windows",
		Long:  "List all change windows and freeze periods, ordered by start time.",
		Args:  cobra.NoArgs,
		RunE:  runCalendarList,
	}
	cmd.Flags().IntVar(&calOptLimit, "limit", 0, "Maximum number of windows to return (0 = all)")
	return cmd
}

// newCalendarShowCmd builds the `levee calendar show <id>` sub-command.
func newCalendarShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show change window details",
		Long:  "Show details of a single change window by its ID.",
		Args:  cobra.ExactArgs(1),
		RunE:  runCalendarShow,
	}
	return cmd
}

// newCalendarCreateCmd builds the `levee calendar create` sub-command.
func newCalendarCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a change window or freeze period",
		Long: "Create a new change window or freeze period. --start and --end " +
			"are RFC3339 timestamps (e.g. 2026-08-16T10:00:00Z). --targets is a " +
			"comma-separated list of target labels. --frozen marks the window " +
			"as a freeze period. --cron adds a 5-field cron recurrence.",
		Args: cobra.NoArgs,
		RunE: runCalendarCreate,
	}
	cmd.Flags().StringVar(&calOptName, "name", "", "Window name (required)")
	cmd.Flags().StringVar(&calOptStart, "start", "", "Start time, RFC3339 (required)")
	cmd.Flags().StringVar(&calOptEnd, "end", "", "End time, RFC3339 (required)")
	cmd.Flags().StringVar(&calOptTargets, "targets", "", "Comma-separated target labels (required)")
	cmd.Flags().BoolVar(&calOptFrozen, "frozen", false, "Mark as freeze period")
	cmd.Flags().StringVar(&calOptCron, "cron", "", "5-field cron recurrence (e.g. '0 2 * * *')")
	cmd.Flags().StringVar(&calOptRepeat, "repeat", "", "Human-readable repeat hint (e.g. weekly)")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("start")
	_ = cmd.MarkFlagRequired("end")
	_ = cmd.MarkFlagRequired("targets")
	return cmd
}

// newCalendarUpdateCmd builds the `levee calendar update <id>` sub-command.
func newCalendarUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a change window",
		Long: "Update an existing change window. Only flags that are set are " +
			"applied; unset flags preserve the existing value.",
		Args: cobra.ExactArgs(1),
		RunE: runCalendarUpdate,
	}
	cmd.Flags().StringVar(&calOptName, "name", "", "New window name")
	cmd.Flags().StringVar(&calOptStart, "start", "", "New start time, RFC3339")
	cmd.Flags().StringVar(&calOptEnd, "end", "", "New end time, RFC3339")
	cmd.Flags().StringVar(&calOptTargets, "targets", "", "New comma-separated target labels")
	cmd.Flags().BoolVar(&calOptFrozen, "frozen", false, "Mark as freeze period")
	cmd.Flags().StringVar(&calOptCron, "cron", "", "New 5-field cron recurrence")
	cmd.Flags().StringVar(&calOptRepeat, "repeat", "", "New human-readable repeat hint")
	return cmd
}

// newCalendarDeleteCmd builds the `levee calendar delete <id>` sub-command.
func newCalendarDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a change window",
		Long:  "Delete a change window or freeze period by its ID.",
		Args:  cobra.ExactArgs(1),
		RunE:  runCalendarDelete,
	}
	return cmd
}

// newCalendarCheckCmd builds the `levee calendar check` sub-command.
func newCalendarCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check current calendar status for a target set",
		Long: "Check whether the given target set is currently frozen, and " +
			"list any active change windows that cover it.",
		Args: cobra.NoArgs,
		RunE: runCalendarCheck,
	}
	cmd.Flags().StringVar(&calOptTargets, "targets", "", "Comma-separated target labels (required)")
	_ = cmd.MarkFlagRequired("targets")
	return cmd
}

// =========================================================================
// Run functions
// =========================================================================

// openCalendarService opens the state store and wraps it with a calendar
// service. The caller is responsible for closing the underlying state store.
func openCalendarService(ctx context.Context) (*calendar.CalendarService, func(), error) {
	store, err := openStore(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	calStore, err := calendar.NewSQLiteStore(ctx, store.DB())
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("open calendar store: %w", err)
	}
	svc := calendar.NewCalendarService(calStore)
	cleanup := func() { _ = store.Close() }
	return svc, cleanup, nil
}

// runCalendarList executes `levee calendar list`.
func runCalendarList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	svc, cleanup, err := openCalendarService(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	windows, err := svc.ListWindows(ctx, calendar.WindowFilter{Limit: calOptLimit})
	if err != nil {
		return fmt.Errorf("list windows: %w", err)
	}

	rows := make([]map[string]any, 0, len(windows))
	for _, w := range windows {
		rows = append(rows, windowToMap(w))
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  rows,
			"meta":  map[string]any{"count": len(rows)},
			"error": nil,
		})
	}
	if optQuiet {
		for _, w := range windows {
			fmt.Fprintln(os.Stdout, w.ID)
		}
		return nil
	}
	printCalendarListHuman(os.Stdout, rows)
	return nil
}

// runCalendarShow executes `levee calendar show <id>`.
func runCalendarShow(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	svc, cleanup, err := openCalendarService(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	id := args[0]
	w, err := svc.GetWindow(ctx, id)
	if err != nil {
		return fmt.Errorf("get window: %w", err)
	}
	if w == nil {
		return fmt.Errorf("window %q not found [exit=4]", id)
	}

	row := windowToMap(w)
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  row,
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, w.ID)
		return nil
	}
	printCalendarShowHuman(os.Stdout, row)
	return nil
}

// runCalendarCreate executes `levee calendar create`.
func runCalendarCreate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	svc, cleanup, err := openCalendarService(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	start, err := parseTimeFlag(calOptStart)
	if err != nil {
		return fmt.Errorf("parse --start: %w [exit=2]", err)
	}
	end, err := parseTimeFlag(calOptEnd)
	if err != nil {
		return fmt.Errorf("parse --end: %w [exit=2]", err)
	}
	targets := parseTargetsFlag(calOptTargets)

	id, err := generateCalendarID()
	if err != nil {
		return fmt.Errorf("generate id: %w", err)
	}
	now := time.Now().UTC()
	w := &calendar.Window{
		ID:           id,
		Name:         calOptName,
		StartTime:    start,
		EndTime:      end,
		TargetLabels: targets,
		IsFrozen:     calOptFrozen,
		RepeatRule:   calOptRepeat,
		CronExpr:     calOptCron,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := svc.CreateWindow(ctx, w); err != nil {
		return fmt.Errorf("create window: %w", err)
	}

	row := windowToMap(w)
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  row,
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, w.ID)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Created window %s\n", w.ID)
	printCalendarShowHuman(os.Stdout, row)
	return nil
}

// runCalendarUpdate executes `levee calendar update <id>`.
func runCalendarUpdate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	svc, cleanup, err := openCalendarService(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	id := args[0]
	w, err := svc.GetWindow(ctx, id)
	if err != nil {
		return fmt.Errorf("get window: %w", err)
	}
	if w == nil {
		return fmt.Errorf("window %q not found [exit=4]", id)
	}

	// Apply only flags that were explicitly set.
	changed := false
	if cmd.Flags().Changed("name") {
		w.Name = calOptName
		changed = true
	}
	if cmd.Flags().Changed("start") {
		start, err := parseTimeFlag(calOptStart)
		if err != nil {
			return fmt.Errorf("parse --start: %w [exit=2]", err)
		}
		w.StartTime = start
		changed = true
	}
	if cmd.Flags().Changed("end") {
		end, err := parseTimeFlag(calOptEnd)
		if err != nil {
			return fmt.Errorf("parse --end: %w [exit=2]", err)
		}
		w.EndTime = end
		changed = true
	}
	if cmd.Flags().Changed("targets") {
		w.TargetLabels = parseTargetsFlag(calOptTargets)
		changed = true
	}
	if cmd.Flags().Changed("frozen") {
		w.IsFrozen = calOptFrozen
		changed = true
	}
	if cmd.Flags().Changed("cron") {
		w.CronExpr = calOptCron
		changed = true
	}
	if cmd.Flags().Changed("repeat") {
		w.RepeatRule = calOptRepeat
		changed = true
	}
	if !changed {
		return fmt.Errorf("no flags set; nothing to update [exit=2]")
	}
	w.UpdatedAt = time.Now().UTC()

	if err := svc.UpdateWindow(ctx, w); err != nil {
		return fmt.Errorf("update window: %w", err)
	}

	row := windowToMap(w)
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  row,
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, w.ID)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Updated window %s\n", w.ID)
	printCalendarShowHuman(os.Stdout, row)
	return nil
}

// runCalendarDelete executes `levee calendar delete <id>`.
func runCalendarDelete(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	svc, cleanup, err := openCalendarService(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	id := args[0]
	if err := svc.DeleteWindow(ctx, id); err != nil {
		return fmt.Errorf("delete window: %w", err)
	}

	output := map[string]any{"id": id, "deleted": true}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, id)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Deleted window %s\n", id)
	return nil
}

// runCalendarCheck executes `levee calendar check --targets <labels>`.
func runCalendarCheck(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	svc, cleanup, err := openCalendarService(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	targets := parseTargetsFlag(calOptTargets)
	now := time.Now().UTC()

	frozen, err := svc.IsFrozenAt(ctx, targets, now)
	if err != nil {
		return fmt.Errorf("check frozen: %w", err)
	}
	active, err := svc.ActiveWindowsAt(ctx, now)
	if err != nil {
		return fmt.Errorf("list active windows: %w", err)
	}
	// Filter active windows to those that share at least one target.
	var covering []*calendar.Window
	targetSet := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		targetSet[t] = struct{}{}
	}
	for _, w := range active {
		for _, l := range w.TargetLabels {
			if _, ok := targetSet[l]; ok {
				covering = append(covering, w)
				break
			}
		}
	}

	rows := make([]map[string]any, 0, len(covering))
	for _, w := range covering {
		rows = append(rows, windowToMap(w))
	}
	output := map[string]any{
		"targets":        targets,
		"now":            now.Format(time.RFC3339),
		"frozen":         frozen,
		"active_windows": rows,
		"active_count":   len(rows),
	}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		if frozen {
			fmt.Fprintln(os.Stdout, "frozen")
		} else {
			fmt.Fprintln(os.Stdout, "ok")
		}
		return nil
	}
	printCalendarCheckHuman(os.Stdout, output)
	return nil
}

// =========================================================================
// Helpers
// =========================================================================

// generateCalendarID creates a new random window identifier.
func generateCalendarID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate calendar id: %w", err)
	}
	return "win-" + hex.EncodeToString(b), nil
}

// parseTimeFlag parses an RFC3339 timestamp. It also accepts a handful of
// common variants (space-separated date/time, without 'Z') for convenience.
func parseTimeFlag(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	// Try RFC3339 first.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	// Try RFC3339 without timezone (assume UTC).
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t.UTC(), nil
	}
	// Try space-separated date time.
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unrecognised time format %q (use RFC3339 e.g. 2026-08-16T10:00:00Z)", s)
}

// parseTargetsFlag splits a comma-separated target label list, trimming
// whitespace and dropping empties.
func parseTargetsFlag(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// windowToMap converts a Window to a map for JSON / human output.
func windowToMap(w *calendar.Window) map[string]any {
	return map[string]any{
		"id":            w.ID,
		"name":          w.Name,
		"start_time":    w.StartTime.Format(time.RFC3339),
		"end_time":      w.EndTime.Format(time.RFC3339),
		"target_labels": w.TargetLabels,
		"is_frozen":     w.IsFrozen,
		"repeat_rule":   w.RepeatRule,
		"cron_expr":     w.CronExpr,
		"created_at":    w.CreatedAt.Format(time.RFC3339),
		"updated_at":    w.UpdatedAt.Format(time.RFC3339),
	}
}

// printCalendarListHuman renders the list in a human-readable form.
func printCalendarListHuman(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No calendar windows found.")
		return
	}
	fmt.Fprintf(w, "%-14s %-20s %-22s %-22s %-8s %s\n",
		"ID", "NAME", "START", "END", "FROZEN", "TARGETS")
	for _, r := range rows {
		id, _ := r["id"].(string)
		name, _ := r["name"].(string)
		start, _ := r["start_time"].(string)
		end, _ := r["end_time"].(string)
		frozen, _ := r["is_frozen"].(bool)
		targets := targetsString(r["target_labels"])
		fmt.Fprintf(w, "%-14s %-20s %-22s %-22s %-8v %s\n",
			id, name, start, end, frozen, targets)
	}
}

// printCalendarShowHuman renders a single window in detail.
func printCalendarShowHuman(w io.Writer, row map[string]any) {
	fmt.Fprintf(w, "ID:         %v\n", row["id"])
	fmt.Fprintf(w, "Name:       %v\n", row["name"])
	fmt.Fprintf(w, "Start:      %v\n", row["start_time"])
	fmt.Fprintf(w, "End:        %v\n", row["end_time"])
	fmt.Fprintf(w, "Targets:    %s\n", targetsString(row["target_labels"]))
	fmt.Fprintf(w, "Frozen:     %v\n", row["is_frozen"])
	if v, _ := row["repeat_rule"].(string); v != "" {
		fmt.Fprintf(w, "Repeat:     %s\n", v)
	}
	if v, _ := row["cron_expr"].(string); v != "" {
		fmt.Fprintf(w, "Cron:       %s\n", v)
	}
	fmt.Fprintf(w, "Created:    %v\n", row["created_at"])
	fmt.Fprintf(w, "Updated:    %v\n", row["updated_at"])
}

// printCalendarCheckHuman renders the check result.
func printCalendarCheckHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Targets: %s\n", targetsString(output["targets"]))
	fmt.Fprintf(w, "Now:     %v\n", output["now"])
	if frozen, _ := output["frozen"].(bool); frozen {
		fmt.Fprintln(w, "Status:  FROZEN — new changes blocked unless emergency approval")
	} else {
		fmt.Fprintln(w, "Status:  OK — no active freeze period")
	}
	rows, _ := output["active_windows"].([]map[string]any)
	fmt.Fprintf(w, "Active windows covering these targets: %v\n", output["active_count"])
	for _, r := range rows {
		fmt.Fprintf(w, "  - %v  %v  [%v -> %v]\n",
			r["id"], r["name"], r["start_time"], r["end_time"])
	}
}

// targetsString renders a target_labels value (any slice) as a comma-joined
// string for human output.
func targetsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []string:
		return strings.Join(t, ",")
	case []any:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			parts = append(parts, fmt.Sprintf("%v", x))
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprintf("%v", t)
	}
}
