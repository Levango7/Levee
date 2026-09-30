// Package backup implements data backup and restore for LEVEE's persistence
// layer. It protects the audit hash chain and WORM evidence by providing:
//
//   - SQLite backups via VACUUM INTO, which produces a consistent snapshot
//     of the database even while the daemon holds the file open in WAL mode.
//   - PostgreSQL backups via a pure-Go SQL dump (no pg_dump dependency),
//     so backups work on Windows and in air-gapped environments.
//   - Restore paths that verify SHA-256 sidecar checksums and (for SQLite)
//     PRAGMA integrity_check before atomically replacing the target.
//
// Every backup file is accompanied by a "<file>.sha256" sidecar containing
// the hex digest of the backup. Verify re-checks both the sidecar and the
// file contents, which is how tampering with either side is detected.
package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/nexus/levee/internal/state"
)

// Supported backend driver names.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// ChecksumSuffix is appended to a backup file path to name its SHA-256
// sidecar file (e.g. "levee.db" -> "levee.db.sha256").
const ChecksumSuffix = ".sha256"

// sqliteMagic is the 16-byte file magic that prefixes every SQLite database.
const sqliteMagic = "SQLite format 3\x00"

// connectTimeout bounds the initial PostgreSQL ping so a bad DSN fails fast.
const connectTimeout = 10 * time.Second

// pgSchemaLockKey serialises backup/restore against schema migrations: the
// state package holds a session-level advisory lock with this key for the
// whole migration run, so taking the same key here makes dumps and restores
// mutually exclusive with in-flight DDL. Keep in sync with
// pgSchemaDDLAdvisoryLockKey in internal/state/pgstore_support.go (and
// clusterSchemaDDLAdvisoryLockKey in internal/cluster/pg_registry.go).
const pgSchemaLockKey int64 = 770_001

// Manager performs backup and restore operations against one storage
// backend. Construct instances with NewManagerSQLite or NewManagerPostgres.
type Manager struct {
	driver string
	dbPath string // SQLite database file (DriverSQLite only)
	dsn    string // PostgreSQL data source name (DriverPostgres only)
}

// NewManagerSQLite returns a Manager that backs up and restores the SQLite
// database stored at dbPath.
func NewManagerSQLite(dbPath string) *Manager {
	return &Manager{driver: DriverSQLite, dbPath: dbPath}
}

// NewManagerPostgres returns a Manager that backs up and restores the
// PostgreSQL database reachable via dsn.
func NewManagerPostgres(dsn string) *Manager {
	return &Manager{driver: DriverPostgres, dsn: dsn}
}

// Driver returns the backend driver name (DriverSQLite or DriverPostgres).
func (m *Manager) Driver() string { return m.driver }

// Source returns the database file path (SQLite) or DSN (PostgreSQL).
func (m *Manager) Source() string {
	if m.driver == DriverSQLite {
		return m.dbPath
	}
	return m.dsn
}

// Backup creates a backup file at outputPath using the backend this Manager
// was constructed for. The dispatch keeps call sites driver-agnostic.
func (m *Manager) Backup(ctx context.Context, outputPath string) error {
	switch m.driver {
	case DriverSQLite:
		return m.BackupSQLite(ctx, outputPath)
	case DriverPostgres:
		return m.BackupPostgreSQL(ctx, outputPath)
	default:
		return fmt.Errorf("backup: unknown driver %q", m.driver)
	}
}

// Restore replaces the backend contents with the backup stored at
// backupPath. The backup is verified (checksum + integrity) first.
func (m *Manager) Restore(ctx context.Context, backupPath string) error {
	switch m.driver {
	case DriverSQLite:
		return m.RestoreSQLite(ctx, backupPath)
	case DriverPostgres:
		return m.RestorePostgreSQL(ctx, backupPath)
	default:
		return fmt.Errorf("backup: unknown driver %q", m.driver)
	}
}

// =========================================================================
// SQLite backup / restore
// =========================================================================

// validateOutputPath guards outputPath before it is embedded in SQL or used
// on disk. NUL bytes, CR and LF are rejected outright because they enable
// control-character injection; single quotes are legal file-name characters
// and are handled by escaping at the SQL call site instead.
func validateOutputPath(outputPath string) error {
	if outputPath == "" {
		return fmt.Errorf("backup: empty output path")
	}
	if strings.ContainsAny(outputPath, "\x00\n\r") {
		return fmt.Errorf("backup: output path contains invalid characters: %q", outputPath)
	}
	if dir := filepath.Dir(outputPath); dir != "." && dir != "" {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return fmt.Errorf("backup: output directory %q does not exist", dir)
		}
	}
	return nil
}

