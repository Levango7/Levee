// target_history_remote_test.go covers `levee target history <host> --remote`.
//
// This is the one wired command that does NOT promise byte-identical output, so
// the tests here are shaped to match: the human table is compared byte-for-byte
// (both modes fill the same targetHistoryRow and share one renderer), while
// --json is checked as a SUBSET contract — every remote key must also exist
// locally with the same value, and the keys that are missing must be exactly
// the ones remoteTargetHistoryLocalOnlyFields declares.
//
// The subset test reads observed bytes on both sides rather than reflecting over
// structs, because the remote shape is a literal map in runRemoteTargetHistory.
// A field added to state.Run without updating the declaration therefore shows up
// as a difference set that no longer matches, which is the drift a comment
// cannot catch.
//
// Timestamps in the fixture are whole-second UTC instants on purpose. SQLite
// decodes stored instants as UTC while PostgreSQL decodes them into the session
// zone, so a day-crossing sub-second fixture would make this test assert a
// dialect difference rather than a wiring one. That divergence is registered as
// an open decision in docs/blocked-items.md, not encoded here.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpcserver "google.golang.org/grpc"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// rollingPatchDoc is a workflow SOURCE document, not a name. It belongs in the
// fixture because run.WorkflowName holds the rendered document, so both modes
// must put it through workflowDisplay before printing; if the remote path
// skipped that step the table would gain a multi-line cell.
const rollingPatchDoc = "name: rolling-patch\nversion: \"1.0\"\n" +
	"target:\n  type: host\n  query: \"env=prod\"\n" +
	"steps:\n  - name: deploy\n    action: shell.exec\n    args:\n      cmd: \"true\"\n"

func historyRun(
	id, workflow, status string,
	created time.Time,
	creator string,
) *state.Run {
	return &state.Run{
		ID: id, WorkflowName: workflow, TemplateName: "tpl-" + id, Params: `{"k":1}`,
		PlanHash: "hash-" + id, PlanJSON: `{"nodes":[]}`,
		Status: status, ApprovalStatus: "approved", ApprovalLevel: "standard",
		Priority: "normal", CreatedAt: created, UpdatedAt: created.Add(time.Minute),
		Creator: creator, IncidentID: "INC-1",
		// tenant_id carries omitempty, so a fixture that left it empty would
		// also hide it from LOCAL --json — and the subset test would then read
		// a declared field as "not missing" for the wrong reason.
		TenantID: "t-prod",
	}
}

// seedHistoryEnv writes two runs and one step each for the host, then serves
// that same database over InventoryService — which is where TargetHistory lives,
// so this reuses ②'s server helper instead of registering the service a third
// time.
func seedHistoryEnv(t *testing.T, host string) (*cliEnv, string) {
	t.Helper()

	e := newCLIEnv(t)
	store := e.open(t)
	ctx := context.Background()

	at := func(minute int) time.Time {
		return time.Date(2024, 3, 17, 8, minute, 0, 0, time.UTC)
	}
	runs := []*state.Run{
		historyRun("run-a", "workflows/patch.yaml", state.BatchStateCompleted, at(5), "alice"),
		historyRun("run-b", rollingPatchDoc, state.BatchStateFailed, at(30), "bob"),
	}
	for i, r := range runs {
		require.NoError(t, store.CreateRun(ctx, r))
		// steps.batch_id is a FK onto batches (schema.sql), so the batch row is
		// part of the minimum legal history fixture, not decoration.
		batchID := fmt.Sprintf("bc-%d", i)
		require.NoError(t, store.CreateBatch(ctx, &state.Batch{
			ID: batchID, RunID: r.ID, BatchNo: 1,
			Status: state.BatchStateCompleted, TotalHosts: 1, Succeeded: 1,
		}))
		require.NoError(t, store.CreateStep(ctx, &state.Step{
			ID:       fmt.Sprintf("step-%d", i),
			RunID:    r.ID,
			BatchID:  batchID,
			Host:     host,
			StepName: "deploy",
			Action:   "shell.exec",
			Status:   state.StepStatusSuccess,
		}))
	}

	return e, startGroupTestServer(t, e.open(t))
}

func historyArgs(extra []string, remote []string) []string {
	args := append([]string{}, remote...)
	args = append(args, "target", "history", "db-01")
	return append(args, extra...)
}

// TestTargetHistoryRemoteHumanMatchesLocal: the table is one renderer and must
// produce one byte sequence in both modes.
func TestTargetHistoryRemoteHumanMatchesLocal(t *testing.T) {
	defer resetRootFlags()
	e, addr := seedHistoryEnv(t, "db-01")

	local := []string{"--config", e.cfgPath}
	remote := []string{"--config", e.cfgPath, "--remote", "--server", addr}

	out, _, err := runSplit(t, historyArgs(nil, local)...)
	require.NoError(t, err)
	require.Contains(t, out, "run-a", "本地没有行，等价断言就空转了")
	require.Contains(t, out, "run-b")

	remoteOut, _, err := runSplit(t, historyArgs(nil, remote)...)
	require.NoError(t, err)

	assert.Equal(t, strings.TrimSpace(out), strings.TrimSpace(remoteOut),
		"人类表格两种模式必须逐字节相同")
	// Named separately because a both-empty or both-garbage table would satisfy
	// the equality above: workflowDisplay must have run on the remote rows too.
	assert.Contains(t, remoteOut, "rolling-patch",
		"远程行也必须过 workflowDisplay，否则多行 YAML 会撑破表格")
	assert.NotContains(t, remoteOut, "action: shell.exec",
		"工作流原文不该出现在表格里")
}

