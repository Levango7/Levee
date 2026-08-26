package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/backup"
	"github.com/nexus/levee/internal/config"
)

// Backup / restore command option variables. They are populated by cobra and
// reset by tests through resetBackupFlags.
var (
	backupOptOutput     string
	backupOptVerifyOnly bool
	backupOptPGDSN      string

	restoreOptInput string
	restoreOptYes   bool
	restoreOptPGDSN string
)

// preRestoreSuffix marks the safety snapshot taken automatically before a
// restore overwrites the live database.
const preRestoreSuffix = ".pre-restore"

func init() {
	RegisterCommand(newBackupCmd())
	RegisterCommand(newRestoreCmd())
}

// newBackupCmd builds the `levee backup` sub-command.
func newBackupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Backup the LEVEE datastore",
		Long: "Create a consistent backup of the LEVEE datastore together with a " +
			"SHA-256 checksum sidecar. SQLite databases are snapshotted via " +
			"VACUUM INTO (safe while the daemon runs); PostgreSQL databases are " +
			"dumped to a SQL script by a pure-Go implementation (no pg_dump " +
			"required). Use --verify-only to re-check an existing backup " +
			"without creating a new one.",
		Args: cobra.NoArgs,
		RunE: runBackup,
	}
	cmd.Flags().StringVar(&backupOptOutput, "output", "",
		"备份输出文件路径（默认 <db 目录>/levee-backup-<时间戳>.db 或 .sql）")
	cmd.Flags().BoolVar(&backupOptVerifyOnly, "verify-only", false,
		"仅校验已有备份文件的校验和与完整性，不创建新备份")
	cmd.Flags().StringVar(&backupOptPGDSN, "pg-dsn", "",
		"PostgreSQL DSN；提供时备份 PostgreSQL（默认 SQLite，可用 LEVEE_PG_DSN 环境变量代替）")
	return cmd
}

// newRestoreCmd builds the `levee restore` sub-command.
func newRestoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore the LEVEE datastore from a backup",
		Long: "Restore the LEVEE datastore from a backup created by `levee backup`. " +
			"The backup's SHA-256 checksum (and, for SQLite, PRAGMA " +
			"integrity_check) is verified before anything is replaced. For " +
			"SQLite a safety snapshot is written next to the database as " +
			"<db>.pre-restore first, so an accidental restore can itself be " +
			"rolled back.",
		Args: cobra.NoArgs,
		RunE: runRestore,
	}
	cmd.Flags().StringVar(&restoreOptInput, "input", "",
		"待恢复的备份文件路径（必需）")
	cmd.Flags().BoolVar(&restoreOptYes, "yes", false,
		"跳过交互确认（脚本 / 非交互模式必须提供）")
	cmd.Flags().StringVar(&restoreOptPGDSN, "pg-dsn", "",
		"PostgreSQL DSN；提供时恢复到 PostgreSQL（默认 SQLite，可用 LEVEE_PG_DSN 环境变量代替）")
	_ = cmd.MarkFlagRequired("input")
	return cmd
}

// resolvePGDSN picks the PostgreSQL DSN from the explicit flag or the
// LEVEE_PG_DSN environment variable; an empty result selects SQLite mode.
func resolvePGDSN(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("LEVEE_PG_DSN")
}

// resolveBackupManager builds the backup.Manager matching the requested
// backend: PostgreSQL when a DSN is available, otherwise the SQLite database
// configured for this profile.
func resolveBackupManager(pgDSN string) (*backup.Manager, error) {
	if dsn := resolvePGDSN(pgDSN); dsn != "" {
		return backup.NewManagerPostgres(dsn), nil
	}
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return backup.NewManagerSQLite(cfg.Database.Path), nil
}

// defaultBackupPath derives the default output location next to the database
// source (SQLite) or in the working directory (PostgreSQL, where no local
// file exists).
func defaultBackupPath(m *backup.Manager) string {
	stamp := time.Now().Format("20060102-150405")
	if m.Driver() == backup.DriverPostgres {
		return fmt.Sprintf("levee-backup-%s.sql", stamp)
	}
	return filepath.Join(filepath.Dir(m.Source()), fmt.Sprintf("levee-backup-%s.db", stamp))
}

// runBackup executes the `levee backup` command.
func runBackup(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	mgr, err := resolveBackupManager(backupOptPGDSN)
	if err != nil {
		return err
	}

	output := backupOptOutput
	if output == "" {
		output = defaultBackupPath(mgr)
	}

	if backupOptVerifyOnly {
		if err := mgr.Verify(output); err != nil {
			return fmt.Errorf("verify backup: %w", err)
		}
		payload := backupPayload("verify", mgr, output)
		payload["verified"] = true
		return emitResult(payload, payload["backup_path"])
	}

	if err := mgr.Backup(ctx, output); err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}
	payload := backupPayload("backup", mgr, output)
	return emitResult(payload, payload["backup_path"])
}

