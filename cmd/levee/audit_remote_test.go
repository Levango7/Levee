// audit_remote_test.go pins the third service_factory wiring: `audit verify
// --remote`.
//
// The claim under test is equivalence, not merely "a verdict came back": the
// local verifier reports EVERY tampered record with its index and both hash
// pairs, while VerifyHashChain used to truncate that to failures[0] on the
// wire. The tamper case below forges two rows on purpose — a single-row test
// would pass against the truncating response.
package main

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // same pure-Go driver the state store uses

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/audit"
)

// TestAuditVerifyRemoteMatchesLocal: an intact chain, three output shapes,
// identical bytes.
func TestAuditVerifyRemoteMatchesLocal(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)

	seedTraces(t, e, "chain-ok", 3)
	require.Equal(t, 3, buildChain(t, e, "chain-ok"))

	store := e.open(t)
	_, addr := startTestServer(t, store, "")

	for name, extra := range map[string][]string{
		"human": nil,
		"quiet": {"--quiet"},
		"json":  {"--json"},
	} {
		t.Run(name, func(t *testing.T) {
			localArgs := append(append([]string{"--config", e.cfgPath}, extra...), "audit", "verify", "chain-ok")
			remoteArgs := append(append([]string{"--config", e.cfgPath, "--remote", "--server", addr}, extra...), "audit", "verify", "chain-ok")

			local, _, err := runSplit(t, localArgs...)
			require.NoError(t, err)
			remote, _, err := runSplit(t, remoteArgs...)
			require.NoError(t, err)

			assert.Contains(t, local, "chain-ok", "本地输出没有内容，等价断言就空转了")
			assert.Equal(t, strings.TrimSpace(local), strings.TrimSpace(remote),
				"%s 形状在两种模式下必须逐字节相同", name)
		})
	}
}

// TestAuditVerifyRemoteReportsEveryTamperedRecord is the gap this wiring had
// to close first: two forged rows must produce two failure entries, byte for
// byte, in both the human and JSON shapes — and the same exit=6 error.
func TestAuditVerifyRemoteReportsEveryTamperedRecord(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)

	traces := seedTraces(t, e, "chain-bad", 3)
	require.Equal(t, 3, buildChain(t, e, "chain-bad"))

	// Forge rows 0 and 2, keeping their stored hashes. Each row is judged on
	// its own content, so both must be reported (the middle row stays intact
	// and its continuity links still hold).
	tamperTraceDetail(t, e, traces[0].ID, `{"step":"forged-0"}`)
	tamperTraceDetail(t, e, traces[2].ID, `{"step":"forged-2"}`)

	store := e.open(t)
	_, addr := startTestServer(t, store, "")

	for name, extra := range map[string][]string{
		"human": nil,
		"json":  {"--json"},
	} {
		t.Run(name, func(t *testing.T) {
			localArgs := append(append([]string{"--config", e.cfgPath}, extra...), "audit", "verify", "chain-bad")
			remoteArgs := append(append([]string{"--config", e.cfgPath, "--remote", "--server", addr}, extra...), "audit", "verify", "chain-bad")

			localOut, localErrOut, localErr := runSplit(t, localArgs...)
			remoteOut, remoteErrOut, remoteErr := runSplit(t, remoteArgs...)

			require.Error(t, localErr, "篡改必须先被本地抓到，否则等价断言比较的是两个成功")
			require.Error(t, remoteErr)
			assert.Equal(t, localErr.Error(), remoteErr.Error(),
				"两条篡改记录时错误文案（含 2 of 3）必须一致")
			assert.Contains(t, remoteErr.Error(), "2 of 3 records tampered")
			assert.Equal(t, strings.TrimSpace(localOut), strings.TrimSpace(remoteOut),
				"%s 形状的逐条 failure 必须逐字节相同（线上不能再只回首条）", name)
			assert.Equal(t, strings.TrimSpace(localErrOut), strings.TrimSpace(remoteErrOut))

			assert.Contains(t, localOut, traces[0].ID)
			assert.Contains(t, localOut, traces[2].ID,
				"本地没列出第二条，等价断言就分不出截断")
		})
	}
}

