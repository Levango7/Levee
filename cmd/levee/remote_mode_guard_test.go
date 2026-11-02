// remote_mode_guard_test.go pins the fail-fast half of the dual-mode flags.
//
// The bug this guards against is not "remote mode is incomplete" — it is that
// `--remote` was accepted by every command and consumed by three, so a
// local-only command ran happily against the local store while the operator
// believed they were talking to the server. That is a silent lie with a data
// consequence: runs, approvals and freeze periods land in a database the server
// never reads. The guard turns it into exit 2 before any store is opened.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findCommand resolves a "levee a b" path against the real command tree, so the
// allowlist is checked against what is registered rather than against its own
// strings.
func findCommand(t *testing.T, path string) *cobra.Command {
	t.Helper()
	parts := strings.Fields(strings.TrimPrefix(path, "levee "))
	cmd, _, err := rootCmd.Find(parts)
	require.NoError(t, err, "名单里的命令解析不到: %s", path)
	require.NotNil(t, cmd)
	require.Equal(t, path, cmd.CommandPath(), "名单条目 %q 没有指向它自己声称的命令", path)
	return cmd
}

func TestRemoteCapabilityListResolves(t *testing.T) {
	require.NotEmpty(t, remoteCapableCommands,
		"空名单会让 --remote 在它真正支持的命令上也被拒")
	for _, path := range remoteCapableCommands {
		findCommand(t, path)
	}
}

// TestRemoteCapabilityListIsExactlyThese is the independent expectation.
// Iterating over the production slice alone cannot catch an entry being removed:
// the loop simply runs one fewer time and still reports green (measured — that
// is how this test came to exist).
func TestRemoteCapabilityListIsExactlyThese(t *testing.T) {
	assert.Equal(t,
		[]string{
			"levee agent list",
			"levee agent show",
			"levee agent remove",
			"levee version",
			"levee system version",
		},
		remoteCapableCommands,
		"名单变了就要在这里说清楚：谁能真的走 --remote，谁只是被误加进来")
}

func TestValidateServiceModeAllowsRemoteCapableCommands(t *testing.T) {
	for _, path := range remoteCapableCommands {
		t.Run(path, func(t *testing.T) {
			defer resetRootFlags()
			cmd := findCommand(t, path)
			optRemote = true
			require.NoError(t, validateServiceMode(cmd))
		})
	}
}

func TestValidateServiceModeRefusesLocalOnlyCommand(t *testing.T) {
	defer resetRootFlags()
	cmd := findCommand(t, "levee list")
	optRemote = true

	err := validateServiceMode(cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not wired for `levee list`")
	assert.Contains(t, err.Error(), "[exit=2]")
	assert.Equal(t, 2, exitCodeFor(err))
}

func TestValidateServiceModeRefusesBothFlagsEvenForRemoteCommand(t *testing.T) {
	defer resetRootFlags()
	cmd := findCommand(t, remoteCapableCommands[0])
	optLocal, optRemote = true, true

	err := validateServiceMode(cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestValidateServiceModeIsSilentInLocalMode(t *testing.T) {
	defer resetRootFlags()
	require.NoError(t, validateServiceMode(findCommand(t, "levee version")))
}

// TestRemoteFlagIsRefusedBeforeTheStoreOpens drives the real root command, so it
// also proves the guard is wired into PersistentPreRunE rather than merely
// defined. The temporary config is what makes "no database was created" safe to
// assert: if the guard regresses, the command writes into t.TempDir() instead of
// the developer's own ~/.levee/data/levee.db.
func TestRemoteFlagIsRefusedBeforeTheStoreOpens(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dataDir, "levee.db")
	cfg := "server:\n  data_dir: " + filepath.ToSlash(dataDir) +
		"\ndatabase:\n  driver: sqlite\n  path: " + filepath.ToSlash(dbPath) + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	// The directory must exist, or a regressed guard fails on "unable to open
	// database file" and the NoFileExists assertion below stops being the thing
	// that catches it.
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	_, _, err := runSplit(t, "--config", cfgPath, "--remote", "list")
	require.Error(t, err, "--remote 在只有本地语义的 list 上必须失败")
	assert.Contains(t, err.Error(), "not wired for `levee list`")
	assert.Equal(t, 2, exitCodeFor(err), "必须是用法错误码 2，不是笼统的 1")
	assert.NoFileExists(t, dbPath, "被拒的命令不该打开（更不该创建）本地库")
}
