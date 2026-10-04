package state

// pgmigrate_clusternodes_test.go drives the v7 PostgreSQL step against the real
// migration table, in schemas only this test can see.
//
// Why an isolated schema: newPGTestStore deliberately never truncates
// cluster_nodes, because `go test ./internal/state/... ./internal/cluster/...`
// runs both test binaries CONCURRENTLY against one database and the cluster
// package owns that table. Adding a UNIQUE(address) to the shared table to prove
// the migration removes it would break those tests mid-run — and would itself
// fail on the duplicate addresses they leave behind. So each fixture lives in its
// own schema, reached by pinning search_path.
//
// Every helper opens its own connection and releases it before returning. The
// test store's pool is MaxOpenConns=5; holding one connection per helper
// deadlocks the pool (observed: the suite hung for the whole package timeout
// instead of failing), so grab-use-release is a correctness requirement here,
// not style.
//
// The statements come from pgMigrations rather than being retyped: change the
// migration and this test changes target instead of staying green while
// disagreeing with it. That the step also runs inside the real ladder is covered
// by TestPGMigrate_V1ToV2_CredentialsTags, which asserts the ledger reaches
// pgCurrentSchemaVersion after a replay.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dbschema"
)

const (
	// stateFirstClusterNodesDDL is internal/state/pgschema.sql's cluster_nodes as
	// it stood before the DDL was unified: it carried `capabilities` and a
	// table-level UNIQUE (address), which PostgreSQL backs with the index and the
	// constraint both named cluster_nodes_address_key.
	stateFirstClusterNodesDDL = `CREATE TABLE cluster_nodes (
    id              TEXT PRIMARY KEY,
    address         TEXT NOT NULL,
    status          TEXT    NOT NULL,
    role            TEXT    NOT NULL,
    last_heartbeat  TIMESTAMPTZ NOT NULL,
    capabilities    TEXT NOT NULL DEFAULT '{}',
    joined_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (address)
)`

	// clusterFirstClusterNodesDDL is internal/cluster/pg_registry.go's shape from
	// the same period: defaulted role/status/heartbeat, no unique on address, and
	// no capabilities column at all.
	clusterFirstClusterNodesDDL = `CREATE TABLE cluster_nodes (
	id             TEXT PRIMARY KEY,
	address        TEXT NOT NULL,
	role           TEXT NOT NULL DEFAULT 'worker',
	status         TEXT NOT NULL DEFAULT 'active',
	last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	joined_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`
)

// v7Stmts returns the migration step under test from the real table, failing if
// it is missing — so deleting the step reddens this file instead of quietly
// removing its coverage.
func v7Stmts(t *testing.T) []string {
	t.Helper()
	for _, step := range pgMigrations {
		if step.version == 7 {
			require.NotEmpty(t, step.stmts)
			return step.stmts
		}
	}
	t.Fatalf("pgMigrations has no version-7 step; the cluster_nodes reconciliation migration is gone")
	return nil
}

// useSchema runs fn on one pinned connection inside a private schema. The schema
// is created before and dropped after the test; the connection is always
// released before returning.
func useSchema(t *testing.T, db *sql.DB, schema string, fn func(conn *sql.Conn)) {
	t.Helper()
	ctx := context.Background()

	// Drop first: a run killed by the package timeout cannot reach its own
	// Cleanup, and a leaked fixture schema would redden every later run.
	_, err := db.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	require.NoError(t, err, "drop stale fixture schema")
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err, "create fixture schema")
	t.Cleanup(func() {
		//nolint:errcheck // best-effort teardown; the drop-first above self-heals a leak
		db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	conn, err := db.Conn(ctx)
	require.NoError(t, err, "acquire pinned connection")
	defer func() {
		require.NoError(t, conn.Close(), "release pinned connection")
	}()
	_, err = conn.ExecContext(ctx, "SET search_path = "+schema+", public")
	require.NoError(t, err, "pin search_path")

	fn(conn)
}

func execIn(t *testing.T, conn *sql.Conn, stmt string) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(), stmt)
	require.NoError(t, err, "statement failed: %s", stmt)
}

