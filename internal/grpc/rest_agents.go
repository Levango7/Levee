// rest_agents.go exposes the agent registry's operator-facing RPCs as
// RESTful routes.
//
// Why this file exists: the AgentService was complete at the gRPC layer
// (registry persisted in schema v8, policy-gated, dual-process E2E) but had no
// REST surface, so the console had no agent view at all — cluster-status shows
// cluster_nodes, and the agents table (#157's identifier space) was invisible
// on the web.
//
// Routes (all under /api/v1):
//
//	GET    /agents            → ListAgents (capability/status filters pass through)
//	GET    /agents/{id}       → GetAgent
//	DELETE /agents/{id}       → RemoveAgent (the refusal is a reply FIELD, not a
//	                            status: it surfaces as 200 with removed=false
//	                            plus the explanation, the same contract the CLI
//	                            prints — the operator chooses --force, which is
//	                            NOT a browser's decision to make on a 409)
//
// The three agent-protocol writes (Register/Heartbeat/Deregister) stay gRPC
// only: an agent speaks gRPC with its credential, and putting registration on
// a browser-reachable path would widen the trust surface for nothing (see
// rpcExemptFromREST in the exposure matrix).
package grpc

import (
	"net/http"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/nexus/levee/internal/grpc/pb"
)

// SetAgents registers the in-process AgentService with the gateway.
func (gw *Gateway) SetAgents(a pb.AgentServiceServer) { gw.agents = a }

// dispatchAgents routes the /agents tree. It is registered in
// restResourceHandlers under "agents".
func (gw *Gateway) dispatchAgents(w http.ResponseWriter, r *http.Request, method, path string) {
	svc := gw.agents
	if svc == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "AgentService not configured")
		return
	}
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))

	switch {
	case path == "/agents" && method == http.MethodGet:
		req := &pb.ListAgentsRequest{}
		q := r.URL.Query()
		req.Capability = q.Get("capability")
		req.Status = q.Get("status")
		resp, err := svc.ListAgents(ctx, req)
		if err != nil {
			writeGRPCError(w, err)
			return
		}
		writeJSON(w, resp.GetAgents())

	default:
		parts := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(path, "/agents"), "/"), "/", 2)
		if len(parts) == 0 || parts[0] == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid agents path")
			return
		}
		id := parts[0]
		switch {
		case len(parts) == 1 && method == http.MethodGet:
			resp, err := svc.GetAgent(ctx, &pb.GetAgentRequest{Id: id})
			if err != nil {
				writeGRPCError(w, err)
				return
			}
			writeJSON(w, resp)
		case len(parts) == 1 && method == http.MethodDelete:
			// --force is a CLI flag, not a query parameter: an operator who
			// needs it has the CLI, and a browser URL that deletes anyway is a
			// footgun the registry's refusal design exists to avoid.
			resp, err := svc.RemoveAgent(ctx, &pb.RemoveAgentRequest{Id: id})
			if err != nil {
				writeGRPCError(w, err)
				return
			}
			// The refusal is a FIELD: 200 with removed=false plus the reason,
			// the same contract the CLI prints.
			writeJSON(w, resp)
		default:
			writeJSONError(w, http.StatusBadRequest, "unknown agents sub-path: /"+path)
		}
	}
}
