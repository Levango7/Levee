// target_remote_test.go pins the third service_factory wiring: `target list
// --remote`.
//
// The claim under test is equivalence, not merely "some output appeared": both
// modes are pointed at the SAME inventory, and the three output shapes must
// come out byte-identical. That required widening the wire first — the local
// path prints state.Target, whose created_at / last_checked_at / tenant_id had
// no protobuf field, and `--group` names a group the RPC could only accept by
// internal ID.
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// targetTestEnv seeds one inventory and returns a config that points the local
// path at exactly that database. Timestamps are pinned to whole seconds: the
// wire carries int64 seconds, so anything finer would compare a formatting
// difference rather than a wiring one.
func targetTestEnv(t *testing.T) (cfgPath string, store state.Store) {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	dbPath := filepath.Join(dataDir, "levee.db")

	var err error
	store, err = state.NewSQLiteStore(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	created := time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC)
	checked := time.Date(2026, 11, 1, 9, 45, 0, 0, time.UTC)

	require.NoError(t, store.UpsertInventoryGroup(ctx, &state.InventoryGroup{
		ID: "grp-prod", Name: "prod/db", CreatedAt: created,
	}))

	// Two labels on db-01: the human renderer joins them, so label order must
	// be deterministic for a byte comparison to mean anything.
	rows := []*state.Target{
		{
			ID: "tgt-db-01", Hostname: "db-01", Port: 22, ChannelType: "ssh",
			Labels:  map[string]string{"env": "prod", "role": "db"},
			GroupID: "grp-prod", Status: state.StatusActive, Reachable: true,
			CreatedAt: created, LastCheckedAt: &checked,
		},
		{
			ID: "tgt-db-02", Hostname: "db-02", Port: 2222, ChannelType: "ssh",
			CredentialRef: "cred-x", GroupID: "grp-prod",
			Status: state.StatusFrozen, Reachable: false, CreatedAt: created,
		},
		{
			ID: "tgt-web-01", Hostname: "web-01", Port: 22, ChannelType: "winrm",
			Status: state.StatusRetired, Reachable: false, CreatedAt: created,
		},
	}
	for _, r := range rows {
		require.NoError(t, store.UpsertTarget(ctx, r))
	}

	cfgPath = filepath.Join(dir, "config.yaml")
	cfg := "server:\n  data_dir: " + filepath.ToSlash(dataDir) +
		"\ndatabase:\n  driver: sqlite\n  path: " + filepath.ToSlash(dbPath) + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	return cfgPath, store
}

// targetTestCfg builds a config whose database does not exist yet, for the
// "remote must not touch the local data directory" assertion.
func targetTestCfg(t *testing.T) (cfgPath, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	dbPath = filepath.Join(dataDir, "levee.db")
	cfgPath = filepath.Join(dir, "config.yaml")
	cfg := "server:\n  data_dir: " + filepath.ToSlash(dataDir) +
		"\ndatabase:\n  driver: sqlite\n  path: " + filepath.ToSlash(dbPath) + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	return cfgPath, dbPath
}

// TestTargetListRemoteMatchesLocal is the equivalence proof: same inventory,
// two modes, identical bytes for every output shape.
func TestTargetListRemoteMatchesLocal(t *testing.T) {
	cfgPath, store := targetTestEnv(t)
	_, addr := startTestServer(t, store, "")

	for name, extra := range map[string][]string{
		"human": nil,
		"quiet": {"--quiet"},
		"json":  {"--json"},
	} {
		t.Run(name, func(t *testing.T) {
			localArgs := append(append([]string{"--config", cfgPath}, extra...), "target", "list")
			remoteArgs := append(append([]string{"--config", cfgPath, "--remote", "--server", addr}, extra...), "target", "list")

			local, _, err := runSplit(t, localArgs...)
			require.NoError(t, err)
			remote, _, err := runSplit(t, remoteArgs...)
			require.NoError(t, err)

			assert.Contains(t, local, "db-01", "本地输出没有内容，等价断言就空转了")
			assert.Equal(t, strings.TrimSpace(local), strings.TrimSpace(remote),
				"%s 形状在两种模式下必须逐字节相同", name)
		})
	}
}

