// review_steps.go is the shared core of SQL review: score a set of
// (step, SQL) pairs with the review engine and aggregate the outcome into the
// artifact-mirror summary.
//
// Why it lives here rather than in the wiring package: two callers must reach
// the SAME verdict for the SAME SQL, and a second implementation is how they
// stop agreeing.
//
//   - the plan-time gate (internal/wiring/sqlreview_gate.go) scores a
//     workflow before the plan is generated; its verdict is stamped onto the
//     artifact and bound by plan_hash, so it is the fact the approval signed;
//   - the re-check path (`levee sqlreview --change <id>`) scores the SQL
//     carried by an already-approved artifact, so an operator can see whether
//     the current rule set still agrees with the verdict that was frozen into
//     the plan.
//
// The second caller is a re-computation, and a re-computation that can differ
// from the original is only meaningful if it is the same computation. Sharing
// the core is what makes "the rules changed" a statement about the rules
// rather than about two implementations drifting apart.

package plan

import (
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/sqlreview"
)

// SQLStepInput is one reviewable SQL statement with the step it belongs to.
// The step name is the anchor the artifact's findings carry, so a finding
// always points back at the workflow step that produced it.
type SQLStepInput struct {
	Step string
	SQL  string
	// Dialect is routing metadata only. It is not serialised into the plan
	// artifact or plan_hash: the SQL text is the artifact, not the parser choice.
	Dialect sqlreview.Dialect
}

// ReviewableSQL returns the SQL batch a step carries, and whether the step is
// one the review engine can score at all.
//
// This predicate is shared on purpose. Three callers ask the same question
// about the same workflow and must get the same answer: the plan-time gate
// (does this change get reviewed before an artifact is minted?), the DB Change
// Ticket (what SQL does this change carry?), and `levee sqlreview --change`
// (what do I re-score?). Each used to carry its own copy of "module == mysql
// && action == query" — and when the pg.query module landed, all three copies
// were wrong at once: PostgreSQL steps were not reviewed before approval, the
// ticket said the change carried no SQL, and the re-check found no surface to
// compare. Three implementations of one rule is three chances to disagree.
//
// The rule: a step is reviewable when it carries a whole SQL batch in its
// `sql` argument. Today that is exactly the two query actions (mysql.query,
// pg.query). mysql.pt_osc is deliberately absent: its `alter` argument is a
// clause fragment, not a statement, and feeding a fragment to the engine
// would score a statement nobody wrote — synthesising "ALTER TABLE <table>
// <fragment>" is a separate product decision, and until it is taken the
// honest position is that pt_osc's alter clause is reviewed by a human.
func ReviewableSQL(module, action string, args map[string]any) (string, bool) {
	if action != "query" || (module != "mysql" && module != "pg") {
		return "", false
	}
	sql, _ := args["sql"].(string)
	if strings.TrimSpace(sql) == "" {
		return "", false
	}
	return sql, true
}

// ReviewSteps scores every step's SQL and aggregates the findings into the
// summary shape the plan artifact carries.
//
// It returns nil when there is nothing to review (no reviewable SQL surface)
// — the same "nil means no SQL face, never 'reviewed and clean'" rule the
// artifact follows.
//
// A fail verdict is NOT an error here: the aggregate is returned as it is, and
// the caller decides what a fail means (the plan gate refuses to mint an
// artifact over it; the re-check reports it). A statement the grammar cannot
// parse is itself a fail finding (SR-01) — that is the engine's own rule, not
// an error here. An engine error (the only one today is an empty batch, which
// is skipped above) is returned as an error: "cannot judge" is not "allowed".
func ReviewSteps(steps []SQLStepInput) (*SQLReviewSummary, error) {
	var summary *SQLReviewSummary
	var impacts []*sqlreview.Impact
	for _, s := range steps {
		if strings.TrimSpace(s.SQL) == "" {
			continue
		}
		var res *sqlreview.ReviewResult
		var err error
		if s.Dialect == sqlreview.DialectPostgres {
			res, err = sqlreview.ReviewPostgres(s.SQL)
		} else {
			res, err = sqlreview.Review(s.SQL)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: step %q: %v", sqlreview.ErrReviewFailed, s.Step, err)
		}
		impacts = append(impacts, &res.Impact)
		if summary == nil {
			summary = &SQLReviewSummary{Verdict: string(res.Verdict)}
		}
		if severityRank(res.Verdict) > severityRank(sqlreview.Verdict(summary.Verdict)) {
			summary.Verdict = string(res.Verdict)
		}
		summary.FailCount += res.FailCount
		summary.WarnCount += res.WarnCount
		for _, f := range res.Findings {
			summary.Findings = append(summary.Findings, SQLReviewFinding{
				Step: s.Step, Rule: string(f.Rule), Severity: string(f.Severity),
				Statement: f.Statement, Message: f.Message,
			})
		}
	}
	if summary != nil {
		// One surface for the change, unioned from the steps' surfaces. It is
		// built only when there IS a surface: a workflow whose SQL face is nil
		// must not acquire an empty impact, which would read as "reviewed, and
		// it touches nothing".
		summary.Impact = sqlreview.MergeImpact(impacts...)
	}
	return summary, nil
}

// severityRank orders review verdicts so aggregation can take the worst.
func severityRank(v sqlreview.Verdict) int {
	switch v {
	case sqlreview.VerdictFail:
		return 2
	case sqlreview.VerdictWarn:
		return 1
	default:
		return 0
	}
}
