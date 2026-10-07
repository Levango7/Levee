// agents_schema_test.go — the shape half of the agent registry: the exact
// column set each engine declares, the struct-to-table mapping, and the real
// v7 -> v8 upgrade.
//
// The column-set pin follows internal/dbschema/cluster_nodes_test.go's style
// (pin the list exactly rather than "covers the users"), but it lives here
// rather than in dbschema because dbschema is only for DDL more than one
// package applies. agents is created by internal/state alone — schema.sql for
// SQLite, pgschema.sql for PostgreSQL — so a second copy is not the failure
// mode to guard; fresh-vs-upgraded-vs-struct drift is.

package state

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expectedAgentColumns is the pinned column set, in declaration order. Order
// is not cosmetic: the v8 step CREATEs the table, so schema.sql and the step
// must append nothing differently, and
// TestMigrate_FreshAndUpgraded_SchemaShapesMatch compares by position.
var expectedAgentColumns = []string{
	"id", "address", "capabilities", "status", "last_heartbeat",
	"registered_at", "active_tasks", "completed_tasks", "failed_tasks",
	"max_concurrent",
}

const agentsCreateSignature = "CREATE TABLE IF NOT EXISTS agents ("

// multiLineColumns parses a CREATE TABLE block out of a .sql script: one
// column per line, stopping at the line that closes the block. Copied in
// spirit from dbschema's columnNames, which learned to read line-by-line
// because a first-")" scan stops inside DEFAULT NOW().
func multiLineColumns(t *testing.T, script, signature string) []string {
	t.Helper()
	i := strings.Index(script, signature)
	require.GreaterOrEqual(t, i, 0, "the script must declare %s", signature)

	var out []string
	for _, line := range strings.Split(script[i+len(signature):], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if strings.HasPrefix(trimmed, ")") {
			break
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "UNIQUE", "CHECK", "CONSTRAINT", "PRIMARY", "FOREIGN":
			continue // table constraints, not columns
		}
		out = append(out, strings.TrimSuffix(fields[0], ","))
	}
	return out
}

// oneLineColumns parses the single-line CREATE TABLE the migration steps use
// (`id TEXT PRIMARY KEY, address TEXT NOT NULL, …`). Safe to split on ", "
// only because no token in these DDL strings contains a comma; the test that
// asserts the result matches the multi-line list is what catches a future
// DEFAULT ('a','b')-style edit breaking that assumption.
func oneLineColumns(t *testing.T, stmt, signature string) []string {
	t.Helper()
	i := strings.Index(stmt, signature)
	require.GreaterOrEqual(t, i, 0, "the statement must declare %s", signature)
	body := stmt[i+len(signature):]
	end := strings.LastIndex(body, ")")
	require.GreaterOrEqual(t, end, 0, "unterminated CREATE TABLE in %q", stmt)

	var out []string
	for _, part := range strings.Split(body[:end], ", ") {
		fields := strings.Fields(strings.TrimSpace(part))
		require.NotEmpty(t, fields, "empty column definition in %q", stmt)
		out = append(out, fields[0])
	}
	return out
}