// runRestore executes the `levee restore` command.
func runRestore(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	mgr, err := resolveBackupManager(restoreOptPGDSN)
	if err != nil {
		return err
	}

	if !restoreOptYes {
		if optJSON {
			return fmt.Errorf("restore overwrites the live datastore: pass --yes in non-interactive mode")
		}
		if !confirmProceed(os.Stdin, os.Stderr) {
			return fmt.Errorf("restore aborted: confirmation not given")
		}
	}

	// Safety net: snapshot the current database before it is overwritten so
	// an accidental restore can itself be rolled back. Only SQLite has a
	// local file to snapshot.
	preRestore := ""
	if mgr.Driver() == backup.DriverSQLite {
		if preRestore, err = snapshotPreRestore(ctx, mgr); err != nil {
			return fmt.Errorf("pre-restore snapshot: %w", err)
		}
	}

	if err := mgr.Restore(ctx, restoreOptInput); err != nil {
		return fmt.Errorf("restore failed: %w", err)
	}

	payload := backupPayload("restore", mgr, restoreOptInput)
	payload["target"] = mgr.Source()
	if preRestore != "" {
		payload["pre_restore_backup"] = preRestore
	}
	return emitResult(payload, payload["target"])
}

// snapshotPreRestore writes <db>.pre-restore next to the SQLite database so
// the pre-restore state can be recovered. A stale snapshot from an earlier
// restore is replaced. Returns "" when the database file does not exist yet.
func snapshotPreRestore(ctx context.Context, mgr *backup.Manager) (string, error) {
	dbPath := mgr.Source()
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat database: %w", err)
	}

	pre := dbPath + preRestoreSuffix
	// VACUUM INTO refuses to overwrite: drop the previous snapshot and its
	// sidecar so this one can be written.
	os.Remove(pre)
	os.Remove(pre + backup.ChecksumSuffix)

	if err := mgr.BackupSQLite(ctx, pre); err != nil {
		return "", err
	}
	return pre, nil
}

// confirmProceed asks for an explicit interactive confirmation. It returns
// true only on "yes" / "y" (case-insensitive); any other input, including
// EOF, aborts the restore.
func confirmProceed(r io.Reader, w io.Writer) bool {
	fmt.Fprint(w, "此操作将用备份覆盖当前数据。输入 yes 确认继续: ")
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "yes" || answer == "y"
}

// backupPayload assembles the shared result document: backup location, size
// and SHA-256 digest, so both human and JSON output report identical facts.
func backupPayload(action string, mgr *backup.Manager, backupPath string) map[string]any {
	payload := map[string]any{
		"action":      action,
		"driver":      mgr.Driver(),
		"source":      mgr.Source(),
		"backup_path": backupPath,
	}
	if st, err := os.Stat(backupPath); err == nil {
		payload["size_bytes"] = st.Size()
	}
	if sum, err := backup.FileSHA256(backupPath); err == nil {
		payload["sha256"] = sum
	}
	return payload
}

// emitResult renders the result document as JSON (--json), bare path
// (--quiet) or the human-readable block (default).
func emitResult(payload map[string]any, quietValue any) error {
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  payload,
			"meta":  nil,
			"error": nil,
		})
	}
	if optQuiet {
		fmt.Fprintln(os.Stdout, quietValue)
		return nil
	}
	printBackupHuman(os.Stdout, payload)
	return nil
}

// printBackupHuman renders the backup/restore/verify result block.
func printBackupHuman(w io.Writer, payload map[string]any) {
	fmt.Fprintf(w, "%s ok\n", payload["action"])
	fmt.Fprintf(w, "  driver:      %v\n", payload["driver"])
	fmt.Fprintf(w, "  source:      %v\n", payload["source"])
	fmt.Fprintf(w, "  backup_path: %v\n", payload["backup_path"])
	if v, ok := payload["verified"]; ok {
		fmt.Fprintf(w, "  verified:    %v\n", v)
	}
	if v, ok := payload["target"]; ok {
		fmt.Fprintf(w, "  target:      %v\n", v)
	}
	if v, ok := payload["pre_restore_backup"]; ok {
		fmt.Fprintf(w, "  pre_restore: %v\n", v)
	}
	if v, ok := payload["size_bytes"]; ok {
		fmt.Fprintf(w, "  size_bytes:  %v\n", v)
	}
	if v, ok := payload["sha256"]; ok {
		fmt.Fprintf(w, "  sha256:      %v\n", v)
	}
}