func TestTargetListRemoteStatusFilterMatchesLocal(t *testing.T) {
	cfgPath, store := targetTestEnv(t)
	_, addr := startTestServer(t, store, "")

	local, _, err := runSplit(t, "--config", cfgPath, "target", "list", "--status", state.StatusFrozen)
	require.NoError(t, err)
	remote, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "target", "list", "--status", state.StatusFrozen)
	require.NoError(t, err)

	assert.Contains(t, local, "db-02")
	assert.NotContains(t, local, "db-01", "本地过滤没生效，等价断言会变成两边都空")
	assert.NotContains(t, local, "web-01")
	assert.Equal(t, strings.TrimSpace(local), strings.TrimSpace(remote),
		"--status 在线上靠客户端过滤：结果必须一致")
}

func TestTargetListRemoteGroupFilterMatchesLocal(t *testing.T) {
	cfgPath, store := targetTestEnv(t)
	_, addr := startTestServer(t, store, "")

	local, _, err := runSplit(t, "--config", cfgPath, "target", "list", "--group", "prod/db")
	require.NoError(t, err)
	remote, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "target", "list", "--group", "prod/db")
	require.NoError(t, err)

	assert.Contains(t, local, "db-01")
	assert.Contains(t, local, "db-02")
	assert.NotContains(t, local, "web-01", "本地分组过滤没生效")
	assert.Equal(t, strings.TrimSpace(local), strings.TrimSpace(remote),
		"--group 走 group_name 由服务端解析：结果必须一致")
}

// TestTargetListRemoteUnknownGroupRefusesLikeLocal: a typo must not look like
// an empty inventory. The local path errors; the remote path must say the same
// thing, not answer with zero rows.
func TestTargetListRemoteUnknownGroupRefusesLikeLocal(t *testing.T) {
	cfgPath, store := targetTestEnv(t)
	_, addr := startTestServer(t, store, "")

	_, _, localErr := runSplit(t, "--config", cfgPath, "target", "list", "--group", "nope")
	require.Error(t, localErr)
	_, _, remoteErr := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "target", "list", "--group", "nope")
	require.Error(t, remoteErr, "远端对未知分组返回空列表就是谎报库存")

	assert.Equal(t, localErr.Error(), remoteErr.Error(),
		"两种模式必须给出同一句拒绝")
	assert.Contains(t, remoteErr.Error(), `group "nope" not found`)
}

// TestTargetListRemotePagesThroughEveryTarget exercises the page loop with
// pages smaller than the inventory: 1000 would hide a missing token handoff
// behind a single round trip.
func TestTargetListRemotePagesThroughEveryTarget(t *testing.T) {
	_, store := targetTestEnv(t)
	_, addr := startTestServer(t, store, "")

	f, err := newServiceFactory(modeRemote, addr, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	svc, err := f.GetTargetService()
	require.NoError(t, err)

	got, err := listAllRemoteTargets(context.Background(), svc, "", 2)
	require.NoError(t, err)

	hosts := make([]string, 0, len(got))
	for _, tgt := range got {
		hosts = append(hosts, tgt.Hostname)
	}
	assert.Equal(t, []string{"db-01", "db-02", "web-01"}, hosts,
		"三个目标、每页两条：少了 token 交接就只会剩两行")
}

// TestTargetListRemoteDialsTheServer is what separates "answered by the
// server" from "answered by the local store": the two read the same seeded
// database in the equivalence test, so only a dead endpoint can show which
// branch actually ran. Falling back to local would answer happily here.
func TestTargetListRemoteDialsTheServer(t *testing.T) {
	cfgPath, _ := targetTestEnv(t)

	stdout, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", "127.0.0.1:1", "target", "list")
	require.Error(t, err, "server 不通时静默退回本地库存，就是操作者以为在问 server")
	assert.Empty(t, strings.TrimSpace(stdout))
}

// TestTargetListRemoteCreatesNoLocalDatabase: asking a server for its
// inventory must not touch the local data directory.
func TestTargetListRemoteCreatesNoLocalDatabase(t *testing.T) {
	cfgPath, dbPath := targetTestCfg(t)
	serverStore := newTestStore(t)
	_, addr := startTestServer(t, serverStore, "")

	_, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "target", "list")
	require.NoError(t, err)
	assert.NoFileExists(t, dbPath, "--remote target list 不该打开（或创建）本地库")
}
