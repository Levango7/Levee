package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/inventory"
	"github.com/nexus/levee/internal/state"
)

// Target command option variables.
var (
	targetListOptFormat             string
	targetImportOptFile             string
	targetImportOptGroup            string
	targetImportOptAllowEnvConflict bool
	targetCheckOptTarget            string
	targetStatusOptTarget           string
	targetHistOptLimit              int
	targetListGroupFilter           string
	targetListStatusFilter          string
)

func init() {
	RegisterCommand(newTargetCmd())
}

// newTargetCmd builds the `levee target` sub-command with its children.
func newTargetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "target",
		Short: "Manage target hosts (inventory)",
		Long: "Manage the persistent target inventory: import from YAML, " +
			"group and label hosts, control lifecycle status, inspect change " +
			"history per host.",
	}

	cmd.AddCommand(newTargetListCmd())
	cmd.AddCommand(newTargetImportCmd())
	cmd.AddCommand(newTargetCheckCmd())
	cmd.AddCommand(newTargetFreezeCmd())
	cmd.AddCommand(newTargetUnfreezeCmd())
	cmd.AddCommand(newTargetRetireCmd())
	cmd.AddCommand(newTargetHistoryCmd())
	return cmd
}

// newTargetListCmd builds the `levee target list` sub-command.
func newTargetListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List targets in the persistent inventory",
		Long:  "List managed targets from the inventory store, including their group, labels and lifecycle status.",
		Args:  cobra.NoArgs,
		RunE:  runTargetList,
	}
	cmd.Flags().StringVar(&targetListOptFormat, "format", "", "Output format (text)")
	cmd.Flags().StringVar(&targetListGroupFilter, "group", "", "Filter by group name")
	cmd.Flags().StringVar(&targetListStatusFilter, "status", "", "Filter by status (active|frozen|retired)")
	return cmd
}

// newTargetImportCmd builds the `levee target import` sub-command.
func newTargetImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import targets from a YAML inventory file",
		Long: "Import groups and targets from a declarative YAML file. The " +
			"import is idempotent: re-importing updates existing rows matched " +
			"by address instead of duplicating them.",
		Args: cobra.NoArgs,
		RunE: runTargetImport,
	}
	cmd.Flags().StringVar(&targetImportOptFile, "file", "", "YAML inventory file path (required)")
	cmd.Flags().StringVar(&targetImportOptGroup, "default-group", "", "Assign targets without an explicit group to this group")
	cmd.Flags().BoolVar(&targetImportOptAllowEnvConflict, "allow-env-conflict", false,
		"Import even when a host's env label contradicts target-ownership.yaml; the override is printed and counted")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// newTargetFreezeCmd builds `levee target freeze <id>`.
func newTargetFreezeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "freeze <target-id>",
		Short: "Freeze a target (rejected for changes while frozen)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetStatusOptTarget = args[0]
			return runTargetSetStatus(state.StatusFrozen)
		},
	}
}

// newTargetUnfreezeCmd builds `levee target unfreeze <id>`.
func newTargetUnfreezeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unfreeze <target-id>",
		Short: "Return a frozen target to active service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetStatusOptTarget = args[0]
			return runTargetSetStatus(state.StatusActive)
		},
	}
}

// newTargetRetireCmd builds `levee target retire <id>`.
func newTargetRetireCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retire <target-id>",
		Short: "Retire a target (kept for history, excluded from changes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetStatusOptTarget = args[0]
			return runTargetSetStatus(state.StatusRetired)
		},
	}
}

// newTargetHistoryCmd builds `levee target history <host>`.
func newTargetHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history <host>",
		Short: "Show recent changes executed on a host",
		Long:  "List the most recent change runs that executed steps on the given host address.",
		Args:  cobra.ExactArgs(1),
		RunE:  runTargetHistory,
	}
	cmd.Flags().IntVar(&targetHistOptLimit, "limit", 20, "Maximum entries to show")
	return cmd
}

// newTargetCheckCmd builds the `levee target check <host>` sub-command.
func newTargetCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <host>",
		Short: "Run precheck on a target host",
		Long:  "Verify reachability of a target host by running a connectivity check.",
		Args:  cobra.ExactArgs(1),
		RunE:  runTargetCheck,
	}
}