// BackupSQLite snapshots the SQLite database into outputPath using
// VACUUM INTO, verifies the snapshot with PRAGMA integrity_check and writes
// the "<outputPath>.sha256" checksum sidecar. VACUUM INTO is atomic at the
// SQLite level: it reads a consistent snapshot even under concurrent WAL
// writers, so the daemon may keep running during the backup.
func (m *Manager) BackupSQLite(ctx context.Context, outputPath string) error {
	if m.dbPath == "" {
		return fmt.Errorf("backup: sqlite: empty database path")
	}
	if err := validateOutputPath(outputPath); err != nil {
		return err
	}

	src, err := sql.Open("sqlite", m.dbPath)
	if err != nil {
		return fmt.Errorf("backup: open sqlite source: %w", err)
	}
	defer func() { _ = src.Close() }()

	if err := src.PingContext(ctx); err != nil {
		return fmt.Errorf("backup: ping sqlite source: %w", err)
	}

	// VACUUM INTO takes a filename SQL literal. validateOutputPath rejects
	// control characters; the remaining injection vector is the quote
	// character itself, which SQL escapes by doubling.
	vacuumStmt := fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(outputPath, "'", "''")) // #nosec G201 -- VACUUM INTO accepts no bind parameter; single quotes escaped above
	if _, err := src.ExecContext(ctx, vacuumStmt); err != nil {
		return fmt.Errorf("backup: vacuum into %q: %w", outputPath, err)
	}

	if err := integrityCheckSQLite(ctx, outputPath); err != nil {
		return err
	}
	if err := WriteChecksumFile(outputPath); err != nil {
		return err
	}
	return nil
}

// integrityCheckSQLite opens dbPath read-only-ish (no writes are issued) and
// runs PRAGMA integrity_check, failing unless SQLite reports "ok".
func integrityCheckSQLite(ctx context.Context, dbPath string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("backup: open %q for integrity check: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("backup: integrity check %q: %w", dbPath, err)
	}
	if result != "ok" {
		return fmt.Errorf("backup: integrity check failed for %q: %s", dbPath, result)
	}
	return nil
}

// RestoreSQLite verifies backupPath (checksum + integrity) and then atomically
// replaces the manager's database file with the backup contents. The
// replacement is staged through a temporary file in the target directory so a
// crash mid-copy never leaves a half-written database behind. Stale
// WAL/journal sidecars of the old database are removed before the swap.
func (m *Manager) RestoreSQLite(ctx context.Context, backupPath string) error {
	if m.dbPath == "" {
		return fmt.Errorf("backup: sqlite: empty database path")
	}
	if err := m.Verify(backupPath); err != nil {
		return err
	}

	dir := filepath.Dir(m.dbPath)
	tmp, err := os.CreateTemp(dir, ".levee-restore-*.db")
	if err != nil {
		return fmt.Errorf("backup: create temp restore file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // best-effort cleanup; no-op after rename

	if err := copyFileContents(backupPath, tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("backup: stage restore file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("backup: close staged restore file: %w", err)
	}

	// Re-check the staged copy so disk corruption between copy and swap is
	// caught before the live database is touched.
	if err := integrityCheckSQLite(ctx, tmpPath); err != nil {
		return err
	}

	// A leftover WAL/journal from the previous database instance would be
	// replayed on top of the restored snapshot and silently corrupt it.
	removeSQLiteSidecars(m.dbPath)

	if err := os.Rename(tmpPath, m.dbPath); err != nil {
		return fmt.Errorf("backup: replace database file: %w (is the database still open?)", err)
	}
	return nil
}

// copyFileContents copies src (an open file positionable from the start) into
// dst and syncs the destination.
func copyFileContents(srcPath string, dst *os.File) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return dst.Sync()
}

// removeSQLiteSidecars deletes the WAL/shared-memory/journal companions of a
// SQLite database file. Missing files are not an error.
func removeSQLiteSidecars(dbPath string) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_ = os.Remove(dbPath + suffix)
	}
}

// =========================================================================
// Checksum sidecar handling
// =========================================================================

// FileSHA256 returns the hex-encoded SHA-256 digest of the file at path.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("backup: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("backup: hash %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WriteChecksumFile writes the SHA-256 digest of path into the sidecar file
// path+ChecksumSuffix using the sha256sum-compatible format
// "<hex-digest>  <basename>".
func WriteChecksumFile(path string) error {
	sum, err := FileSHA256(path)
	if err != nil {
		return err
	}
	content := fmt.Sprintf("%s  %s\n", sum, filepath.Base(path))
	if err := os.WriteFile(path+ChecksumSuffix, []byte(content), 0o600); err != nil {
		return fmt.Errorf("backup: write checksum sidecar: %w", err)
	}
	return nil
}

