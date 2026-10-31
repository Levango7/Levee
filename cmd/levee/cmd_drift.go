// Drift detection CLI for LEVEE.
//
// This file implements the `levee drift` command tree:
//
//   levee drift detect --host <host> [--baseline <file|auto>] [--baseline-name <name>]
//   levee drift detect --hosts <h1,h2,...> [--baseline <file|auto>] [--baseline-name <name>]
//   levee drift baseline set --host <host> --file <baseline.yaml> [--baseline-name <name>]
//   levee drift baseline auto --host <host> --run <run-id> [--baseline-name <name>]
//   levee drift baseline list [--host <host>]
//   levee drift baseline show --host <host> [--baseline-name <name>]
//   levee drift baseline delete --host <host> [--baseline-name <name>]
//   levee drift schedule add --name <name> --cron <expr> --hosts <h1,h2,...> [--baseline-name <name>]
//   levee drift schedule list
//   levee drift schedule remove <job-id>
//   levee drift schedule run <job-id>
//   levee drift report --host <host> [--days <n>]
//
// Baselines and jobs are persisted as JSON files under <dataDir>/drift/ so
// they survive across CLI invocations. A baseline is keyed by (host, name):
// the name is the version label, and the reserved name "default" is what
// every command that is not given --baseline-name resolves to. The file stem
// for a host's default version stays "<host>" so deployments written before
// named versions keep loading, and named versions add one file per name.
// The detector uses a local-file prober that reads file content from the local
// filesystem; remote probing (SSH / WinRM) is left as a future enhancement.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/drift"
)

// drift command option variables.
var (
	driftOptHost         string
	driftOptHosts        string
	driftOptBaseline     string // "auto" or a file path
	driftOptFile         string // baseline file for `baseline set`
	driftOptRunID        string // run id for `baseline auto`
	driftOptName         string
	driftOptBaselineName string // baseline version label ("" = drift.DefaultBaselineName)
	driftOptCron         string
	driftOptDays         int
	driftOptAlert        bool
	driftOptEnabled      bool
)

func init() {
	RegisterCommand(newDriftCmd())
}

// newDriftCmd builds the `levee drift` parent command with its children.
func newDriftCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Detect and report configuration drift",
		Long: "Detect and report configuration drift between the declared " +
			"baseline state and the actual state of target hosts. " +
			"Supports single / batch detection, baseline management, " +
			"scheduled inspection and trend reporting.",
	}
	cmd.AddCommand(newDriftDetectCmd())
	cmd.AddCommand(newDriftBaselineCmd())
	cmd.AddCommand(newDriftScheduleCmd())
	cmd.AddCommand(newDriftReportCmd())
	return cmd
}

// --- --baseline-name --------------------------------------------------------

// baselineNameFlag registers the baseline version label on cmd. Every
// sub-command that stores, reads or deletes a baseline carries it, and the
// wording lives here once because four copies of one sentence is how the
// documented default starts disagreeing with the code. It binds the shared
// option variable like every other drift flag, so the test harness's
// resetDriftFlags restores it — a flag value survives between in-process
// executions of the command tree, which is how one test's --baseline-name
// would otherwise answer for the next test's command.
func baselineNameFlag(cmd *cobra.Command) {
	cmd.Flags().StringVar(&driftOptBaselineName, "baseline-name", "",
		"Stored baseline version label (empty = \""+drift.DefaultBaselineName+"\")")
}

// --- detect -----------------------------------------------------------------

// newDriftDetectCmd builds the `levee drift detect` sub-command.
func newDriftDetectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "detect",
		Short: "Run drift detection on one or more hosts",
		Long: "Run drift detection on one or more hosts against a baseline. " +
			"Use --host for a single host or --hosts for a comma-separated " +
			"list. --baseline accepts 'auto' (use the stored baseline for " +
			"the host) or a path to a baseline YAML file. --baseline-name " +
			"picks which stored version 'auto' means; without it the host's " +
			"'default' version is used. A file path is already a complete " +
			"baseline, so --baseline-name cannot be combined with one.",
		Args: cobra.NoArgs,
		RunE: runDriftDetect,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "", "Single target host")
	cmd.Flags().StringVar(&driftOptHosts, "hosts", "", "Comma-separated target hosts")
	cmd.Flags().StringVar(&driftOptBaseline, "baseline", "auto", "Baseline source: 'auto' or file path")
	baselineNameFlag(cmd)
	return cmd
}

