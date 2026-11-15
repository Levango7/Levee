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
// Classification is AST-primary with a raw-text backstop, and the ORDER
// matters:
//
//   - the embedded grammar (xwb1989/sqlparser) is authoritative for every
//     statement it can read. It catches what an end-anchored regex
//     structurally cannot — an unbounded DELETE carrying a trailing comment;
//   - the regex stays for exactly the surface the grammar cannot judge:
//     statements that fail to parse (DROP USER is unknown to it), admin/read
//     statements whose semantics it hides, and alter-shaped statements whose
//     body it does not expose (the grammar flattens DROP INDEX into an ALTER,
//     so only the raw text names it).
//
// A statement that fails to parse is NOT treated as safe: the regex still gets
// its say. That is fail-closed in the only direction available here — the
// guard cannot prove innocence, so it refuses what the text names.
package sqlguard

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/xwb1989/sqlparser"
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
		}
		// Grammar-readable but not decisively destructive: the regex keeps its
		// raw-text say.
		if err := regexReject(text, action); err != nil {
			return err
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
