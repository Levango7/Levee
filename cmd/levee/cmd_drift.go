// Drift detection CLI for LEVEE.
//
// This file implements the `levee drift` command tree:
//
//   levee drift detect --host <host> [--baseline <file|auto>]
//   levee drift detect --hosts <h1,h2,...> [--baseline <file|auto>]
//   levee drift baseline set --host <host> --file <baseline.yaml>
//   levee drift baseline auto --host <host> --run <run-id>
//   levee drift baseline list
//   levee drift baseline show --host <host>
//   levee drift baseline delete --host <host>
//   levee drift schedule add --name <name> --cron <expr> --hosts <h1,h2,...>
//   levee drift schedule list
//   levee drift schedule remove <job-id>
//   levee drift schedule run <job-id>
//   levee drift report --host <host> [--days <n>]
//
// Baselines and jobs are persisted as JSON files under <dataDir>/drift/ so
// they survive across CLI invocations. The detector uses a local-file prober
// that reads file content from the local filesystem; remote probing (SSH /
// WinRM) is left as a future enhancement.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/drift"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// drift command option variables.
var (
	driftOptHost     string
	driftOptHosts    string
	driftOptBaseline string // "auto" or a file path
	driftOptFile     string // baseline file for `baseline set`
	driftOptRunID    string // run id for `baseline auto`
	driftOptName     string
	driftOptCron     string
	driftOptDays     int
	driftOptAlert    bool
	driftOptEnabled  bool
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

// --- detect -----------------------------------------------------------------

// newDriftDetectCmd builds the `levee drift detect` sub-command.
func newDriftDetectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "detect",
		Short: "Run drift detection on one or more hosts",
		Long: "Run drift detection on one or more hosts against a baseline. " +
			"Use --host for a single host or --hosts for a comma-separated " +
			"list. --baseline accepts 'auto' (use the stored baseline for " +
			"the host) or a path to a baseline YAML file.",
		Args: cobra.NoArgs,
		RunE: runDriftDetect,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "", "Single target host")
	cmd.Flags().StringVar(&driftOptHosts, "hosts", "", "Comma-separated target hosts")
	cmd.Flags().StringVar(&driftOptBaseline, "baseline", "auto", "Baseline source: 'auto' or file path")
	return cmd
}

// runDriftDetect executes `levee drift detect`.
func runDriftDetect(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	hosts := parseTargetsFlag(driftOptHosts)
	if driftOptHost != "" {
		hosts = append(hosts, driftOptHost)
	}
	if len(hosts) == 0 {
		return fmt.Errorf("drift detect: no hosts specified (use --host or --hosts) [exit=2]")
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
		baseline, err := resolveBaseline(ctx, bm, host, driftOptBaseline)
		if err != nil {
			return fmt.Errorf("drift detect: resolve baseline for %q: %w", host, err)
		}
		r, err := detector.Detect(ctx, host, baseline)
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
			"expected_value).",
		Args: cobra.NoArgs,
		RunE: runDriftBaselineSet,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "", "Target host (required)")
	cmd.Flags().StringVar(&driftOptFile, "file", "", "Baseline YAML file (required)")
	cmd.MarkFlagRequired("host")
	cmd.MarkFlagRequired("file")
	return cmd
}

// newDriftBaselineAutoCmd builds `levee drift baseline auto`.
func newDriftBaselineAutoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auto",
		Short: "Auto-generate a baseline from an apply snapshot",
		Long: "Auto-generate the drift baseline for a host from the most " +
			"recent apply snapshot of the given run.",
		Args: cobra.NoArgs,
		RunE: runDriftBaselineAuto,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "", "Target host (required)")
	cmd.Flags().StringVar(&driftOptRunID, "run", "", "Source run ID (required)")
	cmd.MarkFlagRequired("host")
	cmd.MarkFlagRequired("run")
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
	return cmd
}

// newDriftBaselineShowCmd builds `levee drift baseline show`.
func newDriftBaselineShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show the baseline for a host",
		Args:  cobra.NoArgs,
		RunE:  runDriftBaselineShow,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "", "Target host (required)")
	cmd.MarkFlagRequired("host")
	return cmd
}

// newDriftBaselineDeleteCmd builds `levee drift baseline delete`.
func newDriftBaselineDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete the baseline for a host",
		Args:  cobra.NoArgs,
		RunE:  runDriftBaselineDelete,
	}
	cmd.Flags().StringVar(&driftOptHost, "host", "", "Target host (required)")
	cmd.MarkFlagRequired("host")
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

	baseline, err := bm.GenerateFromSnapshot(driftOptHost, "", items)
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
	fmt.Fprintf(os.Stdout, "Baseline set for host %s\n", driftOptHost)
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
	src := newFileSnapshotSource()
	drift.SetSnapshotSource(src)

	baseline, err := bm.AutoGenerate(driftOptHost, driftOptRunID)
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
	fmt.Fprintf(os.Stdout, "Baseline auto-generated for host %s from run %s\n", driftOptHost, driftOptRunID)
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
	fmt.Fprintf(os.Stdout, "%-20s %-24s %-12s %-8s\n", "HOST", "ID", "RUN", "ITEMS")
	for _, b := range baselines {
		fmt.Fprintf(os.Stdout, "%-20s %-24s %-12s %-8d\n",
			b.Host, b.ID, b.SourceRunID, len(b.Items))
	}
	return nil
}