// runDriftDetect executes `levee drift detect`.
func runDriftDetect(cmd *cobra.Command, args []string) error {
	_ctx := context.Background()

	hosts := parseTargetsFlag(driftOptHosts)
	if driftOptHost != "" {
		hosts = append(hosts, driftOptHost)
	}
	if len(hosts) == 0 {
		return fmt.Errorf("drift detect: no hosts specified (use --host or --hosts) [exit=2]")
	}

	// --baseline-name selects which stored version to use. A file path is
	// already a complete baseline, so accepting both would silently drop one
	// of them — name the conflict instead.
	if driftOptBaselineName != "" && driftOptBaseline != "auto" && driftOptBaseline != "" {
		return fmt.Errorf("drift detect: --baseline-name applies only to --baseline auto [exit=2]")
	}

	// Load baselines from disk.
	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift detect: load baselines: %w", err)
	}

	// Build the detector with a local-file prober.
	prober := newLocalFileProber()
	detector := drift.NewDetector(prober)

	// Resolve baseline for each host and run detection.
	var results []*drift.DriftResult
	for _, host := range hosts {
		baseline, err := resolveBaseline(_ctx, bm, host, driftOptBaseline, driftOptBaselineName)
		if err != nil {
			return fmt.Errorf("drift detect: resolve baseline for %q: %w", host, err)
		}
		r, err := detector.Detect(_ctx, host, baseline)
		if r != nil {
			results = append(results, r)
		}
		if err != nil && !strings.Contains(err.Error(), "drift detected") {
			return fmt.Errorf("drift detect: %w", err)
		}
	}

	report := drift.GenerateReport(results)

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  reportToMap(report),
			"meta":  map[string]any{"host_count": len(hosts)},
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, report.ID)
		return nil
	}
	fmt.Fprint(os.Stdout, report.ToTable())
	return nil
}

// --- baseline ---------------------------------------------------------------

// newDriftBaselineCmd builds the `levee drift baseline` parent command.
func newDriftBaselineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "baseline",
		Short: "Manage drift detection baselines",
		Long:  "Manage drift detection baselines: set, auto-generate, list, show, delete.",
	}
	cmd.AddCommand(newDriftBaselineSetCmd())
	cmd.AddCommand(newDriftBaselineAutoCmd())
	cmd.AddCommand(newDriftBaselineListCmd())
	cmd.AddCommand(newDriftBaselineShowCmd())
	cmd.AddCommand(newDriftBaselineDeleteCmd())
	return cmd
}

// newDriftBaselineSetCmd builds `levee drift baseline set`.
func newDriftBaselineSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set a baseline from a YAML file",
		Long: "Set the drift baseline for a host from a YAML file. The file " +
			"must contain a list of baseline items (check_name, type, path, " +
			"expected_value). --baseline-name stores it as one version of that " +
			"host; omitting it replaces the host's \"" + drift.DefaultBaselineName +
			"\" version, which is what detect and scheduled jobs use unless " +
			"they name another.",
		Args: cobra.NoArgs,
		RunE: runDriftBaselineSet,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "", "Target host (required)")
	cmd.Flags().StringVar(&driftOptFile, "file", "", "Baseline YAML file (required)")
	baselineNameFlag(cmd)
	_ = cmd.MarkFlagRequired("host")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// newDriftBaselineAutoCmd builds `levee drift baseline auto`.
func newDriftBaselineAutoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auto",
		Short: "Auto-generate a baseline from an apply snapshot",
		Long: "Auto-generate the drift baseline for a host from the most " +
			"recent apply snapshot of the given run. With --baseline-name the " +
			"snapshot is stored as that version and the host's existing " +
			"versions stay available for comparison.",
		Args: cobra.NoArgs,
		RunE: runDriftBaselineAuto,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "", "Target host (required)")
	cmd.Flags().StringVar(&driftOptRunID, "run", "", "Source run ID (required)")
	baselineNameFlag(cmd)
	_ = cmd.MarkFlagRequired("host")
	_ = cmd.MarkFlagRequired("run")
	return cmd
}

// newDriftBaselineListCmd builds `levee drift baseline list`.
func newDriftBaselineListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all stored baselines",
		Args:  cobra.NoArgs,
		RunE:  runDriftBaselineList,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "",
		"Restrict to one host (empty = every stored baseline); a host with "+
			"named versions yields one row each")
	return cmd
}

// newDriftBaselineShowCmd builds `levee drift baseline show`.
func newDriftBaselineShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show one stored baseline of a host",
		Args:  cobra.NoArgs,
		RunE:  runDriftBaselineShow,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "", "Target host (required)")
	baselineNameFlag(cmd)
	_ = cmd.MarkFlagRequired("host")
	return cmd
}

// newDriftBaselineDeleteCmd builds `levee drift baseline delete`.
func newDriftBaselineDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete one stored baseline of a host",
		Long: "Delete a single stored baseline. Without --baseline-name it " +
			"deletes the host's \"" + drift.DefaultBaselineName + "\" version; " +
			"the host's other versions are never removed as a side effect, so " +
			"dropping several takes one call per name.",
		Args: cobra.NoArgs,
		RunE: runDriftBaselineDelete,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "", "Target host (required)")
	baselineNameFlag(cmd)
	_ = cmd.MarkFlagRequired("host")
	return cmd
}

