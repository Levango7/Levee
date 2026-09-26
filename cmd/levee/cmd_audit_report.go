package main

import (
	"context"
	"fmt"
	"html/template"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

// Audit report option variables.
var (
	auditReportOptSince  string
	auditReportOptUntil  string
	auditReportOptOutput string
	auditReportOptLimit  int
)

// newAuditReportCmd builds the `levee audit report` sub-command.
//
// The command produces a self-contained HTML compliance report for a time
// window: every change run with its approval chain, the hash-chain
// verification verdict per run, rollback outcomes and a summary block.
// It is the "one command for the regulator" deliverable — everything a
// compliance reviewer needs without access to the live system.
func newAuditReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Generate a compliance report (HTML)",
		Long: "Generate a self-contained HTML compliance report for a " +
			"time window: all change runs with approval chains, hash-chain " +
			"verification verdicts and rollback records. Output defaults to " +
			"stdout; use --output to write a file.",
		Args: cobra.NoArgs,
		RunE: runAuditReport,
	}
	cmd.Flags().StringVar(&auditReportOptSince, "since", "", "Window start (inclusive), e.g. 2026-09-01 or 2026-09-01T00:00:00Z")
	cmd.Flags().StringVar(&auditReportOptUntil, "until", "", "Window end (exclusive), e.g. 2026-10-01 or 2026-10-01T00:00:00Z (default: now)")
	cmd.Flags().StringVar(&auditReportOptOutput, "output", "", "Write the report to this file instead of stdout")
	cmd.Flags().IntVar(&auditReportOptLimit, "limit", 1000, "Maximum number of runs to include (safety cap)")
	return cmd
}

// auditReportRun is the per-run section of the report.
type auditReportRun struct {
	ID           string
	Workflow     string
	Status       string
	Creator      string
	CreatedAt    string
	UpdatedAt    string
	ChainValid   bool
	ChainCount   int
	ChainBroken  int
	Approvals    []*state.Approval
	RollbackRuns []*state.Audit
	TraceCount   int
}

// auditReportData is the template payload.
type auditReportData struct {
	GeneratedAt   string
	Since         string
	Until         string
	TotalRuns     int
	CompletedRuns int
	FailedRuns    int
	RolledBack    int
	Cancelled     int
	Interrupted   int
	Running       int
	ChainsValid   int
	ChainsBroken  int
	Runs          []auditReportRun
}

// parseReportTime accepts date-only (2026-09-01, UTC midnight) or full
// RFC3339 timestamps. Empty returns the zero time.
func parseReportTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q: use 2026-09-01 or RFC3339", s)
}

// auditReportWindow is the validated inclusive/exclusive time window.
type auditReportWindow struct {
	since time.Time
	until time.Time
}

func parseAuditReportWindow() (auditReportWindow, error) {
	since, err := parseReportTime(auditReportOptSince)
	if err != nil {
		return auditReportWindow{}, fmt.Errorf("--since: %w", err)
	}
	until, err := parseReportTime(auditReportOptUntil)
	if err != nil {
		return auditReportWindow{}, fmt.Errorf("--until: %w", err)
	}
	if !until.IsZero() && !since.IsZero() && !until.After(since) {
		return auditReportWindow{}, fmt.Errorf("--until must be after --since [exit=2]")
	}
	return auditReportWindow{since: since, until: until}, nil
}

func (w auditReportWindow) contains(t time.Time) bool {
	return (w.since.IsZero() || !t.Before(w.since)) &&
		(w.until.IsZero() || t.Before(w.until))
}