// TestAuditVerifyRemotePrevHashBreakMatchesLocal covers the OTHER failure
// kind: a row removed without rebuilding leaves the next row's PrevHash
// pointing at a hash that is no longer its predecessor. That exercises
// type/index/prev_* fields, which a content-only tamper (type 0, equal
// Expected/Actual) would leave indistinguishable from their zero values.
func TestAuditVerifyRemotePrevHashBreakMatchesLocal(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)

	traces := seedTraces(t, e, "chain-gap", 3)
	require.Equal(t, 3, buildChain(t, e, "chain-gap"))
	deleteTraceRow(t, e.dbPath, traces[1].ID)

	store := e.open(t)
	_, addr := startTestServer(t, store, "")

	localOut, _, localErr := runSplit(t, "--config", e.cfgPath, "--json", "audit", "verify", "chain-gap")
	remoteOut, _, remoteErr := runSplit(t, "--config", e.cfgPath, "--remote", "--server", addr, "--json", "audit", "verify", "chain-gap")

	require.Error(t, localErr)
	require.Error(t, remoteErr)
	assert.Equal(t, localErr.Error(), remoteErr.Error())
	assert.Contains(t, remoteErr.Error(), "1 of 2 records tampered")
	assert.Contains(t, localOut, `"Type": 1`, "夹具必须真的制造出 prev_hash_mismatch，否则字段仍未被覆盖")
	assert.Equal(t, strings.TrimSpace(localOut), strings.TrimSpace(remoteOut),
		"prev_* 与 index 也必须逐字节相同")
}

// deleteTraceRow removes a trace below the store API, past the WORM delete
// trigger — the attacker a hash chain exists to catch.
func deleteTraceRow(t *testing.T, dbPath, traceID string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`DROP TRIGGER IF EXISTS worm_prevent_trace_delete`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM trace WHERE id = ?`, traceID)
	require.NoError(t, err)
}

// TestAuditVerifyRemoteNoTracesRefusesLikeLocal: the server skips trace-less
// runs while walking a whole deployment; for a NAMED run that must not turn
// into "0 records, valid".
func TestAuditVerifyRemoteNoTracesRefusesLikeLocal(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	store := e.open(t)
	_, addr := startTestServer(t, store, "")

	_, _, localErr := runSplit(t, "--config", e.cfgPath, "audit", "verify", "no-such-run")
	require.Error(t, localErr)
	_, _, remoteErr := runSplit(t, "--config", e.cfgPath, "--remote", "--server", addr, "audit", "verify", "no-such-run")
	require.Error(t, remoteErr, "远端对没有 trace 的 run 报 valid 就是说有一条不存在的链")

	assert.Equal(t, localErr.Error(), remoteErr.Error())
	assert.Contains(t, remoteErr.Error(), audit.ErrNoTraces.Error())
}

// TestAuditVerifyRemoteDialsTheServer: the equivalence cases read the same
// seeded database in both modes, so only a dead endpoint can show which branch
// ran. Falling back to local would answer happily here.
func TestAuditVerifyRemoteDialsTheServer(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	seedTraces(t, e, "chain-dead", 2)
	require.Equal(t, 2, buildChain(t, e, "chain-dead"))

	stdout, _, err := runSplit(t, "--config", e.cfgPath, "--remote", "--server", "127.0.0.1:1", "audit", "verify", "chain-dead")
	require.Error(t, err, "server 不通时静默退回本地验证，就是操作者以为在问 server")
	assert.Empty(t, strings.TrimSpace(stdout))
}

// TestAuditVerifyRemoteCreatesNoLocalDatabase: asking a server to verify must
// not touch the local data directory.
func TestAuditVerifyRemoteCreatesNoLocalDatabase(t *testing.T) {
	defer resetRootFlags()
	cfgPath, dbPath := targetTestCfg(t)
	_, addr := startTestServer(t, newTestStore(t), "")

	_, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "audit", "verify", "missing")
	require.Error(t, err, "不存在的 run 仍然要报错")
	_, statErr := os.Stat(dbPath)
	assert.True(t, os.IsNotExist(statErr), "--remote audit verify 不该打开（或创建）本地库")
}
