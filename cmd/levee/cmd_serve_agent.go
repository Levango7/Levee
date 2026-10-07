package main

// cmd_serve_agent.go — the serve-side seam for the agent-registry RPC.
//
// Kept out of cmd_serve.go so the registration block reads as one decision:
// "the daemon serves the registry from the store it owns, or says it cannot".
//
// Why nil is a valid answer here: RegisterExtraServices replaces a nil service
// with the generated Unimplemented stub, so an unsupported store surfaces as
// codes.Unimplemented on the wire. The alternative — registering a service over
// no store — would answer `agent list` with an empty table, which is precisely
// the lie this feature was built to remove.

import (
	"log/slog"

	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// serveAgentRegistry builds the AgentService when the (already unwrapped) store
// can persist agents; otherwise it warns and returns nil. The authorizer is
// passed through exactly as the other served services receive it — nil is the
// documented "no policy configured" case, not a bypass.
func serveAgentRegistry(log *slog.Logger, store state.Store, authorizer *authz.Authorizer) pb.AgentServiceServer {
	agentStore, ok := store.(state.AgentStore)
	if !ok {
		log.Warn("serve: agent registry RPC unavailable on this store; " +
			"`levee agent list --remote` will report Unimplemented rather than an empty registry")
		return nil
	}
	return grpc.NewAgentService(agentStore).WithAuthorizer(authorizer)
}
