package main

// cmd_sqlreview_change.go implements `levee sqlreview --change <change-id>`:
// review the SQL an approved change carries, and compare the fresh verdict
// with the one frozen into its plan artifact.
//
// Why this command exists (the "双向对齐" the roadmap asks for):
//
//   - forward: the DB Change Ticket shows the review verdict that was frozen
//     into the plan artifact — the one plan_hash binds and the approval
//     signed. That is the FACT about the change.
//   - backward: `levee sqlreview --change` re-scores the same SQL with the
//     CURRENT rule set. Comparing the two answers a question the ticket
//     cannot answer on its own: "does the rule set still agree with what this
//     change was approved under?"
//
// The two are deliberately not merged into one surface. The ticket is a
// record of what was decided; this command is a measurement of now. A reader
// who sees only one number would not know which one they are looking at, so
// the comparison is printed as a comparison, and the plan-time verdict is
// always reported next to the fresh one.
//
// The scoring core is shared with the plan-time gate (plan.ReviewSteps), so
// a difference here means the rules or the SQL changed — not that two
// implementations disagree.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/nexus/levee/internal/plan"
)

// sqlReviewChangeResult is the machine-readable shape of the comparison.
type sqlReviewChangeResult struct {
	ChangeID string `json:"change_id"`
	// PlanState mirrors the ticket's three states: none / readable /
	// unreadable. It is here because "cannot compare" must be a value, not a
	// crash or a silent success.
	PlanState string `json:"plan_state"`
	// PlanHash is the artifact hash the approval signed.
	PlanHash string `json:"plan_hash"`
	// Steps is the SQL surface that was re-scored.
	Steps []sqlReviewChangeStep `json:"steps"`
	// Frozen is the verdict carried by the plan artifact (nil = the plan
	// carries no review verdict — absence of evidence, never a clean pass).
	Frozen *plan.SQLReviewSummary `json:"frozen"`
	// Fresh is the verdict computed now with the current rule set.
	Fresh *plan.SQLReviewSummary `json:"fresh"`
	// Aligned says whether the two agree. It is false when the verdicts
	// differ OR when one of them is missing — "no verdict to compare" is not
	// alignment.
	Aligned bool `json:"aligned"`
	// Differences lists the findings that are not on both sides.
	Differences []string `json:"differences"`
	// Note carries the reason a comparison could not be made.
	Note string `json:"note,omitempty"`
}

// sqlReviewChangeStep is one re-scored SQL statement.
type sqlReviewChangeStep struct {
	Step string `json:"step"`
	SQL  string `json:"sql"`
}

// runSQLReviewChange re-scores a change's SQL and compares it with the
// verdict frozen into the plan artifact.
func runSQLReviewChange(changeID string) error {
	ctx := context.Background()

	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	run, err := store.GetRun(ctx, changeID)
	if err != nil {
		return fmt.Errorf("get run %q: %w", changeID, err)
	}
	if run == nil {
		return fmt.Errorf("change %q not found", changeID)
	}

	res := sqlReviewChangeResult{
		ChangeID:  changeID,
		PlanState: "none",
		PlanHash:  run.PlanHash,
	}

	if run.PlanJSON == "" {
		res.Note = "该变更还没有计划制品，无 SQL 面可比对"
		return printSQLReviewChange(res)
	}
	var p plan.Plan
	if err := json.Unmarshal([]byte(run.PlanJSON), &p); err != nil {
		res.PlanState = "unreadable"
		res.Note = "计划制品不可读（plan_json 解析失败），无法复算也无法比对"
		return printSQLReviewChange(res)
	}
	res.PlanState = "readable"
	res.Frozen = p.SQLReview

	steps := planSQLSteps(&p)
	for _, s := range steps {
		res.Steps = append(res.Steps, sqlReviewChangeStep{Step: s.Step, SQL: s.SQL})
	}
	if len(steps) == 0 {
		res.Note = "该计划没有可审核的 SQL 面（无 mysql.query 步骤）"
		return printSQLReviewChange(res)
	}

	fresh, err := plan.ReviewSteps(steps)
	if err != nil {
		// "Cannot judge" is not "allowed": the command refuses rather than
		// reporting an empty verdict that would read like a pass.
		return fmt.Errorf("sqlreview: re-check %q: %w", changeID, err)
	}
	res.Fresh = fresh

	diffs, aligned := compareSQLReview(p.SQLReview, fresh)
	res.Differences = diffs
	res.Aligned = aligned
	return printSQLReviewChange(res)
}

