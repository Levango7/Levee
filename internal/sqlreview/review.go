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
//     a pass — an unaudited statement must never ride through silently.
//     The finding says which of the two readings applies, because "your SQL
//     is wrong" and "your SQL is in a dialect this grammar does not cover"
//     send the operator to two different places; the severity is the same
//     either way (see dialect.go);
//   - a statement the grammar parses but whose body it does not expose
//     (ALTER TABLE, CREATE INDEX) gets a WARN telling the operator the
//     engine could not see the clause body (SR-08) — the engine never
//     pretends it classified what it cannot read.
package sqlreview

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xwb1989/sqlparser"
)

// ErrReviewFailed is the gate refusal carried by every plan path when a
// reviewable SQL batch scores a fail verdict. It lives here (not in the
// wiring or grpc package) because both the plan funnel that raises it and the
// RPC layer that maps it to codes.FailedPrecondition must match on it, and
// the review engine is the leaf both can import without a cycle.
var ErrReviewFailed = errors.New("sql review failed")

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
	// Dialect is set only on an SR-01 finding whose statement carries marker
	// syntax of a dialect the embedded grammar does not cover. It is a hint
	// for the reader (and for a UI that wants to render the two SR-01
	// readings differently) and it never moves the severity: a dialect gap
	// does not make the statement readable, it only says why it was not read
	// (unread ⇒ fail; a DDL read partway ⇒ warn — see sr01PartialFinding).
	// Empty on every other finding.
	Dialect Dialect `json:"dialect,omitempty"`
}

// StatementReview is the review of one statement of the batch.
type StatementReview struct {
	Index    int             `json:"index"`
	Text     string          `json:"text"`
	Kind     StatementKind   `json:"kind"`
	Findings []Finding       `json:"findings"`
	Impact   StatementImpact `json:"impact"`
}

