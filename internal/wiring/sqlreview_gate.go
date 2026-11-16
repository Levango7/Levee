// sqlreview_gate.go implements the plan-time SQL review gate (market doc
// §8.6 step 3): every mysql.query step's SQL is scored by the sqlreview rule
// engine BEFORE the plan is generated, at the same position in the funnel as
// the change-window gate (V19) — because this is the one path every plan
// takes (gRPC PlanChange, `levee plan --local`, the re-plan inside apply), so
// no entry point can mint an approved artifact over SQL the rule engine has
// never scored.
//
// Semantics mirror the window gate exactly: a fail verdict BLOCKS the plan
// (it must not reach approval, where the plan_hash would bless it); a warn
// verdict passes but is stamped onto the artifact, where plan_hash binds it;
// rollback never comes through here, so a failed change stays recoverable.
//
// The verdict travels onto the plan as plan.SQLReview (nil = no reviewable
// SQL surface, never "reviewed and clean"). An engine error from Review is
// "cannot judge", which is not "allowed": the gate fails closed, like the
// unusable-window gate.

package wiring

import (
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/sqlreview"
)

// reviewWorkflowSQL scores every reviewable step's sql argument and returns
// the aggregated verdict for the plan artifact. Which steps those are is
// plan.ReviewableSQL's answer, shared with the ticket and the re-check path —
// this gate was the copy that silently skipped PostgreSQL after pg.query
// landed. Steps without a string sql argument are skipped: typing the argument
// is the executor's contract, not this gate's, and refusing here would
// double-report a malformed step the dsl validator already rejects on the
// compile path.
func reviewWorkflowSQL(wf *dsl.Workflow) (*plan.SQLReviewSummary, error) {
	if wf == nil {
		return nil, nil
	}
	// The scoring itself lives in plan.ReviewSteps, shared with the re-check
	// path (`levee sqlreview --change`). Two implementations of the same
	// review would drift, and then "the rules changed" would be
	// indistinguishable from "the second implementation is different".
	var steps []plan.SQLStepInput
	for _, s := range wf.Steps {
		sql, ok := plan.ReviewableSQL(s.Module, s.Action, s.Args)
		if !ok {
			continue
		}
		steps = append(steps, plan.SQLStepInput{Step: s.Name, SQL: sql})
	}
	summary, err := plan.ReviewSteps(steps)
	if err != nil {
		return nil, err
	}
	if summary != nil && summary.Verdict == string(sqlreview.VerdictFail) {
		return nil, fmt.Errorf("%w: %s", sqlreview.ErrReviewFailed, describeFails(summary.Findings))
	}
	return summary, nil
}

// describeFails renders the fail-severity findings of a refused review for
// the gate error: the operator needs the step, the statement, the rule and
// the rule's own message, not a bare "failed" token.
func describeFails(findings []plan.SQLReviewFinding) string {
	var lines []string
	for _, f := range findings {
		if f.Severity != string(sqlreview.SeverityFail) {
			continue
		}
		lines = append(lines, fmt.Sprintf("step %q statement %d rule %s: %s",
			f.Step, f.Statement, f.Rule, f.Message))
	}
	if len(lines) == 0 {
		return "no fail finding text captured"
	}
	return strings.Join(lines, "; ")
}