// runDriftBaselineSet executes `levee drift baseline set`.
func runDriftBaselineSet(cmd *cobra.Command, args []string) error {
	items, err := loadBaselineYAML(driftOptFile)
	if err != nil {
		return fmt.Errorf("drift baseline set: load file: %w", err)
	}

	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift baseline set: load baselines: %w", err)
	}

	baseline, err := bm.GenerateFromSnapshot(driftOptHost, driftOptBaselineName, "", items)
	if err != nil {
		return fmt.Errorf("drift baseline set: %w", err)
	}

	if err := saveBaselinesToDisk(bm); err != nil {
		return fmt.Errorf("drift baseline set: save: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  baselineToMap(baseline),
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, baseline.ID)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Baseline set for host %s as version %s\n", baseline.Host, baseline.Name)
	fmt.Fprintf(os.Stdout, "  ID: %s\n", baseline.ID)
	fmt.Fprintf(os.Stdout, "  Items: %d\n", len(baseline.Items))
	return nil
}

// runDriftBaselineAuto executes `levee drift baseline auto`.
func runDriftBaselineAuto(cmd *cobra.Command, args []string) error {
	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift baseline auto: load baselines: %w", err)
	}

	// Configure a snapshot source that reads from the rollback snapshot
	// store. For MVP we use a file-based source that reads baseline items
	// from <dataDir>/drift/snapshots/<runID>/<host>.json.
	// SetSnapshotSource mutates a process-wide global, so restore the
	// default no-op source on the way out (nil is the documented default).
	src := newFileSnapshotSource()
	drift.SetSnapshotSource(src)
	defer drift.SetSnapshotSource(nil)

	baseline, err := bm.AutoGenerate(driftOptHost, driftOptBaselineName, driftOptRunID)
	if err != nil {
		return fmt.Errorf("drift baseline auto: %w", err)
	}

	if err := saveBaselinesToDisk(bm); err != nil {
		return fmt.Errorf("drift baseline auto: save: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  baselineToMap(baseline),
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, baseline.ID)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Baseline auto-generated for host %s as version %s from run %s\n",
		baseline.Host, baseline.Name, driftOptRunID)
	fmt.Fprintf(os.Stdout, "  ID: %s\n", baseline.ID)
	fmt.Fprintf(os.Stdout, "  Items: %d\n", len(baseline.Items))
	return nil
}

// runDriftBaselineList executes `levee drift baseline list`.
func runDriftBaselineList(cmd *cobra.Command, args []string) error {
	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift baseline list: load baselines: %w", err)
	}

	baselines := bm.List()
	if driftOptHost != "" {
		filtered := make([]*drift.Baseline, 0, 1)
		for _, b := range baselines {
			if b.Host == driftOptHost {
				filtered = append(filtered, b)
			}
		}
		baselines = filtered
	}
	rows := make([]map[string]any, 0, len(baselines))
	for _, b := range baselines {
		rows = append(rows, baselineToMap(b))
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  rows,
			"meta":  map[string]any{"count": len(rows)},
			"error": nil,
		})
	}
	if optQuiet {
		for _, b := range baselines {
			fmt.Fprintln(os.Stdout, b.ID)
		}
		return nil
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stdout, "No baselines found.")
		return nil
	}
	fmt.Fprintf(os.Stdout, "%-20s %-16s %-24s %-12s %-8s\n", "HOST", "NAME", "ID", "RUN", "ITEMS")
	for _, b := range baselines {
		fmt.Fprintf(os.Stdout, "%-20s %-16s %-24s %-12s %-8d\n",
			b.Host, b.Name, b.ID, b.SourceRunID, len(b.Items))
	}
	return nil
}

// runDriftBaselineShow executes `levee drift baseline show`.
func runDriftBaselineShow(cmd *cobra.Command, args []string) error {
	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift baseline show: load baselines: %w", err)
	}

	baseline, err := bm.Get(driftOptHost, driftOptBaselineName)
	if err != nil {
		return fmt.Errorf("drift baseline show: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  baselineToMap(baseline),
			"meta":  nil,
			"error": nil,
		})
	}
	fmt.Fprintf(os.Stdout, "Host: %s\n", baseline.Host)
	fmt.Fprintf(os.Stdout, "Name: %s\n", baseline.Name)
	fmt.Fprintf(os.Stdout, "ID: %s\n", baseline.ID)
	fmt.Fprintf(os.Stdout, "Source Run: %s\n", baseline.SourceRunID)
	fmt.Fprintf(os.Stdout, "Created: %s\n", baseline.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(os.Stdout, "Items:\n")
	for _, item := range baseline.Items {
		fmt.Fprintf(os.Stdout, "  - %s (%s) %s = %s\n",
			item.CheckName, item.Type, item.Path, item.ExpectedValue)
	}
	return nil
}