// runTargetList executes the `levee target list` command.
func runTargetList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	mode, err := resolveServiceMode()
	if err != nil {
		return err
	}

	var targets []*state.Target
	if mode == modeRemote {
		targets, err = listTargetsRemote(ctx)
	} else {
		targets, err = listTargetsLocal(ctx)
	}
	if err != nil {
		return err
	}
	return printTargetList(targets)
}

// listTargetsLocal reads the inventory from the local store.
func listTargetsLocal(ctx context.Context) ([]*state.Target, error) {
	store, err := openStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	filter := state.TargetFilter{Status: targetListStatusFilter}
	if targetListGroupFilter != "" {
		g, err := store.GetInventoryGroupByName(ctx, targetListGroupFilter)
		if err != nil {
			return nil, fmt.Errorf("resolve group: %w", err)
		}
		if g == nil {
			return nil, fmt.Errorf("group %q not found", targetListGroupFilter)
		}
		filter.GroupID = g.ID
	}

	targets, err := store.ListTargets(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("list targets: %w", err)
	}
	return targets, nil
}

// targetPageSize matches the server's maxPageSize: the largest page keeps the
// loop to one round trip for any realistic inventory.
const targetPageSize = 1000

// listTargetsRemote answers from the SERVER's inventory.
//
// The rows rebuilt here are the same state.Target the local path reads: the
// wire now carries created_at / last_checked_at / tenant_id, and the request
// accepts a group NAME so `--group` does not require knowing an internal ID.
// --status is filtered client-side because ListTargetsRequest has no status
// field; the page loop already materializes the whole (bounded) result.
func listTargetsRemote(ctx context.Context) ([]*state.Target, error) {
	f, err := newServiceFactory(modeRemote, optServer, optAPIToken)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	svc, err := f.GetTargetService()
	if err != nil {
		return nil, err
	}
	return listAllRemoteTargets(ctx, svc, targetListGroupFilter, targetPageSize)
}

// listAllRemoteTargets pages through ListTargets and converts the wire rows.
// pageSize is a parameter so the loop itself can be exercised with tiny pages.
func listAllRemoteTargets(ctx context.Context, svc targetServiceAdapter, groupName string, pageSize int32) ([]*state.Target, error) {
	var targets []*state.Target
	token := ""
	for {
		resp, lerr := svc.ListTargets(ctx, &pb.ListTargetsRequest{
			GroupName: groupName,
			PageSize:  pageSize,
			PageToken: token,
		})
		if lerr != nil {
			// The server resolves --group and answers NotFound for a typo.
			// Its message is the local refusal verbatim, so pass it through
			// instead of nesting an "rpc error" wrapper around it.
			if grpcstatus.Code(lerr) == codes.NotFound {
				return nil, fmt.Errorf("%s", grpcstatus.Convert(lerr).Message())
			}
			return nil, fmt.Errorf("list targets: %w", lerr)
		}
		for _, t := range resp.GetTargets() {
			targets = append(targets, stateTargetFromPB(t))
		}
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
	}

	if targetListStatusFilter != "" {
		filtered := targets[:0]
		for _, t := range targets {
			if t.Status == targetListStatusFilter {
				filtered = append(filtered, t)
			}
		}
		targets = filtered
	}
	return targets, nil
}

// stateTargetFromPB rebuilds the row the local path renders. An empty labels
// map becomes nil so `omitempty` suppresses the key exactly as it does when
// the store hands back a row that never had labels.
func stateTargetFromPB(t *pb.Target) *state.Target {
	out := &state.Target{
		ID:            t.GetId(),
		Hostname:      t.GetHostname(),
		Port:          int(t.GetPort()),
		ChannelType:   t.GetChannelType(),
		CredentialRef: t.GetCredentialRef(),
		GroupID:       t.GetGroupId(),
		Status:        t.GetStatus(),
		Reachable:     t.GetReachable(),
		TenantID:      t.GetTenantId(),
	}
	if len(t.GetLabels()) > 0 {
		out.Labels = t.GetLabels()
	}
	if sec := t.GetCreatedAt(); sec != 0 {
		out.CreatedAt = time.Unix(sec, 0).UTC()
	}
	if sec := t.GetLastCheckedAt(); sec != 0 {
		at := time.Unix(sec, 0).UTC()
		out.LastCheckedAt = &at
	}
	return out
}

