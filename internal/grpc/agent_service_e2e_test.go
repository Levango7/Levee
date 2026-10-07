package grpc

// agent_service_e2e_test.go — the acceptance test for the whole feature: an
// agent registration made through the RPC must be readable by a DIFFERENT
// PROCESS.
//
// Why a real subprocess and not a second store handle: the bug this feature
// fixes was "the registry is a map inside one process", and the failure it
// produced was `levee agent list` printing an empty table while agents were
// live. A second connection in the same test process would exercise the
// database but not the process boundary, and the boundary is the claim. So the
// test re-executes its own binary: the child opens the same SQLite file with
// its own handle and reports what it sees on stdout; the parent asserts on it.
//
// The child is a fresh process: nothing is shared with the parent except the
// file on disk — which is exactly the transport `levee agent list --server`
// relies on in production.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ggrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// agentE2EReadDBEnv turns a re-executed copy of this test binary into the
// reader process: when it is set, the test reads the named database and
// prints the result instead of running the scenario.
const agentE2EReadDBEnv = "LEVEE_AGENT_E2E_READ_DB"

// e2eJSONMarker prefixes the one result line the child prints, so the parent
// can extract it from the test framework's own output.
const e2eJSONMarker = "LEVEE_E2E_JSON "

type e2eReadResult struct {
	Err    string         `json:"err,omitempty"`
	Agents []e2eReadAgent `json:"agents"`
}

type e2eReadAgent struct {
	ID            string   `json:"id"`
	Address       string   `json:"address"`
	Status        string   `json:"status"`
	Capabilities  []string `json:"capabilities"`
	MaxConcurrent int      `json:"max_concurrent"`
	RegisteredAt  int64    `json:"registered_at_unix"`
	LastHeartbeat int64    `json:"last_heartbeat_unix"`
}

func TestAgentService_RegistryCrossesTheProcessBoundary(t *testing.T) {
	// Child branch: this binary was re-executed with a database to read.
	if dbPath := os.Getenv(agentE2EReadDBEnv); dbPath != "" {
		runAgentE2EReader(dbPath)
		return
	}

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "levee.db")

	store1, err := state.NewSQLiteStore(ctx, dbPath)
	require.NoError(t, err)
	closeStore1 := sync.OnceFunc(func() { _ = store1.Close() })
	t.Cleanup(closeStore1)

	// The daemon under test: a real gRPC server over TCP (not bufconn) with the
	// real service over the real store. Auth interceptors are deliberately not
	// stacked here — that this service is never auth-exempt is pinned in
	// agent_service_test.go, and the daemon's full chain is what smoke_serve.sh
	// exercises against the built binary.
	daemon1 := startAgentDaemon(t, store1)

	// 1. Register through the RPC. This is the write half.
	rec, err := daemon1.client.RegisterAgent(ctx, &pb.RegisterAgentRequest{
		Id:            "e2e-agent-1",
		Address:       "127.0.0.1:9101",
		Capabilities:  []string{"shell", "file"},
		MaxConcurrent: 4,
	})
	require.NoError(t, err)
	require.Equal(t, "e2e-agent-1", rec.GetId())
	require.Equal(t, "registered", rec.GetStatus(),
		"a fresh registration lands as registered until the first heartbeat")
	require.Greater(t, rec.GetRegisteredAtUnix(), int64(0))

	// A heartbeat, so the child also has to observe a heartbeat-written column:
	// 4 of 4 slots in use derives "busy".
	hb, err := daemon1.client.AgentHeartbeat(ctx, &pb.AgentHeartbeatRequest{
		Id:          "e2e-agent-1",
		ActiveTasks: 4,
	})
	require.NoError(t, err)
	require.Equal(t, "busy", hb.GetStatus())

	// 2. The read half, from a different OS process.
	seen := readRegistryFromChildProcess(t, dbPath)
	require.Empty(t, seen.Err, "the reader process must not error")
	require.Len(t, seen.Agents, 1,
		"the process that did not register the agent must still see it — this is the bug being fixed")
	child := seen.Agents[0]
	assert.Equal(t, "e2e-agent-1", child.ID)
	assert.Equal(t, "127.0.0.1:9101", child.Address)
	assert.Equal(t, "busy", child.Status)
	assert.Equal(t, []string{"shell", "file"}, child.Capabilities)
	assert.Equal(t, 4, child.MaxConcurrent)
	assert.Equal(t, rec.GetRegisteredAtUnix(), child.RegisteredAt,
		"the registration timestamp must survive the process boundary unchanged")
	assert.Greater(t, child.LastHeartbeat, int64(0),
		"the heartbeat written through the RPC must be visible to the reader process")

	// 3. Daemon restart: stop server and store, reopen everything on the same
	//    file. The registry — and the agent's age — must come back.
	daemon1.stop()
	closeStore1()

	store2, err := state.NewSQLiteStore(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store2.Close() })
	daemon2 := startAgentDaemon(t, store2)

	rec2, err := daemon2.client.GetAgent(ctx, &pb.GetAgentRequest{Id: "e2e-agent-1"})
	require.NoError(t, err)
	assert.Equal(t, rec.GetRegisteredAtUnix(), rec2.GetRegisteredAtUnix(),
		"a restarted daemon must keep the original registration age")
	assert.Equal(t, "busy", rec2.GetStatus())

	// 4. Removal through the RPC must be visible across the boundary too —
	//    a child process reading AFTER the delete sees an empty registry, which
	//    is what makes the earlier non-empty read meaningful rather than a
	//    fixture accident.
	resp, err := daemon2.client.RemoveAgent(ctx, &pb.RemoveAgentRequest{Id: "e2e-agent-1", Force: true})
	require.NoError(t, err)
	require.True(t, resp.GetRemoved(), "refusal: %s", resp.GetRefusal())

	after := readRegistryFromChildProcess(t, dbPath)
	require.Empty(t, after.Err)
	assert.Empty(t, after.Agents, "the removal must be what the other process sees")
}