// runDriftBaselineDelete executes `levee drift baseline delete`.
func runDriftBaselineDelete(cmd *cobra.Command, args []string) error {
	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift baseline delete: load baselines: %w", err)
	}

	baseline, err := bm.Get(driftOptHost, driftOptBaselineName)
	if err != nil {
		return fmt.Errorf("drift baseline delete: %w", err)
	}
	if err := bm.Delete(baseline.Host, baseline.Name); err != nil {
		return fmt.Errorf("drift baseline delete: %w", err)
	}

	if err := saveBaselinesToDisk(bm); err != nil {
		return fmt.Errorf("drift baseline delete: save: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data": map[string]any{
				"host":               baseline.Host,
				"name":               baseline.Name,
				"baseline_id":        baseline.ID,
				"remaining_for_host": len(bm.ListHost(baseline.Host)),
				"deleted":            true,
			},
			"meta":  nil,
			"error": nil,
		})
	}
	fmt.Fprintf(os.Stdout, "Baseline deleted for host %s version %s\n", baseline.Host, baseline.Name)
	if remaining := bm.ListHost(baseline.Host); len(remaining) > 0 {
		names := make([]string, 0, len(remaining))
		for _, b := range remaining {
			names = append(names, b.Name)
		}
		fmt.Fprintf(os.Stdout, "  Still stored for this host: %s\n", strings.Join(names, ", "))
	}
	return nil
}

// --- schedule ---------------------------------------------------------------

// newDriftScheduleCmd builds the `levee drift schedule` parent command.
func newDriftScheduleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Manage drift detection schedules",
		Long:  "Manage drift detection schedules: add, list, remove, run.",
	}
	cmd.AddCommand(newDriftScheduleAddCmd())
	cmd.AddCommand(newDriftScheduleListCmd())
	cmd.AddCommand(newDriftScheduleRemoveCmd())
	cmd.AddCommand(newDriftScheduleRunCmd())
	return cmd
}

// newDriftScheduleAddCmd builds `levee drift schedule add`.
func newDriftScheduleAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a scheduled drift detection job",
		Long: "Add a scheduled drift detection job. Each run probes every " +
			"host in --hosts against that host's stored baseline version; " +
			"--baseline-name pins which version, and without it the job uses " +
			"the \"" + drift.DefaultBaselineName + "\" version. One name applies " +
			"to every host in the job.",
		Args: cobra.NoArgs,
		RunE: runDriftScheduleAdd,
	}
	cmd.Flags().StringVar(&driftOptName, "name", "", "Job name (required)")
	cmd.Flags().StringVar(&driftOptCron, "cron", "", "5-field cron expression (required)")
	cmd.Flags().StringVar(&driftOptHosts, "hosts", "", "Comma-separated target hosts (required)")
	cmd.Flags().BoolVar(&driftOptAlert, "alert", true, "Alert on drift")
	cmd.Flags().BoolVar(&driftOptEnabled, "enabled", true, "Enable the job")
	baselineNameFlag(cmd)
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("cron")
	_ = cmd.MarkFlagRequired("hosts")
	return cmd
}

// newDriftScheduleListCmd builds `levee drift schedule list`.
func newDriftScheduleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all scheduled drift detection jobs",
		Args:  cobra.NoArgs,
		RunE:  runDriftScheduleList,
	}
}

// newDriftScheduleRemoveCmd builds `levee drift schedule remove <job-id>`.
func newDriftScheduleRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <job-id>",
		Short: "Remove a scheduled drift detection job",
		Args:  cobra.ExactArgs(1),
		RunE:  runDriftScheduleRemove,
	}
}

// newDriftScheduleRunCmd builds `levee drift schedule run <job-id>`.
func newDriftScheduleRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <job-id>",
		Short: "Manually trigger a scheduled drift detection job",
		Args:  cobra.ExactArgs(1),
		RunE:  runDriftScheduleRun,
	}
}

// runDriftScheduleAdd executes `levee drift schedule add`.
func runDriftScheduleAdd(cmd *cobra.Command, args []string) error {
	hosts := parseTargetsFlag(driftOptHosts)
	if len(hosts) == 0 {
		return fmt.Errorf("drift schedule add: no hosts specified [exit=2]")
	}

	// Validate the pinned baseline version at write time. A job whose version
	// can never resolve is worse than a refused add: it would fail quietly at
	// 3am every night instead of here.
	baselineName, err := drift.NormalizeBaselineName(driftOptBaselineName)
	if err != nil {
		return fmt.Errorf("drift schedule add: %w", err)
	}

	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift schedule add: load baselines: %w", err)
	}
	prober := newLocalFileProber()
	detector := drift.NewDetector(prober)
	scheduler := drift.NewScheduler(detector, bm)

	// Load existing jobs.
	if err := loadJobsFromDisk(scheduler); err != nil {
		return fmt.Errorf("drift schedule add: load jobs: %w", err)
	}

	job := drift.DriftJob{
		Name:         driftOptName,
		CronExpr:     driftOptCron,
		Hosts:        hosts,
		Enabled:      driftOptEnabled,
		AlertOnDrift: driftOptAlert,
		BaselineName: baselineName,
	}
	added, err := scheduler.AddJob(job)
	if err != nil {
		return fmt.Errorf("drift schedule add: %w", err)
	}

	if err := saveJobsToDisk(scheduler); err != nil {
		return fmt.Errorf("drift schedule add: save: %w", err)
	}

	// added carries the scheduler-generated ID/NextRun: job is passed by
	// value to AddJob, so job.ID would still be empty here.
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  jobToMap(added),
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, added.ID)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Scheduled job added: %s\n", added.ID)
	fmt.Fprintf(os.Stdout, "  Name: %s\n", added.Name)
	fmt.Fprintf(os.Stdout, "  Cron: %s\n", added.CronExpr)
	fmt.Fprintf(os.Stdout, "  Hosts: %s\n", strings.Join(added.Hosts, ","))
	fmt.Fprintf(os.Stdout, "  Baseline version: %s\n", added.BaselineName)
	fmt.Fprintf(os.Stdout, "  Next run: %s\n", added.NextRun.Format(time.RFC3339))
	return nil
}