// printTargetList renders the three output shapes (JSON / quiet / human) once,
// so the local and remote paths cannot drift apart.
func printTargetList(targets []*state.Target) error {
	// An empty inventory is [] on the wire, not null: the local store hands
	// back a nil slice when there are no rows, and the remote pager builds a
	// nil slice the same way.
	if targets == nil {
		targets = []*state.Target{}
	}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  targets,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		for _, t := range targets {
			fmt.Fprintf(os.Stdout, "%s:%d\n", t.Hostname, t.Port)
		}
		return nil
	}

	printTargetListHuman(os.Stdout, targets)
	return nil
}

// runTargetImport executes the `levee target import` command.
func runTargetImport(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	data, err := os.ReadFile(targetImportOptFile)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	f, err := inventory.ParseYAML(data)
	if err != nil {
		return fmt.Errorf("parse inventory yaml: %w", err)
	}

	importer := inventory.NewImporter(store)

	// The local path writes the SAME rows as InventoryService.ImportTargets (it
	// calls the importer directly), so leaving the ownership check only on the
	// service would make `levee target import` the bypass. Same rule, same flag
	// name, same default-refuse posture.
	cfg, err := loadConfigForCmd()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	reg, err := inventory.LoadOwnership(cfg.Server.DataDir)
	if err != nil {
		return fmt.Errorf("load target ownership: %w", err)
	}
	conflicts, err := importer.EnvConflicts(ctx, reg, f)
	if err != nil {
		return fmt.Errorf("check target ownership: %w", err)
	}
	if len(conflicts) > 0 && !targetImportOptAllowEnvConflict {
		return fmt.Errorf("target import refused: %s; pass --allow-env-conflict to import the file as declared (the override is audited)",
			inventory.FormatEnvConflicts(conflicts))
	}

	sum, err := importer.Import(ctx, f, targetImportOptGroup)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	if len(conflicts) > 0 {
		fmt.Fprintf(os.Stderr, "warning: imported %d host(s) whose env contradicts %s: %s\n",
			len(conflicts), inventory.OwnershipFileName, inventory.FormatEnvConflicts(conflicts))
		if err := recordTargetImportEnvConflict(ctx, store, conflicts); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to write target-import env-conflict audit: %v\n", err)
		}
	}

	output := map[string]any{
		"file":    targetImportOptFile,
		"created": sum.Created,
		"updated": sum.Updated,
		"failed":  sum.Failed,
		"errors":  sum.Errors,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": output, "meta": nil, "error": nil})
	}

	printTargetImportHuman(os.Stdout, output)
	return nil
}

// recordTargetImportEnvConflict writes the local half of the
// --allow-env-conflict audit. The import has already committed, so a failure
// here is a warning rather than an undo — the same posture as every other CLI
// audit writer (see cmd_cancel).
func recordTargetImportEnvConflict(ctx context.Context, store state.Store, conflicts []inventory.EnvConflict) error {
	id, err := newTargetImportAuditID()
	if err != nil {
		return err
	}
	return audit.Record(ctx, store, &state.Audit{
		ID:        id,
		Action:    state.AuditActionImportEnvConflict,
		Actor:     currentActor(),
		Target:    inventory.EnvConflictHosts(conflicts),
		Result:    state.AuditResultSuccess,
		Timestamp: time.Now().UTC(),
	})
}

// newTargetImportAuditID mints an unpredictable identity for the override row.
// A timestamp-derived id would be guessable, which the other CLI audit writers
// already refuse.
func newTargetImportAuditID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate target-import audit id: %w", err)
	}
	return "audit-import-" + hex.EncodeToString(b), nil
}

// runTargetSetStatus applies a lifecycle status transition to a target.
func runTargetSetStatus(status string) error {
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.UpdateTargetStatus(ctx, targetStatusOptTarget, status); err != nil {
		return err
	}

	output := map[string]any{"id": targetStatusOptTarget, "status": status}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": output, "meta": nil, "error": nil})
	}
	fmt.Fprintf(os.Stdout, "Target %s is now %s\n", targetStatusOptTarget, status)
	return nil
}

// remoteTargetHistoryLocalOnlyFields names the keys local `target history
// --json` reports that the TargetHistory RPC deliberately does not carry.
//
// The wire row is five fields (run_id / workflow_name / status / creator /
// created_at) while the local path hands back the whole state.Run — fifteen
// keys, plan_json among them. Shipping those on a history LIST would overturn
// the boundary the proto already states for pb.Change.plan ("Populated ONLY
// when include_plan is true … and never for ListChanges"), so the remote rows
// stay five-wide and the CLI names the ten it is not reporting rather than
// letting an absent key read as "this change has no plan".
var remoteTargetHistoryLocalOnlyFields = []string{
	"template_name",
	"params",
	"plan_hash",
	"plan_json",
	"approval_status",
	"approval_level",
	"priority",
	"updated_at",
	"incident_id",
	"tenant_id",
}