// ReviewResult is the outcome of reviewing one batch.
type ReviewResult struct {
	Verdict    Verdict           `json:"verdict"`
	Statements []StatementReview `json:"statements"`
	Findings   []Finding         `json:"findings"`
	FailCount  int               `json:"fail_count"`
	WarnCount  int               `json:"warn_count"`
	// Impact is the static impact surface of the whole batch (see impact.go).
	// It is built from the same parse as the findings and in the same pass, so
	// it cannot disagree with them; it is a value, not a pointer, because a
	// fresh review always HAS an impact surface (possibly an empty one).
	Impact Impact `json:"impact"`
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
	res.Impact = aggregateImpact(res.Statements)
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
// rules. A parse error becomes an SR-01 fail on a statement of unknown kind,
// and a DDL the lax parser only read partway becomes an SR-01 warn on a
// statement whose prefix rules are deliberately not applied: in both cases
// the engine reports what it could not read instead of guessing.
func reviewStatement(text string, index int) StatementReview {
	sr := StatementReview{Index: index, Text: text, Kind: KindUnknown, Findings: []Finding{}}
	stmt, err := sqlparser.Parse(text)
	if err != nil {
		sr.Kind = KindUnknown
		sr.Findings = append(sr.Findings, sr01Finding(text, index, err))
		sr.Impact = unreadImpact(index)
		return sr
	}
	sr.Kind = classify(stmt)
	// The library's lax Parse hands back a partially parsed DDL INSTEAD of an
	// error (its own doc comment says so, and its code logs the error it
	// swallows). Scoring that prefix produces findings about a statement
	// nobody wrote, and worse: a clean PASS for DDL the grammar never read.
	// A PostgreSQL `CREATE TABLE t (id SERIAL PRIMARY KEY)` came back with no
	// findings at all before this check existed, and a MySQL `CREATE TABLE t
	// AS SELECT ...` was told to add a primary key. The strict parse is the
	// honest oracle for "was this read in full": when it says no, the prefix
	// findings are suppressed and one SR-01 warns with the reason instead.
	if sr.Kind == KindDDL || sr.Kind == KindDBDDL {
		if _, strictErr := sqlparser.ParseStrictDDL(text); strictErr != nil {
			sr.Findings = append(sr.Findings, sr01PartialFinding(text, index, strictErr))
			sr.Impact = unreadImpact(index)
			return sr
		}
	}
	sr.Impact = statementImpact(stmt, sr.Kind, index)
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

// sr01Finding builds the SR-01 fail: the grammar read nothing at all of this
// statement, so no rule scored it and nothing may run. The instruction splits
// two ways, and neither way is a softer gate — an unaudited statement never
// passes:
//
//   - a statement carrying another dialect's marker syntax gets the dialect
//     reading: the SQL is probably fine, the grammar is not the one it was
//     written for, so rewrite it or have the target server's parser review it;
//   - anything else gets the syntax reading, stated with its own uncertainty:
//     no recognised marker is not proof of a broken statement, it is proof
//     that the engine has nothing to say about the cause (which is also what
//     a statement the frozen grammar simply never covered — GRANT, say —
//     looks like).
func sr01Finding(text string, index int, parseErr error) Finding {
	f := Finding{Rule: "SR-01", Severity: SeverityFail, Statement: index}
	if d, markers := detectDialect(text); d != "" {
		f.Dialect = d
		f.Message = fmt.Sprintf("statement does not parse under the embedded grammar "+
			"(frozen at the MySQL 5.7 era) and carries %s-only syntax (%s) — most likely a "+
			"dialect gap rather than a syntax error; the engine will not pass a statement it "+
			"could not read, so either rewrite it in MySQL syntax or have the target server's "+
			"own parser review it and attach that review to the change: %v",
			d, strings.Join(markers, ", "), parseErr)
		return f
	}
	f.Message = fmt.Sprintf("statement does not parse under the embedded grammar "+
		"(frozen at the MySQL 5.7 era) and carries no marker of the dialects the engine "+
		"recognises — a syntax error, a statement the frozen grammar never covered (GRANT "+
		"is one of those), or a dialect outside that list; the engine will not pass a "+
		"statement it could not read: %v — fix the statement or route it to a human "+
		"reviewer before execution", parseErr)
	return f
}

// sr01PartialFinding builds the SR-01 WARN for a DDL the grammar read only
// partway. One rule ID carries both degrees on purpose: both findings say the
// same thing — this statement was not fully read, so it was not fully audited
// — and only the amount read differs. Splitting them into two rule IDs would
// make the audit trail count "unread" twice and would tell an operator that
// the product grew a rule when in fact it stopped pretending.
//
// Why warn rather than fail: the library hands back a classified prefix, so
// the statement is not unknown-but-unread (that is the fail case above); it is
// audited-in-part, which is the same contract SR-08 already carries for clause
// bodies it cannot see. MySQL's own partitioned DDL reads this way, and
// refusing it outright would push legitimate work outside the platform. What
// is NOT acceptable — and what this replaced — is the prefix being scored as
// if it were the statement: that told `CREATE TABLE ... AS SELECT` to add a
// primary key and let PostgreSQL DDL pass clean.
func sr01PartialFinding(text string, index int, strictErr error) Finding {
	f := Finding{Rule: "SR-01", Severity: SeverityWarn, Statement: index}
	if d, markers := detectDialect(text); d != "" {
		f.Dialect = d
		f.Message = fmt.Sprintf("this DDL is read only partway by the embedded grammar "+
			"(frozen at the MySQL 5.7 era), stopping at: %v — and it carries %s-only syntax "+
			"(%s), so it most likely belongs to another server; the rules scored only the "+
			"prefix before that point, never the statement you wrote, so confirm the full "+
			"statement by hand (or run it past the target server's own parser) before "+
			"approving", strictErr, d, strings.Join(markers, ", "))
		return f
	}
	f.Message = fmt.Sprintf("this DDL is read only partway by the embedded grammar "+
		"(frozen at the MySQL 5.7 era), stopping at: %v — so the rules scored only the "+
		"prefix before that point, never the statement you wrote (partitioning clauses, "+
		"CREATE TABLE ... LIKE / AS SELECT, FOREIGN KEY clauses and several column types "+
		"all read this way); confirm the clauses the engine could not see by hand before "+
		"approving", strictErr)
	return f
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