func collectAuditReport(ctx context.Context, store state.Store, runs []*state.Run, window auditReportWindow) (auditReportData, error) {
	verifier, err := audit.NewChainVerifier(store)
	if err != nil {
		return auditReportData{}, fmt.Errorf("create chain verifier: %w", err)
	}
	data := auditReportData{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Since:       auditReportOptSince,
		Until:       auditReportOptUntil,
	}
	for _, run := range runs {
		// RunFilter has no timestamp fields yet; keep the store interface
		// unchanged and apply the validated window in memory.
		if !window.contains(run.CreatedAt) {
			continue
		}
		section := auditReportRun{
			ID: run.ID, Workflow: run.WorkflowName, Status: run.Status,
			Creator: run.Creator, CreatedAt: run.CreatedAt.Format(time.RFC3339),
			UpdatedAt: run.UpdatedAt.Format(time.RFC3339),
		}
		if approvals, err := store.ListApprovals(ctx, state.ApprovalFilter{RunID: run.ID}); err == nil {
			section.Approvals = approvals
		}
		if result, err := verifier.Verify(ctx, run.ID); err == nil {
			section.ChainValid = result.Valid
			section.ChainCount = result.Count
			section.ChainBroken = len(result.Failures)
			if result.Valid {
				data.ChainsValid++
			} else {
				data.ChainsBroken++
			}
		}
		if traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: run.ID}); err == nil {
			section.TraceCount = len(traces)
		}
		if audits, err := store.ListAudits(ctx, state.AuditFilter{RunID: run.ID}); err == nil {
			for _, a := range audits {
				if strings.Contains(strings.ToLower(a.Action), "rollback") {
					section.RollbackRuns = append(section.RollbackRuns, a)
				}
			}
		}
		data.Runs = append(data.Runs, section)
		countAuditReportStatus(&data, run.Status)
	}
	return data, nil
}

func countAuditReportStatus(data *auditReportData, status string) {
	data.TotalRuns++
	// Labels come from runstatus so a report never silently stops counting
	// a status because the string was retyped here.
	switch status {
	case runstatus.StatusCompleted:
		data.CompletedRuns++
	case runstatus.StatusFailed, runstatus.StatusRollbackIncomplete:
		data.FailedRuns++
	case runstatus.StatusRolledBack, runstatus.StatusRolledBackPartial:
		data.RolledBack++
	case runstatus.StatusCancelled:
		data.Cancelled++
	case runstatus.StatusInterrupted:
		data.Interrupted++
	case runstatus.StatusRunning, runstatus.StatusPaused:
		data.Running++
	}
}

func writeAuditReport(data auditReportData) error {
	out := os.Stdout
	if auditReportOptOutput != "" {
		f, err := os.Create(auditReportOptOutput)
		if err != nil {
			return fmt.Errorf("create output file: %w", err)
		}
		defer func() { _ = f.Close() }()
		out = f
	}
	tmpl, err := template.New("report").Parse(auditReportTemplate)
	if err != nil {
		return fmt.Errorf("parse report template: %w", err)
	}
	if err := tmpl.Execute(out, data); err != nil {
		return fmt.Errorf("render report: %w", err)
	}
	return nil
}

// runAuditReport executes the `levee audit report` command.
func runAuditReport(_ *cobra.Command, _ []string) error {
	ctx := context.Background()
	window, err := parseAuditReportWindow()
	if err != nil {
		return err
	}
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()
	runs, err := store.ListRuns(ctx, state.RunFilter{Limit: auditReportOptLimit})
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	data, err := collectAuditReport(ctx, store, runs, window)
	if err != nil {
		return err
	}
	return writeAuditReport(data)
}

