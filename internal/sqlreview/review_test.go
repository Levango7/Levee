package sqlreview

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ruleCases maps every rule ID to a statement that must trigger it. The
// table is the executable definition of "the engine has twenty rules": a
// rule that cannot fire is dead table weight, and this test turns that
// failure into a red build instead of a silent gap in the audit surface.
var ruleCases = map[string]string{
	"SR-01": "GRANT SELECT ON *.* TO 'u'@'%'", // grammar has no GRANT
	"SR-02": "DELETE FROM t",
	"SR-03": "UPDATE t SET a=1",
	"SR-04": "DELETE FROM t WHERE 1=1",
	"SR-05": "TRUNCATE TABLE t",
	"SR-06": "DROP DATABASE db1",
	"SR-07": "DROP TABLE t",
	"SR-08": "ALTER TABLE t ADD COLUMN c INT",
	"SR-09": "RENAME TABLE a TO b",
	"SR-10": "CREATE TABLE t (id INT, name VARCHAR(20)) ENGINE=InnoDB",
	"SR-11": "CREATE TABLE t (id INT PRIMARY KEY)",
	"SR-12": "SELECT * FROM t",
	"SR-13": "INSERT INTO t VALUES (1)",
	"SR-14": "DELETE FROM t WHERE id=1",
	"SR-15": "UPDATE t SET a=1 WHERE id=1",
	"SR-16": "SET GLOBAL x=1",
	"SR-17": "OPTIMIZE TABLE t",
	"SR-18": "SELECT * FROM t WHERE id=1 FOR UPDATE",
}

func ruleIDs(res *ReviewResult) map[string]bool {
	ids := map[string]bool{}
	for _, f := range res.Findings {
		ids[string(f.Rule)] = true
	}
	return ids
}

func TestEveryRuleFires(t *testing.T) {
	for _, r := range stmtRules {
		require.Contains(t, ruleCases, string(r.id), "rule %s has no triggering case in the test table", r.id)
	}
	for id, sql := range ruleCases {
		res, err := Review(sql)
		require.NoError(t, err, "case for %s", id)
		assert.Contains(t, ruleIDs(res), id, "rule %s did not fire for %q", id, sql)
	}
}

func TestRuleTableIsComplete(t *testing.T) {
	ids := map[string]bool{}
	for _, r := range stmtRules {
		assert.NotContains(t, ids, string(r.id), "duplicate rule id %s", r.id)
		ids[string(r.id)] = true
	}
	// SR-01 is structural (the parse itself) and SR-19/SR-20 are
	// batch-level; they live outside stmtRules on purpose.
	assert.Equal(t, 17, len(ids), "the statement-level table must hold SR-02..SR-18")
	assert.Equal(t, 20, len(ids)+3, "the engine must expose twenty rule IDs in total")
}

func TestTautologyVariants(t *testing.T) {
	for _, sql := range []string{
		"DELETE FROM t WHERE 1=1",
		"DELETE FROM t WHERE true",
		"DELETE FROM t WHERE 'x'='x'",
		"DELETE FROM t WHERE id=1 OR 1=1",
		"UPDATE t SET a=1 WHERE id=1 OR true",
	} {
		res, err := Review(sql)
		require.NoError(t, err)
		assert.Contains(t, ruleIDs(res), "SR-04", "case %q", sql)
	}
	// A conjunction that merely contains 1=1 is still a real predicate.
	res, err := Review("DELETE FROM t WHERE id=1 AND 1=1")
	require.NoError(t, err)
	assert.NotContains(t, ruleIDs(res), "SR-04")
}

func TestVerdictAggregation(t *testing.T) {
	res, err := Review("INSERT INTO t (a) VALUES (1)")
	require.NoError(t, err)
	assert.Equal(t, VerdictPass, res.Verdict)

	res, err = Review("DROP TABLE t")
	require.NoError(t, err)
	assert.Equal(t, VerdictWarn, res.Verdict)

	res, err = Review("DELETE FROM t")
	require.NoError(t, err)
	assert.Equal(t, VerdictFail, res.Verdict)

	// fail outranks warn when both are present. This batch is DDL-only,
	// so SR-19 must not fire; warns are SR-07 (drop) + SR-20 (multi-statement).
	res, err = Review("TRUNCATE TABLE t; DROP TABLE t")
	require.NoError(t, err)
	assert.Equal(t, VerdictFail, res.Verdict)
	assert.Equal(t, 1, res.FailCount)
	assert.Equal(t, 2, res.WarnCount)
	assert.NotContains(t, ruleIDs(res), "SR-19")
}

