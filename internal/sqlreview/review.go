// Package sqlreview scores SQL statement batches against LEVEE's
// change-governance rules.
//
// The parser it embeds (github.com/xwb1989/sqlparser, Apache-2.0, zero
// dependencies) is a commodity component whose grammar is frozen at the
// MySQL 5.7 era. Every rule in this package is authored in-house from
// public operational practice; no rule text, message or code is taken from
// any GPL/AGPL-licensed review tool. That boundary is a product decision
// recorded in docs/product-roadmap.md, not an implementation detail.
//
// Honesty rules of the engine:
//
//   - a statement the embedded grammar cannot parse is a FAIL (SR-01), not
//     a pass — an unaudited statement must never ride through silently;
//   - a statement the grammar parses but whose body it does not expose
//     (ALTER TABLE, CREATE INDEX) gets a WARN telling the operator the
//     engine could not see the clause body (SR-08) — the engine never
//     pretends it classified what it cannot read.
package sqlreview

import (
	"fmt"
	"strings"

	"github.com/xwb1989/sqlparser"
)

// Severity is the enforcement level of one finding.
type Severity string

const (
	// SeverityFail findings block the batch: the statement must not run.
	SeverityFail Severity = "fail"
	// SeverityWarn findings require a human confirmation before the
	// statement runs; they never block on their own.
	SeverityWarn Severity = "warn"
)

// Verdict is the batch-level outcome, derived as the worst severity seen.
type Verdict string

const (
	VerdictPass Verdict = "pass"
	VerdictWarn Verdict = "warn"
	VerdictFail Verdict = "fail"
)

// RuleID identifies one rule. IDs are stable: they appear in audit output
// and in the product docs, so a retired rule leaves a gap, never a renumber.
type RuleID string

// StatementKind classifies a parsed statement for rule gating.
type StatementKind string

const (
	KindInsert     StatementKind = "insert"
	KindUpdate     StatementKind = "update"
	KindDelete     StatementKind = "delete"
	KindSelect     StatementKind = "select"
	KindSet        StatementKind = "set"
	KindDDL        StatementKind = "ddl"
	KindDBDDL      StatementKind = "db-ddl"
	KindOtherAdmin StatementKind = "other-admin"
	KindOtherRead  StatementKind = "other-read"
	KindUse        StatementKind = "use"
	KindTxn        StatementKind = "txn"
	KindShow       StatementKind = "show"
	KindUnknown    StatementKind = "unknown"
)

// Finding is one rule hitting one statement (Statement == 0 marks a
// batch-level rule, which has no single statement to anchor to).
type Finding struct {
	Rule      RuleID   `json:"rule"`
	Severity  Severity `json:"severity"`
	Statement int      `json:"statement"`
	Message   string   `json:"message"`
}

// StatementReview is the review of one statement of the batch.
type StatementReview struct {
	Index    int           `json:"index"`
	Text     string        `json:"text"`
	Kind     StatementKind `json:"kind"`
	Findings []Finding     `json:"findings"`
}

// ReviewResult is the outcome of reviewing one batch.
type ReviewResult struct {
	Verdict    Verdict           `json:"verdict"`
	Statements []StatementReview `json:"statements"`
	Findings   []Finding         `json:"findings"`
	FailCount  int               `json:"fail_count"`
	WarnCount  int               `json:"warn_count"`
}

