// version_remote_test.go pins the first production wiring of the service
// factory: `levee version` and `levee system version` ask the active mode for
// build information. Local mode answers from this binary's variables; remote
// mode asks the server, which is the question an operator actually has about a
// deployment ("which build is running there?").
//
// The point of going through the factory (rather than calling the gRPC client
// directly, as `agent` does) is that there is ONE place that decides which side
// answers — and that the local side does not need a store to answer.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionLocalAnswersFromThisBinary(t *testing.T) {
	out, _, err := runSplit(t, "version")
	require.NoError(t, err)
	assert.Contains(t, out, version, "本地模式必须报这个二进制的构建信息")
	assert.NotContains(t, out, "test-commit", "本地模式不该拿到服务端的构建信息")
}

func TestVersionRemoteReportsTheServerBuild(t *testing.T) {
	store := newTestStore(t)
	_, addr := startTestServer(t, store, "")

	for _, cmdline := range [][]string{
		{"--remote", "--server", addr, "version"},
		{"--remote", "--server", addr, "system", "version"},
	} {
		out, _, err := runSplit(t, cmdline...)
		require.NoError(t, err, "%v", cmdline)
		assert.Contains(t, out, "test", "%v 必须报服务端的版本", cmdline)
		assert.Contains(t, out, "test-commit", "%v 必须报服务端的 commit", cmdline)
		assert.Contains(t, out, "source:   server "+addr,
			"%v 的人类可读输出必须标明这些数字来自哪台机器", cmdline)
		assert.NotContains(t, out, "unknown",
			"%v 报的是本地构建变量（commitHash 在测试里是 unknown），说明 --remote 没生效", cmdline)
	}
}

func TestVersionRemoteJSONCarriesServerFields(t *testing.T) {
	store := newTestStore(t)
	_, addr := startTestServer(t, store, "")

	res, _, err := freshJSON(t, "--remote", "--server", addr, "--json", "version")
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "test", data["version"])
	assert.Equal(t, "test-commit", data["commit_hash"])
	assert.Equal(t, "test-build", data["build_time"])
	assert.Equal(t, "go-test", data["go_version"])
	// The --json document is an existing contract: four keys, no "source".
	_, hasSource := data["source"]
	assert.False(t, hasSource, "--json 的四键形状是既有契约，不能因为远程模式多出一个键")
}

// TestVersionRemoteCreatesNoLocalDatabase is the lazy-factory property seen
// from the command line: asking a server for its build must not touch the local
// data directory. Before the factory was made lazy, any local build required a
// store, so wiring this command would have created ~/.levee/data/levee.db as a
// side effect of printing four strings.
func TestVersionRemoteCreatesNoLocalDatabase(t *testing.T) {
	store := newTestStore(t)
	_, addr := startTestServer(t, store, "")

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	dbPath := filepath.Join(dataDir, "levee.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "server:\n  data_dir: " + filepath.ToSlash(dataDir) +
		"\ndatabase:\n  driver: sqlite\n  path: " + filepath.ToSlash(dbPath) + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))

	_, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "version")
	require.NoError(t, err)
	assert.NoFileExists(t, dbPath, "--remote version 不该打开（或创建）本地库")
}
