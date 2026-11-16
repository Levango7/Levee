package sqlreview

// dialect.go explains WHY a statement could not be read: is it broken, or is
// it written for another server?
//
// The distinction is for the operator, not for the gate. A statement the
// engine cannot read is a statement it did not audit, so SR-01 fires either
// way. What changes is the instruction the reader gets: "your SQL has a
// syntax error" (fix the SQL) versus "your SQL is not in the dialect this
// grammar covers" (rewrite it, or have the target server's own parser review
// it and attach that review to the change).
//
// What qualifies as a marker is narrow on purpose, and the rule is mechanical
// rather than stylistic: a marker must be syntax the embedded MySQL-5.7-era
// grammar REJECTS (TestMarkersAreRejectedByTheGrammar runs every sample below
// through ParseStrictDDL and requires a parse error). Tokens another dialect
// merely prefers do not qualify, because MySQL reads them as ordinary
// identifiers and the statement parses — Oracle's NVL(), ROWNUM and the DUAL
// pseudo-table are exactly this case: `SELECT NVL(a,0) FROM t` is a valid
// MySQL statement about a column and a table named NVL and t, so claiming
// "Oracle" would be inventing evidence. Oracle is recognisable here only
// where its syntax is genuinely unreadable (VARCHAR2, CONNECT BY), and that
// is the honest ceiling.
//
// Detection is a HINT and is written to be disowned: it looks past string
// literals, quoted identifiers and comments first, and it reports the markers
// it matched so the reader can disagree with it. It is never a validator — a
// statement carrying "::" is not thereby a PostgreSQL statement, it merely
// reads like one. Where a marker collides with an ordinary identifier (a
// column literally named `rownum`), the hint can be wrong; naming the markers
// is what keeps that survivable.

import (
	"regexp"
	"strings"
)

// Dialect names a SQL dialect the engine can recognise but not audit.
type Dialect string

const (
	DialectPostgres  Dialect = "postgresql"
	DialectSQLServer Dialect = "sqlserver"
	DialectOracle    Dialect = "oracle"
	DialectSQLite    Dialect = "sqlite"
	DialectHive      Dialect = "hive"
)

// dialectMarker is one piece of syntax that is distinctive of a dialect and
// that the embedded grammar rejects outright — the two properties together
// are what make it explain a parse failure instead of merely decorating it.
// Sample is the statement the tests use to prove both properties hold, so a
// marker that stops being rejected (or stops being detected) goes red
// instead of sitting in the table as weight nobody checks.
type dialectMarker struct {
	re     *regexp.Regexp
	label  string
	sample string
}

func marker(pattern, label, sample string) dialectMarker {
	return dialectMarker{re: regexp.MustCompile(pattern), label: label, sample: sample}
}