// agentDaemon is a live server+client pair; stop is idempotent so the restart
// leg and the test cleanup can both call it.
type agentDaemon struct {
	lis    net.Listener
	srv    *ggrpc.Server
	conn   *ggrpc.ClientConn
	client pb.AgentServiceClient
	once   sync.Once
}

func (d *agentDaemon) stop() {
	d.once.Do(func() {
		_ = d.conn.Close()
		d.srv.Stop()
		_ = d.lis.Close()
	})
}

func startAgentDaemon(t *testing.T, store state.AgentStore) *agentDaemon {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := ggrpc.NewServer()
	pb.RegisterAgentServiceServer(srv, NewAgentService(store))
	go func() { _ = srv.Serve(lis) }()

	conn, err := ggrpc.NewClient(lis.Addr().String(),
		ggrpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	d := &agentDaemon{
		lis:    lis,
		srv:    srv,
		conn:   conn,
		client: pb.NewAgentServiceClient(conn),
	}
	t.Cleanup(d.stop)
	return d
}

// readRegistryFromChildProcess re-executes this test binary in reader mode and
// returns what it saw. Every failure mode (crash, missing marker, bad JSON)
// reports the child's full output, because "the child said nothing" is the
// least diagnosable shape this test could fail in.
func readRegistryFromChildProcess(t *testing.T, dbPath string) e2eReadResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), agentE2EReadDBEnv+"="+dbPath)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "reader process failed; output:\n%s", out)

	text := string(out)
	i := strings.Index(text, e2eJSONMarker)
	require.GreaterOrEqual(t, i, 0, "reader process did not print its result; output:\n%s", text)
	line := text[i+len(e2eJSONMarker):]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}

	var res e2eReadResult
	require.NoError(t, json.Unmarshal([]byte(line), &res), "reader result %q", line)
	return res
}

// runAgentE2EReader is the child-process body: open the database it was handed
// and report the registry as JSON.
func runAgentE2EReader(dbPath string) {
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		printE2EResult(e2eReadResult{Err: err.Error()})
		return
	}
	defer func() { _ = store.Close() }()

	rows, err := store.ListAgents(ctx)
	if err != nil {
		printE2EResult(e2eReadResult{Err: err.Error()})
		return
	}
	res := e2eReadResult{Agents: make([]e2eReadAgent, 0, len(rows))}
	for _, a := range rows {
		rec := e2eReadAgent{
			ID:            a.ID,
			Address:       a.Address,
			Status:        a.Status,
			Capabilities:  a.Capabilities,
			MaxConcurrent: a.MaxConcurrent,
		}
		if !a.RegisteredAt.IsZero() {
			rec.RegisteredAt = a.RegisteredAt.Unix()
		}
		if !a.LastHeartbeat.IsZero() {
			rec.LastHeartbeat = a.LastHeartbeat.Unix()
		}
		res.Agents = append(res.Agents, rec)
	}
	printE2EResult(res)
}

func printE2EResult(res e2eReadResult) {
	b, err := json.Marshal(res)
	if err != nil {
		// Unreachable for this struct; the fallback keeps the child from
		// exiting with no marker at all.
		b = []byte(`{"err":"marshal failed"}`)
	}
	os.Stdout.WriteString(e2eJSONMarker + string(b) + "\n")
}