// verifyChecksum compares the digest recorded in the sidecar file with the
// actual digest of path. A missing or empty sidecar is an error: backups
// generated by this package always carry one.
func verifyChecksum(path string) error {
	sidecar := path + ChecksumSuffix
	raw, err := os.ReadFile(sidecar)
	if err != nil {
		return fmt.Errorf("backup: read checksum sidecar: %w", err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return fmt.Errorf("backup: empty checksum sidecar %q", sidecar)
	}
	expected := strings.ToLower(fields[0])
	actual, err := FileSHA256(path)
	if err != nil {
		return err
	}
	if expected != actual {
		return fmt.Errorf("backup: checksum mismatch for %q: recorded %s, actual %s", path, expected, actual)
	}
	return nil
}

// isSQLiteFile reports whether path begins with the SQLite file magic.
// Read errors are treated as "not a SQLite file".
func isSQLiteFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, len(sqliteMagic))
	n, _ := io.ReadFull(f, buf)
	return n == len(buf) && string(buf) == sqliteMagic
}

// =========================================================================
// Verification entry point
// =========================================================================

// Verify validates an existing backup file without touching the live
// database. It always re-checks the SHA-256 sidecar; additionally it runs
// PRAGMA integrity_check for SQLite backups and re-parses the statement
// stream for PostgreSQL backups, so both tampering and truncation surface.
func (m *Manager) Verify(backupPath string) error {
	if backupPath == "" {
		return fmt.Errorf("backup: empty backup path")
	}
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("backup: stat backup file: %w", err)
	}
	if err := verifyChecksum(backupPath); err != nil {
		return err
	}

	switch m.driver {
	case DriverSQLite:
		return integrityCheckSQLite(context.Background(), backupPath)
	case DriverPostgres:
		content, err := os.ReadFile(backupPath)
		if err != nil {
			return fmt.Errorf("backup: read backup file: %w", err)
		}
		if _, err := parseSQLStatements(string(content)); err != nil {
			return fmt.Errorf("backup: parse backup file: %w", err)
		}
		return nil
	default:
		// Unknown driver: fall back to content-based detection so Verify
		// still protects SQLite snapshots.
		if isSQLiteFile(backupPath) {
			return integrityCheckSQLite(context.Background(), backupPath)
		}
		return nil
	}
}

// =========================================================================
// PostgreSQL backup / restore (pure Go, no pg_dump)
// =========================================================================

// columnInfo describes one column as reported by information_schema.
type columnInfo struct {
	Name     string
	DataType string
	NotNull  bool
}

// fkEdge is one foreign-key dependency: Table holds a FOREIGN KEY that
// references References, so References must be loaded before Table when
// replaying into a foreign-key-enforced schema.
type fkEdge struct {
	Table      string
	References string
}

// pgSource abstracts the metadata/data access the PostgreSQL dump needs.
// pgLiveSource implements it against a real connection; tests substitute a
// fake so the dump orchestration is exercisable without a server.
type pgSource interface {
	listTables(ctx context.Context) ([]string, error)
	listColumns(ctx context.Context, table string) ([]columnInfo, error)
	// listForeignKeys returns the FK dependencies between public-schema user
	// tables; the dump orders tables topologically by these edges.
	listForeignKeys(ctx context.Context) ([]fkEdge, error)
	// forEachRow streams every row of table, invoking emit with the
	// pre-rendered SQL literals (one per column, in column order).
	forEachRow(ctx context.Context, table string, cols []columnInfo, emit func(literals []string) error) error
}

// listTablesQuery enumerates user tables. table_schema='public' already
// excludes pg_catalog, and the NOT LIKE guard additionally filters any
// pg_-prefixed strays per the backup contract.
const listTablesQuery = `SELECT table_name
FROM information_schema.tables
WHERE table_schema = 'public'
  AND table_type = 'BASE TABLE'
  AND table_name NOT LIKE 'pg\_%'
ORDER BY table_name`

// listColumnsQuery lists one table's columns in their physical order.
const listColumnsQuery = `SELECT column_name, data_type, is_nullable
FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = $1
ORDER BY ordinal_position`

// listForeignKeysQuery enumerates FOREIGN KEY dependencies between
// public-schema user tables. A multi-column constraint joins to duplicate
// (table, referenced) pairs; the live source deduplicates them.
const listForeignKeysQuery = `SELECT kcu.table_name, ccu.table_name
FROM information_schema.table_constraints tc
JOIN information_schema.key_column_usage kcu
  ON tc.constraint_schema = kcu.constraint_schema
 AND tc.constraint_name = kcu.constraint_name
JOIN information_schema.constraint_column_usage ccu
  ON tc.constraint_schema = ccu.constraint_schema
 AND tc.constraint_name = ccu.constraint_name
WHERE tc.constraint_type = 'FOREIGN KEY'
  AND tc.table_schema = 'public'
  AND ccu.table_schema = 'public'`

