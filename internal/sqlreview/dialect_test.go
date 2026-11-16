package sqlreview

// dialect_test.go pins the two SR-01 readings and the marker table behind the
// dialect one.
//
// Two defects drove this file. The message defect: a PostgreSQL statement came
// back as "does not parse", which reads as "your SQL is broken", so operators
// went looking for a typo that was not there. And the gate defect underneath
// it: the embedded library's lax Parse hands back a PARTIALLY parsed DDL
// instead of an error, so a PostgreSQL `CREATE TABLE t (id SERIAL PRIMARY
// KEY)` scored a clean PASS and a MySQL `CREATE TABLE ... AS SELECT` was
// advised to add a primary key — findings about a statement nobody wrote.
//
// The marker table is single-source: every marker carries the sample
// statement that (a) the grammar must refuse and (b) the detector must read as
// that dialect, both asserted below. A marker that stops being refused, or
// stops being detected, goes red instead of sitting in the table unchecked.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xwb1989/sqlparser"
)

// The property that makes a marker a marker: the embedded grammar refuses it.
// A token another dialect merely prefers (Oracle's NVL(), ROWNUM, DUAL) is
// ordinary MySQL syntax, so it is not in the table at all — see dialect.go.
func TestMarkersAreRejectedByTheGrammar(t *testing.T) {
	for _, sig := range dialectSignatures {
		for _, m := range sig.markers {
			require.NotEmpty(t, m.sample, "marker %s has no sample statement", m.label)
			_, err := sqlparser.ParseStrictDDL(m.sample)
			assert.Error(t, err,
				"marker %s (%s) claims to be syntax the grammar refuses, but %q parses",
				m.label, sig.dialect, m.sample)
		}
	}
}

// And the other half: each marker is actually reachable through detection,
// with its own dialect and its own label in the reported evidence.
func TestEveryMarkerIsDetected(t *testing.T) {
	for _, sig := range dialectSignatures {
		for _, m := range sig.markers {
			d, markers := detectDialect(m.sample)
			assert.Equal(t, sig.dialect, d, "dialect for marker %s", m.label)
			assert.Contains(t, markers, m.label,
				"the hint must name %s among the markers it matched", m.label)
		}
	}
}

// Detection is case-insensitive: SQL keywords are, in every dialect here, so
// a lowercase statement is as much evidence as an uppercase one.
func TestDetectDialect_IsCaseInsensitive(t *testing.T) {
	d, _ := detectDialect("update users set a=1 returning id")
	assert.Equal(t, DialectPostgres, d)
	d, _ = detectDialect("select * from users with (nolock)")
	assert.Equal(t, DialectSQLServer, d)
}

// The hint must not fire on prose. A MySQL statement is allowed to mention
// another dialect's keyword inside a literal, a quoted identifier or a
// comment — extracting markers from those would make the engine tell an
// operator their fine MySQL statement is written in the wrong dialect.
func TestDetectDialect_IgnoresLiteralsIdentifiersAndComments(t *testing.T) {
	for _, sql := range []string{
		"SELECT '::' AS s FROM t",
		`SELECT "RETURNING" FROM t`,
		"SELECT `NVL` FROM t",
		"UPDATE t SET note='returning' WHERE id=1",
		"SELECT a FROM t /* RETURNING id, :: */",
		"SELECT a FROM t -- ROWNUM, NVL(, VARCHAR2\n",
		"INSERT INTO t (a) VALUES ('it''s :: in a string')",
	} {
		d, markers := detectDialect(sql)
		assert.Empty(t, d, "no dialect may be inferred from prose: %q (markers: %v)", sql, markers)
	}
}

// Identifier collisions: `serial` is a plausible column name in an ordinary
// MySQL schema. The marker is anchored to the position a type occupies, so a
// column by that name is not evidence of anything.
func TestDetectDialect_OrdinaryIdentifiersDoNotTrigger(t *testing.T) {
	for _, sql := range []string{
		"SELECT serial FROM t",
		"SELECT id, serial FROM t WHERE id=1",
		"UPDATE t SET nvl=1 WHERE id=1",
		"SELECT top FROM t",
	} {
		d, markers := detectDialect(sql)
		assert.Empty(t, d, "%q names an ordinary identifier, not a dialect (markers: %v)", sql, markers)
	}
}

