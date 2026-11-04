// audit_list_remote_test.go covers two more service_factory wirings:
// `audit list --remote` and `audit export --remote`.
//
// Equivalence here has two things to prove beyond "both printed something": the
// row set must survive the store page cap (the handler used to stop at
// maxPageSize, so a long run simply looked short), and the CLI must rebuild each
// row from TraceEntry.stored_detail rather than from .detail, which is a console
// projection of it. The fixtures write a Detail whose projection drops the
// target on purpose, so a mapping that picked the projected field cannot pass.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// seedTracesForRemote writes n traces for runID into e's store.
func seedTracesForRemote(t *testing.T, e *cliEnv, runID string, n int) {
	t.Helper()
	e.seedRun(t, runID, "completed")
	store := e.open(t)
	defer func() { _ = store.Close() }()

	// A deliberately awkward instant: odd nanoseconds, and fixed rather than
	// time.Now() so the two modes are compared against the same bytes every run.
	// The fraction is the point — `audit list` (human and --json) and
	// `audit export --json` all print it, so a wire mapping that dropped it would
	// make those comparisons fail instead of passing on rows that never had one.
	base := time.Date(2024, 3, 17, 8, 5, 3, 123456789, time.UTC)
	for i := range n {
		// One row carries a tenant: state.Trace prints tenant_id with omitempty,
		// so an all-empty fixture would leave that column's mapping untested in
		// both directions (dropped on the server, or dropped on the way back).
		tenant := ""
		if i == 1 {
			tenant = "t-a"
		}
		require.NoError(t, store.CreateTrace(context.Background(), &state.Trace{
			ID:    fmt.Sprintf("%s-tr%02d", runID, i),
			RunID: runID,
			Event: "step.done",
			Actor: "cli-user",
			Detail: fmt.Sprintf(
				`{"step":%d,"target":"host:web-%02d","metadata":{"batch":"b1"}}`, i, i),
			Timestamp: base.Add(time.Duration(i) * time.Second),
			TenantID:  tenant,
		}))
	}
}

// auditArgs builds `<global flags> audit <verb> <run-id> <command flags>`.
func auditArgs(extra []string, remote []string, verb, runID string) []string {
	args := append([]string{}, remote...)
	args = append(args, "audit", verb, runID)
	return append(args, extra...)
}

// TestAuditListRemoteMatchesLocal: three output shapes, identical bytes.
func TestAuditListRemoteMatchesLocal(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)

	seedTracesForRemote(t, e, "list-run", 3)
	// The server reads through this handle for the whole test, so it
	// stays open until the cleanup e.open registered.
	_, addr := startTestServer(t, e.open(t), "")

	remote := []string{"--config", e.cfgPath, "--remote", "--server", addr}
	local := []string{"--config", e.cfgPath}

	for name, extra := range map[string][]string{
		"human": nil,
		"quiet": {"--quiet"},
		"json":  {"--json"},
	} {
		t.Run(name, func(t *testing.T) {
			out, _, err := runSplit(t, auditArgs(extra, local, "list", "list-run")...)
			require.NoError(t, err)
			// Premise first: an empty comparison proves nothing. `--quiet`
			// prints ids only, so it gets the weaker token.
			if name == "quiet" {
				assert.Contains(t, out, "list-run-tr00", "本地输出没有内容，等价断言就空转了")
			} else {
				assert.Contains(t, out, "step.done", "本地输出没有内容，等价断言就空转了")
			}

			remoteOut, _, err := runSplit(t, auditArgs(extra, remote, "list", "list-run")...)
			require.NoError(t, err)

			assert.Equal(t, strings.TrimSpace(out), strings.TrimSpace(remoteOut),
				"%s 形状在两种模式下必须逐字节相同", name)
		})
	}
}

// TestAuditListRemoteUnknownRunRefusesLikeLocal pins the empty answer: an
// unknown run has no traces on either side, and "no traces" must not turn into
// an error in only one mode.
func TestAuditListRemoteUnknownRunRefusesLikeLocal(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)

	// The server reads through this handle for the whole test, so it
	// stays open until the cleanup e.open registered.
	_, addr := startTestServer(t, e.open(t), "")

	out, _, err := runSplit(t, "--config", e.cfgPath, "audit", "list", "ghost")
	require.NoError(t, err)
	remoteOut, _, err := runSplit(t, "--config", e.cfgPath, "--remote", "--server", addr, "audit", "list", "ghost")
	require.NoError(t, err)

	assert.Equal(t, strings.TrimSpace(out), strings.TrimSpace(remoteOut))
	assert.Contains(t, remoteOut, "No traces found.")
}