// pgQuerier is the subset of *sql.DB / *sql.Tx the dump source needs, so the
// dump can run either on a plain pool handle or inside a consistent snapshot
// transaction.
type pgQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// pgLiveSource implements pgSource over a live PostgreSQL querier.
type pgLiveSource struct {
	q pgQuerier
}

func (s *pgLiveSource) listTables(ctx context.Context) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, listTablesQuery)
	if err != nil {
		return nil, fmt.Errorf("backup: enumerate tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("backup: scan table name: %w", err)
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func (s *pgLiveSource) listColumns(ctx context.Context, table string) ([]columnInfo, error) {
	rows, err := s.q.QueryContext(ctx, listColumnsQuery, table)
	if err != nil {
		return nil, fmt.Errorf("backup: enumerate columns of %q: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var cols []columnInfo
	for rows.Next() {
		var c columnInfo
		var nullable string
		if err := rows.Scan(&c.Name, &c.DataType, &nullable); err != nil {
			return nil, fmt.Errorf("backup: scan column of %q: %w", table, err)
		}
		c.NotNull = strings.EqualFold(nullable, "NO")
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// listForeignKeys implements pgSource; duplicate edges produced by
// multi-column constraints are collapsed.
func (s *pgLiveSource) listForeignKeys(ctx context.Context) ([]fkEdge, error) {
	rows, err := s.q.QueryContext(ctx, listForeignKeysQuery)
	if err != nil {
		return nil, fmt.Errorf("backup: enumerate foreign keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := make(map[fkEdge]bool)
	var edges []fkEdge
	for rows.Next() {
		var e fkEdge
		if err := rows.Scan(&e.Table, &e.References); err != nil {
			return nil, fmt.Errorf("backup: scan foreign key: %w", err)
		}
		if !seen[e] {
			seen[e] = true
			edges = append(edges, e)
		}
	}
	return edges, rows.Err()
}

func (s *pgLiveSource) forEachRow(ctx context.Context, table string, cols []columnInfo, emit func(literals []string) error) error {
	rows, err := s.q.QueryContext(ctx, buildSelectSQL(table, cols))
	if err != nil {
		return fmt.Errorf("backup: select from %q: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("backup: scan row of %q: %w", table, err)
		}
		literals := make([]string, len(values))
		for i, v := range values {
			literals[i] = renderSQLValue(v)
		}
		if err := emit(literals); err != nil {
			return err
		}
	}
	return rows.Err()
}

// BackupPostgreSQL dumps the PostgreSQL database referenced by the manager's
// DSN into an SQL script at outputPath and writes the checksum sidecar. The
// dump is implemented in pure Go (no pg_dump binary) so it runs on any
// platform. The output is a plain text SQL file; it is therefore not safe to
// store secrets-only tables unencrypted — treat backup files as sensitive.
//
// Consistency: every catalog and row read runs on ONE pinned connection
// inside a REPEATABLE READ READ ONLY transaction, so the whole dump observes
// a single snapshot even while the daemon keeps writing. The schema advisory
// lock (pgSchemaLockKey, shared with state.pgMigrate) additionally excludes
// concurrent migrations, so a dump never straddles a schema change.
func (m *Manager) BackupPostgreSQL(ctx context.Context, outputPath string) error {
	if outputPath == "" {
		return fmt.Errorf("backup: postgres: empty output path")
	}
	if err := validateOutputPath(outputPath); err != nil {
		return err
	}

	db, err := openPostgres(m.dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// Pin one connection for the whole dump: the advisory lock and the
	// snapshot transaction must live on the same session.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("backup: reserve postgres connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, pgSchemaLockKey); err != nil {
		return fmt.Errorf("backup: schema advisory lock: %w", err)
	}
	// Unlock on a cancellation-proof context; even if the unlock cannot run,
	// closing the connection above releases the session lock.
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, pgSchemaLockKey)
	}()

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("backup: begin snapshot transaction: %w", err)
	}

	f, err := os.Create(outputPath)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("backup: create output file: %w", err)
	}

	if err := dumpPostgres(ctx, &pgLiveSource{q: tx}, f); err != nil {
		_ = tx.Rollback()
		_ = f.Close()
		_ = os.Remove(outputPath) // never leave a partial backup behind
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = f.Close()
		_ = os.Remove(outputPath)
		return fmt.Errorf("backup: commit snapshot transaction: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(outputPath)
		return fmt.Errorf("backup: flush output file: %w", err)
	}
	return WriteChecksumFile(outputPath)
}

// openPostgres parses dsn and returns a pinged *sql.DB backed by pgx.
func openPostgres(dsn string) (*sql.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("backup: empty postgres dsn")
	}
	pgCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("backup: parse postgres dsn: %w", err)
	}
	db := stdlib.OpenDB(*pgCfg)

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("backup: ping postgres: %w", err)
	}
	return db, nil
}

// dumpPostgres writes the full dump (header, DDL, DELETE + INSERT statements)
// for every user table into w. It is driver-agnostic thanks to pgSource.
//
// Tables are emitted in FK-topological order (referenced tables first), so a
// manual psql replay into a foreign-key-enforced schema never inserts a child
// row before its parent. The restore path additionally re-orders statements
// against the live target catalog (see orderRestoreStatements), which keeps
// pre-topological dumps restorable.
func dumpPostgres(ctx context.Context, src pgSource, w io.Writer) error {
	tables, err := src.listTables(ctx)
	if err != nil {
		return err
	}
	edges, err := src.listForeignKeys(ctx)
	if err != nil {
		return err
	}
	ordered, err := topoSortTables(tables, edges)
	if err != nil {
		return err
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)
	if _, err := fmt.Fprintf(w, "-- LEVEE PostgreSQL backup %s\n", timestamp); err != nil {
		return fmt.Errorf("backup: write dump header: %w", err)
	}
	if _, err := fmt.Fprintf(w, "-- tables (FK topological order): %s\n\n", strings.Join(ordered, ", ")); err != nil {
		return fmt.Errorf("backup: write dump header: %w", err)
	}

	for _, table := range ordered {
		cols, err := src.listColumns(ctx, table)
		if err != nil {
			return err
		}
		if len(cols) == 0 {
			return fmt.Errorf("backup: table %q has no columns", table)
		}

		if _, err := fmt.Fprintln(w, renderCreateTable(table, cols)); err != nil {
			return fmt.Errorf("backup: write DDL for %q: %w", table, err)
		}
		// Delete-then-insert makes replaying into a database that already
		// holds (part of) the same data idempotent: rows not present in the
		// backup disappear.
		if _, err := fmt.Fprintf(w, "DELETE FROM %s;\n", quoteIdentifier(table)); err != nil {
			return fmt.Errorf("backup: write DELETE for %q: %w", table, err)
		}

		err = src.forEachRow(ctx, table, cols, func(literals []string) error {
			stmt, err := renderInsert(table, cols, literals)
			if err != nil {
				return err
			}
			_, werr := fmt.Fprintln(w, stmt)
			return werr
		})
		if err != nil {
			return fmt.Errorf("backup: dump rows of %q: %w", table, err)
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return fmt.Errorf("backup: write dump: %w", err)
		}
	}
	return nil
}

// topoSortTables orders tables so that every table is preceded by the tables
// its foreign keys reference (parents first). Tables without dependencies
// keep lexicographic order, making the dump deterministic. Edges referencing
// tables outside the dumped set are ignored. A dependency cycle — including
// a self-referencing FK — is an error, not a silently wrong order: restoring
// cyclically constrained data would need deferred constraints, which the
// replay path does not support.
func topoSortTables(tables []string, edges []fkEdge) ([]string, error) {
	inSet := make(map[string]bool, len(tables))
	parents := make(map[string]map[string]bool, len(tables))
	remaining := make(map[string]bool, len(tables))
	for _, t := range tables {
		inSet[t] = true
		parents[t] = map[string]bool{}
		remaining[t] = true
	}
	for _, e := range edges {
		if !inSet[e.Table] || !inSet[e.References] {
			continue
		}
		parents[e.Table][e.References] = true
	}

	// Kahn's algorithm with lexicographic tie-breaking.
	ordered := make([]string, 0, len(tables))
	for len(remaining) > 0 {
		var ready []string
		for t := range remaining {
			blocked := false
			for p := range parents[t] {
				if remaining[p] {
					blocked = true
					break
				}
			}
			if !blocked {
				ready = append(ready, t)
			}
		}
		if len(ready) == 0 {
			cycle := make([]string, 0, len(remaining))
			for t := range remaining {
				cycle = append(cycle, t)
			}
			sort.Strings(cycle)
			return nil, fmt.Errorf("backup: foreign-key dependency cycle among tables: %s", strings.Join(cycle, ", "))
		}
		sort.Strings(ready)
		for _, t := range ready {
			ordered = append(ordered, t)
			delete(remaining, t)
		}
	}
	return ordered, nil
}

// RestoreOptions tunes PostgreSQL restore behaviour. The zero value is the
// safe main path.
type RestoreOptions struct {
	// AllowDestructive permits restoring into a database that already
	// contains data. The replay then runs with user triggers suspended
	// (ALTER TABLE ... DISABLE TRIGGER USER, which includes the WORM
	// append-only guards) and re-enables + re-verifies them before commit.
	// Without this flag a non-empty target is refused outright.
	AllowDestructive bool
}

// RestorePostgreSQL replays the SQL script at backupPath into the PostgreSQL
// database referenced by the manager's DSN — the safe main path: pending
// migrations are applied first (a fresh database is built from pgschema.sql),
// a target that still holds data is refused, and the replay runs in a single
// transaction. Use RestorePostgreSQLOpt with AllowDestructive for disaster
// recovery into a live database.
func (m *Manager) RestorePostgreSQL(ctx context.Context, backupPath string) error {
	return m.RestorePostgreSQLOpt(ctx, backupPath, RestoreOptions{})
}

// RestorePostgreSQLOpt is RestorePostgreSQL with explicit options.
//
// Verification (checksum + parse) happens before any connection is opened.
// The dump statements are then re-grouped per table and re-ordered against
// the LIVE target catalog's foreign-key graph (orderRestoreStatements), so
// dumps written before the dump side learned topological ordering restore
// correctly as well. schema_version rows are never replayed: the schema
// version belongs to the migration runner, and restoring an older row could
// re-run non-idempotent ALTER steps on the next startup.
func (m *Manager) RestorePostgreSQLOpt(ctx context.Context, backupPath string, opts RestoreOptions) error {
	if err := m.Verify(backupPath); err != nil {
		return err
	}
	content, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("backup: read postgres backup: %w", err)
	}
	stmts, err := parseSQLStatements(string(content))
	if err != nil {
		return fmt.Errorf("backup: parse postgres backup: %w", err)
	}
	if len(stmts) == 0 {
		return fmt.Errorf("backup: postgres backup %q contains no statements", backupPath)
	}

	db, err := openPostgres(m.dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// Guarantee the real LEVEE schema before replaying: on a fresh database
	// this replays pgschema.sql + pending migrations; on an existing one it
	// applies pending steps (or is a no-op). The dump's own simplified
	// CREATE TABLE stubs stay a no-op fallback (IF NOT EXISTS) — the INSERTs
	// land in the migration-built schema with its FKs, defaults and WORM
	// triggers.
	if err := state.MigratePostgres(ctx, db); err != nil {
		return fmt.Errorf("backup: ensure postgres schema: %w", err)
	}

	src := &pgLiveSource{q: db}
	liveTables, err := src.listTables(ctx)
	if err != nil {
		return err
	}
	edges, err := src.listForeignKeys(ctx)
	if err != nil {
		return err
	}
	ordered, err := orderRestoreStatements(stmts, liveTables, edges)
	if err != nil {
		return err
	}
	if len(ordered) == 0 {
		return fmt.Errorf("backup: postgres backup %q contains no restorable statements", backupPath)
	}

	nonEmpty, err := pgNonEmptyTables(ctx, db, liveTables)
	if err != nil {
		return err
	}
	if len(nonEmpty) > 0 && !opts.AllowDestructive {
		return fmt.Errorf("backup: restore target is not empty (tables holding data: %s); "+
			"restore into a fresh database or retry with --allow-destructive-restore",
			strings.Join(nonEmpty, ", "))
	}
	if len(nonEmpty) > 0 {
		return execDestructiveRestore(ctx, db, ordered, liveTables)
	}
	return execSafeRestore(ctx, db, ordered)
}

// pgNonEmptyTables returns the subset of tables holding at least one row, in
// input order. schema_version is excluded: a freshly migrated database always
// carries a version row, which must not brand the database as "in use".
func pgNonEmptyTables(ctx context.Context, q pgQuerier, tables []string) ([]string, error) {
	var nonEmpty []string
	for _, t := range tables {
		if t == "schema_version" {
			continue
		}
		var hasRows bool
		// #nosec G201 -- table name comes from the catalog and is quoted.
		query := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s)`, quoteIdentifier(t))
		if err := q.QueryRowContext(ctx, query).Scan(&hasRows); err != nil {
			return nil, fmt.Errorf("backup: check whether %q is empty: %w", t, err)
		}
		if hasRows {
			nonEmpty = append(nonEmpty, t)
		}
	}
	return nonEmpty, nil
}

// classifyDumpStatement identifies the target table of the three statement
// kinds a LEVEE dump contains. Anything else is rejected: silently skipping
// an unrecognised statement could drop data the operator expects back.
func classifyDumpStatement(stmt string) (string, error) {
	for _, prefix := range []string{`INSERT INTO "`, `DELETE FROM "`, `CREATE TABLE IF NOT EXISTS "`} {
		if rest, ok := strings.CutPrefix(stmt, prefix); ok {
			end := strings.Index(rest, `"`)
			if end <= 0 {
				return "", fmt.Errorf("backup: malformed dump statement: %.60q", stmt)
			}
			return rest[:end], nil
		}
	}
	return "", fmt.Errorf("backup: unrecognised statement in postgres dump: %.60q", stmt)
}

// stmtGroup collects one table's dump statements, preserving dump order.
type stmtGroup struct {
	create  string
	del     string
	inserts []string
}

// orderRestoreStatements regroups a parsed dump per table and emits the
// groups in FK-topological order computed from the live target schema, so
// dumps written before the dump side learned topological ordering (v1.13 and
// earlier: plain lexicographic) still replay into a foreign-key-enforced
// schema without violations. Groups for tables the live catalog no longer
// knows (dropped by newer schemas) are appended last in dump order, so their
// data is preserved in the dump's simplified stub table. schema_version
// statements are dropped (see RestorePostgreSQLOpt).
func orderRestoreStatements(stmts []string, liveTables []string, edges []fkEdge) ([]string, error) {
	groups := make(map[string]*stmtGroup)
	var firstSeen []string
	for _, stmt := range stmts {
		table, err := classifyDumpStatement(stmt)
		if err != nil {
			return nil, err
		}
		if table == "schema_version" {
			continue
		}
		g, ok := groups[table]
		if !ok {
			g = &stmtGroup{}
			groups[table] = g
			firstSeen = append(firstSeen, table)
		}
		switch {
		case strings.HasPrefix(stmt, `CREATE TABLE`):
			g.create = stmt
		case strings.HasPrefix(stmt, `DELETE FROM`):
			g.del = stmt
		default:
			g.inserts = append(g.inserts, stmt)
		}
	}

	ordered, err := topoSortTables(liveTables, edges)
	if err != nil {
		return nil, err
	}

	var out []string
	emitted := make(map[string]bool, len(groups))
	emit := func(table string) {
		g := groups[table]
		if g == nil || emitted[table] {
			return
		}
		emitted[table] = true
		if g.create != "" {
			out = append(out, g.create)
		}
		if g.del != "" {
			out = append(out, g.del)
		}
		out = append(out, g.inserts...)
	}
	for _, t := range ordered {
		emit(t)
	}
	for _, t := range firstSeen {
		emit(t) // no-op for tables already emitted via the live catalog
	}
	return out, nil
}

// pgExecutor is satisfied by *sql.DB and *sql.Tx, letting tests drive the
// statement loop against SQLite while production code uses a transaction.
type pgExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// execStatements runs stmts in order, reporting the 1-based position of the
// first failure. Transaction boundaries are the caller's job.
func execStatements(ctx context.Context, ex pgExecutor, stmts []string) error {
	for i, stmt := range stmts {
		if _, err := ex.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("backup: restore statement %d: %w", i+1, err)
		}
	}
	return nil
}