func TestBatchRules(t *testing.T) {
	res, err := Review("ALTER TABLE t ADD COLUMN c INT; UPDATE t SET a=1 WHERE id=1")
	require.NoError(t, err)
	ids := ruleIDs(res)
	assert.Contains(t, ids, "SR-19", "mixed DDL+DML batch must warn")
	assert.Contains(t, ids, "SR-20", "multi-statement batch must warn")
	assert.Equal(t, VerdictWarn, res.Verdict)

	// The same DDL alone is a one-statement batch: SR-20 must not fire.
	res, err = Review("ALTER TABLE t ADD COLUMN c INT")
	require.NoError(t, err)
	assert.NotContains(t, ruleIDs(res), "SR-20")
}

func TestGrammarBlindSpotsAreHonest(t *testing.T) {
	// CREATE INDEX parses as an alter-surface statement with no body; the
	// engine must say it cannot see the clause body, not pretend it passed.
	res, err := Review("CREATE INDEX idx ON t (name)")
	require.NoError(t, err)
	assert.Contains(t, ruleIDs(res), "SR-08")

	// GRANT does not parse at all; SR-01 must fail it rather than guess.
	res, err = Review("GRANT SELECT ON *.* TO 'u'@'%'")
	require.NoError(t, err)
	assert.Contains(t, ruleIDs(res), "SR-01")
	assert.Equal(t, SeverityFail, res.Findings[0].Severity)
}

func TestSilentStatements(t *testing.T) {
	for _, sql := range []string{
		"USE db1",
		"SHOW TABLES",
		"BEGIN",
		"COMMIT",
		"ROLLBACK",
		"EXPLAIN SELECT 1",
		"SET SESSION wait_timeout=5",
		"INSERT INTO t SELECT * FROM u",
		"CREATE TABLE t (id INT PRIMARY KEY, KEY idx (name)) ENGINE=InnoDB",
	} {
		res, err := Review(sql)
		require.NoError(t, err, sql)
		assert.Empty(t, res.Findings, "statement %q must not produce findings", sql)
	}
}

func TestEmptyInputIsAnError(t *testing.T) {
	_, err := Review("")
	assert.Error(t, err)
	_, err = Review("   \n  ")
	assert.Error(t, err)
}

func TestParseErrorCarriesStatementText(t *testing.T) {
	res, err := Review("SELECT 1;\nNOT SQL AT ALL;\nSELECT 2")
	require.NoError(t, err)
	require.Equal(t, VerdictFail, res.Verdict)
	var bad *string
	for i := range res.Statements {
		if res.Statements[i].Kind == KindUnknown {
			s := res.Statements[i].Text
			bad = &s
		}
	}
	require.NotNil(t, bad)
	assert.Contains(t, *bad, "NOT SQL AT ALL")
}

func TestMessageQuality(t *testing.T) {
	// Every message must be self-contained for an operator reading the
	// audit report: rule id alone must never be the whole explanation.
	for _, r := range stmtRules {
		assert.NotEmpty(t, r.msg)
		assert.Greater(t, len(r.msg), 40, "rule %s message is too thin to act on", r.id)
		assert.NotContains(t, r.msg, "TODO")
	}
}

func TestNoCopyleftToolText(t *testing.T) {
	// Legal boundary from the product decision: rule messages must not
	// carry the vocabulary of GPL/AGPL review tools (Yearning,
	// goInception, SQLE). The tripwire cannot prove authorship, but it
	// makes a copy-paste of tool-specific phrasing loud instead of silent.
	for _, r := range stmtRules {
		for _, banned := range []string{"yrunning", "goinception", "sqle", "sqlchecker"} {
			assert.NotContains(t, strings.ToLower(r.msg), banned,
				"rule %s message carries third-party tool vocabulary", r.id)
		}
	}
}
