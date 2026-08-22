package state

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =========================================================================
// Closed-store behaviour: every Store method must return an error instead of
// panicking once the store has been closed. This also pins down the error
// wrapping of every CRUD method.
// =========================================================================

func TestClosedStore_AllMethodsReturnErrors(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	require.NoError(t, store.Close())

	now := time.Now().UTC().Truncate(time.Second)

	tests := []struct {
		name string
		call func() error
	}{
		{"CreateRun", func() error { return store.CreateRun(ctx, &Run{ID: "r"}) }},
		{"GetRun", func() error { _, err := store.GetRun(ctx, "r"); return err }},
		{"UpdateRun", func() error { return store.UpdateRun(ctx, &Run{ID: "r"}) }},
		{"ListRuns", func() error { _, err := store.ListRuns(ctx, RunFilter{}); return err }},
		{"DeleteRun", func() error { return store.DeleteRun(ctx, "r") }},

		{"CreateBatch", func() error { return store.CreateBatch(ctx, &Batch{ID: "b"}) }},
		{"GetBatch", func() error { _, err := store.GetBatch(ctx, "b"); return err }},
		{"UpdateBatch", func() error { return store.UpdateBatch(ctx, &Batch{ID: "b"}) }},
		{"ListBatches", func() error { _, err := store.ListBatches(ctx, BatchFilter{}); return err }},
		{"DeleteBatch", func() error { return store.DeleteBatch(ctx, "b") }},

		{"CreateStep", func() error { return store.CreateStep(ctx, &Step{ID: "s"}) }},
		{"GetStep", func() error { _, err := store.GetStep(ctx, "s"); return err }},
		{"UpdateStep", func() error { return store.UpdateStep(ctx, &Step{ID: "s"}) }},
		{"ListSteps", func() error { _, err := store.ListSteps(ctx, StepFilter{}); return err }},
		{"DeleteStep", func() error { return store.DeleteStep(ctx, "s") }},

		{"CreateTrace", func() error { return store.CreateTrace(ctx, &Trace{ID: "t"}) }},
		{"GetTrace", func() error { _, err := store.GetTrace(ctx, "t"); return err }},
		{"UpdateTrace", func() error { return store.UpdateTrace(ctx, &Trace{ID: "t"}) }},
		{"ListTraces", func() error { _, err := store.ListTraces(ctx, TraceFilter{}); return err }},
		{"DeleteTrace", func() error { return store.DeleteTrace(ctx, "t") }},

		{"CreateApproval", func() error { return store.CreateApproval(ctx, &Approval{ID: "a"}) }},
		{"GetApproval", func() error { _, err := store.GetApproval(ctx, "a"); return err }},
		{"UpdateApproval", func() error { return store.UpdateApproval(ctx, &Approval{ID: "a"}) }},
		{"ListApprovals", func() error { _, err := store.ListApprovals(ctx, ApprovalFilter{}); return err }},
		{"DeleteApproval", func() error { return store.DeleteApproval(ctx, "a") }},

		{"CreateLock", func() error { return store.CreateLock(ctx, &Lock{ID: "l"}) }},
		{"GetLock", func() error { _, err := store.GetLock(ctx, "l"); return err }},
		{"GetLockByScope", func() error { _, err := store.GetLockByScope(ctx, "scope"); return err }},
		{"UpdateLock", func() error { return store.UpdateLock(ctx, &Lock{ID: "l"}) }},
		{"UpdateLockOwnedBy", func() error {
			_, err := store.UpdateLockOwnedBy(ctx, "l", "owner", 60, now)
			return err
		}},
		{"ListLocks", func() error { _, err := store.ListLocks(ctx); return err }},
		{"DeleteLock", func() error { return store.DeleteLock(ctx, "l") }},
		{"DeleteExpiredLocks", func() error {
			_, err := store.DeleteExpiredLocks(ctx, now)
			return err
		}},

		{"CreateCredential", func() error { return store.CreateCredential(ctx, &Credential{ID: "c"}) }},
		{"GetCredential", func() error { _, err := store.GetCredential(ctx, "c"); return err }},
		{"GetCredentialByName", func() error { _, err := store.GetCredentialByName(ctx, "name"); return err }},
		{"UpdateCredential", func() error { return store.UpdateCredential(ctx, &Credential{ID: "c"}) }},
		{"ListCredentials", func() error { _, err := store.ListCredentials(ctx); return err }},
		{"DeleteCredential", func() error { return store.DeleteCredential(ctx, "c") }},

		{"CreateAudit", func() error { return store.CreateAudit(ctx, &Audit{ID: "au"}) }},
		{"GetAudit", func() error { _, err := store.GetAudit(ctx, "au"); return err }},
		{"ListAudits", func() error { _, err := store.ListAudits(ctx, AuditFilter{}); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			require.Error(t, err, "closed store must return an error, not succeed or panic")
			assert.Contains(t, err.Error(), "state:")
		})
	}

	// Closing twice is safe.
	require.NoError(t, store.Close())
}