// A plain MySQL statement the grammar never covered (GRANT is absent from the
// frozen grammar) carries no marker, and the engine must say so rather than
// invent a dialect: the syntax reading names that possibility explicitly.
func TestDetectDialect_GrantHasNoDialectHint(t *testing.T) {
	d, markers := detectDialect(ruleCases["SR-01"])
	assert.Empty(t, d, "the grammar gap on GRANT is not a dialect gap")
	assert.Empty(t, markers)
}

func TestReview_SR01NamesTheDialectGap(t *testing.T) {
	res, err := Review("UPDATE users SET a=1 RETURNING id")
	require.NoError(t, err)
	require.Len(t, res.Findings, 1)
	f := res.Findings[0]

	assert.Equal(t, RuleID("SR-01"), f.Rule)
	assert.Equal(t, SeverityFail, f.Severity, "nothing was read, so nothing may run")
	assert.Equal(t, DialectPostgres, f.Dialect)
	assert.Contains(t, f.Message, "postgresql-only syntax")
	assert.Contains(t, f.Message, "RETURNING", "the hint must show its evidence")
	assert.Contains(t, f.Message, "dialect gap")
	assert.NotContains(t, f.Message, "no marker of the dialects")
}

func TestReview_SR01NamesTheSyntaxReading(t *testing.T) {
	res, err := Review("SELECT * FROM")
	require.NoError(t, err)
	require.Len(t, res.Findings, 1)
	f := res.Findings[0]

	assert.Equal(t, RuleID("SR-01"), f.Rule)
	assert.Equal(t, SeverityFail, f.Severity)
	assert.Empty(t, f.Dialect, "no dialect may be claimed without a marker")
	assert.Contains(t, f.Message, "no marker of the dialects the engine recognises")
	assert.NotContains(t, f.Message, "dialect gap")
}

// The refusal did not get softer: a statement of which the grammar read
// NOTHING still scores a fail, whatever dialect it turns out to be in. This is
// the whole point — the message got more useful, the gate did not move.
func TestReview_UnreadStatementIsStillAFail(t *testing.T) {
	for _, sql := range []string{
		"SELECT id::text FROM users",
		"SELECT TOP (10) * FROM users",
		"SELECT * FROM users WITH (NOLOCK)",
		"PRAGMA table_info(users)",
		"SELECT * FROM t LATERAL VIEW explode(x) v AS y",
		"SELECT * FROM t START WITH id=1 CONNECT BY PRIOR id=pid",
	} {
		res, err := Review(sql)
		require.NoError(t, err, "case %q", sql)
		assert.Equal(t, VerdictFail, res.Verdict, "a statement the engine cannot read must not pass: %q", sql)
		assert.Equal(t, 1, res.FailCount)
		assert.Equal(t, SeverityFail, res.Findings[0].Severity)
	}
}

// The DDL fail-open this change closes: these statements come back from the
// lax parser as a disjoint prefix, and before the strict check they were
// scored as if the prefix were the statement — PostgreSQL, Transact-SQL and
// SQLite DDL scored a clean PASS, and MySQL's own `CREATE TABLE ... AS
// SELECT` / `LIKE` forms were lectured about a missing primary key. None of
// them may pass now, and none may wear a finding about a statement nobody
// wrote.
func TestReview_PartiallyReadDDLNeverPasses(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE t (id SERIAL PRIMARY KEY)",
		"CREATE TABLE t (j JSONB)",
		"CREATE TABLE t (id INT IDENTITY(1,1))",
		"CREATE TABLE t (a VARCHAR2(10))",
		"CREATE TABLE t (a NUMBER(10))",
		"CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT)",
		"CREATE TABLE t (a INT) PARTITIONED BY (dt STRING) STORED AS ORC",
		"CREATE TABLE t (a INT) TBLPROPERTIES ('k'='v')",
		"CREATE TABLE t AS SELECT * FROM t2",
		"CREATE TABLE t LIKE t2",
		"CREATE TABLE t (id INT, FOREIGN KEY (id) REFERENCES t2(id))",
		"CREATE TABLE t (a INT) PARTITION BY RANGE (id) (PARTITION p0 VALUES LESS THAN (10))",
	} {
		res, err := Review(sql)
		require.NoError(t, err, "case %q", sql)
		assert.NotEqual(t, VerdictPass, res.Verdict, "a DDL that was only read partway must not pass: %q", sql)

		ids := ruleIDs(res)
		assert.Contains(t, ids, "SR-01", "the partial read must be reported as SR-01: %q", sql)
		require.Len(t, res.Findings, 1, "only the partial read may be reported: %q", sql)
		assert.Equal(t, SeverityWarn, res.Findings[0].Severity)
		assert.NotContains(t, res.Findings[0].Message, "no primary key",
			"the prefix must not be scored: %q", sql)
	}
}