func runV7(t *testing.T, conn *sql.Conn) {
	t.Helper()
	for _, stmt := range v7Stmts(t) {
		_, err := conn.ExecContext(context.Background(), stmt)
		require.NoError(t, err, "migration statement failed: %s", stmt)
	}
}

func hasConstraint(t *testing.T, conn *sql.Conn, schema, name string) bool {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pg_constraint c
		 JOIN pg_namespace n ON n.oid = c.connamespace
		 WHERE n.nspname = $1 AND c.conname = $2`, schema, name).Scan(&n))
	return n > 0
}

func hasColumn(t *testing.T, conn *sql.Conn, schema, table, column string) bool {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`,
		schema, table, column).Scan(&n))
	return n > 0
}

func columnNames(t *testing.T, conn *sql.Conn, schema string) []string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(),
		`SELECT column_name FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 'cluster_nodes'
		 ORDER BY column_name`, schema)
	require.NoError(t, err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, out, "no cluster_nodes columns in schema %s", schema)
	return out
}

// TestPGMigrate_V7_StateFirstLosesAddressUniqueness is the failure mode the
// migration exists for, proven in both directions: the duplicate-address
// re-registration FAILS before the step and SUCCEEDS after it.
func TestPGMigrate_V7_StateFirstLosesAddressUniqueness(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	db := store.DB()
	ctx := context.Background()

	const schema = "levee_mig_v7_state"
	useSchema(t, db, schema, func(conn *sql.Conn) {
		execIn(t, conn, stateFirstClusterNodesDDL)
		require.True(t, hasConstraint(t, conn, schema, "cluster_nodes_address_key"),
			"the fixture is not the state-first shape without the unique constraint")
		require.True(t, hasColumn(t, conn, schema, "cluster_nodes", "capabilities"))

		execIn(t, conn, `INSERT INTO cluster_nodes (id, address, status, role, last_heartbeat)
			VALUES ('node-1', '10.0.0.11:9091', 'active', 'master', NOW())`)

		// The behaviour being repaired: a node re-registers the same listener
		// address under a new id. That is exactly what failover and takeover do.
		_, err := conn.ExecContext(ctx, `INSERT INTO cluster_nodes (id, address, status, role, last_heartbeat)
			VALUES ('node-2', '10.0.0.11:9091', 'active', 'worker', NOW())`)
		require.Error(t, err, "the state-first shape must still reject address reuse, or this test proves nothing")
		assert.Contains(t, err.Error(), "23505", "expected a unique violation, got: %v", err)

		runV7(t, conn)

		assert.False(t, hasConstraint(t, conn, schema, "cluster_nodes_address_key"),
			"v7 must drop the state-only unique constraint")
		assert.True(t, hasColumn(t, conn, schema, "cluster_nodes", "capabilities"),
			"v7 must not disturb the column state-first databases already have")

		_, err = conn.ExecContext(ctx, `INSERT INTO cluster_nodes (id, address, status, role, last_heartbeat)
			VALUES ('node-2', '10.0.0.11:9091', 'active', 'worker', NOW())`)
		require.NoError(t, err, "after v7 the takeover move must be insertable")

		// Rows survive: the migration narrows the shape, it does not rewrite data.
		var remaining int
		require.NoError(t, conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM cluster_nodes WHERE address = '10.0.0.11:9091'`).Scan(&remaining))
		assert.Equal(t, 2, remaining)
	})
}

// TestPGMigrate_V7_ClusterFirstGainsCapabilities covers the other historical
// shape: no capabilities column. Nothing in production reads or writes that
// column today, so this half is shape convergence rather than a live bug —
// asserted here so the two shapes provably converge instead of drifting again.
func TestPGMigrate_V7_ClusterFirstGainsCapabilities(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	db := store.DB()

	const schema = "levee_mig_v7_cluster"
	useSchema(t, db, schema, func(conn *sql.Conn) {
		execIn(t, conn, clusterFirstClusterNodesDDL)
		require.False(t, hasColumn(t, conn, schema, "cluster_nodes", "capabilities"))
		require.False(t, hasConstraint(t, conn, schema, "cluster_nodes_address_key"))

		runV7(t, conn)

		assert.True(t, hasColumn(t, conn, schema, "cluster_nodes", "capabilities"),
			"v7 must add the column cluster-first databases never had")
		assert.False(t, hasConstraint(t, conn, schema, "cluster_nodes_address_key"))
	})
}

// TestPGMigrate_V7_ConvergesBothShapesToTheSingleDefinition is the agreement
// half: after the step, each historical shape exposes exactly the column set of
// dbschema's unified DDL. The reference is the product's own definition, so the
// test follows a future column change instead of freezing today's list.
func TestPGMigrate_V7_ConvergesBothShapesToTheSingleDefinition(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	db := store.DB()

	var unified []string
	useSchema(t, db, "levee_mig_v7_ref", func(conn *sql.Conn) {
		execIn(t, conn, dbschema.ClusterNodesDDL)
		unified = columnNames(t, conn, "levee_mig_v7_ref")
		require.Contains(t, unified, "capabilities", "the reference must be the unified shape")
	})

	cases := []struct {
		schema string
		ddl    string
		// columnsDifferPre records whether this historical shape differs from the
		// unified one in its columns. It must not be true for both by accident:
		// the state-first shape's only divergence was UNIQUE (address), which a
		// column set cannot see, so demanding inequality there would assert
		// something the fixture never claimed.
		columnsDifferPre bool
	}{
		{"levee_mig_v7_conv_state", stateFirstClusterNodesDDL, false},
		{"levee_mig_v7_conv_cluster", clusterFirstClusterNodesDDL, true},
	}
	differing := 0
	for _, tc := range cases {
		useSchema(t, db, tc.schema, func(conn *sql.Conn) {
			execIn(t, conn, tc.ddl)
			before := columnNames(t, conn, tc.schema)
			if tc.columnsDifferPre {
				require.NotEqual(t, unified, before,
					"%s must start out differing from the unified shape, or this case proves nothing", tc.schema)
				differing++
			}
			runV7(t, conn)
			assert.Equal(t, unified, columnNames(t, conn, tc.schema),
				"after v7 %s must equal the single definition", tc.schema)
		})
	}
	assert.Equal(t, 1, differing,
		"exactly one fixture should need the column add (the other differed only by its unique constraint)")
}

// TestPGMigrate_V7_IsIdempotent runs the step twice. The ladder guards replay
// with the ledger, but re-running after a partial failure must not error, and
// both statements are written to be safe to repeat.
func TestPGMigrate_V7_IsIdempotent(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	db := store.DB()

	const schema = "levee_mig_v7_twice"
	useSchema(t, db, schema, func(conn *sql.Conn) {
		execIn(t, conn, stateFirstClusterNodesDDL)
		runV7(t, conn)
		runV7(t, conn)

		assert.False(t, hasConstraint(t, conn, schema, "cluster_nodes_address_key"))
		assert.True(t, hasColumn(t, conn, schema, "cluster_nodes", "capabilities"))
	})
}

// TestPGMigrate_V7_EndsTheLadder pins the bookkeeping: the step table must be
// gapless and its top version must equal pgCurrentSchemaVersion, or a database
// would stop short of this migration forever.
func TestPGMigrate_V7_EndsTheLadder(t *testing.T) {
	require.NotEmpty(t, pgMigrations)
	last := pgMigrations[len(pgMigrations)-1]
	assert.Equal(t, pgCurrentSchemaVersion, last.version,
		"pgCurrentSchemaVersion must be the newest step, otherwise v7 never runs")
	for i, step := range pgMigrations {
		assert.Equal(t, pgBaseSchemaVersion+1+i, step.version, "ladder must be gapless at index %d", i)
	}
}
