// Package sqlguard holds the R2 boundary both database modules enforce: a
// statement the platform cannot vouch for must not ride the idempotent
// `*.query` action.
//
// It lives in its own package because there are two callers (mysql, pg) and
// the rule is about the SQL, not about the client binary. Duplicating the
// classifier was how the two modules would eventually disagree about whether
// `DROP TABLE` is destructive — the same drift class the repo keeps paying for
// (one conclusion written twice, half of it updated later).
//
// Classification shares the dialect reviewer's fail boundary before applying
// the query surface's extra structural restrictions, and the ORDER matters:
//
//   - Review (MySQL) or ReviewPostgres (PG) owns every fail-level decision.
//     An unreadable statement is SR-01/fail, so it cannot ride through merely
//     because the raw-text backstop does not recognise its shape;
//   - the MySQL AST and raw-text backstop still reject DROP/TRUNCATE even when
//     review classifies them as warnings. They also retain defence in depth
//     for shapes such as an unbounded DELETE carrying a trailing comment.
//
// Plan and execution therefore cannot drift on unreadable SQL, unbounded
// UPDATE/DELETE or tautological predicates; the idempotent query surface may
// only be stricter than plan review, never more permissive.
package sqlguard

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/xwb1989/sqlparser"

	"github.com/nexus/levee/internal/sqlreview"
)

// destructiveSQLRe matches the destructive statements that must never ride an
// idempotent query action: DROP (TABLE/DATABASE/SCHEMA/INDEX/USER), TRUNCATE,
// and unconditional DELETE FROM (no WHERE). These require the workflow author
// to mark the step irreversible and author a dedicated reviewed step instead.
//
// It is a backstop, not the primary classifier — see the package doc for why
// the AST gets the first say.
var destructiveSQLRe = regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|DATABASE|SCHEMA|INDEX|USER)|TRUNCATE(\s+TABLE)?\s|DELETE\s+FROM\s+[^\s]+\s*;?\s*$)`)

// RejectDestructive returns an error when sql contains a statement the
// idempotent query surface must not run. action names the surface (e.g.
// "mysql.query") so the refusal tells the author where the statement was
// going.
//
// The returned error is always a plain error: callers wrap it with their own
// action prefix, and a typed error here would force every caller to know about
// this package's error taxonomy.
func RejectDestructive(sql, action string) error {
	if action == "pg.query" {
		return rejectDestructivePG(sql, action)
	}
	if strings.TrimSpace(sql) == "" {
		return nil
	}
	// Plan-time MySQL review and execution-time guard must agree on every
	// fail-level finding (SR-01 unreadable, SR-02..SR-06 unbounded/destructive
	// DML and DDL). Run the same review first so a statement the platform
	// cannot vouch for is refused here even when the AST path below would
	// otherwise let it through.
	if res, err := sqlreview.Review(sql); err == nil {
		for _, statement := range res.Statements {
			for _, finding := range statement.Findings {
				if finding.Severity == sqlreview.SeverityFail {
					return destructiveErr(statement.Text, action)
				}
			}
		}
	} else {
		return destructiveErr(sql, action)
	}
	pieces, err := sqlparser.SplitStatementToPieces(sql)
	if err != nil {
		return regexReject(sql, action)
	}
	for _, piece := range pieces {
		text := strings.TrimSpace(piece)
		if text == "" {
			continue
		}
		stmt, perr := sqlparser.Parse(text)
		if perr != nil {
			// Unreadable: the regex still gets its say (fail-closed in the only
			// direction available).
			if err := regexReject(text, action); err != nil {
				return err
			}
			continue
		}
		switch s := stmt.(type) {
		case *sqlparser.DDL:
			if s.Action == sqlparser.DropStr || s.Action == sqlparser.TruncateStr {
				return destructiveErr(text, action)
			}
		case *sqlparser.DBDDL:
			if s.Action == sqlparser.DropStr {
				return destructiveErr(text, action)
			}
		case *sqlparser.Delete:
			if s.Where == nil {
				return destructiveErr(text, action)
			}
		case *sqlparser.Update:
			if s.Where == nil {
				return destructiveErr(text, action)
			}
		}
		// Grammar-readable but not decisively destructive: the regex keeps its
		// raw-text say.
		if err := regexReject(text, action); err != nil {
			return err
		}
	}
	return nil
}

// rejectDestructivePG applies the exact same PostgreSQL AST and fail boundary
// as the plan gate. DROP/TRUNCATE additionally remain forbidden on the
// idempotent query surface even where their review finding is a warning.
// MySQL follows the same fail-level contract before its structural DROP
// backstop; no second dialect rule engine is allowed to grow here.
func rejectDestructivePG(sql, action string) error {
	if strings.TrimSpace(sql) == "" {
		return nil
	}
	res, err := sqlreview.ReviewPostgres(sql)
	if err != nil {
		return destructiveErr(sql, action)
	}
	for _, statement := range res.Statements {
		switch statement.Impact.Operation {
		case "drop", "drop database", "truncate":
			return destructiveErr(statement.Text, action)
		}
		for _, finding := range statement.Findings {
			if finding.Severity == sqlreview.SeverityFail {
				return destructiveErr(statement.Text, action)
			}
		}
	}
	return nil
}

func destructiveErr(text, action string) error {
	return fmt.Errorf("destructive statement rejected: %q must not ride %s; mark the step irreversible and author an explicit reviewed statement instead", text, action)
}

func regexReject(text, action string) error {
	if destructiveSQLRe.MatchString(text) {
		return destructiveErr(text, action)
	}
	return nil
}