// execSafeRestore replays stmts into an empty, migrated database inside one
// transaction. The advisory xact lock serialises the replay against schema
// migrations holding the same key family (pgSchemaLockKey).
func execSafeRestore(ctx context.Context, db *sql.DB, stmts []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backup: begin restore transaction: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, pgSchemaLockKey); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("backup: restore advisory lock: %w", err)
	}
	if err := execStatements(ctx, tx, stmts); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backup: commit restore transaction: %w", err)
	}
	return nil
}

// execDestructiveRestore replays stmts into a database that already holds
// data (opt-in via RestoreOptions.AllowDestructive). Everything — trigger
// suspension, replay, trigger re-enabling and the WORM re-verification —
// runs in ONE transaction, so any failure rolls the database back to its
// pre-restore state untouched, triggers included.
func execDestructiveRestore(ctx context.Context, db *sql.DB, stmts, tables []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backup: begin restore transaction: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, pgSchemaLockKey); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("backup: restore advisory lock: %w", err)
	}

	// The WORM append-only guards would veto the replay's DELETEs (trace is
	// never deletable, not even via FK CASCADE). DISABLE TRIGGER USER
	// suspends user triggers only — foreign-key enforcement stays active —
	// and needs table ownership rather than superuser.
	for _, t := range tables {
		// #nosec G201 -- table name comes from the catalog and is quoted.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER USER`, quoteIdentifier(t))); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("backup: disable triggers on %q (requires table ownership): %w", t, err)
		}
	}
	if err := execStatements(ctx, tx, stmts); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, t := range tables {
		// #nosec G201 -- table name comes from the catalog and is quoted.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ENABLE TRIGGER USER`, quoteIdentifier(t))); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("backup: re-enable triggers on %q: %w", t, err)
		}
	}
	if slices.Contains(tables, "trace") {
		if err := verifyWormTriggers(ctx, tx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backup: commit restore transaction: %w", err)
	}
	return nil
}