func TestClose_NilDBIsNoop(t *testing.T) {
	// A zero-value store has no db handle; Close must not panic.
	require.NoError(t, (&SQLiteStore{}).Close())
}

// =========================================================================
// Migrate internals: failure paths and the statement splitter
// =========================================================================

func TestMigrate_ClosedDBReturnsError(t *testing.T) {
	store := newTestStore(t)
	db := store.DB()
	require.NoError(t, store.Close())

	err := Migrate(context.Background(), db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create schema_version")
}

func TestExecMultiStatement_ErrorNamesFirstLine(t *testing.T) {
	store := newTestStore(t)

	err := execMultiStatement(context.Background(), store.DB(),
		"CREATE TABLE totally_broken ((\n    SELECT 1;")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exec statement")
	assert.Contains(t, err.Error(), "CREATE TABLE totally_broken ((")
}

func TestExecMultiStatement_StripsLineComments(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	script := "-- a leading comment\n" +
		"CREATE TABLE comment_probe (v TEXT);\n" +
		"-- another comment\n" +
		"INSERT INTO comment_probe VALUES ('ok');\n" +
		"-- trailing comment with no statement"
	require.NoError(t, execMultiStatement(ctx, store.DB(), script))

	var n int
	err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM comment_probe`).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestSplitSQLStatements(t *testing.T) {
	tests := []struct {
		name         string
		script       string
		wantCount    int
		wantFirstHas []string // substrings required in the first statement
	}{
		{
			name:      "simple statements split on semicolons",
			script:    "SELECT 1;\nSELECT 2;\n",
			wantCount: 2,
		},
		{
			name:         "leftover text without trailing semicolon is kept",
			script:       "SELECT 3",
			wantCount:    1,
			wantFirstHas: []string{"SELECT 3"},
		},
		{
			name: "trigger block with embedded semicolons stays intact",
			script: "CREATE TRIGGER tr_before_delete BEFORE DELETE ON t\n" +
				"BEGIN\n" +
				"    INSERT INTO audit_log VALUES ('x;y');\n" +
				"END;\n" +
				"SELECT 9;",
			wantCount:    2,
			wantFirstHas: []string{"CREATE TRIGGER", "BEGIN", "x;y", "END"},
		},
		{
			name: "BEGIN inside data does not enter trigger mode",
			script: "INSERT INTO notes VALUES ('hello');\n" +
				"BEGIN-like text without CREATE TRIGGER ahead;\n",
			wantCount: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitSQLStatements(tt.script)
			require.Len(t, got, tt.wantCount)
			if len(tt.wantFirstHas) > 0 {
				for _, sub := range tt.wantFirstHas {
					assert.Contains(t, got[0], sub)
				}
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"multiline picks first non-empty", "\n  \nSELECT 1;\nmore", "SELECT 1;"},
		{"single line", "SELECT 1;", "SELECT 1;"},
		{"no newline at all", "just text", "just text"},
		{"blank input falls back", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, firstLine(tt.in))
		})
	}
}

// =========================================================================
// PostgreSQL support helpers (pure logic, no database required)
// =========================================================================

func TestPgJoinPlaceholders(t *testing.T) {
	tests := []struct {
		name    string
		clauses []string
		want    string
	}{
		{"empty", nil, ""},
		{"single", []string{"status = $%d"}, "status = $1"},
		{
			"multiple numbered sequentially",
			[]string{"run_id = $%d", "level = $%d", "status = $%d"},
			"run_id = $1 AND level = $2 AND status = $3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pgJoinPlaceholders(tt.clauses))
		})
	}
}

func TestPgSplitSQLStatements(t *testing.T) {
	t.Run("simple statements", func(t *testing.T) {
		got := pgSplitSQLStatements("SELECT 1; SELECT 2;")
		require.Len(t, got, 2)
		assert.Equal(t, "SELECT 1;", got[0])
	})

	t.Run("dollar quoted function body keeps inner semicolons", func(t *testing.T) {
		script := "CREATE FUNCTION f() RETURNS void AS $$\n" +
			"BEGIN\n" +
			"  PERFORM 1; PERFORM 2;\n" +
			"END\n" +
			"$$ LANGUAGE plpgsql; SELECT 3;"
		got := pgSplitSQLStatements(script)
		require.Len(t, got, 2)
		assert.Contains(t, got[0], "PERFORM 1; PERFORM 2;")
		assert.Contains(t, got[0], "$$")
		assert.Equal(t, " SELECT 3;", got[1])
	})

	t.Run("tagged dollar quotes pair on the same tag", func(t *testing.T) {
		script := "$fn$ body with ; semicolon $fn$; SELECT 4;"
		got := pgSplitSQLStatements(script)
		require.Len(t, got, 2)
		assert.Contains(t, got[0], "body with ; semicolon")
		assert.Equal(t, " SELECT 4;", got[1])
	})

	t.Run("begin end blocks outside dollar quotes are not split", func(t *testing.T) {
		script := "CREATE TRIGGER t BEFORE DELETE ON r BEGIN PERFORM raise(); END; SELECT 5;"
		got := pgSplitSQLStatements(script)
		require.Len(t, got, 2)
		assert.Contains(t, got[0], "BEGIN")
		assert.Contains(t, got[0], "END")
		assert.Equal(t, " SELECT 5;", got[1])
	})

	t.Run("lowercase begin end recognised case insensitively", func(t *testing.T) {
		script := "begin perform 1; end; select 6;"
		got := pgSplitSQLStatements(script)
		require.Len(t, got, 2)
		assert.Equal(t, " select 6;", got[1])
	})

	t.Run("word boundaries prevent false BEGIN matches", func(t *testing.T) {
		// "BEGINS" must not be treated as the BEGIN keyword.
		script := "SELECT BEGINS; SELECT 7;"
		got := pgSplitSQLStatements(script)
		require.Len(t, got, 2)
		assert.Equal(t, " SELECT 7;", got[1])
	})

	t.Run("leftover without trailing semicolon", func(t *testing.T) {
		got := pgSplitSQLStatements("SELECT 8")
		require.Len(t, got, 1)
		assert.Equal(t, "SELECT 8", got[0])
	})
}

func TestIndexDollarQuoteEnd(t *testing.T) {
	tests := []struct {
		name   string
		script string
		start  int
		want   int
	}{
		{"double dollar", "$$body$$", 0, 1},
		{"tagged dollar", "$fn$body$fn$", 0, 3},
		{"dollar mid-string", "abc$$rest", 3, 4},
		{"start is not a dollar", "abc", 0, -1},
		{"space terminates the tag", "$f d$", 0, -1},
		{"unterminated tag", "$fn", 0, -1},
		{"bare dollar at end", "$", 0, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, indexDollarQuoteEnd(tt.script, tt.start))
		})
	}
}

func TestHasWordAt(t *testing.T) {
	tests := []struct {
		name   string
		script string
		index  int
		word   string
		want   bool
	}{
		{"exact match at end", "BEGIN", 0, "BEGIN", true},
		{"case insensitive", "begin x", 0, "BEGIN", true},
		{"mid string with space boundary", "an END here", 3, "END", true},
		{"followed by letter is not a word", "BEGINX", 0, "BEGIN", false},
		{"followed by underscore is not a word", "BEGIN_X", 0, "BEGIN", false},
		{"followed by digit is not a word", "END1", 0, "END", false},
		{"word longer than remainder", "BE", 0, "BEGIN", false},
		{"mismatched letters", "BEGAN x", 0, "BEGIN", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasWordAt(tt.script, tt.index, tt.word))
		})
	}
}

func TestPgExecMultiStatement_AgainstSQLite(t *testing.T) {
	// pgExecMultiStatement only depends on the database/sql surface, so the
	// splitting + execution behaviour can be exercised against the SQLite
	// driver for statements whose syntax both dialects share.
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("valid script executes statement by statement", func(t *testing.T) {
		script := "CREATE TABLE IF NOT EXISTS pg_exec_probe (v TEXT);\n" +
			"-- comment between statements\n" +
			"INSERT INTO pg_exec_probe VALUES ('ok');"
		require.NoError(t, pgExecMultiStatement(ctx, store.DB(), script))

		var n int
		err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_exec_probe`).Scan(&n)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
	})

	t.Run("invalid statement surfaces the offending line", func(t *testing.T) {
		err := pgExecMultiStatement(ctx, store.DB(), "NOT VALID PG SQL EITHER;")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exec pg statement")
	})
}