func TestTargetHistoryRemoteEmptyMatchesLocal(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	addr := startGroupTestServer(t, e.open(t))

	out, _, err := runSplit(t, historyArgs(nil, []string{"--config", e.cfgPath})...)
	require.NoError(t, err)
	remoteOut, _, err := runSplit(t,
		historyArgs(nil, []string{"--config", e.cfgPath, "--remote", "--server", addr})...)
	require.NoError(t, err)

	assert.Equal(t, "No changes recorded on db-01.\n", out)
	assert.Equal(t, strings.TrimSpace(out), strings.TrimSpace(remoteOut),
		"空答案在两种模式下都得是同一句，不能一边报错")
}

// TestTargetHistoryRemoteJSONIsDeclaredSubsetOfLocal is the contract the whole
// ④ design rests on: remote gives the same keys with the same values, minus a
// list it states out loud.
func TestTargetHistoryRemoteJSONIsDeclaredSubsetOfLocal(t *testing.T) {
	defer resetRootFlags()
	e, addr := seedHistoryEnv(t, "db-01")

	localOut, _, err := runSplit(t, historyArgs([]string{"--json"}, []string{"--config", e.cfgPath})...)
	require.NoError(t, err)
	remoteOut, _, err := runSplit(t,
		historyArgs([]string{"--json"}, []string{"--config", e.cfgPath, "--remote", "--server", addr})...)
	require.NoError(t, err)

	local := historyRows(t, localOut)
	remote := historyRows(t, remoteOut)
	require.Len(t, local, 2, "夹具必须真的出行")
	require.Len(t, remote, len(local), "两种模式必须给同样多的行")

	localKeys := keySet(local)
	remoteKeys := keySet(remote)

	for k := range remoteKeys {
		assert.Contains(t, localKeys, k,
			"远程出现了本地没有的键 %q：--json 的消费方会因模式不同而多出字段", k)
	}

	missing := []string{}
	for k := range localKeys {
		if _, ok := remoteKeys[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	assert.Equal(t, sortedStrings(remoteTargetHistoryLocalOnlyFields), missing,
		"实际少的键和 remoteTargetHistoryLocalOnlyFields 声明的不一致")

	byID := func(rows []map[string]json.RawMessage) map[string]map[string]json.RawMessage {
		out := map[string]map[string]json.RawMessage{}
		for _, r := range rows {
			var id string
			require.NoError(t, json.Unmarshal(r["id"], &id), "远程行的主键必须仍叫 id")
			out[id] = r
		}
		return out
	}
	l, r := byID(local), byID(remote)

	for _, id := range []string{"run-a", "run-b"} {
		require.Contains(t, r, id)
		for _, key := range []string{"workflow_name", "status", "creator"} {
			assert.Equal(t, string(l[id][key]), string(r[id][key]),
				"%s.%s 两边必须同值", id, key)
		}
		// created_at is formatted by each mode, so compare the instants.
		var lt, rt time.Time
		require.NoError(t, json.Unmarshal(l[id]["created_at"], &lt))
		require.NoError(t, json.Unmarshal(r[id]["created_at"], &rt))
		assert.True(t, lt.Equal(rt), "%s.created_at 必须是同一个时刻", id)
	}
}

// TestTargetHistoryRemoteNamesItsLocalOnlyKeysInMeta: meta.local_only is how an
// automation told "these ten keys are absent" learns they are absent by design
// rather than by a broken mapping.
func TestTargetHistoryRemoteNamesItsLocalOnlyKeysInMeta(t *testing.T) {
	defer resetRootFlags()
	e, addr := seedHistoryEnv(t, "db-01")

	out, _, err := runSplit(t,
		historyArgs([]string{"--json"}, []string{"--config", e.cfgPath, "--remote", "--server", addr})...)
	require.NoError(t, err)

	var envelope struct {
		Data []map[string]json.RawMessage `json:"data"`
		Meta struct {
			Mode      string   `json:"mode"`
			Server    string   `json:"server"`
			LocalOnly []string `json:"local_only"`
		} `json:"meta"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &envelope))

	assert.Equal(t, "remote", envelope.Meta.Mode)
	assert.Equal(t, addr, envelope.Meta.Server)
	assert.Equal(t, sortedStrings(remoteTargetHistoryLocalOnlyFields),
		sortedStrings(envelope.Meta.LocalOnly), "meta.local_only 必须点名缺的键")
	require.NotEmpty(t, envelope.Meta.LocalOnly,
		"meta.local_only 为空说明这一侧根本没申报缺口，等价断言会退化成静默少字段")

	// The local path keeps meta nil, so a consumer cannot confuse "no server
	// told me anything" with "the server told me this subset".
	localOut, _, err := runSplit(t,
		historyArgs([]string{"--json"}, []string{"--config", e.cfgPath})...)
	require.NoError(t, err)
	var localEnvelope struct {
		Meta *map[string]any `json:"meta"`
	}
	require.NoError(t, json.Unmarshal([]byte(localOut), &localEnvelope))
	assert.Nil(t, localEnvelope.Meta, "本地 --json 的 meta 必须仍是 null")
}

// TestTargetHistoryRemoteHonoursLimit: --limit has to reach the server. Without
// this, a mapping that forgot the field would still pass every equality test
// above, because both modes happen to return the same two fixture rows.
func TestTargetHistoryRemoteHonoursLimit(t *testing.T) {
	defer resetRootFlags()
	e, addr := seedHistoryEnv(t, "db-01")

	localOut, _, err := runSplit(t,
		historyArgs([]string{"--json"}, []string{"--config", e.cfgPath})...)
	require.NoError(t, err)
	require.Len(t, historyRows(t, localOut), 2,
		"夹具的默认 --limit 必须容得下两行，否则下面这条只是在测 limit 本身")

	out, _, err := runSplit(t,
		historyArgs([]string{"--limit", "1", "--json"},
			[]string{"--config", e.cfgPath, "--remote", "--server", addr})...)
	require.NoError(t, err)
	assert.Len(t, historyRows(t, out), 1, "--limit 必须真的穿到线上并被服务端遵守")
}

// recordingHistoryServer answers TargetHistory with nothing but records the
// limit it was handed, so the int32 clamp is asserted on the wire rather than
// inferred from the code.
type recordingHistoryServer struct {
	pb.UnimplementedInventoryServiceServer
	mu   sync.Mutex
	got  int32
	hits int
}

func (s *recordingHistoryServer) TargetHistory(
	_ context.Context,
	req *pb.TargetHistoryRequest,
) (*pb.TargetHistoryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = req.GetLimit()
	s.hits++
	return &pb.TargetHistoryResponse{}, nil
}

func (s *recordingHistoryServer) seen() (int32, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got, s.hits
}

// TestTargetHistoryRemoteClampsLimitToInt32: --limit is an int while the wire
// field is int32. Unclamped, 3000000000 truncates to -1294967296 and the server
// then reformats that as its own default of 20 — the operator's number and the
// executed number stop being the same thing.
func TestTargetHistoryRemoteClampsLimitToInt32(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)

	rec := &recordingHistoryServer{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpcserver.NewServer()
	pb.RegisterInventoryServiceServer(srv, rec)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	_, _, err = runSplit(t,
		historyArgs([]string{"--limit", "3000000000"},
			[]string{"--config", e.cfgPath, "--remote", "--server", lis.Addr().String()})...)
	require.NoError(t, err)

	got, hits := rec.seen()
	require.Equal(t, 1, hits, "请求没有到达 server，这条断言在空转")
	assert.EqualValues(t, math.MaxInt32, got,
		"--limit 超过 int32 时必须按上限请求，而不是把操作员输入的数字静默换成另一个")
}

func TestTargetHistoryRemoteDialsTheServer(t *testing.T) {
	defer resetRootFlags()
	e, _ := seedHistoryEnv(t, "db-01")

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dead := lis.Addr().String()
	require.NoError(t, lis.Close())

	stdout, _, err := runSplit(t,
		historyArgs(nil, []string{"--config", e.cfgPath, "--remote", "--server", dead})...)
	require.Error(t, err, "打不通 server 时必须报错，而不是静默读本地库")
	assert.Empty(t, strings.TrimSpace(stdout))
}

func TestTargetHistoryRemoteCreatesNoLocalDatabase(t *testing.T) {
	defer resetRootFlags()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "levee.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte("database:\n  driver: sqlite\n  path: "+filepath.ToSlash(dbPath)+"\n"), 0o600))

	addr := startGroupTestServer(t, newTestStore(t))

	_, _, err := runSplit(t,
		historyArgs(nil, []string{"--config", cfgPath, "--remote", "--server", addr})...)
	require.NoError(t, err)
	assert.NoFileExists(t, dbPath, "--remote target history 不该打开（或创建）本地库")
}

// historyRows returns the data array of a --json envelope as raw per-key values.
func historyRows(t *testing.T, out string) []map[string]json.RawMessage {
	t.Helper()
	var envelope struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &envelope), "输出不是一个 --json 信封: %s", out)
	return envelope.Data
}

func keySet(rows []map[string]json.RawMessage) map[string]bool {
	set := map[string]bool{}
	for _, r := range rows {
		for k := range r {
			set[k] = true
		}
	}
	return set
}

func sortedStrings(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}
