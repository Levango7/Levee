package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- SQLite test helpers -----------------------------------------------------

// newTestSQLiteDB creates a temporary SQLite database with one table and two
// rows, then closes it and returns its path.
func newTestSQLiteDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "source.db")

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE items (
		id   TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		qty  INTEGER,
		data BLOB
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO items VALUES ('a', 'alpha', 1, x'0102')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO items VALUES ('b', 'it''s beta', 2, NULL)`)
	require.NoError(t, err)
	return dbPath
}

// queryItemName returns the name column for the given id, or "" when the row
// does not exist.
func queryItemName(t *testing.T, dbPath, id string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var name string
	err = db.QueryRow(`SELECT name FROM items WHERE id = ?`, id).Scan(&name)
	if err != nil {
		return ""
	}
	return name
}

// --- Manager constructors / dispatch ------------------------------------------

func TestNewManagerSQLite(t *testing.T) {
	m := NewManagerSQLite("/tmp/levee.db")
	assert.Equal(t, DriverSQLite, m.Driver())
	assert.Equal(t, "/tmp/levee.db", m.Source())
}

func TestNewManagerPostgres(t *testing.T) {
	m := NewManagerPostgres("postgres://u:p@h:5432/db")
	assert.Equal(t, DriverPostgres, m.Driver())
	assert.Equal(t, "postgres://u:p@h:5432/db", m.Source())
}

func TestBackupDispatchUnknownDriver(t *testing.T) {
	m := &Manager{driver: "mysql"}
	err := m.Backup(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown driver")
}

func TestRestoreDispatchUnknownDriver(t *testing.T) {
	m := &Manager{driver: "mysql"}
	err := m.Restore(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown driver")
}

// --- SQLite backup + restore round trip ---------------------------------------

func TestBackupSQLiteRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbPath := newTestSQLiteDB(t)
	m := NewManagerSQLite(dbPath)
	backupPath := filepath.Join(t.TempDir(), "backup.db")

	require.NoError(t, m.BackupSQLite(ctx, backupPath))

	// Backup file and sidecar must exist.
	_, err := os.Stat(backupPath)
	require.NoError(t, err)
	_, err = os.Stat(backupPath + ChecksumSuffix)
	require.NoError(t, err)

	// Verify passes on the fresh backup.
	require.NoError(t, m.Verify(backupPath))

	// Mutate the live database after the backup was taken.
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE items SET name = 'CHANGED' WHERE id = 'a'`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	assert.Equal(t, "CHANGED", queryItemName(t, dbPath, "a"))

	// Restore and check the original contents are back.
	require.NoError(t, m.RestoreSQLite(ctx, backupPath))
	assert.Equal(t, "alpha", queryItemName(t, dbPath, "a"))
	assert.Equal(t, "it's beta", queryItemName(t, dbPath, "b"))
}

func TestBackupSQLiteDispatchViaBackup(t *testing.T) {
	ctx := context.Background()
	dbPath := newTestSQLiteDB(t)
	m := NewManagerSQLite(dbPath)
	backupPath := filepath.Join(t.TempDir(), "via-dispatch.db")

	require.NoError(t, m.Backup(ctx, backupPath))
	_, err := os.Stat(backupPath)
	assert.NoError(t, err)

	// Restore through the generic dispatch as well.
	require.NoError(t, m.Restore(ctx, backupPath))
	assert.Equal(t, "alpha", queryItemName(t, dbPath, "a"))
}

func TestBackupSQLiteRestoreIntoFreshTarget(t *testing.T) {
	ctx := context.Background()
	dbPath := newTestSQLiteDB(t)
	src := NewManagerSQLite(dbPath)
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	require.NoError(t, src.BackupSQLite(ctx, backupPath))

	// A manager pointing at a not-yet-existing database file.
	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	dst := NewManagerSQLite(freshPath)
	require.NoError(t, dst.RestoreSQLite(ctx, backupPath))
	assert.Equal(t, "alpha", queryItemName(t, freshPath, "a"))
}