func TestPgMigrate_NilDBRejected(t *testing.T) {
	err := pgMigrate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil db")
}

func TestPgAppliedSchemaVersion_ReadsSQLiteVersionTable(t *testing.T) {
	// The version query is dialect-neutral; run it against the SQLite-backed
	// schema_version table created by newTestStore.
	store := newTestStore(t)
	v, err := pgAppliedSchemaVersion(context.Background(), store.DB())
	require.NoError(t, err)
	assert.Equal(t, currentSchemaVersion, v)
}

func TestApplyPGPoolConfig(t *testing.T) {
	t.Run("positive values are applied", func(t *testing.T) {
		db, err := sql.Open("sqlite", ":memory:")
		require.NoError(t, err)
		defer func() { _ = db.Close() }()

		applyPGPoolConfig(db, PGPoolConfig{
			MaxOpenConns:    7,
			MaxIdleConns:    3,
			ConnMaxLifetime: time.Minute,
			ConnMaxIdleTime: 10 * time.Second,
		})
		// database/sql does not expose the configured max-idle/lifetime values,
		// so only max-open is directly observable.
		assert.Equal(t, 7, db.Stats().MaxOpenConnections)
	})

	t.Run("zero values leave defaults untouched", func(t *testing.T) {
		db, err := sql.Open("sqlite", ":memory:")
		require.NoError(t, err)
		defer func() { _ = db.Close() }()

		applyPGPoolConfig(db, PGPoolConfig{})
		assert.Equal(t, 0, db.Stats().MaxOpenConnections) // 0 = unlimited default
	})
}

func TestPgxParseConfig(t *testing.T) {
	t.Run("valid dsn", func(t *testing.T) {
		cfg, err := pgxParseConfig("postgres://user:pass@localhost:5432/levee?sslmode=disable")
		require.NoError(t, err)
		require.NotNil(t, cfg)
	})

	t.Run("invalid dsn", func(t *testing.T) {
		_, err := pgxParseConfig("postgres://%zz@localhost/db")
		require.Error(t, err)
	})
}

func TestNewPGStore_RejectsBadDSN(t *testing.T) {
	ctx := context.Background()

	_, err := NewPGStore(ctx, "", PGPoolConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty postgres dsn")

	_, err = NewPGStore(ctx, "postgres://%zz@localhost/db", PGPoolConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse postgres dsn")

	// A syntactically valid DSN pointing at a closed port fails the ping with
	// a clear error instead of returning a half-initialised store.
	_, err = NewPGStore(ctx, "postgres://postgres:pw@127.0.0.1:1/levee?sslmode=disable&connect_timeout=1", PGPoolConfig{})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "ping postgres"), "unexpected error: %v", err)
}
