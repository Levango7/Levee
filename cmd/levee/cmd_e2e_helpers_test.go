// Package-level shared harness for the E-2 command end-to-end tests
// (docs/quality-hardening-2026-09.md): every command resolves configuration
// through config.Load(optConfigPath) and opens its own SQLite store, so a
// temp YAML with absolute data_dir/database.path is sufficient to drive
// RunE bodies against real persistence, locally.
//
// NOTE: executeCommand/captureStdout calls resetRootFlags(), which wipes
// optConfigPath — every executed command line must therefore pass
// `--config <cfgPath>` itself (tests below thread cfgPath through helpers).
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// cliEnv is the resolved temp-store environment for a command test run.
type cliEnv struct {
	dataDir string
	dbPath  string
	cfgPath string
}

// newCLIEnv writes a minimal valid config YAML (absolute Windows-safe paths
// single-quoted so backslashes stay literal) into a temp directory and returns
// the environment WITHOUT touching optConfigPath — pass e.cfgPath as
// `--config` on every executeCommand invocation.
func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "levee.db")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "server:\n  data_dir: '" + dataDir + "'\ndatabase:\n  path: '" + dbPath + "'\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))
	return &cliEnv{dataDir: dataDir, dbPath: dbPath, cfgPath: cfgPath}
}

// open opens the same SQLite store the commands will open, for seeding and
// assertions. The store is closed on test cleanup.
func (e *cliEnv) open(t *testing.T) *state.SQLiteStore {
	t.Helper()
	store, err := state.NewSQLiteStore(context.Background(), e.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedRun inserts a minimal run row with the given status.
func (e *cliEnv) seedRun(t *testing.T, id, status string) {
	t.Helper()
	store := e.open(t)
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID: id, WorkflowName: "wf", TemplateName: "tpl", Params: "{}",
		PlanHash: "abcd", Status: status, ApprovalStatus: "approved",
		CreatedAt: now, UpdatedAt: now, Creator: "cli-user",
	}))
}

// resetDriftFlags restores cmd_drift.go package globals to their registration
// defaults (note: baseline defaults to "auto", alert/enabled default to true).
func resetDriftFlags() {
	driftOptHost = ""
	driftOptHosts = ""
	driftOptBaseline = "auto"
	driftOptFile = ""
	driftOptRunID = ""
	driftOptName = ""
	driftOptCron = ""
	driftOptDays = 30
	driftOptAlert = true
	driftOptEnabled = true
}

// resetCalendarFlags restores cmd_calendar.go package globals.
func resetCalendarFlags() {
	calOptName = ""
	calOptStart = ""
	calOptEnd = ""
	calOptTargets = ""
	calOptFrozen = false
	calOptCron = ""
	calOptRepeat = ""
	calOptLimit = 0
}

// resetTargetFlags restores cmd_target.go package globals.
func resetTargetFlags() {
	targetListOptFormat = ""
	targetImportOptFile = ""
	targetImportOptGroup = ""
	targetCheckOptTarget = ""
	targetStatusOptTarget = ""
	targetHistOptLimit = 20
	targetListGroupFilter = ""
	targetListStatusFilter = ""
}

// resetSecretFlags restores cmd_secret.go package globals.
func resetSecretFlags() {
	secretAddOptName = ""
	secretAddOptType = "ssh_password"
	secretAddOptValue = ""
	secretRotateOptName = ""
	secretRevokeOptName = ""
	secretShowOptName = ""
}

// resetPluginFlags restores cmd_plugin.go package globals.
func resetPluginFlags() {
	pluginOptVerifySig = false
}

// resetAuditFlags restores cmd_audit.go package globals.
func resetAuditFlags() {
	auditExportOptFormat = "json"
}

// resetPauseFlags restores cmd_pause.go package globals.
func resetPauseFlags() {
	pauseAllOptReason = ""
	resumeAllOptReason = ""
}

// resetSystemFlags restores cmd_system.go package globals.
// (resetChatopsFlags lives in cmd_chatops_test.go alongside the chatops unit
// tests; resetAllCmdFlags below reuses it.)
func resetSystemFlags() {
	statusOptFormat = ""
}

// resetPushFlags restores cmd_push.go package globals and the manager
// singleton (sync.Once means the first config seen wins process-wide).
func resetPushFlags(t *testing.T) {
	t.Helper()
	resetPushOpts()
}

// resetPushOpts is resetPushFlags without the *testing.T so resetAllCmdFlags
// can reuse it before every command execution.
func resetPushOpts() {
	resetPushVars()
	resetPushManagerForTest()
}

