// assignment_result_db_constraint_test.go — assignments.result is a closed
// vocabulary, and since v10 the database says so too.
//
// The Go write surface (SetAssignmentResult) has refused unknown tokens since
// 3b33e33c, but that guard only covers callers that go through the store. These
// cases pin the layer below it: the SQLite triggers reject the same values, and
// the two migrations' literal lists are compared against AssignResultNames()
// rather than against a hand copy — the failure mode that made the original
// read/write mismatch possible was two lists drifting apart quietly.
package state

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assignmentResultDomain is what the column may hold: every AssignResult
// constant plus the empty string ("no verdict recorded yet", the column's
// documented default).
func assignmentResultDomain(t *testing.T) []string {
	t.Helper()
	names := AssignResultNames()
	require.NotEmpty(t, names, "AssignResultNames must not be empty")
	out := append([]string{""}, names...)
	sort.Strings(out)
	return out
}

// inListFrom returns the sorted quoted literals of the first `IN (...)` in
// stmt, failing if there is none — a rewritten statement that drops the list
// would otherwise turn these assertions into no-ops.
func inListFrom(t *testing.T, stmt string) []string {
	t.Helper()
	m := regexp.MustCompile(`IN \(([^)]*)\)`).FindStringSubmatch(stmt)
	require.Len(t, m, 2, "statement has no IN (...) list; the guard moved and this test went blind: %s", firstLine(stmt))
	var out []string
	for _, lit := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, lit[1])
	}
	require.NotEmpty(t, out, "IN (...) list parsed empty in: %s", firstLine(stmt))
	sort.Strings(out)
	return out
}

func stepStmts(t *testing.T, steps []migrationStep, version int) []string {
	t.Helper()
	for _, step := range steps {
		if step.version == version {
			require.NotEmpty(t, step.stmts, "the version-%d step has no statements", version)
			return step.stmts
		}
	}
	t.Fatalf("no version-%d step in the ladder: the result-domain migration is gone", version)
	return nil
}

// TestRunAssignmentResultDomainIsEnforcedByTheDatabase exercises the triggers
// the only way that proves them: a writer that never goes through the store.
func TestRunAssignmentResultDomainIsEnforcedByTheDatabase(t *testing.T) {
	ctx := context.Background()
	st, err := NewSQLiteStore(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	insert := func(runID, result string) error {
		_, err := st.DB().ExecContext(ctx,
			`INSERT INTO run_assignment (run_id, owner_node, epoch, state, result) VALUES (?, ?, ?, ?, ?)`,
			runID, "node-1", 1, "done", result)
		return err
	}

	// '' is the documented "no verdict recorded yet" and must stay writable.
	require.NoError(t, insert("run-empty", ""), "'' is the column's default, not a violation")

	for _, bad := range []string{"running", "interrupted", "rolled_back_partial", "done", "Completed"} {
		err := insert("run-"+bad, bad)
		require.Error(t, err, "raw INSERT with %q must be refused by the database", bad)
		assert.Contains(t, err.Error(), "is not an assignment result", "result %q", bad)
	}

	require.NoError(t, insert("run-good", AssignResultCompleted))
	_, err = st.DB().ExecContext(ctx,
		`UPDATE run_assignment SET result = ? WHERE run_id = ?`, "running", "run-good")
	require.Error(t, err, "raw UPDATE to %q must be refused by the database", "running")
	assert.Contains(t, err.Error(), "is not an assignment result")
}

func TestSQLiteResultDomainMigrationMatchesTheVocabulary(t *testing.T) {
	stmts := stepStmts(t, migrations, 10)
	require.Len(t, stmts, 3, "v10 = backfill + two triggers")

	assert.Equal(t, assignmentResultDomain(t), inListFrom(t, stmts[0]),
		"the backfill must fold exactly the values outside the vocabulary")
	assert.Contains(t, stmts[0], "SET result = 'failed'",
		"folded rows take the same value the executor's default branch records")

	for _, stmt := range stmts[1:] {
		assert.Contains(t, stmt, "CREATE TRIGGER IF NOT EXISTS run_assignment_result_domain_",
			"v10 must install both insert and update guards")
		assert.Equal(t, assignmentResultDomain(t), inListFrom(t, stmt),
			"a trigger whose list differs from the vocabulary would refuse a legal verdict")
	}

	// Fresh databases never replay steps, so schema.sql has to carry the same
	// triggers or the column would be guarded only on upgraded installations.
	for _, name := range []string{"run_assignment_result_domain_insert", "run_assignment_result_domain_update"} {
		assert.True(t, strings.Contains(schemaSQL, "CREATE TRIGGER IF NOT EXISTS "+name),
			"schema.sql is missing %s; fresh databases would be built without the guard", name)
	}
}

func TestPostgresResultDomainMigrationMatchesTheVocabulary(t *testing.T) {
	stmts := stepStmts(t, pgMigrations, 10)
	require.Len(t, stmts, 2, "pg v10 = backfill + guarded ADD CONSTRAINT")

	assert.Equal(t, assignmentResultDomain(t), inListFrom(t, stmts[0]),
		"the PostgreSQL backfill must fold exactly the values outside the vocabulary")
	assert.Contains(t, stmts[1], "ADD CONSTRAINT run_assignment_result_domain CHECK",
		"the constraint must be the one pgschema.sql declares")
	assert.Equal(t, assignmentResultDomain(t), inListFrom(t, stmts[1]),
		"the constraint list must match the vocabulary")
	assert.Contains(t, stmts[1], "pg_constraint",
		"without the existence check the step is not replay-safe")

	assert.True(t, strings.Contains(pgSchemaSQL, "CONSTRAINT run_assignment_result_domain CHECK"),
		"pgschema.sql must carry the same constraint for fresh PostgreSQL databases")
}
