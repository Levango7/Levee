// audit_show_remote_test.go covers one more service_factory wiring:
// `audit show --remote`.
//
// `show` is the one audit reader that names a ROW rather than a run, so it needs
// its own RPC (AuditService.GetAuditTrace) — ListAuditTraces filters by run, and
// walking every run to reach an id the operator already typed is both slow and
// wrong under visibility filtering. What has to be proven here is that the row
// the server hands back is the row the local store holds, byte for byte, in all
// three output shapes — including the two parts of the row that a lossy mapping
// would silently drop: the stored detail column (field 12 is a projection of it)
// and the sub-second remainder of the timestamp (field 8 is whole seconds).
//
// The fixture is deliberately not second-aligned and deliberately carries a
// tenant, so "identical" cannot be satisfied by both sides losing the same
// thing. Each case asserts that premise on the LOCAL output first.
package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// showTraceID is the row these tests ask for: seedTracesForRemote gives index 1
// a tenant, and every row a Detail whose console projection ({"batch":"b1"})
// drops the target the stored JSON carries.
const showTraceID = "show-run-tr01"

func seedShowEnv(t *testing.T) (*cliEnv, string) {
	t.Helper()
	e := newCLIEnv(t)
	seedTracesForRemote(t, e, "show-run", 2)
	// The server reads through this handle for the whole test, so it
	// stays open until the cleanup e.open registered.
	_, addr := startTestServer(t, e.open(t), "")
	return e, addr
}

// showArgs builds `<global flags> audit show <trace-id> <command flags>`.
func showArgs(extra, remote []string, traceID string) []string {
	args := append([]string{}, remote...)
	args = append(args, "audit", "show", traceID)
	return append(args, extra...)
}

// TestAuditShowRemoteMatchesLocal: three output shapes, identical bytes.
func TestAuditShowRemoteMatchesLocal(t *testing.T) {
	defer resetRootFlags()
	e, addr := seedShowEnv(t)

	remote := []string{"--config", e.cfgPath, "--remote", "--server", addr}
	local := []string{"--config", e.cfgPath}

	// The premise differs per shape because each shape prints a different part of
	// the row: --json is the only one that carries the fraction of a second and
	// the tenant column, human is the only one that prints the stored detail, and
	// --quiet prints the id and nothing else.
	premise := map[string]string{
		"human": `host:web-01`,
		"quiet": showTraceID,
		"json":  `.123456789`,
	}

	for name, extra := range map[string][]string{
		"human": nil,
		"quiet": {"--quiet"},
		"json":  {"--json"},
	} {
		t.Run(name, func(t *testing.T) {
			out, _, err := runSplit(t, showArgs(extra, local, showTraceID)...)
			require.NoError(t, err)
			assert.Contains(t, out, premise[name],
				"本地输出没有要保住的那一部分，等价断言就空转了")

			remoteOut, _, err := runSplit(t, showArgs(extra, remote, showTraceID)...)
			require.NoError(t, err)

			assert.Equal(t, strings.TrimSpace(out), strings.TrimSpace(remoteOut),
				"%s 形状在两种模式下必须逐字节相同", name)
		})
	}

	// The three lossy-mapping traps, named — parsed rather than substring-matched,
	// because PrintJSON indents and escapes, and because the equality above would
	// also hold if BOTH modes lost the same thing: the stored detail column (not
	// field 12's projection), the tenant column, and the fraction of a second.
	remoteJSON, _, err := runSplit(t, showArgs([]string{"--json"}, remote, showTraceID)...)
	require.NoError(t, err)
	var envelope struct {
		Data state.Trace `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(remoteJSON), &envelope))
	assert.Contains(t, envelope.Data.Detail, `"target":"host:web-01"`,
		"远程取回的必须是入库原文，不是 field 12 的投影")
	assert.Equal(t, "t-a", envelope.Data.TenantID, "租户列也要上线")
	assert.Equal(t, 123456789, envelope.Data.Timestamp.Nanosecond(),
		"亚秒余数必须活着穿过线上，否则 --json 的等价只是两边都丢了它")
}

// TestAuditShowRemoteUnknownIDRefusesLikeLocal: a missing row must read the same
// in both modes, not as an rpc error with a NotFound prefix.
func TestAuditShowRemoteUnknownIDRefusesLikeLocal(t *testing.T) {
	defer resetRootFlags()
	e, addr := seedShowEnv(t)

	_, _, localErr := runSplit(t, "--config", e.cfgPath, "audit", "show", "show-nope")
	require.Error(t, localErr)
	_, _, remoteErr := runSplit(t, "--config", e.cfgPath, "--remote", "--server", addr,
		"audit", "show", "show-nope")
	require.Error(t, remoteErr)

	assert.Equal(t, localErr.Error(), remoteErr.Error(), "两种模式必须给出同一条错误")
	assert.Contains(t, remoteErr.Error(), `trace "show-nope" not found`)
}

// TestAuditShowRemoteDialsTheServer is the branch control: the equivalence cases
// read the same seeded file on both sides, so only a dead endpoint can tell
// "asked the server" from "quietly fell back to local".
//
// The address is one we bound and closed ourselves rather than a hard-coded
// 127.0.0.1:1 — port 1 being unused is an assumption about the runner, not about
// the code, and a refused connection resolves immediately everywhere instead of
// turning into a connect timeout.
func TestAuditShowRemoteDialsTheServer(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	seedTracesForRemote(t, e, "show-run", 2)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dead := lis.Addr().String()
	require.NoError(t, lis.Close())

	stdout, _, err := runSplit(t, "--config", e.cfgPath, "--remote", "--server", dead,
		"audit", "show", showTraceID)
	require.Error(t, err, "audit show 打不通 server 时必须报错，而不是静默读本地库")
	assert.Empty(t, strings.TrimSpace(stdout))
}

// TestAuditShowRemoteCreatesNoLocalDatabase: asking a server for one row must not
// create (or open) the local data directory.
func TestAuditShowRemoteCreatesNoLocalDatabase(t *testing.T) {
	defer resetRootFlags()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "levee.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte("database:\n  driver: sqlite\n  path: "+filepath.ToSlash(dbPath)+"\n"), 0o600))

	e := newCLIEnv(t)
	seedTracesForRemote(t, e, "show-run", 2)
	_, addr := startTestServer(t, e.open(t), "")

	_, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr,
		"audit", "show", showTraceID)
	require.NoError(t, err)
	assert.NoFileExists(t, dbPath, "--remote audit show 不该打开（或创建）本地库")
}