// agentColumn is one field of the Agent struct, snake_cased the way the table
// names it. ID is spelled out because strings.ToLower("ID") would be "id"
// anyway; the two all-caps acronyms in this struct are the only hazard.
func agentColumn(name string) string {
	if name == "ID" {
		return "id"
	}
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteByte(c - 'A' + 'a')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func TestAgentStructMapsOneColumnPerField(t *testing.T) {
	typ := reflect.TypeOf(Agent{})
	var fromStruct []string
	for i := 0; i < typ.NumField(); i++ {
		fromStruct = append(fromStruct, agentColumn(typ.Field(i).Name))
	}
	assert.Equal(t, expectedAgentColumns, fromStruct,
		"every Agent field must have exactly one column, and no column may be orphaned")
}

func TestAgentsDDL_ColumnSetIsPinnedInBothEngines(t *testing.T) {
	// schema.sql is what a fresh SQLite database is built from.
	assert.Equal(t, expectedAgentColumns, multiLineColumns(t, schemaSQL, agentsCreateSignature),
		"schema.sql agents column set changed")
	// pgschema.sql is what a fresh PostgreSQL database is built from.
	assert.Equal(t, expectedAgentColumns, multiLineColumns(t, pgSchemaSQL, agentsCreateSignature),
		"pgschema.sql agents column set changed")

	// And the two must not drift from each other: same names, same order, so a
	// reader cannot get columns back in a different sequence per backend.
	assert.Equal(t,
		multiLineColumns(t, schemaSQL, agentsCreateSignature),
		multiLineColumns(t, pgSchemaSQL, agentsCreateSignature),
		"the two engines must declare the same columns in the same order")
}

func TestAgentsMigrationStep_MatchesTheBaseSchema(t *testing.T) {
	// The step that creates the table must create the SAME table. This is the
	// text-level half of what TestMigrate_FreshAndUpgraded_SchemaShapesMatch
	// checks against a live database, and it fails with a readable diff.
	assert.Equal(t, expectedAgentColumns,
		oneLineColumns(t, v8Stmts(t, migrations)[0], agentsCreateSignature),
		"the SQLite v8 step's agents DDL drifted from schema.sql")
	assert.Equal(t, expectedAgentColumns,
		oneLineColumns(t, v8Stmts(t, pgMigrations)[0], agentsCreateSignature),
		"the PostgreSQL v8 step's agents DDL drifted from pgschema.sql")
}

// TestAgentsDDL_OnlyHeartbeatIsNullable pins the two shape decisions that carry
// meaning: last_heartbeat is the single nullable column (NULL = "no heartbeat
// has ever arrived", the zero value of Agent.LastHeartbeat), and the identity
// columns are not.
func TestAgentsDDL_OnlyHeartbeatIsNullable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"sqlite", schemaSQL},
		{"postgres", pgSchemaSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := agentsBlock(t, tc.script)
			for _, line := range strings.Split(block, "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "--") {
					continue
				}
				name := strings.Fields(trimmed)[0]
				if name == "last_heartbeat" {
					assert.NotContains(t, trimmed, "NOT NULL",
						"last_heartbeat must stay nullable: NULL is the only honest 'never'")
					continue
				}
				assert.True(t, strings.Contains(trimmed, "NOT NULL") || name == "id",
					"column %q must be NOT NULL (only id is nullable-by-key)", name)
			}
			// An agent record is platform-scoped like locks: no tenant column.
			assert.NotContains(t, block, "tenant_id",
				"agents must not gain tenant_id; see nonTenantOwnedTables")
		})
	}
}

// agentsBlock returns the text between "CREATE TABLE IF NOT EXISTS agents ("
// and the line that closes it.
func agentsBlock(t *testing.T, script string) string {
	t.Helper()
	i := strings.Index(script, agentsCreateSignature)
	require.GreaterOrEqual(t, i, 0, "the script must declare the agents table")
	rest := script[i+len(agentsCreateSignature):]
	j := strings.Index(rest, "\n)")
	require.GreaterOrEqual(t, j, 0, "unterminated agents CREATE TABLE")
	return rest[:j]
}

// v8Stmts returns the version-8 step's statements from the real ladder, failing
// if the step is gone — so deleting it reddens these tests instead of quietly
// removing their coverage. Same device as v7Stmts in
// pgmigrate_clusternodes_test.go, parameterised over the two ladders.
func v8Stmts(t *testing.T, steps []migrationStep) []string {
	t.Helper()
	for _, step := range steps {
		if step.version == 8 {
			require.NotEmpty(t, step.stmts)
			return step.stmts
		}
	}
	t.Fatalf("no version-8 step in the ladder: the agents migration is gone")
	return nil
}