// targetHistoryRow is what the human table prints. Both modes fill it, so the
// table cannot grow a second, mode-specific rendering — the divergence between
// the modes is confined to --json, where it is declared.
type targetHistoryRow struct {
	RunID     string
	Workflow  string
	Status    string
	CreatedAt time.Time
}

// runTargetHistory executes the `levee target history <host>` command.
func runTargetHistory(cmd *cobra.Command, args []string) error {
	host := args[0]
	ctx := context.Background()
	mode, err := resolveServiceMode()
	if err != nil {
		return err
	}

	limit := targetHistOptLimit
	if limit <= 0 {
		limit = 20
	}

	if mode == modeRemote {
		return runRemoteTargetHistory(ctx, host, limit)
	}
	return runLocalTargetHistory(ctx, host, limit)
}

func runLocalTargetHistory(ctx context.Context, host string, limit int) error {
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	steps, err := store.ListSteps(ctx, state.StepFilter{Host: host, Limit: limit * 10})
	if err != nil {
		return fmt.Errorf("list steps: %w", err)
	}

	seen := map[string]bool{}
	var entries []*state.Run
	for _, s := range steps {
		if seen[s.RunID] {
			continue
		}
		seen[s.RunID] = true
		run, err := store.GetRun(ctx, s.RunID)
		if err != nil || run == nil {
			continue
		}
		entries = append(entries, run)
		if len(entries) >= limit {
			break
		}
	}

	if optJSON {
		if entries == nil {
			entries = []*state.Run{}
		}
		return PrintJSON(os.Stdout, map[string]any{"data": entries, "meta": nil, "error": nil})
	}

	rows := make([]*targetHistoryRow, 0, len(entries))
	for _, r := range entries {
		rows = append(rows, &targetHistoryRow{
			RunID: r.ID, Workflow: r.WorkflowName, Status: r.Status, CreatedAt: r.CreatedAt,
		})
	}
	printTargetHistoryHuman(os.Stdout, host, rows)
	return nil
}

func runRemoteTargetHistory(ctx context.Context, host string, limit int) error {
	f, err := newServiceFactory(modeRemote, optServer, optAPIToken)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	svc, err := f.GetInventoryService()
	if err != nil {
		return err
	}
	// The wire field is int32 while --limit is an unbounded int. Without this
	// clamp, `--limit 3000000000` would ask the server for a different — and
	// when the truncation lands negative, silently reformatted to 20 — number of
	// rows than the operator typed. Capping at MaxInt32 instead keeps the
	// request honest for every limit the store could actually satisfy.
	wireLimit := limit
	if wireLimit > math.MaxInt32 {
		wireLimit = math.MaxInt32
	}

	resp, err := svc.TargetHistory(ctx, &pb.TargetHistoryRequest{Host: host, Limit: int32(wireLimit)})
	if err != nil {
		return err
	}

	entries := resp.GetEntries()
	rows := make([]*targetHistoryRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, &targetHistoryRow{
			RunID:    e.GetRunId(),
			Workflow: e.GetWorkflowName(),
			Status:   e.GetStatus(),
			// The wire carries an instant in seconds; this renders it UTC. The
			// local path renders whatever zone its store hands back — UTC on
			// SQLite, the session zone on PostgreSQL — so the two can show a
			// different DAY for the same instant on a PostgreSQL deployment.
			// That is registered as an open decision, not papered over here.
			CreatedAt: time.Unix(e.GetCreatedAt(), 0).UTC(),
		})
	}

	if optJSON {
		// The keys and the timestamp spelling are local `--json`'s, narrowed to
		// what the wire carries: `id` (not the RPC's run_id) because that is the
		// name state.Run marshals under, and RFC3339 because a bare second-count
		// would make every consumer of `--json | jq` branch on --remote. A
		// reader that tolerates the missing keys needs no other change.
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, map[string]any{
				"id":            e.GetRunId(),
				"workflow_name": e.GetWorkflowName(),
				"status":        e.GetStatus(),
				"creator":       e.GetCreator(),
				"created_at":    time.Unix(e.GetCreatedAt(), 0).UTC().Format(time.RFC3339Nano),
			})
		}
		return PrintJSON(os.Stdout, map[string]any{
			"data": out,
			"meta": map[string]any{
				"mode":       "remote",
				"server":     optServer,
				"local_only": remoteTargetHistoryLocalOnlyFields,
			},
			"error": nil,
		})
	}

	printTargetHistoryHuman(os.Stdout, host, rows)
	return nil
}