// runDriftScheduleList executes `levee drift schedule list`.
func runDriftScheduleList(cmd *cobra.Command, args []string) error {
	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift schedule list: load baselines: %w", err)
	}
	prober := newLocalFileProber()
	detector := drift.NewDetector(prober)
	scheduler := drift.NewScheduler(detector, bm)

	if err := loadJobsFromDisk(scheduler); err != nil {
		return fmt.Errorf("drift schedule list: load jobs: %w", err)
	}

	jobs := scheduler.ListJobs()
	rows := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, jobToMap(&j))
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  rows,
			"meta":  map[string]any{"count": len(rows)},
			"error": nil,
		})
	}
	if optQuiet {
		for _, j := range jobs {
			fmt.Fprintln(os.Stdout, j.ID)
		}
		return nil
	}
	if len(jobs) == 0 {
		fmt.Fprintln(os.Stdout, "No scheduled jobs found.")
		return nil
	}
	fmt.Fprintf(os.Stdout, "%-20s %-20s %-18s %-10s %-10s\n", "ID", "NAME", "CRON", "ENABLED", "HOSTS")
	for _, j := range jobs {
		fmt.Fprintf(os.Stdout, "%-20s %-20s %-18s %-10v %s\n",
			j.ID, j.Name, j.CronExpr, j.Enabled, strings.Join(j.Hosts, ","))
	}
	return nil
}

// runDriftScheduleRemove executes `levee drift schedule remove <job-id>`.
func runDriftScheduleRemove(cmd *cobra.Command, args []string) error {
	jobID := args[0]

	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift schedule remove: load baselines: %w", err)
	}
	prober := newLocalFileProber()
	detector := drift.NewDetector(prober)
	scheduler := drift.NewScheduler(detector, bm)

	if err := loadJobsFromDisk(scheduler); err != nil {
		return fmt.Errorf("drift schedule remove: load jobs: %w", err)
	}

	if err := scheduler.RemoveJob(jobID); err != nil {
		return fmt.Errorf("drift schedule remove: %w", err)
	}

	if err := saveJobsToDisk(scheduler); err != nil {
		return fmt.Errorf("drift schedule remove: save: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  map[string]any{"job_id": jobID, "removed": true},
			"meta":  nil,
			"error": nil,
		})
	}
	fmt.Fprintf(os.Stdout, "Job %s removed\n", jobID)
	return nil
}

// runDriftScheduleRun executes `levee drift schedule run <job-id>`.
func runDriftScheduleRun(cmd *cobra.Command, args []string) error {
	_ctx := context.Background()
	jobID := args[0]

	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift schedule run: load baselines: %w", err)
	}
	prober := newLocalFileProber()
	detector := drift.NewDetector(prober)
	scheduler := drift.NewScheduler(detector, bm)

	if err := loadJobsFromDisk(scheduler); err != nil {
		return fmt.Errorf("drift schedule run: load jobs: %w", err)
	}

	report, err := scheduler.RunOnce(_ctx, jobID)
	if err != nil {
		return fmt.Errorf("drift schedule run: %w", err)
	}

	if err := saveJobsToDisk(scheduler); err != nil {
		return fmt.Errorf("drift schedule run: save: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  reportToMap(report),
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, report.ID)
		return nil
	}
	fmt.Fprint(os.Stdout, report.ToTable())
	return nil
}

// --- report -----------------------------------------------------------------

// newDriftReportCmd builds `levee drift report`.
func newDriftReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Show drift report and trend for a host",
		Long: "Show the drift report and historical trend for a host. " +
			"--days limits the trend window (default 30).",
		Args: cobra.NoArgs,
		RunE: runDriftReport,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "", "Target host (required)")
	cmd.Flags().IntVar(&driftOptDays, "days", 30, "Trend window in days")
	_ = cmd.MarkFlagRequired("host")
	return cmd
}