// TestAuditExportRemoteMatchesLocal covers both export formats. CSV is the
// stricter one: its timestamp format is second-precise on both sides, so the
// bytes must match exactly, and its detail column is where a projected mapping
// would show up.
func TestAuditExportRemoteMatchesLocal(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)

	seedTracesForRemote(t, e, "export-run", 3)
	// The server reads through this handle for the whole test, so it
	// stays open until the cleanup e.open registered.
	_, addr := startTestServer(t, e.open(t), "")

	remote := []string{"--config", e.cfgPath, "--remote", "--server", addr}
	local := []string{"--config", e.cfgPath}

	for name, extra := range map[string][]string{
		"json": nil,
		"csv":  {"--format", "csv"},
	} {
		t.Run(name, func(t *testing.T) {
			out, _, err := runSplit(t, auditArgs(extra, local, "export", "export-run")...)
			require.NoError(t, err)
			// Fixture premise: the stored payload really carries the target, and
			// the console projection ({"batch":"b1"}) really does not — so
			// "identical bytes" below cannot be satisfied by both sides losing it.
			assert.Contains(t, out, "web-01", "导出里没有入库原文，比对就没有意义")

			remoteOut, _, err := runSplit(t, auditArgs(extra, remote, "export", "export-run")...)
			require.NoError(t, err)

			assert.Contains(t, remoteOut, "web-01", "远程导出丢了入库原文：detail 取了投影字段")
			assert.Equal(t, strings.TrimSpace(out), strings.TrimSpace(remoteOut),
				"export 的 %s 在两种模式下必须逐字节相同", name)
		})
	}
}

// TestAuditListRemotePagesThroughEveryTrace drives the page loop with a wire
// page of two, so a missing token hand-off shows up as a short result.
func TestAuditListRemotePagesThroughEveryTrace(t *testing.T) {
	e := newCLIEnv(t)
	seedTracesForRemote(t, e, "paged-run", 3)
	_, addr := startTestServer(t, e.open(t), "")

	f, err := newServiceFactory(modeRemote, addr, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	svc, err := f.GetAuditService()
	require.NoError(t, err)

	got, err := listAllRemoteTraces(context.Background(), svc, "paged-run", 2)
	require.NoError(t, err)

	ids := make([]string, 0, len(got))
	for _, tr := range got {
		ids = append(ids, tr.ID)
	}
	assert.Equal(t, []string{"paged-run-tr00", "paged-run-tr01", "paged-run-tr02"}, ids,
		"三条 trace、每页两条：少了 token 交接就只会剩两行")
}

// TestAuditRemoteDialsTheServer is the branch control for both commands: the
// equivalence cases read the same seeded file on both sides, so only a dead
// endpoint can tell "asked the server" from "quietly fell back to local".
//
// The address is one we bound and closed ourselves rather than a hard-coded
// 127.0.0.1:1 — port 1 being unused is an assumption about the runner, not
// about the code, and a refused connection resolves immediately everywhere
// instead of turning into a connect timeout.
func TestAuditRemoteDialsTheServer(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	seedTracesForRemote(t, e, "dial-run", 1)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dead := lis.Addr().String()
	require.NoError(t, lis.Close())

	for _, verb := range []string{"list", "export"} {
		stdout, _, err := runSplit(t, "--config", e.cfgPath, "--remote", "--server", dead,
			"audit", verb, "dial-run")
		require.Error(t, err, "audit %s 打不通 server 时必须报错，而不是静默读本地库", verb)
		assert.Empty(t, strings.TrimSpace(stdout))
	}
}

// TestAuditRemoteCreatesNoLocalDatabase: asking a server for a run's traces must
// not create (or open) the local data directory.
func TestAuditRemoteCreatesNoLocalDatabase(t *testing.T) {
	defer resetRootFlags()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "levee.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte("database:\n  driver: sqlite\n  path: "+filepath.ToSlash(dbPath)+"\n"), 0o600))

	serverStore := newTestStore(t)
	_, addr := startTestServer(t, serverStore, "")

	for _, verb := range []string{"list", "export"} {
		_, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "audit", verb, "any-run")
		require.NoError(t, err)
	}
	assert.NoFileExists(t, dbPath, "--remote audit 不该打开（或创建）本地库")
}