func printTargetHistoryHuman(w io.Writer, host string, rows []*targetHistoryRow) {
	if len(rows) == 0 {
		fmt.Fprintf(w, "No changes recorded on %s.\n", host)
		return
	}
	fmt.Fprintf(w, "Recent changes on %s:\n\n%-22s %-20s %-10s %-12s\n",
		host, "RUN", "WORKFLOW", "STATUS", "CREATED")
	for _, r := range rows {
		fmt.Fprintf(w, "%-22s %-20s %-10s %-12s\n",
			r.RunID, workflowDisplay(r.Workflow), r.Status, r.CreatedAt.Format("2006-01-02"))
	}
}

// runTargetCheck executes the `levee target check <host>` command.
func runTargetCheck(cmd *cobra.Command, args []string) error {
	targetCheckOptTarget = args[0]
	ctx := context.Background()

	// Build a simple target for precheck.
	tgt := &simpleTarget{host: targetCheckOptTarget}

	// Create a prechecker. We use no channel and no limiter since we
	// don't have a real transport in CLI mode; instead we report the
	// target as needing manual verification.
	prechecker := channel.NewPrechecker(nil, nil)

	// Run the precheck. Without a channel, the prechecker reports the
	// target as unreachable (no channel available).
	report := prechecker.Check(ctx, []channel.Target{tgt})

	result := channel.PrecheckResult{}
	if len(report.Results) > 0 {
		result = report.Results[0]
	}

	output := map[string]any{
		"target":    targetCheckOptTarget,
		"reachable": result.Reachable,
		"latency":   result.Latency.String(),
		"error":     result.Error,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, targetCheckOptTarget)
		return nil
	}

	printTargetCheckHuman(os.Stdout, output)
	return nil
}

// simpleTarget implements channel.Target for CLI usage.
type simpleTarget struct {
	host string
}

func (t *simpleTarget) Host() string                       { return t.host }
func (t *simpleTarget) Port() int                          { return 0 }
func (t *simpleTarget) Type() string                       { return "ssh" }
func (t *simpleTarget) Credentials() channel.CredentialRef { return channel.CredentialRef{} }

// printTargetListHuman renders the target list in a human-readable format.
func printTargetListHuman(w io.Writer, targets []*state.Target) {
	if len(targets) == 0 {
		fmt.Fprintln(w, "No targets in the inventory. Import some with `levee target import -f <file>`.")
		return
	}
	fmt.Fprintf(w, "%-28s %-6s %-7s %-8s %s\n", "ADDRESS", "PORT", "CHANNEL", "STATUS", "LABELS")
	for _, t := range targets {
		labels := make([]string, 0, len(t.Labels))
		for k, v := range t.Labels {
			labels = append(labels, k+"="+v)
		}
		// Map iteration order is random; sorting is what lets the same rows
		// render identically in both modes (and across repeated runs).
		sort.Strings(labels)
		fmt.Fprintf(w, "%-28s %-6d %-7s %-8s %s\n",
			t.Hostname, t.Port, t.ChannelType, t.Status, strings.Join(labels, ","))
	}
}

// printTargetImportHuman renders the target import output.
func printTargetImportHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Imported %v (created) / %v (updated) from %v\n",
		output["created"], output["updated"], output["file"])
	if failed, _ := output["failed"].(int); failed > 0 {
		errs, _ := output["errors"].([]string)
		for _, e := range errs {
			fmt.Fprintf(w, "  error: %s\n", e)
		}
	}
}

// printTargetCheckHuman renders the target check output.
func printTargetCheckHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Target: %v\n", output["target"])
	fmt.Fprintf(w, "  Reachable: %v\n", output["reachable"])
	fmt.Fprintf(w, "  Latency:   %v\n", output["latency"])
	if output["error"] != "" && output["error"] != nil {
		fmt.Fprintf(w, "  Error:     %v\n", output["error"])
	}
}
