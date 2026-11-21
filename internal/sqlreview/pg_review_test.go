package sqlreview

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reviewPG(t *testing.T, sql string) *ReviewResult {
	t.Helper()
	res, err := ReviewPostgres(sql)
	require.NoError(t, err)
	return res
}

func TestReviewPostgres_ParseFailureStaysFailClosed(t *testing.T) {
	res := reviewPG(t, "THIS IS NOT SQL ((")
	assert.Equal(t, VerdictFail, res.Verdict)
	require.Len(t, res.Findings, 1)
	assert.Equal(t, RuleID("SR-01"), res.Findings[0].Rule)
	assert.Equal(t, DialectPostgres, res.Findings[0].Dialect)
	assert.True(t, res.Impact.Incomplete)
	assert.Empty(t, res.Impact.Tables)
}

func TestReviewPostgres_DMLRulesUsePostgresAST(t *testing.T) {
	cases := []struct {
		sql  string
		rule RuleID
	}{
		{"DELETE FROM users RETURNING id", "SR-02"},
		{"UPDATE users SET active = true RETURNING id", "SR-03"},
		{"DELETE FROM users WHERE id = 1 OR 1 = 1", "SR-04"},
		{"UPDATE users SET active = true WHERE true", "SR-04"},
		{"TRUNCATE TABLE users", "SR-05"},
		{"DROP DATABASE appdb", "SR-06"},
		{"DROP TABLE users CASCADE", "SR-07"},
		{"ALTER TABLE users ADD COLUMN note text", "SR-08"},
		{"CREATE INDEX idx_users_email ON users(email)", "SR-08"},
		{"ALTER TABLE users RENAME TO customers", "SR-09"},
		{"SELECT * FROM users", "SR-12"},
		{"INSERT INTO users VALUES (1)", "SR-13"},
		{"DELETE FROM users WHERE id = 7", "SR-14"},
		{"UPDATE users SET active = true WHERE id = 7", "SR-15"},
		{"SELECT id FROM users FOR UPDATE", "SR-18"},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			res := reviewPG(t, tc.sql)
			assert.Contains(t, ruleIDs(res), string(tc.rule), "rule %s should fire: %#v", tc.rule, res.Findings)
			assert.NotContains(t, ruleIDs(res), "SR-01", "valid PostgreSQL must not be called a dialect gap")
		})
	}
}

func TestReviewPostgres_DoesNotSuggestNonexistentDMLLimit(t *testing.T) {
	for _, sql := range []string{"DELETE FROM users WHERE id = 7", "UPDATE users SET active = true WHERE id = 7"} {
		res := reviewPG(t, sql)
		require.Len(t, res.Findings, 1)
		assert.Contains(t, res.Findings[0].Message, "ctid or a unique key")
		assert.NotContains(t, res.Findings[0].Message, "add LIMIT")
	}
}

func TestReviewPostgres_MySQLOnlyRulesDoNotMisfire(t *testing.T) {
	res := reviewPG(t, "CREATE TABLE t (id SERIAL PRIMARY KEY); SET search_path TO public")
	ids := ruleIDs(res)
	for _, id := range []string{"SR-01", "SR-10", "SR-11", "SR-16"} {
		assert.NotContains(t, ids, id)
	}
	assert.Equal(t, VerdictWarn, res.Verdict, "only the multi-statement SR-20 warning remains")
}

func TestReviewPostgres_SelectIsAReadNotAWrite(t *testing.T) {
	res := reviewPG(t, "SELECT * FROM users")
	require.Len(t, res.Statements, 1)
	assert.Empty(t, res.Statements[0].Impact.Writes)
	assert.Equal(t, []string{"users"}, res.Statements[0].Impact.Reads)
	assert.True(t, res.Statements[0].Impact.WholeTable)
}

func TestReviewPostgres_CountStarIsNotSelectStar(t *testing.T) {
	res := reviewPG(t, "SELECT count(*) FROM users")
	assert.NotContains(t, ruleIDs(res), "SR-12")
}

func TestReviewPostgres_CTASIsReadInFull(t *testing.T) {
	res := reviewPG(t, "CREATE TABLE archive AS SELECT * FROM users")
	ids := ruleIDs(res)
	assert.NotContains(t, ids, "SR-01")
	assert.NotContains(t, ids, "SR-10")
	assert.NotContains(t, ids, "SR-11")
	assert.Contains(t, ids, "SR-12")
	require.Len(t, res.Statements, 1)
	imp := res.Statements[0].Impact
	assert.Equal(t, []string{"archive"}, imp.Writes)
	assert.Equal(t, []string{"users"}, imp.Reads)
	assert.True(t, imp.Complete)
}

func TestReviewPostgres_ImpactUsesRealTablesNotAliases(t *testing.T) {
	res := reviewPG(t, "WITH old AS (SELECT id FROM users) UPDATE users SET active = true WHERE id IN (SELECT id FROM old)")
	require.Len(t, res.Impact.Tables, 1)
	assert.Equal(t, "users", res.Impact.Tables[0].Table)
	assert.Contains(t, res.Impact.Tables[0].Operations, "update")
	assert.False(t, res.Impact.Tables[0].WholeTable)
}

func TestReviewPostgres_BatchUsesPostgresAtomicityWording(t *testing.T) {
	res := reviewPG(t, "ALTER TABLE users ADD COLUMN note text; UPDATE users SET active = true WHERE id = 7")
	ids := ruleIDs(res)
	assert.Contains(t, ids, "SR-19")
	assert.Contains(t, ids, "SR-20")
	for _, f := range res.Findings {
		if f.Rule == "SR-19" {
			assert.Contains(t, f.Message, "PostgreSQL")
			assert.NotContains(t, f.Message, "implicit commit in MySQL")
		}
	}
}

func TestReviewPostgres_OliphantDialectSamplesStayReadable(t *testing.T) {
	for _, sample := range pgDialectSamples {
		t.Run(sample.name, func(t *testing.T) {
			res := reviewPG(t, sample.sql)
			assert.NotContains(t, ruleIDs(res), "SR-01", "the PG parser must cover the spike sample")
		})
	}
}
