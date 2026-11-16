// SQL review CLI for LEVEE.
//
// This file implements the `levee sqlreview` command:
//
//   levee sqlreview --sql "DELETE FROM t"
//   levee sqlreview --file change.sql
//   levee sqlreview --change <change-id>
//
// The third form is the alignment path with the DB Change Ticket: it reviews
// the SQL an approved change actually carries and compares the fresh verdict
// with the one frozen into the plan artifact (the one plan_hash binds and
// the approval signed). A difference means the rule set moved since the
// change was approved — which is a fact about the change, not a number to
// hide.
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
	sqlreviewOptSQL    string
	sqlreviewOptFile   string
	sqlreviewOptChange string
)

func init() {
	RegisterCommand(newSQLReviewCmd())
}

// newSQLReviewCmd builds the `levee sqlreview` sub-command.
func newSQLReviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sqlreview (--sql <batch> | --file <path> | --change <change-id>)",
		Short: "Review a SQL batch against the change-governance rule set",
		Long: "Review a SQL batch against LEVEE's change-governance rule set.\n\n" +
			"The batch is one or more statements separated by semicolons. Every\n" +
			"statement is parsed with the embedded Apache-2.0 parser and scored by\n" +
			"the in-house rule set (SR-01..SR-20); a statement the grammar cannot\n" +
			"parse fails review instead of passing silently, and names the other SQL\n" +
			"dialect it looks like when it carries one (the grammar is frozen at the\n" +
			"MySQL 5.7 era). A DDL the grammar reads only partway warns instead: the\n" +
			"rules scored a prefix, not your statement, so confirm it by hand.\n" +
			"Exit code is 1 when the verdict is fail, so pipelines can gate on the\n" +
			"review result.\n\n" +
			"--change <change-id> reviews the SQL an approved change carries and\n" +
			"compares it with the verdict frozen into its plan artifact.",
		Args: cobra.NoArgs,
		RunE: runSQLReview,
	}
	cmd.Flags().StringVar(&sqlreviewOptSQL, "sql", "", "SQL batch to review (statements separated by semicolons)")
	cmd.Flags().StringVar(&sqlreviewOptFile, "file", "", "path to a .sql file holding the batch to review")
	cmd.Flags().StringVar(&sqlreviewOptChange, "change", "", "review the SQL an approved change carries and compare it with the plan's frozen verdict")
	return cmd
}

// runSQLReview executes the `levee sqlreview` command.
func runSQLReview(cmd *cobra.Command, args []string) error {
	given := 0
	for _, v := range []string{sqlreviewOptSQL, sqlreviewOptFile, sqlreviewOptChange} {
		if v != "" {
			given++
		}
	}
	if given != 1 {
		return fmt.Errorf("sqlreview: pass exactly one of --sql, --file or --change")
	}
	if sqlreviewOptChange != "" {
		return runSQLReviewChange(sqlreviewOptChange)
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