// runDriftReport executes `levee drift report`.
func runDriftReport(cmd *cobra.Command, args []string) error {
	if driftOptHost == "" {
		return fmt.Errorf("drift report: no host specified (use --host) [exit=2]")
	}
	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift report: load baselines: %w", err)
	}
	prober := newLocalFileProber()
	detector := drift.NewDetector(prober)
	scheduler := drift.NewScheduler(detector, bm)

	if err := loadJobsFromDisk(scheduler); err != nil {
		return fmt.Errorf("drift report: load jobs: %w", err)
	}

	// Collect history from all jobs that cover this host.
	var history []*drift.DriftReport
	cutoff := time.Now().UTC().Add(-time.Duration(driftOptDays) * 24 * time.Hour)
	for _, j := range scheduler.ListJobs() {
		coversHost := false
		for _, h := range j.Hosts {
			if h == driftOptHost {
				coversHost = true
				break
			}
		}
		if !coversHost {
			continue
		}
		for _, r := range scheduler.GetHistory(j.ID) {
			if r.Timestamp.After(cutoff) {
				history = append(history, r)
			}
		}
	}

	trend := drift.AnalyzeTrend(history, driftOptHost)

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  trendToMap(trend),
			"meta":  map[string]any{"days": driftOptDays, "points": len(trend.Points)},
			"error": nil,
		})
	}
	fmt.Fprintf(os.Stdout, "Drift trend for host %s (last %d days)\n", driftOptHost, driftOptDays)
	fmt.Fprintf(os.Stdout, "Direction: %s\n", trend.TrendDirection)
	fmt.Fprintf(os.Stdout, "Average drift: %.2f\n", trend.AverageDrift)
	fmt.Fprintf(os.Stdout, "Data points: %d\n\n", len(trend.Points))
	if len(trend.Points) > 0 {
		fmt.Fprintf(os.Stdout, "%-24s %-12s %-10s\n", "TIMESTAMP", "DRIFTS", "HOSTS")
		for _, p := range trend.Points {
			fmt.Fprintf(os.Stdout, "%-24s %-12d %-10d\n",
				p.Timestamp.Format(time.RFC3339), p.DriftCount, p.HostCount)
		}
	}
	return nil
}

// --- Helpers ----------------------------------------------------------------

// driftDataDir returns the directory where drift baselines and jobs are
// persisted. It is derived from the LEVEE data directory.
func driftDataDir() (string, error) {
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}
	applySecurityConfig(cfg)
	dir := filepath.Join(cfg.Server.DataDir, "drift")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create drift dir: %w", err)
	}
	return dir, nil
}

// baselinesDir returns the directory for baseline files.
func baselinesDir() (string, error) {
	dir, err := driftDataDir()
	if err != nil {
		return "", err
	}
	bd := filepath.Join(dir, "baselines")
	if err := os.MkdirAll(bd, 0o750); err != nil {
		return "", fmt.Errorf("create baselines dir: %w", err)
	}
	return bd, nil
}

// jobsDir returns the directory for job files.
func jobsDir() (string, error) {
	dir, err := driftDataDir()
	if err != nil {
		return "", err
	}
	jd := filepath.Join(dir, "jobs")
	if err := os.MkdirAll(jd, 0o750); err != nil {
		return "", fmt.Errorf("create jobs dir: %w", err)
	}
	return jd, nil
}

// loadBaselinesFromDisk reads all baseline JSON files from the baselines
// directory and returns a populated BaselineManager.
func loadBaselinesFromDisk() (*drift.BaselineManager, error) {
	bm := drift.NewBaselineManager()

	bd, err := baselinesDir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(bd)
	if err != nil {
		return nil, fmt.Errorf("read baselines dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(bd, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue // skip unreadable files
		}
		var baseline drift.Baseline
		if err := json.Unmarshal(data, &baseline); err != nil {
			continue // skip malformed files
		}
		// The file's own record is the truth: a baseline written before named
		// versions existed has no name field and lands on the default
		// version. Two files claiming the same (host, name) collapse to the
		// one stored last, in ReadDir (lexical) order — and re-save as a
		// single file, which is the only shape that can round-trip.
		if err := bm.Set(&baseline); err != nil {
			continue
		}
	}
	return bm, nil
}

// saveBaselinesToDisk writes all baselines to the baselines directory as
// individual JSON files keyed by (host, name), deleting files whose baseline
// no longer exists (otherwise `baseline delete` would leave the file behind
// and every later command would resurrect the baseline from disk).
func saveBaselinesToDisk(bm *drift.BaselineManager) error {
	bd, err := baselinesDir()
	if err != nil {
		return err
	}

	listing := bm.List()
	owner := make(map[string]*drift.Baseline, len(listing))
	desired := make(map[string][]byte, len(listing))
	for _, b := range listing {
		stem := baselineFileStem(b.Host, b.Name)
		// The directory is reconciled by deleting every *.json it was not
		// asked to keep, so two versions that land on one stem do more than
		// overwrite each other — the loser is removed from disk as stale.
		// Refuse the whole write instead of guessing which version survives.
		if other, taken := owner[stem]; taken {
			return fmt.Errorf("drift: baselines %s/%s and %s/%s both map to file %q.json; rename one with --baseline-name",
				other.Host, other.Name, b.Host, b.Name, stem)
		}
		owner[stem] = b
		data, err := json.MarshalIndent(b, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal baseline: %w", err)
		}
		desired[stem] = data
	}
	return syncJSONDir(bd, desired)
}