// wormTriggerNames are the append-only guards the destructive restore path
// must provably leave in place before committing.
var wormTriggerNames = []string{"worm_prevent_trace_update", "worm_prevent_trace_delete"}

// verifyWormTriggers fails unless every WORM trigger is present on the
// public.trace table. Called inside the restore transaction, so a failure
// rolls the whole restore back.
func verifyWormTriggers(ctx context.Context, q pgQuerier) error {
	rows, err := q.QueryContext(ctx, `SELECT t.tgname
FROM pg_trigger t
JOIN pg_class c ON c.oid = t.tgrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relname = 'trace' AND NOT t.tgisinternal`)
	if err != nil {
		return fmt.Errorf("backup: list trace triggers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("backup: scan trigger name: %w", err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backup: list trace triggers: %w", err)
	}
	for _, want := range wormTriggerNames {
		if !found[want] {
			return fmt.Errorf("backup: WORM trigger %q missing on trace after restore; refusing to commit", want)
		}
	}
	return nil
}

// =========================================================================
// SQL generation / parsing helpers (pure, independently testable)
// =========================================================================

// escapeSQLLiteral escapes a string for inclusion in a single-quoted SQL
// literal by doubling embedded quotes. With standard_conforming_strings on
// (the PostgreSQL default) no further escaping is required.
func escapeSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// quoteIdentifier double-quotes an SQL identifier, doubling any embedded
// quote character.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// renderSQLValue converts a database/sql scan result into a PostgreSQL SQL
// literal. Byte slices use the bytea hex format; times are rendered with an
// explicit UTC offset so timestamptz columns round-trip exactly.
func renderSQLValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if t {
			return "TRUE"
		}
		return "FALSE"
	case int64:
		return strconv.FormatInt(t, 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'g', -1, 32)
	case []byte:
		return `'\x` + hex.EncodeToString(t) + `'`
	case time.Time:
		return "'" + t.UTC().Format("2006-01-02 15:04:05.999999-07:00") + "'"
	case string:
		return "'" + escapeSQLLiteral(t) + "'"
	default:
		return "'" + escapeSQLLiteral(fmt.Sprintf("%v", t)) + "'"
	}
}