func TestBackupSQLiteQuoteInPath(t *testing.T) {
	// A single quote in the output path exercises the SQL escaping path of
	// VACUUM INTO without breaking the file name.
	ctx := context.Background()
	dbPath := newTestSQLiteDB(t)
	m := NewManagerSQLite(dbPath)
	backupPath := filepath.Join(t.TempDir(), "back'up.db")

	require.NoError(t, m.BackupSQLite(ctx, backupPath))
	_, err := os.Stat(backupPath)
	assert.NoError(t, err)
	require.NoError(t, m.Verify(backupPath))
}

// --- SQLite verification / tamper detection ------------------------------------

func TestRestoreSQLiteChecksumSidecarTampered(t *testing.T) {
	ctx := context.Background()
	dbPath := newTestSQLiteDB(t)
	m := NewManagerSQLite(dbPath)
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	require.NoError(t, m.BackupSQLite(ctx, backupPath))

	// Corrupt the sidecar digest.
	require.NoError(t, os.WriteFile(backupPath+ChecksumSuffix,
		[]byte("0000000000000000000000000000000000000000000000000000000000000000  backup.db\n"), 0o600))

	err := m.RestoreSQLite(ctx, backupPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")

	// The live database must be untouched.
	assert.Equal(t, "alpha", queryItemName(t, dbPath, "a"))
}

func TestRestoreSQLiteMissingSidecar(t *testing.T) {
	ctx := context.Background()
	dbPath := newTestSQLiteDB(t)
	m := NewManagerSQLite(dbPath)
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	require.NoError(t, m.BackupSQLite(ctx, backupPath))
	require.NoError(t, os.Remove(backupPath+ChecksumSuffix))

	err := m.RestoreSQLite(ctx, backupPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum sidecar")
}

func TestBackupSQLiteFileTamperDetectedByVerify(t *testing.T) {
	ctx := context.Background()
	dbPath := newTestSQLiteDB(t)
	m := NewManagerSQLite(dbPath)
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	require.NoError(t, m.BackupSQLite(ctx, backupPath))

	// Append garbage to the backup: checksum must now mismatch.
	f, err := os.OpenFile(backupPath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.Write([]byte("tampered"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	err = m.Verify(backupPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")
}

func TestIntegrityCheckSQLiteCorruptFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "big.db")

	// Build a multi-page database so page 2 exists.
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE big (id TEXT PRIMARY KEY, payload BLOB)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO big VALUES ('x', zeroblob(100000))`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Corrupt the page-type byte of page 2 (default page size is 4096);
	// the header page stays parseable, so integrity_check must detect the
	// invalid page type instead of the open failing outright.
	raw, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	require.Greater(t, len(raw), 4100)
	raw[4096] ^= 0xFF
	corruptPath := filepath.Join(dir, "corrupt.db")
	require.NoError(t, os.WriteFile(corruptPath, raw, 0o600))

	err = integrityCheckSQLite(ctx, corruptPath)
	assert.Error(t, err, "corrupted database must fail the integrity check")
}

func TestIntegrityCheckSQLiteNotADatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "garbage.db")
	require.NoError(t, os.WriteFile(path, []byte("this is not a sqlite file at all"), 0o600))

	err := integrityCheckSQLite(ctx, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "integrity check")
}

func TestVerifyMissingBackupFile(t *testing.T) {
	m := NewManagerSQLite("/whatever/source.db")
	err := m.Verify(filepath.Join(t.TempDir(), "nope.db"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat backup file")
}

func TestVerifyEmptyPath(t *testing.T) {
	m := NewManagerSQLite("/whatever/source.db")
	err := m.Verify("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty backup path")
}

func TestVerifyUnknownDriverFallsBackToMagic(t *testing.T) {
	// SQLite snapshot verified through a manager with an unknown driver:
	// magic-byte detection still runs the integrity check.
	ctx := context.Background()
	dbPath := newTestSQLiteDB(t)
	src := NewManagerSQLite(dbPath)
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	require.NoError(t, src.BackupSQLite(ctx, backupPath))

	unknown := &Manager{driver: "oracle"}
	assert.NoError(t, unknown.Verify(backupPath))

	// A non-SQLite file passes checksum but skips integrity.
	textPath := filepath.Join(t.TempDir(), "notes.sql")
	require.NoError(t, os.WriteFile(textPath, []byte("SELECT 1;\n"), 0o600))
	require.NoError(t, WriteChecksumFile(textPath))
	assert.NoError(t, unknown.Verify(textPath))
}

func TestVerifyPostgresParseFailure(t *testing.T) {
	dir := t.TempDir()
	sqlPath := filepath.Join(dir, "backup.sql")
	require.NoError(t, os.WriteFile(sqlPath, []byte("INSERT INTO t VALUES ('unterminated;\n"), 0o600))
	require.NoError(t, WriteChecksumFile(sqlPath))

	m := NewManagerPostgres("postgres://u:p@h/db")
	err := m.Verify(sqlPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse backup file")
}

func TestBackupSQLiteSourceNotADatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "garbage.db")
	require.NoError(t, os.WriteFile(srcPath, []byte("not a database"), 0o600))

	m := NewManagerSQLite(srcPath)
	err := m.BackupSQLite(ctx, filepath.Join(dir, "out.db"))
	require.Error(t, err)
}

func TestBackupSQLiteOutputAlreadyExists(t *testing.T) {
	ctx := context.Background()
	dbPath := newTestSQLiteDB(t)
	m := NewManagerSQLite(dbPath)

	out := filepath.Join(t.TempDir(), "existing.db")
	require.NoError(t, os.WriteFile(out, []byte("occupied"), 0o600))

	// VACUUM INTO refuses to overwrite an existing file.
	err := m.BackupSQLite(ctx, out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vacuum into")
}

func TestRestoreSQLiteTargetIsDirectory(t *testing.T) {
	ctx := context.Background()
	dbPath := newTestSQLiteDB(t)
	src := NewManagerSQLite(dbPath)
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	require.NoError(t, src.BackupSQLite(ctx, backupPath))

	// Pointing the manager at a directory makes the final rename fail on
	// every platform (rename cannot replace a directory with a file).
	dirTarget := filepath.Join(t.TempDir(), "target.db")
	require.NoError(t, os.Mkdir(dirTarget, 0o755))
	m := NewManagerSQLite(dirTarget)

	err := m.RestoreSQLite(ctx, backupPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "replace database file")
}

func TestWriteChecksumFileSidecarPathOccupied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	require.NoError(t, os.WriteFile(path, []byte("z"), 0o600))
	// Occupy the sidecar path with a directory so the write fails.
	require.NoError(t, os.Mkdir(path+ChecksumSuffix, 0o755))

	err := WriteChecksumFile(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write checksum sidecar")
}

func TestDispatchPostgresBackup(t *testing.T) {
	m := NewManagerPostgres(unreachableDSN)
	err := m.Backup(context.Background(), filepath.Join(t.TempDir(), "x.sql"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ping postgres")
}

func TestDispatchPostgresRestore(t *testing.T) {
	path := writeValidSQLBackup(t, "DELETE FROM runs;\n")
	m := NewManagerPostgres(unreachableDSN)
	err := m.Restore(context.Background(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ping postgres")
}

// --- SQLite backup error paths --------------------------------------------------

func TestBackupSQLiteEmptyManagerPath(t *testing.T) {
	m := NewManagerSQLite("")
	err := m.BackupSQLite(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty database path")
}

func TestRestoreSQLiteEmptyManagerPath(t *testing.T) {
	m := NewManagerSQLite("")
	err := m.RestoreSQLite(context.Background(), "whatever.db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty database path")
}

func TestValidateOutputPath(t *testing.T) {
	assert.Error(t, validateOutputPath(""))
	assert.Error(t, validateOutputPath("bad\x00name"))
	assert.Error(t, validateOutputPath("bad\nname"))
	assert.Error(t, validateOutputPath("bad\rname"))
	assert.Error(t, validateOutputPath(filepath.Join(t.TempDir(), "missing-dir", "x.db")))
	assert.NoError(t, validateOutputPath(filepath.Join(t.TempDir(), "ok.db")))
	assert.NoError(t, validateOutputPath("relative.db"))
}

// --- Checksum helpers ------------------------------------------------------------

func TestWriteAndVerifyChecksumRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	require.NoError(t, os.WriteFile(path, []byte("hello levee"), 0o600))

	require.NoError(t, WriteChecksumFile(path))

	raw, err := os.ReadFile(path + ChecksumSuffix)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "  data.bin\n")
	assert.True(t, strings.HasPrefix(string(raw), sha256Of("hello levee")))

	assert.NoError(t, verifyChecksum(path))
}

func TestVerifyChecksumEmptySidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(path+ChecksumSuffix, []byte("   \n"), 0o600))

	err := verifyChecksum(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty checksum sidecar")
}

func TestFileSHA256MissingFile(t *testing.T) {
	_, err := FileSHA256(filepath.Join(t.TempDir(), "nope"))
	require.Error(t, err)
}

func TestIsSQLiteFile(t *testing.T) {
	dir := t.TempDir()

	dbPath := newTestSQLiteDB(t)
	assert.True(t, isSQLiteFile(dbPath))

	textPath := filepath.Join(dir, "plain.txt")
	require.NoError(t, os.WriteFile(textPath, []byte("nope"), 0o600))
	assert.False(t, isSQLiteFile(textPath))

	assert.False(t, isSQLiteFile(filepath.Join(dir, "missing")))

	// Short file: fewer bytes than the magic.
	shortPath := filepath.Join(dir, "short.bin")
	require.NoError(t, os.WriteFile(shortPath, []byte("SQLite"), 0o600))
	assert.False(t, isSQLiteFile(shortPath))
}

// sha256Of hashes a literal string; tiny helper to keep assertions readable.
func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// --- SQL generation helpers ------------------------------------------------------

func TestEscapeSQLLiteral(t *testing.T) {
	assert.Equal(t, "plain", escapeSQLLiteral("plain"))
	assert.Equal(t, "it''s", escapeSQLLiteral("it's"))
	assert.Equal(t, "''''", escapeSQLLiteral("''"))
}

func TestQuoteIdentifier(t *testing.T) {
	assert.Equal(t, `"runs"`, quoteIdentifier("runs"))
	assert.Equal(t, `"we""ird"`, quoteIdentifier(`we"ird`))
}

func TestRenderSQLValue(t *testing.T) {
	assert.Equal(t, "NULL", renderSQLValue(nil))
	assert.Equal(t, "TRUE", renderSQLValue(true))
	assert.Equal(t, "FALSE", renderSQLValue(false))
	assert.Equal(t, "42", renderSQLValue(int64(42)))
	assert.Equal(t, "-7", renderSQLValue(int32(-7)))
	assert.Equal(t, "3.5", renderSQLValue(float64(3.5)))
	assert.Equal(t, "1.25", renderSQLValue(float32(1.25)))
	assert.Equal(t, `'it''s'`, renderSQLValue("it's"))
	assert.Equal(t, `'\x0102'`, renderSQLValue([]byte{0x01, 0x02}))

	ts := time.Date(2026, 8, 27, 10, 30, 0, 0, time.UTC)
	assert.Equal(t, "'2026-08-27 10:30:00+00:00'", renderSQLValue(ts))

	// Unknown types fall back to a quoted string form.
	assert.Equal(t, "'13'", renderSQLValue(uint(13)))
}

func TestRenderCreateTable(t *testing.T) {
	ddl := renderCreateTable("runs", []columnInfo{
		{Name: "id", DataType: "TEXT", NotNull: true},
		{Name: "status", DataType: "character varying", NotNull: false},
	})
	assert.Contains(t, ddl, `CREATE TABLE IF NOT EXISTS "runs"`)
	assert.Contains(t, ddl, `"id" TEXT NOT NULL,`)
	assert.Contains(t, ddl, `"status" character varying`)
	assert.True(t, strings.HasSuffix(ddl, ");"))
	// Last column must not carry a trailing comma.
	assert.NotContains(t, ddl, `character varying,`)
}

func TestBuildSelectSQL(t *testing.T) {
	s := buildSelectSQL("trace", []columnInfo{{Name: "id"}, {Name: "curr_hash"}})
	assert.Equal(t, `SELECT "id", "curr_hash" FROM "trace"`, s)
}

func TestRenderInsert(t *testing.T) {
	cols := []columnInfo{{Name: "id"}, {Name: "name"}}
	stmt, err := renderInsert("items", cols, []string{"'a'", "'alpha'"})
	require.NoError(t, err)
	assert.Equal(t, `INSERT INTO "items" ("id", "name") VALUES ('a', 'alpha');`, stmt)

	_, err = renderInsert("items", cols, []string{"'only-one'"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "values for")
}

// --- SQL statement parser ----------------------------------------------------------

func TestParseSQLStatementsBasic(t *testing.T) {
	content := `-- header comment
CREATE TABLE IF NOT EXISTS "t" (
  "id" TEXT
);
DELETE FROM "t";
INSERT INTO "t" ("id") VALUES ('x');
`
	stmts, err := parseSQLStatements(content)
	require.NoError(t, err)
	require.Len(t, stmts, 3)
	assert.True(t, strings.HasPrefix(stmts[0], "CREATE TABLE"))
	assert.Equal(t, `DELETE FROM "t"`, stmts[1])
	assert.Equal(t, `INSERT INTO "t" ("id") VALUES ('x')`, stmts[2])
}

func TestParseSQLStatementsSemicolonInsideString(t *testing.T) {
	content := `INSERT INTO steps (stdout) VALUES ('line1; still same statement;');
INSERT INTO steps (stdout) VALUES ('second');`
	stmts, err := parseSQLStatements(content)
	require.NoError(t, err)
	require.Len(t, stmts, 2)
	assert.Contains(t, stmts[0], "still same statement")
}

func TestParseSQLStatementsEscapedQuotes(t *testing.T) {
	content := `INSERT INTO items (name) VALUES ('it''s; fine');`
	stmts, err := parseSQLStatements(content)
	require.NoError(t, err)
	require.Len(t, stmts, 1)
	assert.Contains(t, stmts[0], "it''s; fine")
}

func TestParseSQLStatementsUnterminatedString(t *testing.T) {
	_, err := parseSQLStatements("INSERT INTO t VALUES ('never closed;")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unterminated string")
}

func TestParseSQLStatementsEmptyAndTrailing(t *testing.T) {
	stmts, err := parseSQLStatements("")
	require.NoError(t, err)
	assert.Empty(t, stmts)

	stmts, err = parseSQLStatements("-- only a comment\n")
	require.NoError(t, err)
	assert.Empty(t, stmts)

	// Trailing statement without a final semicolon is still flushed.
	stmts, err = parseSQLStatements("SELECT 1")
	require.NoError(t, err)
	require.Len(t, stmts, 1)
}

// --- dumpPostgres orchestration (fake pgSource) -------------------------------------

// fakePGSource is a scriptable stand-in for pgLiveSource.
type fakePGSource struct {
	tables      []string
	tablesErr   error
	cols        map[string][]columnInfo
	colsErr     error
	rows        map[string][][]string
	rowsErr     error
	emittedRows int
}

func (f *fakePGSource) listTables(ctx context.Context) ([]string, error) {
	return f.tables, f.tablesErr
}

func (f *fakePGSource) listColumns(ctx context.Context, table string) ([]columnInfo, error) {
	if f.colsErr != nil {
		return nil, f.colsErr
	}
	return f.cols[table], nil
}

func (f *fakePGSource) forEachRow(ctx context.Context, table string, cols []columnInfo, emit func([]string) error) error {
	if f.rowsErr != nil {
		return f.rowsErr
	}
	for _, row := range f.rows[table] {
		f.emittedRows++
		if err := emit(row); err != nil {
			return err
		}
	}
	return nil
}

func newFakePGSource() *fakePGSource {
	return &fakePGSource{
		tables: []string{"runs", "trace"},
		cols: map[string][]columnInfo{
			"runs":  {{Name: "id", DataType: "text", NotNull: true}, {Name: "status", DataType: "text"}},
			"trace": {{Name: "id", DataType: "text", NotNull: true}},
		},
		rows: map[string][][]string{
			"runs":  {{"'run-1'", "'completed'"}, {"'run-2'", "'failed'"}},
			"trace": {},
		},
	}
}

func TestDumpPostgresHappyPath(t *testing.T) {
	var buf strings.Builder
	require.NoError(t, dumpPostgres(context.Background(), newFakePGSource(), &buf))

	out := buf.String()
	assert.Contains(t, out, "-- LEVEE PostgreSQL backup ")
	assert.Contains(t, out, "-- tables: runs, trace")
	assert.Contains(t, out, `CREATE TABLE IF NOT EXISTS "runs"`)
	assert.Contains(t, out, `DELETE FROM "runs";`)
	assert.Contains(t, out, `INSERT INTO "runs" ("id", "status") VALUES ('run-1', 'completed');`)
	// The empty table still gets DDL + DELETE but no INSERT.
	assert.Contains(t, out, `CREATE TABLE IF NOT EXISTS "trace"`)
	assert.NotContains(t, out, `INSERT INTO "trace"`)
}

func TestDumpPostgresListTablesError(t *testing.T) {
	f := newFakePGSource()
	f.tablesErr = fmt.Errorf("boom")
	err := dumpPostgres(context.Background(), f, &strings.Builder{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestDumpPostgresListColumnsError(t *testing.T) {
	f := newFakePGSource()
	f.colsErr = fmt.Errorf("no columns today")
	err := dumpPostgres(context.Background(), f, &strings.Builder{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no columns today")
}

func TestDumpPostgresTableWithoutColumns(t *testing.T) {
	f := newFakePGSource()
	f.cols["runs"] = nil
	err := dumpPostgres(context.Background(), f, &strings.Builder{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no columns")
}

func TestDumpPostgresRowError(t *testing.T) {
	f := newFakePGSource()
	f.rowsErr = fmt.Errorf("row stream broke")
	err := dumpPostgres(context.Background(), f, &strings.Builder{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row stream broke")
}

func TestDumpPostgresWriteError(t *testing.T) {
	err := dumpPostgres(context.Background(), newFakePGSource(), errWriter{})
	require.Error(t, err)
}

// errWriter always fails; used to exercise the dump's write-error branches.
type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, fmt.Errorf("disk full") }

// limitedWriter accepts the first ok writes and fails afterwards, letting
// tests hit the later write-error branches of dumpPostgres.
type limitedWriter struct {
	ok     int
	writes int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.ok {
		return 0, fmt.Errorf("late disk full")
	}
	return len(p), nil
}

func TestDumpPostgresLateWriteErrors(t *testing.T) {
	// Header (2 writes) then DDL fails.
	err := dumpPostgres(context.Background(), newFakePGSource(), &limitedWriter{ok: 2})
	require.Error(t, err)

	// Header + DDL ok, DELETE fails.
	err = dumpPostgres(context.Background(), newFakePGSource(), &limitedWriter{ok: 3})
	require.Error(t, err)

	// Header + DDL + DELETE ok, first INSERT fails.
	err = dumpPostgres(context.Background(), newFakePGSource(), &limitedWriter{ok: 4})
	require.Error(t, err)
}

func TestCopyFileContentsMissingSource(t *testing.T) {
	dst, err := os.Create(filepath.Join(t.TempDir(), "dst"))
	require.NoError(t, err)
	defer func() { _ = dst.Close() }()

	err = copyFileContents(filepath.Join(t.TempDir(), "missing-src"), dst)
	require.Error(t, err)
}

func TestRestoreSQLiteTargetDirMissing(t *testing.T) {
	ctx := context.Background()
	dbPath := newTestSQLiteDB(t)
	m := NewManagerSQLite(filepath.Join(t.TempDir(), "no-such-dir", "target.db"))

	src := NewManagerSQLite(dbPath)
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	require.NoError(t, src.BackupSQLite(ctx, backupPath))

	err := m.RestoreSQLite(ctx, backupPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "temp restore file")
}

// --- PostgreSQL live code against a SQLite-backed information_schema -------------
//
// SQLite cannot host a real PostgreSQL, but the information_schema catalog
// can be emulated: a second database file holding plain tables named
// "tables" and "columns" is ATTACHed under the alias "information_schema",
// so queries against information_schema.tables / information_schema.columns
// hit those tables. Combined with a real "runs" table in the main schema,
// this exercises pgLiveSource end to end without external services.

func newInfoSchemaSQLite(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()

	db, err := sql.Open("sqlite", filepath.Join(dir, "pg-emulation.db"))
	require.NoError(t, err)
	// ATTACH is per-connection; pin the pool to a single connection. Close
	// must run even on early failures so TempDir cleanup can remove the
	// locked files.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`CREATE TABLE runs (
		id     TEXT NOT NULL,
		status TEXT,
		n      INTEGER
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO runs VALUES ('r1', 'completed', 3)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO runs VALUES ('r2', 'it''s done', NULL)`)
	require.NoError(t, err)

	// Build the catalog file with plain tables (views cannot cross schemas
	// in SQLite, real tables in an ATTACHed database can be queried freely).
	infoDB, err := sql.Open("sqlite", filepath.Join(dir, "info.db"))
	require.NoError(t, err)
	_, err = infoDB.Exec(`CREATE TABLE tables (
		table_schema TEXT, table_type TEXT, table_name TEXT
	)`)
	require.NoError(t, err)
	_, err = infoDB.Exec(`INSERT INTO tables VALUES ('public', 'BASE TABLE', 'runs')`)
	require.NoError(t, err)
	_, err = infoDB.Exec(`CREATE TABLE columns (
		table_schema TEXT, table_name TEXT, column_name TEXT,
		data_type TEXT, is_nullable TEXT, ordinal_position INTEGER
	)`)
	require.NoError(t, err)
	_, err = infoDB.Exec(`INSERT INTO columns VALUES
		('public', 'runs', 'id',     'TEXT',    'NO',  1),
		('public', 'runs', 'status', 'TEXT',    'YES', 2),
		('public', 'runs', 'n',      'INTEGER', 'YES', 3)`)
	require.NoError(t, err)
	require.NoError(t, infoDB.Close())

	escaped := strings.ReplaceAll(filepath.Join(dir, "info.db"), "'", "''")
	_, err = db.Exec(fmt.Sprintf(`ATTACH '%s' AS information_schema`, escaped))
	require.NoError(t, err)

	return db
}

func TestPGLiveSourceAgainstEmulatedInfoSchema(t *testing.T) {
	ctx := context.Background()
	src := &pgLiveSource{db: newInfoSchemaSQLite(t)}

	tables, err := src.listTables(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"runs"}, tables)

	cols, err := src.listColumns(ctx, "runs")
	require.NoError(t, err)
	require.Len(t, cols, 3)
	assert.Equal(t, columnInfo{Name: "id", DataType: "TEXT", NotNull: true}, cols[0])
	assert.Equal(t, "status", cols[1].Name)
	assert.False(t, cols[1].NotNull)

	var got [][]string
	err = src.forEachRow(ctx, "runs", cols, func(literals []string) error {
		got = append(got, literals)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []string{"'r1'", "'completed'", "3"}, got[0])
	assert.Equal(t, []string{"'r2'", `'it''s done'`, "NULL"}, got[1])

	// The full dump pipeline produces replayable SQL for the emulated table.
	var buf strings.Builder
	require.NoError(t, dumpPostgres(ctx, src, &buf))
	assert.Contains(t, buf.String(), `INSERT INTO "runs" ("id", "status", "n") VALUES ('r1', 'completed', 3);`)
}

// --- PostgreSQL backup/restore error paths (no server required) -------------------

const unreachableDSN = "postgres://user:pw@127.0.0.1:1/levee?sslmode=disable&connect_timeout=1"

func TestOpenPostgresErrors(t *testing.T) {
	_, err := openPostgres("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty postgres dsn")

	_, err = openPostgres("postgres://%zz@localhost/db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse postgres dsn")

	_, err = openPostgres(unreachableDSN)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ping postgres")
}

func TestBackupPostgresEmptyOutputPath(t *testing.T) {
	m := NewManagerPostgres(unreachableDSN)
	err := m.BackupPostgreSQL(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty output path")
}

func TestBackupPostgresInvalidOutputPath(t *testing.T) {
	m := NewManagerPostgres(unreachableDSN)
	err := m.BackupPostgreSQL(context.Background(), filepath.Join(t.TempDir(), "no-dir", "x.sql"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestBackupPostgresUnreachable(t *testing.T) {
	m := NewManagerPostgres(unreachableDSN)
	err := m.BackupPostgreSQL(context.Background(), filepath.Join(t.TempDir(), "x.sql"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ping postgres")
}

// writeValidSQLBackup creates a syntactically valid .sql backup with sidecar.
func writeValidSQLBackup(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.sql")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, WriteChecksumFile(path))
	return path
}

func TestRestorePostgresMissingFile(t *testing.T) {
	m := NewManagerPostgres(unreachableDSN)
	err := m.RestorePostgreSQL(context.Background(), filepath.Join(t.TempDir(), "nope.sql"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat backup file")
}

func TestRestorePostgresNoStatements(t *testing.T) {
	m := NewManagerPostgres(unreachableDSN)
	path := writeValidSQLBackup(t, "-- only comments, nothing to replay\n")
	err := m.RestorePostgreSQL(context.Background(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contains no statements")
}

func TestRestorePostgresUnreachable(t *testing.T) {
	m := NewManagerPostgres(unreachableDSN)
	path := writeValidSQLBackup(t, "DELETE FROM runs;\n")
	err := m.RestorePostgreSQL(context.Background(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ping postgres")
}

// execRestoreStatements is plain transactional DML and can be exercised with
// SQLite: the statements are equally valid there.
func TestExecRestoreStatementsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "tx.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE runs (id TEXT PRIMARY KEY, status TEXT)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO runs VALUES ('old', 'superseded')`)
	require.NoError(t, err)

	stmts := []string{
		`DELETE FROM runs`,
		`INSERT INTO runs (id, status) VALUES ('new', 'restored')`,
	}
	require.NoError(t, execRestoreStatements(ctx, db, stmts))

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM runs WHERE id = 'new'`).Scan(&status))
	assert.Equal(t, "restored", status)

	// The pre-restore row must be gone.
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestExecRestoreStatementsRollsBack(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "tx.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE runs (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO runs VALUES ('keep-me')`)
	require.NoError(t, err)

	stmts := []string{
		`DELETE FROM runs`,
		`INSERT INTO nonexistent_table VALUES (1)`, // fails mid-stream
	}
	err = execRestoreStatements(ctx, db, stmts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restore statement 2")

	// Rollback must have restored the original row.
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&count))
	assert.Equal(t, 1, count)
}