// auditReportTemplate is the self-contained HTML template. It deliberately
// inlines all CSS (no external resources) so the exported file is a single
// artifact that can be archived or e-mailed as-is.
const auditReportTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>LEVEE Compliance Report</title>
<style>
  body { font-family: -apple-system, "Segoe UI", sans-serif; margin: 2rem; color: #1f2933; }
  h1 { border-bottom: 3px solid #1b6ec2; padding-bottom: .4rem; }
  h2 { margin-top: 2rem; color: #1b6ec2; }
  table { border-collapse: collapse; width: 100%; margin: 1rem 0; font-size: .9rem; }
  th, td { border: 1px solid #cbd2d9; padding: .45rem .7rem; text-align: left; vertical-align: top; }
  th { background: #eef4fb; }
  .ok { color: #0b7a3e; font-weight: 600; }
  .broken { color: #b3261e; font-weight: 600; }
  .status-completed { color: #0b7a3e; }
  .status-failed { color: #b3261e; }
  .status-rolled_back { color: #a3571f; }
  .status-rolled_back_partial { color: #a3571f; }
  .status-rollback_incomplete { color: #b3261e; }
  .status-interrupted { color: #b3261e; }
  .summary td { font-size: 1rem; }
  .meta { color: #52606d; font-size: .85rem; margin-bottom: 1.5rem; }
  .run { border: 1px solid #cbd2d9; border-radius: 6px; padding: 1rem 1.25rem; margin: 1rem 0; }
  .run h3 { margin: 0 0 .5rem; font-size: 1.05rem; }
  .badge { display: inline-block; padding: .1rem .5rem; border-radius: 4px; font-size: .8rem; background: #eef4fb; }
</style>
</head>
<body>
<h1>LEVEE Compliance Report</h1>
<p class="meta">
  Generated: {{.GeneratedAt}}
  {{if .Since}}&nbsp;|&nbsp;Window: {{.Since}} → {{if .Until}}{{.Until}}{{else}}now{{end}}{{end}}
</p>

<h2>Summary</h2>
<table class="summary">
  <tr><th>Total change runs</th><td>{{.TotalRuns}}</td></tr>
  <tr><th>Completed</th><td>{{.CompletedRuns}}</td></tr>
  <tr><th>Failed</th><td>{{.FailedRuns}}</td></tr>
  <tr><th>Rolled back</th><td>{{.RolledBack}}</td></tr>
  <tr><th>Cancelled</th><td>{{.Cancelled}}</td></tr>
  <tr><th>Interrupted (takeover)</th><td>{{.Interrupted}}</td></tr>
  <tr><th>In flight (running/paused)</th><td>{{.Running}}</td></tr>
  <tr><th>Hash chains verified intact</th><td><span class="ok">{{.ChainsValid}}</span></td></tr>
  <tr><th>Hash chains broken</th><td>{{if .ChainsBroken}}<span class="broken">{{.ChainsBroken}}</span>{{else}}0{{end}}</td></tr>
</table>

<h2>Change runs ({{.TotalRuns}})</h2>
{{range .Runs}}
<div class="run">
  <h3>{{.ID}} <span class="badge status-{{.Status}}">{{.Status}}</span></h3>
  <table>
    <tr><th>Workflow</th><td>{{.Workflow}}</td></tr>
    <tr><th>Creator</th><td>{{.Creator}}</td></tr>
    <tr><th>Created</th><td>{{.CreatedAt}}</td></tr>
    <tr><th>Last update</th><td>{{.UpdatedAt}}</td></tr>
    <tr>
      <th>Hash chain</th>
      <td>{{if .ChainValid}}<span class="ok">intact ({{.ChainCount}} records)</span>
          {{else if .ChainCount}}<span class="broken">BROKEN ({{.ChainBroken}} of {{.ChainCount}} records)</span>
          {{else}}no trace records{{end}}</td>
    </tr>
    <tr><th>Trace records</th><td>{{.TraceCount}}</td></tr>
  </table>

  {{if .Approvals}}
  <p><strong>Approval chain</strong></p>
  <table>
    <tr><th>Level</th><th>Approver</th><th>Status</th><th>Acted at</th><th>Comment</th></tr>
    {{range .Approvals}}
    <tr><td>{{.Level}}</td><td>{{.Approver}}</td><td>{{.Status}}</td><td>{{if .ActedAt.IsZero}}-{{else}}{{.ActedAt.Format "2006-01-02T15:04:05Z"}}{{end}}</td><td>{{.Comment}}</td></tr>
    {{end}}
  </table>
  {{end}}

  {{if .RollbackRuns}}
  <p><strong>Rollback records</strong></p>
  <table>
    <tr><th>Action</th><th>Actor</th><th>Result</th><th>Timestamp</th></tr>
    {{range .RollbackRuns}}
    <tr><td>{{.Action}}</td><td>{{.Actor}}</td><td>{{.Result}}</td><td>{{.Timestamp.Format "2006-01-02T15:04:05Z"}}</td></tr>
    {{end}}
  </table>
  {{end}}
</div>
{{else}}
<p>No change runs in the selected window.</p>
{{end}}

<p class="meta">Report generated by <code>levee audit report</code>. Hash-chain verdicts are produced by the same ChainVerifier that powers <code>levee audit verify</code>.</p>
</body>
</html>
`