// renderCreateTable builds a simplified CREATE TABLE IF NOT EXISTS statement
// from information_schema column metadata. Primary keys, defaults and foreign
// keys are intentionally omitted: the dump targets data recovery, and the
// LEVEE schema is normally recreated by migrations before replay.
func renderCreateTable(table string, cols []columnInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (\n", quoteIdentifier(table))
	for i, c := range cols {
		line := "  " + quoteIdentifier(c.Name) + " " + c.DataType
		if c.NotNull {
			line += " NOT NULL"
		}
		if i < len(cols)-1 {
			line += ","
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString(");")
	return b.String()
}

// buildSelectSQL builds the column-explicit SELECT used to stream a table.
func buildSelectSQL(table string, cols []columnInfo) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdentifier(c.Name)
	}
	return fmt.Sprintf("SELECT %s FROM %s", strings.Join(names, ", "), quoteIdentifier(table))
}

// renderInsert builds one INSERT statement from pre-rendered literals. The
// literal count must match the column count.
func renderInsert(table string, cols []columnInfo, literals []string) (string, error) {
	if len(literals) != len(cols) {
		return "", fmt.Errorf("backup: row of %q has %d values for %d columns", table, len(literals), len(cols))
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdentifier(c.Name)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s);",
		quoteIdentifier(table), strings.Join(names, ", "), strings.Join(literals, ", ")), nil
}