// dialectSignatures is the curated marker table, in a fixed order that also
// breaks ties (see detectDialect). Every pattern is case-insensitive: SQL
// keywords are case-insensitive in all of these dialects, so a case-sensitive
// table would miss `returning` and `nvarchar`.
var dialectSignatures = []struct {
	dialect Dialect
	markers []dialectMarker
}{
	{
		dialect: DialectPostgres,
		markers: []dialectMarker{
			marker(`::`, "::", "SELECT id::text FROM users"),
			marker(`(?i)\bRETURNING\b`, "RETURNING", "UPDATE users SET a=1 RETURNING id"),
			marker(`(?i)\b(?:BIG|SMALL)?SERIAL\b\s*(?:PRIMARY|NOT|UNIQUE|,|\)|$)`, "SERIAL", "CREATE TABLE t (id SERIAL PRIMARY KEY)"),
			marker(`(?i)\bJSONB\b`, "JSONB", "CREATE TABLE t (j JSONB)"),
			marker(`(?i)\bLATERAL\s*\(`, "LATERAL (", "SELECT * FROM a, LATERAL (SELECT 1) x"),
			marker(`(?i)\bON\s+CONFLICT\b`, "ON CONFLICT", "INSERT INTO t (a) VALUES (1) ON CONFLICT (a) DO NOTHING"),
			marker(`(?i)\bGENERATED\s+ALWAYS\s+AS\s+IDENTITY\b`, "GENERATED ALWAYS AS IDENTITY", "CREATE TABLE t (id INT GENERATED ALWAYS AS IDENTITY)"),
			marker(`\$[0-9]+`, "$n parameter", "INSERT INTO t (a) VALUES ($1)"),
			marker(`\$\$`, "$$ quote", "SELECT $$ body $$ FROM t"),
			marker(`(?i)\bVACUUM\b`, "VACUUM", "VACUUM FULL users"),
		},
	},
	{
		dialect: DialectSQLServer,
		markers: []dialectMarker{
			marker(`(?i)\bTOP\s*\(`, "TOP (", "SELECT TOP (10) * FROM users"),
			marker(`(?i)\bIDENTITY\s*\(`, "IDENTITY(", "CREATE TABLE t (id INT IDENTITY(1,1))"),
			marker(`(?i)\bNOLOCK\b`, "NOLOCK", "SELECT * FROM users WITH (NOLOCK)"),
			marker(`(?i)\bNVARCHAR2?\b`, "NVARCHAR", "SELECT CAST(a AS NVARCHAR(10)) FROM t"),
			// DECLARE, not "@": MySQL has user variables of its own, so bare
			// @name is ordinary MySQL and would be a false hint here.
			marker(`(?i)\bDECLARE\s+@`, "DECLARE @var", "DECLARE @n INT"),
			marker(`(?mi)^\s*GO\s*$`, "GO batch separator", "SELECT 1 FROM t\nGO\nSELECT 2 FROM t"),
		},
	},
	{
		dialect: DialectOracle,
		markers: []dialectMarker{
			marker(`(?i)\bVARCHAR2\b`, "VARCHAR2", "CREATE TABLE t (a VARCHAR2(10))"),
			marker(`(?i)\bCONNECT\s+BY\b`, "CONNECT BY", "SELECT * FROM t START WITH id=1 CONNECT BY PRIOR id=pid"),
			marker(`(?i)\bNUMBER\s*\(`, "NUMBER(", "CREATE TABLE t (a NUMBER(10))"),
		},
	},
	{
		dialect: DialectSQLite,
		markers: []dialectMarker{
			marker(`(?i)\bAUTOINCREMENT\b`, "AUTOINCREMENT", "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT)"),
			marker(`(?i)^\s*PRAGMA\b`, "PRAGMA", "PRAGMA table_info(users)"),
		},
	},
	{
		dialect: DialectHive,
		markers: []dialectMarker{
			marker(`(?i)\bPARTITIONED\s+BY\b`, "PARTITIONED BY", "CREATE TABLE t (a INT) PARTITIONED BY (dt STRING)"),
			marker(`(?i)\bTBLPROPERTIES\b`, "TBLPROPERTIES", "CREATE TABLE t (a INT) TBLPROPERTIES ('k'='v')"),
			marker(`(?i)\bLATERAL\s+VIEW\b`, "LATERAL VIEW", "SELECT * FROM t LATERAL VIEW explode(x) v AS y"),
		},
	},
}

// detectDialect reports the dialect whose marker syntax the statement
// carries most of, together with the markers matched, in table order. An
// empty dialect means no marker matched — which is NOT a verdict that the
// statement is MySQL: it means the engine has nothing to say about why the
// parse failed. Ties break in table order, so the result is deterministic.
func detectDialect(sql string) (Dialect, []string) {
	code := stripNonCode(sql)
	var best Dialect
	var bestMarkers []string
	for _, sig := range dialectSignatures {
		var seen []string
		for _, m := range sig.markers {
			if m.re.MatchString(code) {
				seen = append(seen, m.label)
			}
		}
		if len(seen) > len(bestMarkers) {
			best, bestMarkers = sig.dialect, seen
		}
	}
	return best, bestMarkers
}

// blockCommentEndsAt reports whether the block comment closes at i. It exists
// so the scan loop reads as "until the comment ends" instead of negating a
// two-clause condition in place (staticcheck's QF1001, and the reader's
// patience, both prefer the name).
func blockCommentEndsAt(sql string, i int) bool {
	return sql[i] == '*' && sql[i+1] == '/'
}

// stripNonCode blanks out string literals, quoted identifiers and comments so
// the markers are matched against code, not against prose. A marker inside
// 'RETURNING' or inside /* :: */ is not evidence of a dialect, and matching it
// would make the hint fire on ordinary MySQL statements that merely mention
// another dialect's keyword.
//
// `#` is deliberately not treated as a comment: it would swallow SQL Server
// temporary-table names (`#tmp`), and a `#` comment carrying a marker is a far
// rarer miss than that.
func stripNonCode(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	for i := 0; i < len(sql); {
		switch c := sql[i]; c {
		case '\'', '"', '`':
			b.WriteByte(' ')
			i++
			for i < len(sql) {
				if sql[i] == '\\' && c != '`' && i+1 < len(sql) {
					i += 2
					continue
				}
				if sql[i] == c {
					// A doubled quote is an escaped quote, not the end.
					if i+1 < len(sql) && sql[i+1] == c {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case '-':
			if i+1 < len(sql) && sql[i+1] == '-' {
				b.WriteByte(' ')
				for i < len(sql) && sql[i] != '\n' {
					i++
				}
				continue
			}
			b.WriteByte(c)
			i++
		case '/':
			if i+1 < len(sql) && sql[i+1] == '*' {
				b.WriteByte(' ')
				i += 2
				for i+1 < len(sql) && !blockCommentEndsAt(sql, i) {
					i++
				}
				if i+1 < len(sql) {
					i += 2
				} else {
					i = len(sql)
				}
				continue
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
