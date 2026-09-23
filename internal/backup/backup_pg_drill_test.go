package backup

// PostgreSQL backup/restore drill (D-3 v2 acceptance): a full
// build-schema -> seed -> dump -> restore round trip against a real
// PostgreSQL instance, including the non-empty refusal and the opt-in
// destructive path with WORM re-verification. Runs only when
// LEVEE_PG_TEST_DSN points at a reachable PostgreSQL (the CI integration job
// provides postgres:16-alpine); skipped otherwise, same convention as the
// state/cluster PG suites. Scratch databases are created and dropped by the
// drill, so the shared CI database is never touched.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func pgDrillDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LEVEE_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL backup drill")
	}
	return dsn
}

// drillDatabase creates a scratch database on the drill cluster and returns
// its DSN; the database is dropped (FORCE) on cleanup.
func drillDatabase(t *testing.T, baseDSN, prefix string) string {
	t.Helper()
	cfg, err := pgx.ParseConfig(baseDSN)
	require.NoError(t, err)

	suffix := make([]byte, 4)
	_, err = rand.Read(suffix)
	require.NoError(t, err)
	name := fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(suffix)) // hex-only identifier

	admin, err := sql.Open("pgx", baseDSN)
	require.NoError(t, err)
	// #nosec G201 -- name is prefix + random hex, generated above.
	_, err = admin.Exec(fmt.Sprintf(`CREATE DATABASE %s`, name))
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	t.Cleanup(func() {
		admin, err := sql.Open("pgx", baseDSN)
		if err != nil {
			return
		}
		defer func() { _ = admin.Close() }()
		// #nosec G201 -- name is prefix + random hex, generated above.
		_, _ = admin.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})

	// Build the scratch DSN from parsed fields: ConnConfig.ConnString() can
	// echo the original URL unchanged, which would silently point the
	// "fresh target" back at the source database.
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, name)
	if cfg.TLSConfig == nil {
		dsn += " sslmode=disable"
	}
	return dsn
}

// openScratch opens a pooled handle to a scratch database, closed on cleanup.
func openScratch(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// drillTables are the FK-linked tables the drill seeds and compares.
var drillTables = []string{"runs", "batches", "steps", "trace", "approvals"}

// drillTableChecksums maps each drill table to "<count>/<md5 of ordered row
// texts>", a compact full-content fingerprint that survives a dump/restore
// round trip only when every row and value did.
func drillTableChecksums(t *testing.T, ctx context.Context, db *sql.DB) map[string]string {
	t.Helper()
	out := make(map[string]string, len(drillTables))
	for _, table := range drillTables {
		var count int
		var sum string
		// table is a constant from drillTables; row-to-text cast keeps the
		// fingerprint generic across column sets.
		// #nosec G201 -- constant identifiers only.
		query := fmt.Sprintf(`SELECT COUNT(*), COALESCE(md5(string_agg(rowtext, '|' ORDER BY rowtext)), '')
FROM (SELECT %s::text AS rowtext FROM %s) s`, table, table)
		require.NoError(t, db.QueryRowContext(ctx, query).Scan(&count, &sum))
		out[table] = fmt.Sprintf("%d/%s", count, sum)
	}
	return out
}

// seedDrillData inserts one FK-linked run graph (run -> batch -> step, run ->
// trace x2 with a mini hash chain, run -> approval).
func seedDrillData(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO runs (id, workflow_name, template_name, plan_hash, status, created_at, updated_at, creator)
		 VALUES ('run-1', 'wf', 'tpl', 'hash-1', 'completed', NOW(), NOW(), 'drill')`,
		`INSERT INTO batches (id, run_id, batch_no, status, total_hosts, succeeded) VALUES ('b-1', 'run-1', 1, 'completed', 1, 1)`,
		`INSERT INTO steps (id, run_id, batch_id, host, step_name, action, status) VALUES ('s-1', 'run-1', 'b-1', 'h1', 'step', 'cmd', 'success')`,
		`INSERT INTO trace (id, run_id, event, actor, prev_hash, curr_hash, timestamp) VALUES
			('t-1', 'run-1', 'plan', 'drill', '', 'hash-t1', NOW()),
			('t-2', 'run-1', 'verify', 'drill', 'hash-t1', 'hash-t2', NOW())`,
		`INSERT INTO approvals (id, run_id, level, approver, status) VALUES ('a-1', 'run-1', 'low', 'op', 'approved')`,
	}
	for _, s := range stmts {
		_, err := db.ExecContext(ctx, s)
		require.NoError(t, err)
	}
}

func TestPostgresBackupRestoreDrill(t *testing.T) {
	base := pgDrillDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Source: real schema (via the migration runner) + FK-linked fixture.
	srcDSN := drillDatabase(t, base, "levee_drill_src")
	src := openScratch(t, srcDSN)
	require.NoError(t, state.MigratePostgres(ctx, src))
	seedDrillData(t, ctx, src)
	before := drillTableChecksums(t, ctx, src)

	// Dump (single-connection REPEATABLE READ snapshot + FK-ordered output).
	mgr := NewManagerPostgres(srcDSN)
	backupPath := filepath.Join(t.TempDir(), "drill.sql")
	require.NoError(t, mgr.Backup(ctx, backupPath))

	content, err := os.ReadFile(backupPath)
	require.NoError(t, err)
	dump := string(content)
	assert.Contains(t, dump, "-- tables (FK topological order):")
	assert.Less(t, strings.Index(dump, `INSERT INTO "runs"`),
		strings.Index(dump, `INSERT INTO "trace"`), "parent rows must be dumped before child rows")

	// Main path: restore into a FRESH database (no schema yet — the restore
	// replays migrations first), then compare full table fingerprints.
	dstDSN := drillDatabase(t, base, "levee_drill_dst")
	dstMgr := NewManagerPostgres(dstDSN)
	require.NoError(t, dstMgr.Restore(ctx, backupPath))

	dst := openScratch(t, dstDSN)
	assert.Equal(t, before, drillTableChecksums(t, ctx, dst))

	// A non-empty target is refused by default...
	err = dstMgr.Restore(ctx, backupPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not empty")

	// ...and the opt-in destructive restore succeeds, leaving WORM
	// protection provably live (the DELETE must still be vetoed).
	require.NoError(t, dstMgr.RestorePostgreSQLOpt(ctx, backupPath, RestoreOptions{AllowDestructive: true}))
	assert.Equal(t, before, drillTableChecksums(t, ctx, dst))
	_, err = dst.ExecContext(ctx, `DELETE FROM trace`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WORM violation")
}