// TestMigrate_V7ToV8_AgentsTable upgrades a database that stopped at version 7
// and proves the v8 step is what gives it the registry.
//
// The v7 state is simulated by dropping the table and pinning the ledger at 7
// (as TestPGMigrate_V1ToV2_CredentialsTags does for PostgreSQL) rather than by
// building a full legacy fixture: the point is that exactly ONE step is pending,
// so the table cannot appear as a side effect of some earlier step.
func TestMigrate_V7ToV8_AgentsTable(t *testing.T) {
	ctx := context.Background()
	fresh := newTestStore(t)
	upgraded := newTestStore(t)

	_, err := upgraded.DB().ExecContext(ctx, `DROP TABLE IF EXISTS agents`)
	require.NoError(t, err)
	require.False(t, tableExists(t, upgraded, "agents"), "fixture must start without the table")

	_, err = upgraded.DB().ExecContext(ctx, `DELETE FROM schema_version`)
	require.NoError(t, err)
	_, err = upgraded.DB().ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (7)`)
	require.NoError(t, err)
	v, err := appliedSchemaVersion(ctx, upgraded.DB())
	require.NoError(t, err)
	require.Equal(t, 7, v, "the fixture is a v7 database")
	require.Less(t, v, currentSchemaVersion, "v8 must be a forward step")

	require.NoError(t, Migrate(ctx, upgraded.DB()))

	v, err = appliedSchemaVersion(ctx, upgraded.DB())
	require.NoError(t, err)
	assert.Equal(t, currentSchemaVersion, v, "the ledger must advance to v8")
	assert.True(t, tableExists(t, upgraded, "agents"), "v8 must create the agents table")

	// The upgraded table has the identical shape of a fresh one, column for
	// column, in declaration order.
	assert.Equal(t, agentColumnShape(t, fresh), agentColumnShape(t, upgraded),
		"schema.sql and the v8 step must produce the same agents shape")

	// And it is usable through the contract, not merely present.
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, upgraded.UpsertAgent(ctx, &Agent{
		ID: "upgraded-1", Address: "10.0.0.7:9000",
		Capabilities: []string{"shell", "a,b"}, Status: "idle",
		RegisteredAt: now, MaxConcurrent: 3,
	}))
	got, err := upgraded.GetAgent(ctx, "upgraded-1")
	require.NoError(t, err)
	require.NotNil(t, got, "a v8-upgraded database must serve the registry")
	assert.Equal(t, []string{"shell", "a,b"}, got.Capabilities)

	// Re-running the ladder is a no-op: the step must not replay its CREATE.
	require.NoError(t, Migrate(ctx, upgraded.DB()))
}

// agentColumnShape reads pragma_table_info the way
// TestMigrate_FreshAndUpgraded_SchemaShapesMatch does: name, type and the
// NOT NULL/default pair, in declaration order.
func agentColumnShape(t *testing.T, store *SQLiteStore) []string {
	t.Helper()
	rows, err := store.DB().QueryContext(context.Background(),
		`SELECT name || '|' || type || '|' || "notnull" || '|' || COALESCE(dflt_value, '<none>')
		 FROM pragma_table_info('agents') ORDER BY cid`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, out, "the agents table is missing")
	return out
}

// TestPGMigrate_V8_AgentsDDL runs the real PostgreSQL step on a live server in
// a private schema (the isolation pgmigrate_clusternodes_test.go established:
// the shared database is used concurrently by other packages and runs).
func TestPGMigrate_V8_AgentsDDL(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	db := store.DB()
	ctx := context.Background()

	const schema = "levee_mig_v8"
	useSchema(t, db, schema, func(conn *sql.Conn) {
		for _, stmt := range v8Stmts(t, pgMigrations) {
			execIn(t, conn, stmt)
		}

		var n int
		require.NoError(t, conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.tables
			 WHERE table_schema = $1 AND table_name = 'agents'`, schema).Scan(&n))
		assert.Equal(t, 1, n, "the v8 step must create agents on PostgreSQL")

		// The created shape is the pinned one, in declaration order.
		rows, err := conn.QueryContext(ctx,
			`SELECT column_name FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 'agents' ORDER BY ordinal_position`, schema)
		require.NoError(t, err)
		var cols []string
		for rows.Next() {
			var c string
			require.NoError(t, rows.Scan(&c))
			cols = append(cols, c)
		}
		require.NoError(t, rows.Close())
		assert.Equal(t, expectedAgentColumns, cols,
			"the v8 step's PostgreSQL column set drifted")

		// Idempotent replay: a database that already has the table must not
		// fail on the step, which is exactly the fresh-database path.
		for _, stmt := range v8Stmts(t, pgMigrations) {
			execIn(t, conn, stmt)
		}
	})
}