// baselineFileStem maps a (host, name) pair to the stem of its JSON file. The
// default version keeps the pre-naming layout, "<host>", so an existing
// deployment loads and re-saves without a migration step; each named version
// adds one file, "<host>.<name>".
//
// The mapping is not injective — host "web-01" with version "prod" and host
// "web-01.prod" with the default version both give "web-01.prod". saveBaselines
// checks for that and refuses rather than letting one version silently delete
// the other.
func baselineFileStem(host string, name string) string {
	if name == drift.DefaultBaselineName {
		return host
	}
	return host + "." + name
}

// syncJSONDir reconciles dir with the desired set of entries, writing each
// blob as "<name>.json" and removing any *.json file whose stem is not
// desired. Names become file names, so path separators and dot-names are
// rejected (they would escape the directory).
func syncJSONDir(dir string, desired map[string][]byte) error {
	for name, data := range desired {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("invalid entry name %q", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if _, keep := desired[strings.TrimSuffix(entry.Name(), ".json")]; keep {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("remove stale %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// loadJobsFromDisk reads all job JSON files from the jobs directory and adds
// them to the scheduler.
func loadJobsFromDisk(s *drift.DriftScheduler) error {
	jd, err := jobsDir()
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(jd)
	if err != nil {
		return fmt.Errorf("read jobs dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(jd, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var job drift.DriftJob
		if err := json.Unmarshal(data, &job); err != nil {
			continue
		}
		// Re-add the job; ignore "already exists" errors from race
		// conditions.
		if _, err := s.AddJob(job); err != nil {
			continue
		}
	}
	return nil
}

// saveJobsToDisk writes all jobs to the jobs directory as individual JSON
// files keyed by job ID, deleting files whose job no longer exists (otherwise
// `schedule remove` would leave the file behind and the job would resurrect
// on the next command that loads jobs from disk).
func saveJobsToDisk(s *drift.DriftScheduler) error {
	jd, err := jobsDir()
	if err != nil {
		return err
	}

	desired := make(map[string][]byte, 0)
	for _, j := range s.ListJobs() {
		data, err := json.MarshalIndent(j, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal job: %w", err)
		}
		desired[j.ID] = data
	}
	return syncJSONDir(jd, desired)
}

// loadBaselineYAML reads a baseline YAML file and returns the parsed items.
func loadBaselineYAML(path string) ([]drift.BaselineItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read baseline file: %w", err)
	}

	// yamlBaselineItem is a YAML-specific view of BaselineItem. The drift
	// package uses json tags; YAML needs its own field mapping.
	type yamlBaselineItem struct {
		CheckName     string `yaml:"check_name"`
		Type          string `yaml:"type"`
		Path          string `yaml:"path"`
		ExpectedValue string `yaml:"expected_value"`
	}

	convert := func(items []yamlBaselineItem) []drift.BaselineItem {
		out := make([]drift.BaselineItem, len(items))
		for i, item := range items {
			out[i] = drift.BaselineItem{
				CheckName:     item.CheckName,
				Type:          drift.CheckType(item.Type),
				Path:          item.Path,
				ExpectedValue: item.ExpectedValue,
			}
		}
		return out
	}

	// The YAML file can be either a list of items or a map with an "items"
	// key.
	var yamlItems []yamlBaselineItem
	if err := yaml.Unmarshal(data, &yamlItems); err == nil && len(yamlItems) > 0 {
		return convert(yamlItems), nil
	}

	var wrapper struct {
		Items []yamlBaselineItem `yaml:"items"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("parse baseline yaml: %w", err)
	}
	if len(wrapper.Items) == 0 {
		return nil, fmt.Errorf("baseline file contains no items")
	}
	return convert(wrapper.Items), nil
}

// resolveBaseline returns the baseline for the given host. When source is
// "auto" it loads the stored baseline; otherwise it loads from a YAML file.
func resolveBaseline(_ctx context.Context, bm *drift.BaselineManager, host string, source string, name string) (*drift.Baseline, error) {
	if source == "auto" || source == "" {
		// An empty name is the manager's cue for the default version.
		return bm.Get(host, name)
	}
	// Load from file.
	items, err := loadBaselineYAML(source)
	if err != nil {
		return nil, fmt.Errorf("load baseline file: %w", err)
	}
	// A file source is a complete baseline handed over ad hoc. It is never
	// stored, so it carries no version label — inventing one here would make
	// the output claim a stored version that does not exist.
	return &drift.Baseline{
		ID:        "adhoc",
		Host:      host,
		CreatedAt: time.Now().UTC(),
		Items:     items,
	}, nil
}

// --- Local file prober ------------------------------------------------------

// localFileProber implements drift.StateProber by reading file content from
// the local filesystem. It supports CheckTypeFile; other check types return
// an error indicating the operation is unsupported in local mode.
type localFileProber struct{}

// newLocalFileProber returns a new localFileProber.
func newLocalFileProber() *localFileProber {
	return &localFileProber{}
}

// Probe reads the actual state for each check. For file checks it reads the
// file content; for other types it returns an error in the StateItem.
func (p *localFileProber) Probe(_ctx context.Context, host string, checks []drift.Check) ([]drift.StateItem, error) {
	items := make([]drift.StateItem, len(checks))
	for i, c := range checks {
		item := drift.StateItem{
			CheckName:     c.Name,
			ExpectedValue: c.ExpectedValue,
		}
		switch c.Type {
		case drift.CheckTypeFile:
			data, err := os.ReadFile(c.Path)
			if err != nil {
				item.ActualValue = ""
				item.Drifted = true
				item.Diff = fmt.Sprintf("read file %s: %v", c.Path, err)
			} else {
				item.ActualValue = string(data)
			}
		default:
			item.Drifted = true
			item.Diff = fmt.Sprintf("check type %q not supported in local mode", c.Type)
		}
		items[i] = item
	}
	return items, nil
}

// --- File snapshot source ---------------------------------------------------

// fileSnapshotSource implements drift.SnapshotSource by reading baseline items
// from <dataDir>/drift/snapshots/<runID>/<host>.json.
type fileSnapshotSource struct{}

// newFileSnapshotSource returns a new fileSnapshotSource.
func newFileSnapshotSource() *fileSnapshotSource {
	return &fileSnapshotSource{}
}

// ExtractItems reads the snapshot file for the given host and run.
func (s *fileSnapshotSource) ExtractItems(host string, runID string) ([]drift.BaselineItem, error) {
	dir, err := driftDataDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "snapshots", runID, host+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	var items []drift.BaselineItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parse snapshot: %w", err)
	}
	return items, nil
}

// --- Output helpers ---------------------------------------------------------

// baselineToMap converts a Baseline to a map for JSON output.
func baselineToMap(b *drift.Baseline) map[string]any {
	items := make([]map[string]any, 0, len(b.Items))
	for _, item := range b.Items {
		items = append(items, map[string]any{
			"check_name":     item.CheckName,
			"type":           string(item.Type),
			"path":           item.Path,
			"expected_value": item.ExpectedValue,
		})
	}
	return map[string]any{
		"id":            b.ID,
		"host":          b.Host,
		"name":          b.Name,
		"source_run_id": b.SourceRunID,
		"created_at":    b.CreatedAt.Format(time.RFC3339),
		"items":         items,
	}
}

// jobToMap converts a DriftJob to a map for JSON output.
func jobToMap(j *drift.DriftJob) map[string]any {
	m := map[string]any{
		"id":             j.ID,
		"name":           j.Name,
		"cron_expr":      j.CronExpr,
		"hosts":          j.Hosts,
		"enabled":        j.Enabled,
		"alert_on_drift": j.AlertOnDrift,
		// The stored value verbatim: jobs written before named versions
		// existed have no pin, and an empty pin means the default version.
		// Rendering it as "default" here would claim a stored choice nobody
		// made — `levee drift schedule add` writes the normalized label.
		"baseline_name": j.BaselineName,
	}
	if !j.LastRun.IsZero() {
		m["last_run"] = j.LastRun.Format(time.RFC3339)
	}
	if !j.NextRun.IsZero() {
		m["next_run"] = j.NextRun.Format(time.RFC3339)
	}
	return m
}

// reportToMap converts a DriftReport to a map for JSON output.
func reportToMap(r *drift.DriftReport) map[string]any {
	results := make([]map[string]any, 0, len(r.Results))
	for _, res := range r.Results {
		if res == nil {
			continue
		}
		items := make([]map[string]any, 0, len(res.Items))
		for _, item := range res.Items {
			items = append(items, map[string]any{
				"check_name":     item.CheckName,
				"actual_value":   item.ActualValue,
				"expected_value": item.ExpectedValue,
				"drifted":        item.Drifted,
				"diff":           item.Diff,
			})
		}
		results = append(results, map[string]any{
			"host":         res.Host,
			"timestamp":    res.Timestamp.Format(time.RFC3339),
			"items":        items,
			"drift_count":  res.DriftCount,
			"total_checks": res.TotalChecks,
		})
	}
	return map[string]any{
		"id":           r.ID,
		"timestamp":    r.Timestamp.Format(time.RFC3339),
		"results":      results,
		"total_hosts":  r.TotalHosts,
		"total_drifts": r.TotalDrifts,
		"total_checks": r.TotalChecks,
		"summary":      r.Summary,
	}
}

// trendToMap converts a DriftTrend to a map for JSON output.
func trendToMap(t *drift.DriftTrend) map[string]any {
	points := make([]map[string]any, 0, len(t.Points))
	for _, p := range t.Points {
		points = append(points, map[string]any{
			"timestamp":   p.Timestamp.Format(time.RFC3339),
			"drift_count": p.DriftCount,
			"host_count":  p.HostCount,
		})
	}
	return map[string]any{
		"host":            t.Host,
		"points":          points,
		"trend_direction": t.TrendDirection,
		"average_drift":   t.AverageDrift,
	}
}
