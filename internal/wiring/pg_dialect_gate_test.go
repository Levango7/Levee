package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/sqlreview"
)

func TestGeneratePlan_AllowsPostgresDialectAndStampsImpact(t *testing.T) {
	eng, _, runID := seedSQLRun(t, sqlWorkflowModule("pg-upsert-plan", "pg", "INSERT INTO users(id) VALUES (1) ON CONFLICT (id) DO NOTHING"))

	_, stored, err := eng.GeneratePlan(context.Background(), runID, []string{"web-1"})
	require.NoError(t, err)
	require.NotNil(t, stored)

	var artifact plan.Plan
	require.NoError(t, json.Unmarshal([]byte(stored.JSON), &artifact))
	require.NotNil(t, artifact.SQLReview)
	assert.Equal(t, string(sqlreview.VerdictPass), artifact.SQLReview.Verdict)
	require.NotNil(t, artifact.SQLReview.Impact)
	require.Len(t, artifact.SQLReview.Impact.Tables, 1)
	assert.Equal(t, "users", artifact.SQLReview.Impact.Tables[0].Table)
	assert.Contains(t, artifact.SQLReview.Impact.Tables[0].Operations, "insert")
	assert.False(t, artifact.SQLReview.Impact.Incomplete)

	assert.Equal(t, plan.ComputeHash(&artifact), stored.Hash,
		"the persisted artifact, PG impact included, must hash back to itself")
}

// The PG route is explicit (pg.query), not a parser guess. The same statement
// remains an SR-01 failure on the MySQL route, proving the two paths cannot
// silently change each other's contract.
func TestReviewWorkflowSQL_PGQueryUsesPostgresParser(t *testing.T) {
	src := sqlWorkflowModule("pg-upsert", "pg", "INSERT INTO users(id) VALUES (1) ON CONFLICT (id) DO NOTHING")
	wf, err := dsl.NewParser().ParseBytes([]byte(src))
	require.NoError(t, err)

	summary, err := reviewWorkflowSQL(wf)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, string(sqlreview.VerdictPass), summary.Verdict)
	assert.Empty(t, summary.Findings)
}

func TestReviewWorkflowSQL_PGCreateTableDoesNotAskForMySQLEngine(t *testing.T) {
	src := sqlWorkflowModule("pg-create", "pg", "CREATE TABLE t (id SERIAL PRIMARY KEY)")
	wf, err := dsl.NewParser().ParseBytes([]byte(src))
	require.NoError(t, err)

	summary, err := reviewWorkflowSQL(wf)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, string(sqlreview.VerdictPass), summary.Verdict)
	for _, finding := range summary.Findings {
		assert.NotEqual(t, "SR-01", finding.Rule)
		assert.NotEqual(t, "SR-10", finding.Rule)
		assert.NotEqual(t, "SR-11", finding.Rule)
	}
}

func TestReviewWorkflowSQL_MySQLPathIsUnchanged(t *testing.T) {
	src := sqlWorkflowModule("mysql-upsert", "mysql", "INSERT INTO users(id) VALUES (1) ON CONFLICT (id) DO NOTHING")
	wf, err := dsl.NewParser().ParseBytes([]byte(src))
	require.NoError(t, err)

	_, err = reviewWorkflowSQL(wf)
	require.Error(t, err)
	assert.True(t, errors.Is(err, sqlreview.ErrReviewFailed))
	assert.Contains(t, err.Error(), "SR-01")
	assert.Contains(t, err.Error(), "postgresql")
}
