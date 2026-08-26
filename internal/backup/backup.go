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
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite" // pure-Go SQLite driver
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
	vacuumStmt := fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(outputPath, "'", "''"))
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
	defer os.Remove(tmpPath) // best-effort cleanup; no-op after rename

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

// pgSource abstracts the metadata/data access the PostgreSQL dump needs.
// pgLiveSource implements it against a real connection; tests substitute a
// fake so the dump orchestration is exercisable without a server.
type pgSource interface {
	listTables(ctx context.Context) ([]string, error)
	listColumns(ctx context.Context, table string) ([]columnInfo, error)
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

// pgLiveSource implements pgSource over a *sql.DB talking to PostgreSQL.
type pgLiveSource struct {
	db *sql.DB
}

func (s *pgLiveSource) listTables(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, listTablesQuery)
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
	rows, err := s.db.QueryContext(ctx, listColumnsQuery, table)
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

func (s *pgLiveSource) forEachRow(ctx context.Context, table string, cols []columnInfo, emit func(literals []string) error) error {
	rows, err := s.db.QueryContext(ctx, buildSelectSQL(table, cols))
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

	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("backup: create output file: %w", err)
	}

	if err := dumpPostgres(ctx, &pgLiveSource{db: db}, f); err != nil {
		_ = f.Close()
		os.Remove(outputPath) // never leave a partial backup behind
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(outputPath)
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
func dumpPostgres(ctx context.Context, src pgSource, w io.Writer) error {
	tables, err := src.listTables(ctx)
	if err != nil {
		return err
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)
	if _, err := fmt.Fprintf(w, "-- LEVEE PostgreSQL backup %s\n", timestamp); err != nil {
		return fmt.Errorf("backup: write dump header: %w", err)
	}
	if _, err := fmt.Fprintf(w, "-- tables: %s\n\n", strings.Join(tables, ", ")); err != nil {
		return fmt.Errorf("backup: write dump header: %w", err)
	}

	for _, table := range tables {
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
		// Truncate-then-insert makes restoring into an existing database
		// idempotent: rows not present in the backup disappear.
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

// RestorePostgreSQL replays the SQL script at backupPath into the PostgreSQL
// database referenced by the manager's DSN. Verification (checksum + parse)
// happens before any connection is opened; the statements execute inside a
// single transaction, so a failure rolls the database back cleanly.
func (m *Manager) RestorePostgreSQL(ctx context.Context, backupPath string) error {
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

	return execRestoreStatements(ctx, db, stmts)
}

// execRestoreStatements executes every statement inside one transaction so a
// failure mid-stream rolls the database back to its pre-restore state.
func execRestoreStatements(ctx context.Context, db *sql.DB, stmts []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backup: begin restore transaction: %w", err)
	}
	for i, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("backup: restore statement %d: %w", i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backup: commit restore transaction: %w", err)
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
