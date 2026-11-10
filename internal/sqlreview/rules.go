package sqlreview

import (
	"strings"

	"github.com/xwb1989/sqlparser"
)

// This file is the rule layer of the review engine and the product's
// differentiator: every rule below is written in-house. The parser it runs
// on is the embedded Apache-2.0 component; the rule text, the messages and
// the severity calls are LEVEE's own, derived from public operational
// practice (high-risk change management, not any third-party tool's rule
// set). Rule IDs are part of the audit surface — see the package comment.

// stmtRule is one statement-level rule: a kind gate, a predicate over the
// parsed statement, and the message the operator reads when it fires.
type stmtRule struct {
	id    RuleID
	sev   Severity
	kinds map[StatementKind]bool
	match func(sqlparser.Statement) bool
	msg   string
}

// stmtRules is the complete statement-level rule set. SR-01 lives in
// reviewStatement (it is about the parse itself, not the parsed tree);
// SR-19/SR-20 live in batchRules (they need the whole batch). The
// TestEveryRuleFires test in review_test.go asserts each ID below has a
// triggering statement, so a rule cannot rot into dead table rows.
var stmtRules = []stmtRule{
	{
		id: "SR-02", sev: SeverityFail,
		kinds: k(KindDelete),
		match: func(s sqlparser.Statement) bool {
			return s.(*sqlparser.Delete).Where == nil
		},
		msg: "DELETE without WHERE deletes every row of the table; add a WHERE " +
			"predicate, or run it inside an irreversible-marked run with an explicit " +
			"row-count estimate and a verified backup",
	},
	{
		id: "SR-03", sev: SeverityFail,
		kinds: k(KindUpdate),
		match: func(s sqlparser.Statement) bool {
			return s.(*sqlparser.Update).Where == nil
		},
		msg: "UPDATE without WHERE rewrites every row of the table; add a WHERE " +
			"predicate, or run it inside an irreversible-marked run with an explicit " +
			"row-count estimate and a verified backup",
	},
	{
		id: "SR-04", sev: SeverityFail,
		kinds: k(KindDelete, KindUpdate),
		match: func(s sqlparser.Statement) bool {
			switch st := s.(type) {
			case *sqlparser.Delete:
				return st.Where != nil && isTautology(st.Where.Expr)
			case *sqlparser.Update:
				return st.Where != nil && isTautology(st.Where.Expr)
			}
			return false
		},
		msg: "WHERE clause is always true (constant tautology such as 1=1, or an OR " +
			"arm that is always true), so the statement matches every row; rewrite it " +
			"with a predicate that actually selects rows",
	},
	{
		id: "SR-05", sev: SeverityFail,
		kinds: k(KindDDL),
		match: func(s sqlparser.Statement) bool {
			return s.(*sqlparser.DDL).Action == sqlparser.TruncateStr
		},
		msg: "TRUNCATE drops and recreates the table and cannot be rolled back " +
			"row-by-row; it belongs in an irreversible-marked run with a verified backup",
	},
	{
		id: "SR-06", sev: SeverityFail,
		kinds: k(KindDBDDL),
		match: func(s sqlparser.Statement) bool {
			return s.(*sqlparser.DBDDL).Action == sqlparser.DropStr
		},
		msg: "DROP DATABASE destroys every object in the database; refuse it in " +
			"ordinary change runs and require a dedicated irreversible-marked run",
	},
	{
		id: "SR-07", sev: SeverityWarn,
		kinds: k(KindDDL),
		match: func(s sqlparser.Statement) bool {
			return s.(*sqlparser.DDL).Action == sqlparser.DropStr
		},
		msg: "DROP removes a schema object permanently; require a verified backup " +
			"and an irreversible-marked run before execution",
	},
	{
		id: "SR-08", sev: SeverityWarn,
		kinds: k(KindDDL),
		match: func(s sqlparser.Statement) bool {
			return s.(*sqlparser.DDL).Action == sqlparser.AlterStr
		},
		msg: "the embedded grammar exposes only the action and the table for this " +
			"statement (ALTER TABLE and CREATE INDEX share this surface), not the clause " +
			"body, so the engine cannot classify the change itself; a human must review " +
			"the statement text",
	},
	{
		id: "SR-09", sev: SeverityWarn,
		kinds: k(KindDDL),
		match: func(s sqlparser.Statement) bool {
			return s.(*sqlparser.DDL).Action == sqlparser.RenameStr
		},
		msg: "RENAME changes the name every caller references; confirm downstream " +
			"queries, views and scheduled jobs before execution",
	},
	{
		id: "SR-10", sev: SeverityWarn,
		kinds: k(KindDDL),
		match: func(s sqlparser.Statement) bool {
			ddl := s.(*sqlparser.DDL)
			return ddl.Action == sqlparser.CreateStr && ddl.TableSpec != nil &&
				!strings.Contains(strings.ToLower(sqlparser.String(ddl.TableSpec)), "primary key")
		},
		msg: "CREATE TABLE declares no PRIMARY KEY; InnoDB then builds a hidden row-id " +
			"table, which degrades point-update review and replication; declare an explicit key",
	},
	{
		id: "SR-11", sev: SeverityWarn,
		kinds: k(KindDDL),
		match: func(s sqlparser.Statement) bool {
			ddl := s.(*sqlparser.DDL)
			return ddl.Action == sqlparser.CreateStr && ddl.TableSpec != nil &&
				!strings.Contains(strings.ToLower(ddl.TableSpec.Options), "engine=")
		},
		msg: "CREATE TABLE does not declare ENGINE=; the server default applies and the " +
			"audit trail cannot show which engine the change targeted; declare ENGINE= explicitly",
	},
	{
		id: "SR-12", sev: SeverityWarn,
		kinds: k(KindSelect),
		match: func(s sqlparser.Statement) bool {
			return hasStarExpr(s.(*sqlparser.Select).SelectExprs)
		},
		msg: "SELECT * returns every column including ones added later; list the columns " +
			"the consumer actually reads so schema drift cannot change the result shape",
	},
	{
		id: "SR-13", sev: SeverityWarn,
		kinds: k(KindInsert),
		match: func(s sqlparser.Statement) bool {
			ins := s.(*sqlparser.Insert)
			_, isValues := ins.Rows.(sqlparser.Values)
			return len(ins.Columns) == 0 && isValues
		},
		msg: "INSERT without a column list binds values by table order, so adding a " +
			"column later silently reorders the insert; name the columns explicitly",
	},
	{
		id: "SR-14", sev: SeverityWarn,
		kinds: k(KindDelete),
		match: func(s sqlparser.Statement) bool {
			st := s.(*sqlparser.Delete)
			return st.Where != nil && st.Limit == nil
		},
		msg: "DELETE has a WHERE but no LIMIT; a predicate matching more rows than " +
			"intended cannot be stopped mid-statement — add LIMIT (with ORDER BY) or split " +
			"the change into batches",
	},
	{
		id: "SR-15", sev: SeverityWarn,
		kinds: k(KindUpdate),
		match: func(s sqlparser.Statement) bool {
			st := s.(*sqlparser.Update)
			return st.Where != nil && st.Limit == nil
		},
		msg: "UPDATE has a WHERE but no LIMIT; a predicate matching more rows than " +
			"intended cannot be stopped mid-statement — add LIMIT or split the change into " +
			"batches",
	},
	{
		id: "SR-16", sev: SeverityWarn,
		kinds: k(KindSet),
		match: func(s sqlparser.Statement) bool {
			return s.(*sqlparser.Set).Scope == sqlparser.GlobalStr
		},
		msg: "SET GLOBAL mutates cluster-wide state for every connection, not just this " +
			"change; run it in a dedicated window-scoped run so the blast radius is recorded",
	},
	{
		id: "SR-17", sev: SeverityWarn,
		kinds: k(KindOtherAdmin),
		match: func(sqlparser.Statement) bool { return true },
		msg: "administrative statement (repair/optimize/etc.); the grammar does not expose " +
			"its semantics, so the engine cannot audit it — a human must confirm intent and " +
			"the maintenance window",
	},
	{
		id: "SR-18", sev: SeverityWarn,
		kinds: k(KindSelect),
		match: func(s sqlparser.Statement) bool {
			return s.(*sqlparser.Select).Lock != ""
		},
		msg: "locking read (FOR UPDATE / LOCK IN SHARE MODE) holds row locks until the " +
			"transaction ends and blocks concurrent writers; confirm the transaction is short " +
			"and the isolation level is intended",
	},
}

