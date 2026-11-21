package plan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/sqlreview"
)

func pgSummaryRuleIDs(s *SQLReviewSummary) map[string]bool {
	out := map[string]bool{}
	for _, f := range s.Findings {
		out[f.Rule] = true
	}
	return out
}

func TestReviewStepsRoutesPostgresByExplicitDialect(t *testing.T) {
	const sql = "UPDATE users SET active = true WHERE id = 7 RETURNING id"
	pg, err := ReviewSteps([]SQLStepInput{{Step: "pg", SQL: sql, Dialect: sqlreview.DialectPostgres}})
	require.NoError(t, err)
	require.NotNil(t, pg)
	assert.Equal(t, string(sqlreview.VerdictWarn), pg.Verdict)
	assert.Contains(t, pgSummaryRuleIDs(pg), "SR-15")
	assert.NotContains(t, pgSummaryRuleIDs(pg), "SR-01")
	require.Len(t, pg.Impact.Tables, 1)
	assert.Equal(t, "users", pg.Impact.Tables[0].Table)

	mysql, err := ReviewSteps([]SQLStepInput{{Step: "mysql", SQL: sql}})
	require.NoError(t, err)
	require.NotNil(t, mysql)
	assert.Equal(t, string(sqlreview.VerdictFail), mysql.Verdict)
	assert.Contains(t, pgSummaryRuleIDs(mysql), "SR-01")
	assert.True(t, mysql.Impact.Incomplete)
}
