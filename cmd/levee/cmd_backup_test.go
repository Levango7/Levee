package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/backup"
	"github.com/nexus/levee/internal/state"
)

// resetBackupFlags restores the backup/restore command flag globals to their
// zero values so tests do not leak state into siblings (cobra does not reset
// bound variables between Execute calls).
func resetBackupFlags() {
	backupOptOutput = ""
	backupOptVerifyOnly = false
	backupOptPGDSN = ""
	restoreOptInput = ""
	restoreOptYes = false
	restoreOptPGDSN = ""
}

// withBackupDataDir points LEVEE_SERVER_DATA_DIR at a temp directory so the
// backup commands operate on an isolated SQLite database.
func withBackupDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LEVEE_SERVER_DATA_DIR", dir)
	return dir
}

// --- registration --------------------------------------------------------------

func TestBackupCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	defer resetBackupFlags()

	cmd := findSub("backup")
	require.NotNil(t, cmd, "backup subcommand should be registered")
	for _, name := range []string{"output", "verify-only", "pg-dsn"} {
		assert.NotNil(t, cmd.Flags().Lookup(name), "backup should have --%s", name)
	}
}

func TestRestoreCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	defer resetBackupFlags()

	cmd := findSub("restore")
	require.NotNil(t, cmd, "restore subcommand should be registered")
	for _, name := range []string{"input", "yes", "pg-dsn"} {
		assert.NotNil(t, cmd.Flags().Lookup(name), "restore should have --%s", name)
	}

	// --input is mandatory.
	cmd.SetArgs([]string{})
	assert.Error(t, cmd.ValidateRequiredFlags(), "restore should require --input")
}

// --- helpers -------------------------------------------------------------------

func TestResolvePGDSN(t *testing.T) {
	t.Setenv("LEVEE_PG_DSN", "env-dsn")
	assert.Equal(t, "flag-dsn", resolvePGDSN("flag-dsn"))
	assert.Equal(t, "env-dsn", resolvePGDSN(""))

	t.Setenv("LEVEE_PG_DSN", "")
	assert.Equal(t, "", resolvePGDSN(""))
}

func TestResolveBackupManager(t *testing.T) {
	dir := withBackupDataDir(t)

	m, err := resolveBackupManager("postgres://u:p@h/db")
	require.NoError(t, err)
	assert.Equal(t, backup.DriverPostgres, m.Driver())
	assert.Equal(t, "postgres://u:p@h/db", m.Source())

	t.Setenv("LEVEE_PG_DSN", "")
	m, err = resolveBackupManager("")
	require.NoError(t, err)
	assert.Equal(t, backup.DriverSQLite, m.Driver())
	assert.Equal(t, filepath.Join(dir, "levee.db"), m.Source())
}

func TestDefaultBackupPath(t *testing.T) {
	sqliteMgr := backup.NewManagerSQLite(filepath.Join("/data/levee", "levee.db"))
	p := defaultBackupPath(sqliteMgr)
	assert.True(t, strings.HasPrefix(p, filepath.Join("/data/levee", "levee-backup-")), p)
	assert.True(t, strings.HasSuffix(p, ".db"), p)

	pgMgr := backup.NewManagerPostgres("postgres://u:p@h/db")
	p = defaultBackupPath(pgMgr)
	assert.True(t, strings.HasPrefix(p, "levee-backup-"), p)
	assert.True(t, strings.HasSuffix(p, ".sql"), p)
}

func TestConfirmProceed(t *testing.T) {
	var buf bytes.Buffer
	assert.True(t, confirmProceed(strings.NewReader("yes\n"), &buf))
	assert.True(t, confirmProceed(strings.NewReader("y\n"), &buf))
	assert.True(t, confirmProceed(strings.NewReader("YES\n"), &buf))
	assert.False(t, confirmProceed(strings.NewReader("no\n"), &buf))
	assert.False(t, confirmProceed(strings.NewReader("\n"), &buf))
	assert.False(t, confirmProceed(strings.NewReader(""), &buf)) // EOF aborts
}

func TestPrintBackupHuman(t *testing.T) {
	var buf bytes.Buffer
	printBackupHuman(&buf, map[string]any{
		"action":             "backup",
		"driver":             "sqlite",
		"source":             "/data/levee.db",
		"backup_path":        "/data/levee-backup-1.db",
		"size_bytes":         int64(8192),
		"sha256":             "abc123",
		"target":             "/data/levee.db",
		"pre_restore_backup": "/data/levee.db.pre-restore",
		"verified":           true,
	})
	out := buf.String()
	for _, want := range []string{
		"backup ok", "sqlite", "/data/levee.db", "abc123", "8192",
		"pre-restore", "verified",
	} {
		assert.Contains(t, out, want)
	}
}

// --- end-to-end CLI lifecycle ----------------------------------------------------

// insertTestAudit seeds one audit row through the real store layer.
func insertTestAudit(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, dbPath)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID:        "audit-backup-1",
		Action:    "backup-test",
		Actor:     "tester",
		Target:    "host-1",
		Result:    "ok",
		Timestamp: time.Now(),
	}))
}