// Review parses batch and scores every statement against the rule set.
// A batch is one or more statements separated by semicolons; statements
// are reviewed independently (SR-20 warns about exactly that boundary).
// An empty batch is an engine error, not a clean pass.
func Review(batch string) (*ReviewResult, error) {
	if strings.TrimSpace(batch) == "" {
		return nil, fmt.Errorf("sqlreview: empty review input")
	}
	pieces, err := sqlparser.SplitStatementToPieces(batch)
	if err != nil {
		return nil, fmt.Errorf("sqlreview: split batch: %w", err)
	}

	res := &ReviewResult{Findings: []Finding{}}
	var sawDDL, sawDML bool
	for _, piece := range pieces {
		text := strings.TrimSpace(piece)
		if text == "" {
			continue
		}
		res.Statements = append(res.Statements, reviewStatement(text, len(res.Statements)+1))
		switch res.Statements[len(res.Statements)-1].Kind {
		case KindDDL, KindDBDDL:
			sawDDL = true
		case KindInsert, KindUpdate, KindDelete:
			sawDML = true
		}
	}
	if len(res.Statements) == 0 {
		return nil, fmt.Errorf("sqlreview: empty review input")
	}

	for _, sr := range res.Statements {
		res.Findings = append(res.Findings, sr.Findings...)
	}
	res.Findings = append(res.Findings, batchRules(res.Statements, sawDDL, sawDML)...)
	for _, f := range res.Findings {
		switch f.Severity {
		case SeverityFail:
			res.FailCount++
		case SeverityWarn:
			res.WarnCount++
		}
	}
	switch {
	case res.FailCount > 0:
		res.Verdict = VerdictFail
	case res.WarnCount > 0:
		res.Verdict = VerdictWarn
	default:
		res.Verdict = VerdictPass
	}
	return res, nil
}

// reviewStatement parses one statement and applies the statement-level
// rules. A parse error becomes a SR-01 fail on a statement of unknown
// kind: the engine reports what it could not read instead of guessing.
func reviewStatement(text string, index int) StatementReview {
	sr := StatementReview{Index: index, Text: text, Kind: KindUnknown, Findings: []Finding{}}
	stmt, err := sqlparser.Parse(text)
	if err != nil {
		sr.Kind = KindUnknown
		sr.Findings = append(sr.Findings, Finding{
			Rule:      "SR-01",
			Severity:  SeverityFail,
			Statement: index,
			Message: fmt.Sprintf("statement does not parse under the embedded grammar "+
				"(frozen at the MySQL 5.7 era): %v — route to a human reviewer or run it "+
				"against the target server's own parser before execution", err),
		})
		return sr
	}
	sr.Kind = classify(stmt)
	for _, r := range stmtRules {
		if !r.kinds[sr.Kind] {
			continue
		}
		if r.match(stmt) {
			sr.Findings = append(sr.Findings, Finding{
				Rule:      r.id,
				Severity:  r.sev,
				Statement: index,
				Message:   r.msg,
			})
		}
	}
	return sr
}

// classify maps a parsed statement to the kind gate the rule table uses.
func classify(stmt sqlparser.Statement) StatementKind {
	switch stmt.(type) {
	case *sqlparser.Insert:
		return KindInsert
	case *sqlparser.Update:
		return KindUpdate
	case *sqlparser.Delete:
		return KindDelete
	case *sqlparser.Select, *sqlparser.Union, *sqlparser.ParenSelect:
		return KindSelect
	case *sqlparser.Set:
		return KindSet
	case *sqlparser.DDL:
		return KindDDL
	case *sqlparser.DBDDL:
		return KindDBDDL
	case *sqlparser.OtherAdmin:
		return KindOtherAdmin
	case *sqlparser.OtherRead:
		return KindOtherRead
	case *sqlparser.Use:
		return KindUse
	case *sqlparser.Begin, *sqlparser.Commit, *sqlparser.Rollback:
		return KindTxn
	case *sqlparser.Show:
		return KindShow
	default:
		return KindUnknown
	}
}

// batchRules applies the rules that need to see the whole batch at once.
func batchRules(stmts []StatementReview, sawDDL, sawDML bool) []Finding {
	var out []Finding
	if sawDDL && sawDML {
		out = append(out, Finding{
			Rule:      "SR-19",
			Severity:  SeverityWarn,
			Statement: 0,
			Message: "batch mixes DDL and DML; DDL issues an implicit commit in MySQL, so DML " +
				"executed before it cannot be rolled back as one unit — split schema changes and " +
				"data changes into separate runs",
		})
	}
	if len(stmts) > 1 {
		out = append(out, Finding{
			Rule:      "SR-20",
			Severity:  SeverityWarn,
			Statement: 0,
			Message: fmt.Sprintf("batch carries %d statements; each was scored on its own and "+
				"cross-statement atomicity is not asserted — wrap the batch in an explicit "+
				"transaction if it must be atomic", len(stmts)),
		})
	}
	return out
}