// planSQLSteps extracts the reviewable SQL surface from a plan artifact.
//
// The extraction rule is plan.ReviewableSQL's — the same one the plan-time
// gate and the DB Change Ticket use, now literally the same function. It used
// to be three copies of "module == mysql && action == query", which is how all
// three came to be wrong at the same time when pg.query landed.
func planSQLSteps(p *plan.Plan) []plan.SQLStepInput {
	var steps []plan.SQLStepInput
	if p == nil {
		return steps
	}
	for _, b := range p.Batches {
		for _, s := range b.Steps {
			sql, ok := plan.ReviewableSQL(s.Module, s.Action, s.Args)
			if !ok {
				continue
			}
			steps = append(steps, plan.SQLStepInput{Step: s.Name, SQL: sql})
		}
	}
	return steps
}

// compareSQLReview compares the frozen verdict with the fresh one.
//
// A finding is identified by (rule, step, statement, severity): the same rule
// on the same statement with a different severity is a difference worth
// reporting, and the same rule on a different statement is two findings.
func compareSQLReview(frozen, fresh *plan.SQLReviewSummary) ([]string, bool) {
	if frozen == nil || fresh == nil {
		return nil, false
	}
	key := func(f plan.SQLReviewFinding) string {
		return fmt.Sprintf("%s|%s|%d|%s", f.Rule, f.Step, f.Statement, f.Severity)
	}
	frozenSet := map[string]plan.SQLReviewFinding{}
	for _, f := range frozen.Findings {
		frozenSet[key(f)] = f
	}
	freshSet := map[string]plan.SQLReviewFinding{}
	for _, f := range fresh.Findings {
		freshSet[key(f)] = f
	}

	var diffs []string
	for k, f := range freshSet {
		if _, ok := frozenSet[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("新增 %s（%s，步骤 %q 语句 %d）：%s",
				f.Rule, f.Severity, f.Step, f.Statement, f.Message))
		}
	}
	for k, f := range frozenSet {
		if _, ok := freshSet[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("消失 %s（%s，步骤 %q 语句 %d）：%s",
				f.Rule, f.Severity, f.Step, f.Statement, f.Message))
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		return diffs, false
	}
	// Same findings but a different aggregate verdict would be a contradiction
	// inside the engine; report it rather than hiding it behind "aligned".
	if frozen.Verdict != fresh.Verdict {
		return []string{fmt.Sprintf("结论不同：计划时 %s，现规则复算 %s",
			frozen.Verdict, fresh.Verdict)}, false
	}
	return nil, true
}

// printSQLReviewChange renders the comparison for a human reader and for
// --json.
func printSQLReviewChange(res sqlReviewChangeResult) error {
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  res,
			"meta":  nil,
			"error": nil,
		})
	}

	fmt.Fprintf(os.Stdout, "变更 %s 的 SQL 复算\n", res.ChangeID)
	fmt.Fprintf(os.Stdout, "  计划状态: %s\n", res.PlanState)
	if res.PlanHash != "" {
		fmt.Fprintf(os.Stdout, "  计划哈希: %s\n", res.PlanHash)
	}
	if res.Note != "" {
		fmt.Fprintf(os.Stdout, "  说明: %s\n", res.Note)
		return nil
	}

	fmt.Fprintf(os.Stdout, "  计划时结论（固化，绑定 plan_hash）: %s\n", verdictText(res.Frozen))
	fmt.Fprintf(os.Stdout, "  现规则复算结论: %s\n", verdictText(res.Fresh))
	if res.Aligned {
		fmt.Fprintln(os.Stdout, "  比对: 一致（计划时结论与现规则复算相同）")
		return nil
	}
	fmt.Fprintln(os.Stdout, "  比对: 不一致")
	for _, d := range res.Differences {
		fmt.Fprintf(os.Stdout, "    - %s\n", d)
	}
	return nil
}

// verdictText renders a verdict summary, with the "no verdict" case spelled
// out rather than rendered as an empty string.
func verdictText(s *plan.SQLReviewSummary) string {
	if s == nil {
		return "无结论（该计划没有可审核的 SQL 面）"
	}
	return fmt.Sprintf("%s（fail %d / warn %d）", s.Verdict, s.FailCount, s.WarnCount)
}