func TestBackupRestoreEndToEnd(t *testing.T) {
	defer resetRootFlags()
	defer resetBackupFlags()

	dir := withBackupDataDir(t)
	dbPath := filepath.Join(dir, "levee.db")
	insertTestAudit(t, dbPath)

	// 1. Backup with default output naming.
	out, err := executeCommand("backup", "--json")
	require.NoError(t, err)

	var env map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	require.Nil(t, env["error"])
	data, ok := env["data"].(map[string]any)
	require.True(t, ok, "expected data object, got: %s", out)

	backupPath, ok := data["backup_path"].(string)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(backupPath, filepath.Join(dir, "levee-backup-")), backupPath)
	assert.True(t, strings.HasSuffix(backupPath, ".db"), backupPath)
	assert.Equal(t, "backup", data["action"])
	assert.NotEmpty(t, data["sha256"])
	assert.NotEmpty(t, data["size_bytes"])

	_, err = os.Stat(backupPath)
	require.NoError(t, err)
	_, err = os.Stat(backupPath + backup.ChecksumSuffix)
	require.NoError(t, err)

	// 2. Tamper with the live database after the snapshot.
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE audit SET action = 'TAMPERED' WHERE id = 'audit-backup-1'`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// 3. Restore with --yes (non-interactive).
	out, err = executeCommand("restore", "--input", backupPath, "--yes", "--json")
	require.NoError(t, err)

	require.NoError(t, json.Unmarshal([]byte(out), &env))
	require.Nil(t, env["error"])
	data, ok = env["data"].(map[string]any)
	require.True(t, ok, "expected data object, got: %s", out)
	assert.Equal(t, "restore", data["action"])
	assert.Equal(t, backupPath, data["backup_path"])
	assert.Equal(t, dbPath, data["target"])

	preRestore, ok := data["pre_restore_backup"].(string)
	require.True(t, ok, "restore should report the pre-restore snapshot")
	assert.Equal(t, dbPath+preRestoreSuffix, preRestore)
	_, err = os.Stat(preRestore)
	assert.NoError(t, err)

	// 4. The tampered row must be back to its original value.
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, dbPath)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	audit, err := store.GetAudit(ctx, "audit-backup-1")
	require.NoError(t, err)
	require.NotNil(t, audit)
	assert.Equal(t, "backup-test", audit.Action)
}

func TestBackupCmdExplicitOutputAndVerifyOnly(t *testing.T) {
	defer resetRootFlags()
	defer resetBackupFlags()

	dir := withBackupDataDir(t)
	dbPath := filepath.Join(dir, "levee.db")
	insertTestAudit(t, dbPath)
	backupPath := filepath.Join(dir, "manual-backup.db")

	out, err := executeCommand("backup", "--output", backupPath, "--json")
	require.NoError(t, err)

	var env map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	data, ok := env["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, backupPath, data["backup_path"])

	// verify-only on the fresh backup succeeds.
	out, err = executeCommand("backup", "--output", backupPath, "--verify-only", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"verified": true`)

	// Tamper with the sidecar: verify-only must now fail.
	require.NoError(t, os.WriteFile(backupPath+backup.ChecksumSuffix,
		[]byte("1111111111111111111111111111111111111111111111111111111111111111  manual-backup.db\n"), 0o600))
	_, err = executeCommand("backup", "--output", backupPath, "--verify-only", "--json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")
}

func TestBackupCmdOutputMissingDir(t *testing.T) {
	defer resetRootFlags()
	defer resetBackupFlags()

	dir := withBackupDataDir(t)
	insertTestAudit(t, filepath.Join(dir, "levee.db"))

	_, err := executeCommand("backup", "--output", filepath.Join(dir, "no-such-dir", "x.db"), "--json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestRestoreCmdRequiresYesInJSONMode(t *testing.T) {
	defer resetRootFlags()
	defer resetBackupFlags()
	withBackupDataDir(t)

	_, err := executeCommand("restore", "--input", "whatever.db", "--json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--yes")
}

func TestRestoreCmdMissingInputFile(t *testing.T) {
	defer resetRootFlags()
	defer resetBackupFlags()

	dir := withBackupDataDir(t)
	// SQLite target does not exist yet: no pre-restore snapshot, and the
	// restore itself fails because the backup file is missing.
	_, err := executeCommand("restore", "--input", filepath.Join(dir, "missing.db"), "--yes", "--json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat backup file")
}

func TestBackupCmdPostgresUnreachable(t *testing.T) {
	defer resetRootFlags()
	defer resetBackupFlags()
	t.Setenv("LEVEE_PG_DSN", "")

	_, err := executeCommand("backup",
		"--pg-dsn", "postgres://user:pw@127.0.0.1:1/levee?sslmode=disable&connect_timeout=1",
		"--output", filepath.Join(t.TempDir(), "x.sql"), "--json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ping postgres")
}

func TestBackupCmdQuietOutput(t *testing.T) {
	defer resetRootFlags()
	defer resetBackupFlags()

	dir := withBackupDataDir(t)
	insertTestAudit(t, filepath.Join(dir, "levee.db"))
	backupPath := filepath.Join(dir, "quiet-backup.db")

	out, err := executeCommand("backup", "--output", backupPath, "--quiet")
	require.NoError(t, err)
	assert.Equal(t, backupPath, strings.TrimSpace(out))
}