// runDriftBaselineShow executes `levee drift baseline show`.
func runDriftBaselineShow(cmd *cobra.Command, args []string) error {
	bm, err := loadBaselinesFromDisk()
	if err != nil {
		return fmt.Errorf("drift baseline show: load baselines: %w", err)
	}

	baseline, err := bm.Get(driftOptHost)
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

	if err := bm.Delete(driftOptHost); err != nil {
		return fmt.Errorf("drift baseline delete: %w", err)
	}

	if err := saveBaselinesToDisk(bm); err != nil {
		return fmt.Errorf("drift baseline delete: save: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  map[string]any{"host": driftOptHost, "deleted": true},
			"meta":  nil,
			"error": nil,
		})
	}
	fmt.Fprintf(os.Stdout, "Baseline deleted for host %s\n", driftOptHost)
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
		Args:  cobra.NoArgs,
		RunE:  runDriftScheduleAdd,
	}
	cmd.Flags().StringVar(&driftOptName, "name", "", "Job name (required)")
	cmd.Flags().StringVar(&driftOptCron, "cron", "", "5-field cron expression (required)")
	cmd.Flags().StringVar(&driftOptHosts, "hosts", "", "Comma-separated target hosts (required)")
	cmd.Flags().BoolVar(&driftOptAlert, "alert", true, "Alert on drift")
	cmd.Flags().BoolVar(&driftOptEnabled, "enabled", true, "Enable the job")
	cmd.MarkFlagRequired("name")
	cmd.MarkFlagRequired("cron")
	cmd.MarkFlagRequired("hosts")
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
	}
	if err := scheduler.AddJob(job); err != nil {
		return fmt.Errorf("drift schedule add: %w", err)
	}

	if err := saveJobsToDisk(scheduler); err != nil {
		return fmt.Errorf("drift schedule add: save: %w", err)
	}

	added, _ := scheduler.GetJob(job.ID)
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  jobToMap(added),
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, job.ID)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Scheduled job added: %s\n", job.ID)
	fmt.Fprintf(os.Stdout, "  Name: %s\n", added.Name)
	fmt.Fprintf(os.Stdout, "  Cron: %s\n", added.CronExpr)
	fmt.Fprintf(os.Stdout, "  Hosts: %s\n", strings.Join(added.Hosts, ","))
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
	ctx := context.Background()
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

	report, err := scheduler.RunOnce(ctx, jobID)
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
	cmd.MarkFlagRequired("host")
	return cmd
}

// runDriftReport executes `levee drift report`.
func runDriftReport(cmd *cobra.Command, args []string) error {
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
	dir := filepath.Join(cfg.Server.DataDir, "drift")
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	if err := os.MkdirAll(bd, 0o755); err != nil {
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
	if err := os.MkdirAll(jd, 0o755); err != nil {
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
		if err := bm.Set(baseline.Host, &baseline); err != nil {
			continue
		}
	}
	return bm, nil
}

// saveBaselinesToDisk writes all baselines to the baselines directory as
// individual JSON files keyed by host.
func saveBaselinesToDisk(bm *drift.BaselineManager) error {
	bd, err := baselinesDir()
	if err != nil {
		return err
	}

	for _, b := range bm.List() {
		data, err := json.MarshalIndent(b, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal baseline: %w", err)
		}
		filename := filepath.Join(bd, b.Host+".json")
		if err := os.WriteFile(filename, data, 0o644); err != nil {
			return fmt.Errorf("write baseline: %w", err)
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
		if err := s.AddJob(job); err != nil {
			continue
		}
	}
	return nil
}

// saveJobsToDisk writes all jobs to the jobs directory as individual JSON
// files keyed by job ID.
func saveJobsToDisk(s *drift.DriftScheduler) error {
	jd, err := jobsDir()
	if err != nil {
		return err
	}

	for _, j := range s.ListJobs() {
		data, err := json.MarshalIndent(j, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal job: %w", err)
		}
		filename := filepath.Join(jd, j.ID+".json")
		if err := os.WriteFile(filename, data, 0o644); err != nil {
			return fmt.Errorf("write job: %w", err)
		}
	}
	return nil
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
func resolveBaseline(ctx context.Context, bm *drift.BaselineManager, host string, source string) (*drift.Baseline, error) {
	if source == "auto" || source == "" {
		return bm.Get(host)
	}
	// Load from file.
	items, err := loadBaselineYAML(source)
	if err != nil {
		return nil, fmt.Errorf("load baseline file: %w", err)
	}
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
func (p *localFileProber) Probe(ctx context.Context, host string, checks []drift.Check) ([]drift.StateItem, error) {
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
