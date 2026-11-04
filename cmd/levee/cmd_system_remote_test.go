// cmd_system_remote_test.go pins the "remote shows less" decision for
// `system status --remote`.
//
// The local command answers two different questions at once: whether this
// machine's configured database is reachable, and where that database is. The
// remote GetStatus RPC answers a third: how the server itself is doing. It
// cannot answer the first two because GetStatus performs authentication but no
// policy check — config paths and database endpoints are deployment topology,
// and GetConfig already gates that class behind ActionAdmin. Keeping the fields
// local is the decision; naming them in the output is what keeps it from
// looking like a broken --remote.
package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemStatusRemoteReportsRuntimeFacts(t *testing.T) {
	store := newTestStore(t)
	_, addr := startTestServer(t, store, "")

	out, _, err := runSplit(t, "--remote", "--server", addr, "system", "status")
	require.NoError(t, err)

	assert.Contains(t, out, "levee test (server "+addr+")",
		"远端输出必须标明版本与回答的是哪台机器")
	assert.Contains(t, out, "status:    healthy")
	assert.Contains(t, out, "runs:      0 active, 0 paused")
	assert.Contains(t, out, "store:     sqlite")
	assert.Contains(t, out, "warnings:  none")
	assert.Contains(t, out,
		"local-only: config_path, db_status, db_driver, db_location, db_path",
		"少了哪些字段必须写出来，否则操作者会以为 --remote 没生效")
	// The local renderer's keys must not leak into the remote answer: they
	// describe a database this process never opened.
	assert.NotContains(t, out, "config:")
	assert.NotContains(t, out, "db:      ok")
}

func TestSystemStatusRemoteJSONShape(t *testing.T) {
	store := newTestStore(t)
	_, addr := startTestServer(t, store, "")

	res, _, err := freshJSON(t, "--remote", "--server", addr, "--json", "system", "status")
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, "remote", data["mode"])
	assert.Equal(t, addr, data["server"])
	assert.Equal(t, "test", data["version"])
	assert.Equal(t, "healthy", data["status"])
	assert.Equal(t, float64(0), data["active_runs"])
	assert.Equal(t, float64(0), data["paused_runs"])
	assert.Equal(t, "sqlite", data["store_type"])
	require.Contains(t, data, "uptime_seconds")

	warnings, ok := data["warnings"].([]any)
	require.True(t, ok, "warnings 必须是数组而不是 null：空数组与缺字段是两种契约")
	assert.Empty(t, warnings)

	localOnly, ok := data["local_only"].([]any)
	require.True(t, ok)
	assert.Equal(t, []any{
		"config_path", "db_status", "db_driver", "db_location", "db_path",
	}, localOnly)

	for _, key := range []string{
		"config_path", "db_status", "db_driver", "db_location", "db_path",
	} {
		_, present := data[key]
		assert.False(t, present,
			"%s 是本地事实，远程 GetStatus 不报；它若出现，说明有人把匿名健康接口加宽了", key)
	}
}

// TestSystemStatusRemoteCreatesNoLocalDatabase is the same lazy-factory
// property version_remote_test.go pins, applied to a command whose LOCAL
// implementation does open a store: choosing --remote must bypass that entire
// branch, not run it and discard the result.
func TestSystemStatusRemoteCreatesNoLocalDatabase(t *testing.T) {
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

	_, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr,
		"system", "status")
	require.NoError(t, err)
	assert.NoFileExists(t, dbPath,
		"--remote system status 不该打开（或创建）本地库：它回答的是服务端的事实")
}

// TestSystemStatusRemoteUnreachableServerFails pins the fail-fast contract.
//
// The address is one we bound and closed ourselves rather than a hard-coded
// 127.0.0.1:1. Both denote "nothing listens here", but port 1 is only unused by
// convention — it is an assumption about the runner, not about the code — and
// a refused connection is the one failure mode that resolves immediately on
// every platform, so the assertion cannot turn into a connect timeout on a
// runner whose stack treats the address differently.
func TestSystemStatusRemoteUnreachableServerFails(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	require.NoError(t, lis.Close())

	_, _, err = runSplit(t, "--remote", "--server", addr, "system", "status")
	require.Error(t, err, "连不上 server 时必须报错，而不是回退到本地状态")
	assert.Contains(t, err.Error(), "get version")
}