// The two readings of a partially read DDL: one for DDL that belongs to
// another server, one for the MySQL clauses the frozen grammar never covered.
func TestReview_PartialDDLReadings(t *testing.T) {
	res, err := Review("CREATE TABLE t (id SERIAL PRIMARY KEY)")
	require.NoError(t, err)
	f := res.Findings[0]
	assert.Equal(t, DialectPostgres, f.Dialect)
	assert.Contains(t, f.Message, "read only partway")
	assert.Contains(t, f.Message, "postgresql-only syntax (SERIAL)")
	assert.Contains(t, f.Message, "another server")

	res, err = Review("CREATE TABLE t (a INT) PARTITION BY RANGE (id) (PARTITION p0 VALUES LESS THAN (10))")
	require.NoError(t, err)
	f = res.Findings[0]
	assert.Empty(t, f.Dialect, "a MySQL partitioning clause is not another dialect")
	assert.Contains(t, f.Message, "read only partway")
	assert.Contains(t, f.Message, "never the statement you wrote")
}

// A DDL the grammar does not read AT ALL is a fail, not the prefix warn: the
// lax parser has no prefix to hand back, so nothing was classified and nothing
// may run. The distinction is mechanical, not a judgement call.
func TestReview_UnreadableDDLIsAFailNotAWarn(t *testing.T) {
	res, err := Review("CREATE TABLE")
	require.NoError(t, err)
	require.Len(t, res.Findings, 1)
	assert.Equal(t, SeverityFail, res.Findings[0].Severity)
	assert.Equal(t, KindUnknown, res.Statements[0].Kind)
	assert.Equal(t, VerdictFail, res.Verdict)
}

// A DDL the grammar reads in full keeps the ordinary treatment: the strict
// check must not turn ordinary schema changes into partial-read warnings.
func TestReview_FullyReadDDLKeepsItsFindings(t *testing.T) {
	res, err := Review("ALTER TABLE t ADD COLUMN c INT")
	require.NoError(t, err)
	assert.Equal(t, VerdictWarn, res.Verdict)
	assert.Contains(t, ruleIDs(res), "SR-08", "the ordinary alter finding must survive")
	assert.NotContains(t, res.Findings[0].Message, "read only partway")
}

// The hint has to survive the JSON envelope: that is the channel a UI or a
// pipeline reads, and an omitted field would make the readings
// indistinguishable for any consumer that does not parse English.
func TestReview_DialectHintIsInTheJSONEnvelope(t *testing.T) {
	res, err := Review("SELECT id::text FROM users")
	require.NoError(t, err)
	blob, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(blob), `"dialect":"postgresql"`)

	plain, err := Review("SELECT * FROM")
	require.NoError(t, err)
	blob, err = json.Marshal(plain)
	require.NoError(t, err)
	assert.NotContains(t, string(blob), `"dialect"`,
		"the syntax reading carries no dialect, not an empty one")
}

// A batch can hold both readings at once, and each statement keeps its own.
func TestReview_MixedBatchKeepsBothReadings(t *testing.T) {
	res, err := Review("SELECT * FROM; SELECT id::text FROM users")
	require.NoError(t, err)
	require.Len(t, res.Statements, 2)
	require.Len(t, res.Statements[0].Findings, 1)
	require.Len(t, res.Statements[1].Findings, 1)

	assert.Empty(t, res.Statements[0].Findings[0].Dialect)
	assert.Equal(t, DialectPostgres, res.Statements[1].Findings[0].Dialect)
	assert.Equal(t, VerdictFail, res.Verdict)
}