// k is the kind-gate constructor for the rule table.
func k(kinds ...StatementKind) map[StatementKind]bool {
	m := make(map[StatementKind]bool, len(kinds))
	for _, kind := range kinds {
		m[kind] = true
	}
	return m
}

// hasStarExpr reports whether the select list contains a bare or qualified
// star (SELECT * / SELECT t.*). AliasedExpr entries are named columns and
// are not stars.
func hasStarExpr(exprs sqlparser.SelectExprs) bool {
	for _, e := range exprs {
		if _, ok := e.(*sqlparser.StarExpr); ok {
			return true
		}
	}
	return false
}

// isTautology reports whether expr is always true: a literal true, an
// equality of two identical literals (1=1, 'x'='x'), or a disjunction with
// any always-true arm (id=1 OR 1=1 matches every row). A conjunction is
// only a tautology when every arm is one. Parentheses are transparent.
func isTautology(e sqlparser.Expr) bool {
	switch v := e.(type) {
	case sqlparser.BoolVal:
		return bool(v)
	case *sqlparser.ComparisonExpr:
		if v.Operator != sqlparser.EqualStr {
			return false
		}
		l, ok1 := v.Left.(*sqlparser.SQLVal)
		r, ok2 := v.Right.(*sqlparser.SQLVal)
		return ok1 && ok2 && l.Type == r.Type && string(l.Val) == string(r.Val)
	case *sqlparser.OrExpr:
		return isTautology(v.Left) || isTautology(v.Right)
	case *sqlparser.AndExpr:
		return isTautology(v.Left) && isTautology(v.Right)
	case *sqlparser.ParenExpr:
		return isTautology(v.Expr)
	default:
		return false
	}
}