// resetPushVars clears only the flag variables, keeping the manager
// singleton — needed by the push family whose commands intentionally share
// the in-process device registry.
func resetPushVars() {
	pushOptUser = ""
	pushOptToken = ""
	pushOptPlatform = "ios"
	pushOptTitle = ""
	pushOptBody = ""
	pushOptAPNsKeyFile = ""
	pushOptAPNsTeamID = ""
	pushOptAPNsKeyID = ""
	pushOptAPNsBundleID = ""
	pushOptAPNsProduction = false
	pushOptFCMKeyFile = ""
	pushOptFCMProjectID = ""
}

// resetAllCmdFlags clears every sub-command option global before an
// execution. Cobra retains parsed flag VALUES on these variables across
// in-process executeCommand calls (resetRootFlags only covers the root
// persistent flags), so without this a `--host web-01` from one command
// leaks into the next — real CLI invocations are fresh processes.
func resetAllCmdFlags() {
	resetDriftFlags()
	resetCalendarFlags()
	resetTargetFlags()
	resetSecretFlags()
	resetPluginFlags()
	resetAuditFlags()
	resetPauseFlags()
	resetChatopsFlags()
	resetSystemFlags()
	resetPushOpts()
	resetFlagChangedMarkers()
}

// resetFlagChangedMarkers clears pflag's Changed bit on every flag of the
// whole command tree. RunE bodies use cmd.Flags().Changed(...) for
// partial-update semantics, and cobra never clears the bit itself between
// in-process executions — a stale true would apply a (now reset to default)
// option variable as if the user had passed the flag.
func resetFlagChangedMarkers() {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
		c.PersistentFlags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
}

// mustRun executes the CLI and requires success, returning stdout.
func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	resetAllCmdFlags()
	out, err := executeCommand(args...)
	require.NoError(t, err, "cli output: %s", out)
	return out
}

// runErr executes the CLI and requires failure, returning the error.
func runErr(t *testing.T, args ...string) error {
	t.Helper()
	resetAllCmdFlags()
	out, err := executeCommand(args...)
	require.Error(t, err, "expected failure, got output: %s", out)
	return err
}

// freshJSON resets command option globals, executes, and parses the output as
// the JSON envelope (the reset-mustRun/runErr counterpart for the
// executeCommandJSON call sites in tests).
func freshJSON(t *testing.T, args ...string) (map[string]any, string, error) {
	t.Helper()
	resetAllCmdFlags()
	return executeCommandJSON(args...)
}

// execKeepCfg runs the CLI with optConfigPath injected AFTER the harness
// reset, without passing `--config` on the command line. Needed by the
// chatops family: those subcommands register their own local `-c/--config`
// (bot config), which shadows the root persistent --config flag.
func execKeepCfg(t *testing.T, cfgPath string, args ...string) (string, error) {
	t.Helper()
	resetAllCmdFlags()
	return captureStdout(func() error {
		// captureStdout ran resetRootFlags(), so the assignment below
		// survives into Execute.
		optConfigPath = cfgPath
		rootCmd.SetArgs(args)
		return rootCmd.Execute()
	})
}

// mustRunKeepCfg is mustRun on top of execKeepCfg.
func mustRunKeepCfg(t *testing.T, cfgPath string, args ...string) string {
	t.Helper()
	out, err := execKeepCfg(t, cfgPath, args...)
	require.NoError(t, err, "cli output: %s", out)
	return out
}

// runErrKeepCfg is runErr on top of execKeepCfg.
func runErrKeepCfg(t *testing.T, cfgPath string, args ...string) error {
	t.Helper()
	out, err := execKeepCfg(t, cfgPath, args...)
	require.Error(t, err, "expected failure, got output: %s", out)
	return err
}

// execKeepCfgJSON is freshJSON on top of execKeepCfg.
func execKeepCfgJSON(t *testing.T, cfgPath string, args ...string) (map[string]any, string, error) {
	t.Helper()
	out, err := execKeepCfg(t, cfgPath, args...)
	var result map[string]any
	if jerr := json.Unmarshal([]byte(out), &result); jerr != nil {
		return nil, out, err
	}
	return result, out, err
}

// containsAll asserts haystack holds every needle.
func containsAll(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		require.Contains(t, haystack, n)
	}
}

// trimOut trims surrounding whitespace for compact assertions.
func trimOut(s string) string { return strings.TrimSpace(s) }
