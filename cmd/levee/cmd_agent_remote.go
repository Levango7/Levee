package main

// cmd_agent_remote.go — the remote half of `levee agent ...`.
//
// Why this exists: the registry was a map inside whichever process constructed
// it, and every CLI read (list / show / remove) read that same local map. So
// against a running master the commands reported an empty table and
// "not found" — not because nothing was registered, but because the answer came
// from the wrong process. In --remote mode the answer now comes from the master.
//
// Why there is no remote `agent start`: the master has no agent task transport
// (no stream RPC in the proto, no agent-side dispatcher), so switching the
// lifecycle to gRPC would leave an agent that registers, then waits forever on a
// task channel that does not exist. `agent start` therefore stays in-process
// until that transport is a decided feature; this file only moves the READ side
// — list / show / remove — to the process that actually owns the registry.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/agent"
	"github.com/nexus/levee/internal/grpc/pb"
)

// dialAgentRegistry connects to the master at --server (bearer token from
// --token when one is configured) and returns the registry client plus the
// function that releases the connection.
//
// It blocks on readiness on purpose: a lazy dial would surface as a confusing
// per-RPC deadline instead of one clear "cannot reach the master at X".
func dialAgentRegistry(ctx context.Context) (pb.AgentServiceClient, func() error, error) {
	client, err := newGRPCClient(optServer, optAPIToken)
	if err != nil {
		return nil, nil, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, defaultConnectTimeout)
	defer cancel()
	if err := client.waitForReady(readyCtx); err != nil {
		_ = client.close()
		return nil, nil, fmt.Errorf("cannot reach master at %s: %w", optServer, err)
	}
	return client.agent, client.close, nil
}

// recordsToInfos converts wire records back into the registry's own type so the
// existing renderers (human, --json, --quiet) stay a single code path for both
// modes. Divergent output between local and remote would otherwise be a new way
// for the same registry to print two different answers.
func recordsToInfos(recs []*pb.AgentRecord) []agent.AgentInfo {
	out := make([]agent.AgentInfo, 0, len(recs))
	for _, r := range recs {
		out = append(out, recordToInfo(r))
	}
	return out
}

func recordToInfo(r *pb.AgentRecord) agent.AgentInfo {
	info := agent.AgentInfo{
		ID:             r.GetId(),
		Address:        r.GetAddress(),
		Capabilities:   r.GetCapabilities(),
		Status:         agent.AgentStatus(r.GetStatus()),
		ActiveTasks:    int(r.GetActiveTasks()),
		CompletedTasks: r.GetCompletedTasks(),
		FailedTasks:    r.GetFailedTasks(),
		MaxConcurrent:  int(r.GetMaxConcurrent()),
	}
	if ts := r.GetLastHeartbeatUnix(); ts > 0 {
		info.LastHeartbeat = time.Unix(ts, 0)
	}
	return info
}

// fetchRegistryAgents returns the registry view for the current mode: the
// master's answer in --remote mode, the process-local registry otherwise.
//
// The status filter is applied HERE rather than server-side so that a bad filter
// value produces the same error text in both modes. A filter that silently
// matched nothing on the server would be indistinguishable from "no agents
// registered", which is the very ambiguity this feature exists to remove.
func fetchRegistryAgents(ctx context.Context, statusFilter string) ([]agent.AgentInfo, error) {
	if !optRemote {
		return filterAgentsByStatus(getGlobalAgentRegistry().List(), statusFilter)
	}
	client, closeFn, err := dialAgentRegistry(ctx)
	if err != nil {
		return nil, err
	}
	defer closeFn()

	resp, err := client.ListAgents(ctx, &pb.ListAgentsRequest{})
	if err != nil {
		return nil, err
	}
	return filterAgentsByStatus(recordsToInfos(resp.GetAgents()), statusFilter)
}

// lookupRegistryAgent resolves one agent id in the current mode.
func lookupRegistryAgent(ctx context.Context, id string) (agent.AgentInfo, error) {
	if !optRemote {
		return getGlobalAgentRegistry().Get(id)
	}
	client, closeFn, err := dialAgentRegistry(ctx)
	if err != nil {
		return agent.AgentInfo{}, err
	}
	defer closeFn()

	rec, err := client.GetAgent(ctx, &pb.GetAgentRequest{Id: id})
	if err != nil {
		return agent.AgentInfo{}, err
	}
	return recordToInfo(rec), nil
}

// removeRegistryAgent deletes one record in the current mode. In remote mode the
// master's refusal (in-flight tasks, nothing to remove) is surfaced as an error
// so scripts see a failed command instead of a printed "Removed".
func removeRegistryAgent(ctx context.Context, id string, force bool) error {
	if !optRemote {
		return getGlobalAgentRegistry().Deregister(id)
	}
	client, closeFn, err := dialAgentRegistry(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	resp, err := client.RemoveAgent(ctx, &pb.RemoveAgentRequest{Id: id, Force: force})
	if err != nil {
		return err
	}
	if !resp.GetRemoved() {
		return errors.New(resp.GetRefusal())
	}
	return nil
}
