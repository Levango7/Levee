// SQL review CLI for LEVEE.
//
// This file implements the `levee sqlreview` command:
//
//   levee sqlreview --sql "DELETE FROM t"
//   levee sqlreview --file change.sql
//
// The batch is scored by internal/sqlreview, the in-house rule engine that
// sits on top of the embedded Apache-2.0 parser. The command is the demo
// surface for the database-change-publication wedge and the same engine the
// plan-time pre-check calls (see docs/product-roadmap.md). Exit code is 1
// when the verdict is fail, so pipelines can gate on the review result.

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/sqlreview"
)

// sqlreview command option variables.
var (
	sqlreviewOptSQL  string
	sqlreviewOptFile string
)

func init() {
	RegisterCommand(newSQLReviewCmd())
}

// newSQLReviewCmd builds the `levee sqlreview` sub-command.
func newSQLReviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sqlreview (--sql <batch> | --file <path>)",
		Short: "Review a SQL batch against the change-governance rule set",
		Long: "Review a SQL batch against LEVEE's change-governance rule set.\n\n" +
			"The batch is one or more statements separated by semicolons. Every\n" +
			"statement is parsed with the embedded Apache-2.0 parser and scored by\n" +
			"the in-house rule set (SR-01..SR-20); a statement the grammar cannot\n" +
			"parse fails review instead of passing silently. Exit code is 1 when the\n" +
			"verdict is fail, so pipelines can gate on the review result.",
		Args: cobra.NoArgs,
		RunE: runSQLReview,
	}
	cmd.Flags().StringVar(&sqlreviewOptSQL, "sql", "", "SQL batch to review (statements separated by semicolons)")
	cmd.Flags().StringVar(&sqlreviewOptFile, "file", "", "path to a .sql file holding the batch to review")
	return cmd
}

// runSQLReview executes the `levee sqlreview` command.
func runSQLReview(cmd *cobra.Command, args []string) error {
	if (sqlreviewOptSQL == "") == (sqlreviewOptFile == "") {
		return fmt.Errorf("sqlreview: pass exactly one of --sql or --file")
	}
	batch := sqlreviewOptSQL
	if sqlreviewOptFile != "" {
		src, err := os.ReadFile(sqlreviewOptFile)
		if err != nil {
			return fmt.Errorf("sqlreview: read file: %w", err)
		}
		batch = string(src)
	}

	res, err := sqlreview.Review(batch)
	if err != nil {
		return err
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  res,
			"meta":  nil,
			"error": nil,
		})
	}

	printSQLReviewHuman(os.Stdout, res)

	if res.Verdict == sqlreview.VerdictFail {
		return fmt.Errorf("sqlreview: verdict fail (%d failing findings)", res.FailCount)
	}
	return nil
}

// printSQLReviewHuman renders the report an operator reads in a terminal.
// Batch-level findings carry statement index 0 and print under their own
// heading so they are not mistaken for a verdict on one statement.
func printSQLReviewHuman(w io.Writer, res *sqlreview.ReviewResult) {
	for _, sr := range res.Statements {
		fmt.Fprintf(w, "statement %d (%s): %s\n", sr.Index, sr.Kind, sr.Text)
		for _, f := range sr.Findings {
			fmt.Fprintf(w, "  [%s] %s: %s\n", f.Severity, f.Rule, f.Message)
		}
	}
	for _, f := range res.Findings {
		if f.Statement != 0 {
			continue
		}
		fmt.Fprintf(w, "batch: [%s] %s: %s\n", f.Severity, f.Rule, f.Message)
	}
	fmt.Fprintf(w, "verdict: %s (%d fail, %d warn)\n", res.Verdict, res.FailCount, res.WarnCount)
}