// parseSQLStatements splits a backup script into individual statements. It is
// a small state machine rather than a naive semicolon split so that
// semicolons inside string literals (common in LEVEE stdout payloads) survive
// the round trip. Line comments (-- ...) outside strings are dropped; an
// unterminated string literal is reported as an error.
func parseSQLStatements(content string) ([]string, error) {
	var stmts []string
	var cur strings.Builder

	inString := false
	inLineComment := false

	flush := func() {
		stmt := strings.TrimSpace(cur.String())
		if stmt != "" {
			stmts = append(stmts, stmt)
		}
		cur.Reset()
	}

	for i := 0; i < len(content); i++ {
		ch := content[i]

		if inLineComment {
			if ch == '\n' {
				inLineComment = false
				cur.WriteByte('\n')
			}
			continue
		}

		if inString {
			cur.WriteByte(ch)
			if ch == '\'' {
				// Two consecutive quotes are an escaped quote, not the end.
				if i+1 < len(content) && content[i+1] == '\'' {
					cur.WriteByte(content[i+1])
					i++
					continue
				}
				inString = false
			}
			continue
		}

		switch {
		case ch == '\'':
			inString = true
			cur.WriteByte(ch)
		case ch == '-' && i+1 < len(content) && content[i+1] == '-':
			inLineComment = true
			i++ // consume the second dash as well
		case ch == ';':
			flush()
		default:
			cur.WriteByte(ch)
		}
	}

	if inString {
		return nil, fmt.Errorf("backup: unterminated string literal in SQL")
	}
	flush()
	return stmts, nil
}
