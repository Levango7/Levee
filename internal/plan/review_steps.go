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
	for _, s := range steps {
		if strings.TrimSpace(s.SQL) == "" {
			continue
		}
		res, err := sqlreview.Review(s.SQL)
		if err != nil {
			return nil, fmt.Errorf("%w: step %q: %v", sqlreview.ErrReviewFailed, s.Step, err)
		}
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
