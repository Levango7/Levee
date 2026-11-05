// group_remote_test.go covers one more service_factory wiring: `group list
// --remote`.
//
// Equivalence here is byte-for-byte, because the command has exactly one
// renderer and both modes hand it []*state.InventoryGroup. That is only worth
// asserting if the fixture carries the columns a lossy mapping would drop, so
// every group below has a deliberately non-second-aligned created_at and one of
// them carries a tenant — the two fields state.InventoryGroup publishes through
// `--json` (created_at has no omitempty, tenant_id does) and the two the wire
// did not carry before this change.
//
// The server here is bespoke rather than startTestServer: that helper does not
// register InventoryService, and registering it inside the shared helper would
// change what every other remote test is talking to.
package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// startGroupTestServer runs a server that answers InventoryService on the same
// store the local path reads, so the two modes are comparing one dataset.
func startGroupTestServer(t *testing.T, store state.Store) string {
	t.Helper()

	srv := grpc.NewServer(store, grpc.WithListenAddr(":0"))
	pb.RegisterInventoryServiceServer(srv.GrpcServer(), grpc.NewInventoryService(store))

	go func() { _ = srv.Start(":0") }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if addr := srv.Addr(); addr != "" {
			t.Cleanup(func() { _ = srv.Stop() })
			return addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("group test server did not bind within 2 seconds")
	return ""
}

// seedGroupEnv writes the fixture and returns the env plus a listening address
// serving that same database.
func seedGroupEnv(t *testing.T) (*cliEnv, string) {
	t.Helper()

	e := newCLIEnv(t)
	store := e.open(t)

	// Fixed, deliberately awkward instants: odd nanoseconds, and not time.Now()
	// so both modes are compared against the same bytes on every run. A
	// truncate-to-second fixture would make the created_at columns untestable —
	// the comparison would pass with a seconds-only mapping.
	type row struct {
		id, name, parent string
		created          time.Time
		tenant           string
	}
	rows := []row{
		{"grp-a", "prod/db", "", time.Date(2024, 3, 17, 8, 5, 3, 123456789, time.UTC), ""},
		{"grp-b", "prod/db/primary", "grp-a", time.Date(2024, 3, 18, 9, 6, 4, 987654321, time.UTC), "t-a"},
	}
	for _, r := range rows {
		require.NoError(t, store.UpsertInventoryGroup(context.Background(), &state.InventoryGroup{
			ID: r.id, Name: r.name, ParentID: r.parent, CreatedAt: r.created, TenantID: r.tenant,
		}))
	}

	return e, startGroupTestServer(t, e.open(t))
}

func groupArgs(extra []string, remote []string) []string {
	args := append([]string{}, remote...)
	args = append(args, "group", "list")
	return append(args, extra...)
}

// TestGroupListRemoteMatchesLocal: both output shapes, identical bytes.
func TestGroupListRemoteMatchesLocal(t *testing.T) {
	defer resetRootFlags()
	e, addr := seedGroupEnv(t)

	remote := []string{"--config", e.cfgPath, "--remote", "--server", addr}
	local := []string{"--config", e.cfgPath}

	for name, extra := range map[string][]string{
		"human": nil,
		"json":  {"--json"},
	} {
		t.Run(name, func(t *testing.T) {
			out, _, err := runSplit(t, groupArgs(extra, local)...)
			require.NoError(t, err)
			// Premise first: an empty comparison proves nothing, and for --json
			// it must show the two columns a partial mapping would lose.
			assert.Contains(t, out, "prod/db", "本地输出没有内容，等价断言就空转了")
			if name == "json" {
				assert.Contains(t, out, ".123456789", "夹具必须真的带亚秒，否则这条比较只是在测自己")
				assert.Contains(t, out, "t-a", "夹具必须真的带租户，否则 tenant_id 的映射没人查")
			}

			remoteOut, _, err := runSplit(t, groupArgs(extra, remote)...)
			require.NoError(t, err)

			assert.Equal(t, strings.TrimSpace(out), strings.TrimSpace(remoteOut),
				"%s 形状在两种模式下必须逐字节相同", name)
		})
	}
}

// TestGroupListRemoteEmptyMatchesLocal pins the empty answer: no groups must
// read the same in both modes rather than becoming an error on one side.
func TestGroupListRemoteEmptyMatchesLocal(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	addr := startGroupTestServer(t, e.open(t))

	out, _, err := runSplit(t, "--config", e.cfgPath, "group", "list")
	require.NoError(t, err)
	remoteOut, _, err := runSplit(t, "--config", e.cfgPath, "--remote", "--server", addr, "group", "list")
	require.NoError(t, err)

	assert.Contains(t, remoteOut, "No groups.", "空表要照本地那句话报，不能只报「两边一样」")
	assert.Equal(t, strings.TrimSpace(out), strings.TrimSpace(remoteOut))
}

// TestGroupListRemoteDialsTheServer is the branch control: the equivalence
// cases above read the same seeded file on both sides, so only a dead endpoint
// can tell "asked the server" from "quietly fell back to local".
func TestGroupListRemoteDialsTheServer(t *testing.T) {
	defer resetRootFlags()
	e, _ := seedGroupEnv(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dead := lis.Addr().String()
	require.NoError(t, lis.Close())

	stdout, _, err := runSplit(t, "--config", e.cfgPath, "--remote", "--server", dead, "group", "list")
	require.Error(t, err, "打不通 server 时必须报错，而不是静默读本地库")
	assert.Empty(t, strings.TrimSpace(stdout))
}

// TestGroupListRemoteCreatesNoLocalDatabase: asking a server for groups must not
// create (or open) the local data directory.
func TestGroupListRemoteCreatesNoLocalDatabase(t *testing.T) {
	defer resetRootFlags()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "levee.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte("database:\n  driver: sqlite\n  path: "+filepath.ToSlash(dbPath)+"\n"), 0o600))

	addr := startGroupTestServer(t, newTestStore(t))

	_, _, err := runSplit(t, "--config", cfgPath, "--remote", "--server", addr, "group", "list")
	require.NoError(t, err)
	assert.NoFileExists(t, dbPath, "--remote group list 不该打开（或创建）本地库")
}

// TestGroupListRemoteCarriesTheStoredRow names the two lossy-mapping traps
// directly, by parsing the envelope: `PrintJSON` indents and escapes, and the
// byte-equality above would also hold if BOTH modes lost the same thing.
func TestGroupListRemoteCarriesTheStoredRow(t *testing.T) {
	defer resetRootFlags()
	e, addr := seedGroupEnv(t)

	out, _, err := runSplit(t, "--config", e.cfgPath, "--remote", "--server", addr, "group", "list", "--json")
	require.NoError(t, err)

	var envelope struct {
		Data []state.InventoryGroup `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &envelope))
	require.Len(t, envelope.Data, 2)

	var primary *state.InventoryGroup
	for i := range envelope.Data {
		if envelope.Data[i].ID == "grp-b" {
			primary = &envelope.Data[i]
		}
	}
	require.NotNil(t, primary, "远程结果里没有 grp-b：%s", out)

	assert.Equal(t, "prod/db/primary", primary.Name)
	assert.Equal(t, "grp-a", primary.ParentID)
	assert.Equal(t, "t-a", primary.TenantID, "租户列也要上线，否则 --json 少一个键")
	assert.Equal(t, 987654321, primary.CreatedAt.Nanosecond(),
		"亚秒余数必须活着穿过线上，否则 --json 的等价只是两边都丢了它")
	assert.Equal(t, time.Date(2024, 3, 18, 9, 6, 4, 0, time.UTC), primary.CreatedAt.Truncate(time.Second))
}

// TestInventoryAdapterMirrorsTheWholeRPCSet: the adapter interface's own comment
// claims it mirrors the service surface, so assert that instead of trusting it.
// A method added to InventoryService later would otherwise leave one mode
// silently unable to call it — which is exactly how a command gets written
// against one mode only.
func TestInventoryAdapterMirrorsTheWholeRPCSet(t *testing.T) {
	adapter := reflect.TypeOf((*inventoryServiceAdapter)(nil)).Elem()
	service := reflect.TypeOf((*pb.InventoryServiceServer)(nil)).Elem()

	// Compile-time proof that both concrete types satisfy what the factory
	// hands out; without these the reflection below could pass on an interface
	// no implementation actually fills.
	var _ inventoryServiceAdapter = (*localInventoryAdapter)(nil)
	var _ inventoryServiceAdapter = (*remoteInventoryAdapter)(nil)

	assert.GreaterOrEqual(t, service.NumMethod(), 6,
		"底数不能太小：InventoryService 方法数变了，这条断言的射程也变了")

	checked := 0
	for i := 0; i < service.NumMethod(); i++ {
		m := service.Method(i)
		if !m.IsExported() {
			continue // mustEmbedUnimplemented… is not part of the callable surface
		}
		checked++
		if _, ok := adapter.MethodByName(m.Name); !ok {
			t.Errorf("inventoryServiceAdapter 少了 %s：InventoryService 加了 RPC 却没镜像", m.Name)
		}
	}
	assert.GreaterOrEqual(t, checked, 6, "只比到 %d 个方法，覆盖不足", checked)
}
