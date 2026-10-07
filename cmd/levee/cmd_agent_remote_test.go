package main

// cmd_agent_remote_test.go — the exact path `levee agent list|show|remove
// --remote` takes, against a daemon-shaped server.
//
// The wire contract is covered in internal/grpc (unit + cross-process E2E), and
// the five original services' factory is covered in grpc_client_test.go. What
// is only covered here is the CLI's own half: fetchRegistryAgents /
// lookupRegistryAgent / removeRegistryAgent, driven through the same server
// composition `levee serve` builds (NewServer + RegisterExtraServices with
// serveAgentRegistry), with the global flags set the way --remote sets them.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/state"
)

// startAgentTestDaemon composes a server the way cmd_serve.go does — the real
// server over the store, with the agent service built by the same
// serveAgentRegistry seam — so breaking the serve wiring reddens this test
// instead of only the smoke script.
func startAgentTestDaemon(t *testing.T, store state.Store) string {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	agentSvc := serveAgentRegistry(log, state.Underlying(store), nil)
	require.NotNil(t, agentSvc, "the CLI test store must be able to serve the agent registry")

	srv := grpc.NewServer(store, grpc.WithListenAddr(":0"))
	grpc.RegisterExtraServices(srv.GrpcServer(), grpc.ExtraServicesConfig{Agent: agentSvc})

	go func() { _ = srv.Start(":0") }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if addr := srv.Addr(); addr != "" {
			t.Cleanup(func() { _ = srv.Stop() })
			return addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("agent test server did not bind within 2 seconds")
	return ""
}

func TestAgentRemote_ListShowRemove(t *testing.T) {
	defer resetRootFlags()

	store := newTestStore(t)
	addr := startAgentTestDaemon(t, store)

	// Seed the registry the way the daemon would hold it: one idle record and
	// one that reports its slots full.
	agentStore, ok := state.Underlying(store).(state.AgentStore)
	require.True(t, ok)
	ctx := context.Background()
	require.NoError(t, agentStore.UpsertAgent(ctx, &state.Agent{
		ID: "cli-idle", Address: "127.0.0.1:9101",
		Capabilities: []string{"shell"}, Status: "idle", MaxConcurrent: 4,
	}))
	require.NoError(t, agentStore.UpsertAgent(ctx, &state.Agent{
		ID: "cli-busy", Address: "127.0.0.1:9102",
		Capabilities: []string{"file"}, Status: "busy",
		ActiveTasks: 4, MaxConcurrent: 4,
	}))

	optRemote, optServer, optAPIToken = true, addr, ""

	// list: the daemon's answer, ordered by id, not an empty table.
	agents, err := fetchRegistryAgents(ctx, "")
	require.NoError(t, err)
	require.Len(t, agents, 2)
	assert.Equal(t, "cli-busy", agents[0].ID)
	assert.Equal(t, "cli-idle", agents[1].ID)

	// The status filter applies on the CLI side in remote mode too, and an
	// unknown filter value is an error rather than a silent empty result.
	busyOnly, err := fetchRegistryAgents(ctx, "busy")
	require.NoError(t, err)
	require.Len(t, busyOnly, 1)
	assert.Equal(t, "cli-busy", busyOnly[0].ID)

	_, err = fetchRegistryAgents(ctx, "no-such-status")
	require.Error(t, err)

	// show: one record, and a missing id is NotFound — the same answer the
	// master would give, not a transport error.
	info, err := lookupRegistryAgent(ctx, "cli-idle")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9101", info.Address)
	assert.Equal(t, "idle", string(info.Status))
	assert.Equal(t, []string{"shell"}, info.Capabilities)

	_, err = lookupRegistryAgent(ctx, "ghost")
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))

	// remove: refused while tasks are in flight ...
	err = removeRegistryAgent(ctx, "cli-busy", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in-flight",
		"the master's refusal must reach the operator as the command's error")

	// ... allowed with force, and then gone from the daemon's list.
	require.NoError(t, removeRegistryAgent(ctx, "cli-busy", true))
	after, err := fetchRegistryAgents(ctx, "")
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, "cli-idle", after[0].ID)
}
