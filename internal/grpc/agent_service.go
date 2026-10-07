// agent_service.go — gRPC AgentService: the master-side agent registry, backed
// by the persistent store.
//
// Why this exists: the registry used to be a map inside whichever process
// constructed it (cmd/levee/cmd_agent_support.go's process-global singleton),
// so an agent that registered against the daemon was invisible to any other
// process — `levee agent list` printed an empty table and `agent show` answered
// not-found even while agents were live. Serving the persisted store makes the
// answer independent of who asks.
//
// Two rules this file holds on purpose:
//
//   - Nothing here widens the auth surface. Every method runs behind the
//     standard interceptor chain with an authenticated subject; registration is
//     a write to operator-visible state and is treated as such.
//   - Status vocabulary is validated, never passed through. The registry can
//     produce exactly four states (agent.AgentStatusValues); accepting an
//     arbitrary string would let a typo park an agent in a state that reads
//     healthy forever, and a filter on a nonsense status would look identical
//     to "no agents registered".

package grpc

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/agent"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// agentStaleWindow is how long a registration survives without a heartbeat.
// It is DERIVED from the two constants the agent side already uses to decide
// when to send, so client and server cannot drift apart on the number that
// defines "gone quiet".
const agentStaleWindow = agent.DefaultHeartbeatInterval * agent.HeartbeatMissThreshold

// AgentService serves the agent-registry RPCs on the persistent store.
type AgentService struct {
	pb.UnimplementedAgentServiceServer

	store state.AgentStore
	// now is a seam so the staleness arithmetic is testable without sleeping.
	now func() time.Time
}

// NewAgentService builds the service on top of store. A nil store is accepted
// at construction (so callers can wire conditionally) and answered at call time
// with codes.Unimplemented — a missing registry must never read as an
// empty-but-working one.
func NewAgentService(store state.AgentStore) *AgentService {
	return &AgentService{store: store, now: time.Now}
}

var _ pb.AgentServiceServer = (*AgentService)(nil)

// RegisterAgent creates or refreshes an agent record.
func (s *AgentService) RegisterAgent(ctx context.Context, req *pb.RegisterAgentRequest) (*pb.AgentRecord, error) {
	if s.store == nil {
		return nil, status.Error(codes.Unimplemented, "agent registry is not configured on this server")
	}
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "agent id is required")
	}
	addr := strings.TrimSpace(req.GetAddress())
	if err := validateAgentAddress(addr); err != nil {
		return nil, err
	}
	caps, err := normalizeAgentCapabilities(req.GetCapabilities())
	if err != nil {
		return nil, err
	}
	if req.GetMaxConcurrent() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "agent %q: max_concurrent must not be negative", id)
	}

	rec := &state.Agent{
		ID:           id,
		Address:      addr,
		Capabilities: caps,
		// "registered" until the first heartbeat proves liveness: a record that
		// has never been seen must not look idle and get work assigned.
		Status:        string(agent.StatusRegistered),
		MaxConcurrent: int(req.GetMaxConcurrent()),
	}
	// RegisteredAt is set here but the store keeps the ORIGINAL value when the
	// id already exists, so re-registering does not reset the agent's age.
	rec.RegisteredAt = s.now()

	if err := s.store.UpsertAgent(ctx, rec); err != nil {
		return nil, status.Errorf(codes.Internal, "register agent %q: %v", id, err)
	}
	stored, err := s.store.GetAgent(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read back agent %q: %v", id, err)
	}
	if stored == nil {
		// Reachable only if the write vanished; report it instead of returning
		// a fabricated record.
		return nil, status.Errorf(codes.Internal, "agent %q missing after register", id)
	}
	return agentRecordToPB(stored), nil
}

// AgentHeartbeat records liveness and the agent's own task counters.
func (s *AgentService) AgentHeartbeat(ctx context.Context, req *pb.AgentHeartbeatRequest) (*pb.AgentHeartbeatReply, error) {
	if s.store == nil {
		return nil, status.Error(codes.Unimplemented, "agent registry is not configured on this server")
	}
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "agent id is required")
	}
	if req.GetActiveTasks() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "agent %q: active_tasks must not be negative", id)
	}
	at := s.now()
	if ts := req.GetClientTimestampUnix(); ts > 0 {
		at = time.Unix(ts, 0)
	}

	cur, err := s.store.GetAgent(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "look up agent %q: %v", id, err)
	}
	if cur == nil {
		return nil, heartbeatUnknownAgent(id)
	}

	// The status is normally DERIVED here rather than trusted: the agent side's
	// own Heartbeat message carries no status field, and re-deriving the rule
	// (agent.DeriveStatus) keeps the persisted record consistent with what the
	// in-process registry would have computed for the same load. An explicit
	// status is still honoured, but validated against the same vocabulary.
	st := agent.DeriveStatus(int(req.GetActiveTasks()), cur.MaxConcurrent)
	if raw := strings.TrimSpace(req.GetStatus()); raw != "" {
		st, err = parseAgentStatus(raw)
		if err != nil {
			return nil, err
		}
	}

	known, err := s.store.TouchAgentHeartbeat(ctx, id, string(st), int(req.GetActiveTasks()),
		req.GetCompletedTasks(), req.GetFailedTasks(), at)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "heartbeat for agent %q: %v", id, err)
	}
	if !known {
		return nil, heartbeatUnknownAgent(id)
	}
	return &pb.AgentHeartbeatReply{
		Id:             id,
		Status:         string(st),
		StaleAfterUnix: at.Add(agentStaleWindow).Unix(),
	}, nil
}

// heartbeatUnknownAgent is the answer to a heartbeat for an agent nobody has:
// its registration is gone (removed, or the store was reset), so the caller
// must register again rather than keep beating against an id nothing holds.
func heartbeatUnknownAgent(id string) error {
	return status.Errorf(codes.NotFound, "agent %q is not registered; call RegisterAgent again", id)
}

// DeregisterAgent is the agent's own goodbye.
func (s *AgentService) DeregisterAgent(ctx context.Context, req *pb.DeregisterAgentRequest) (*pb.DeregisterAgentReply, error) {
	if s.store == nil {
		return nil, status.Error(codes.Unimplemented, "agent registry is not configured on this server")
	}
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "agent id is required")
	}
	removed, err := s.store.DeleteAgent(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "deregister agent %q: %v", id, err)
	}
	return &pb.DeregisterAgentReply{Removed: removed}, nil
}

// ListAgents returns the registry, optionally filtered by capability and status.
func (s *AgentService) ListAgents(ctx context.Context, req *pb.ListAgentsRequest) (*pb.ListAgentsReply, error) {
	if s.store == nil {
		return nil, status.Error(codes.Unimplemented, "agent registry is not configured on this server")
	}
	wantStatus := ""
	if raw := strings.TrimSpace(req.GetStatus()); raw != "" {
		st, err := parseAgentStatus(raw)
		if err != nil {
			return nil, err
		}
		wantStatus = string(st)
	}
	wantCap := strings.TrimSpace(req.GetCapability())

	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list agents: %v", err)
	}
	out := make([]*pb.AgentRecord, 0, len(agents))
	for _, a := range agents {
		if wantStatus != "" && a.Status != wantStatus {
			continue
		}
		if wantCap != "" && !agentHasCapability(a.Capabilities, wantCap) {
			continue
		}
		out = append(out, agentRecordToPB(a))
	}
	return &pb.ListAgentsReply{Agents: out}, nil
}

// GetAgent returns one record.
func (s *AgentService) GetAgent(ctx context.Context, req *pb.GetAgentRequest) (*pb.AgentRecord, error) {
	if s.store == nil {
		return nil, status.Error(codes.Unimplemented, "agent registry is not configured on this server")
	}
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "agent id is required")
	}
	a, err := s.store.GetAgent(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get agent %q: %v", id, err)
	}
	if a == nil {
		return nil, status.Errorf(codes.NotFound, "agent %q not found", id)
	}
	return agentRecordToPB(a), nil
}

// RemoveAgent is the operator-side deletion. It refuses while the record still
// reports in-flight tasks, because dropping it then would erase the only record
// of what is running where; the caller says `force` to accept that.
func (s *AgentService) RemoveAgent(ctx context.Context, req *pb.RemoveAgentRequest) (*pb.RemoveAgentReply, error) {
	if s.store == nil {
		return nil, status.Error(codes.Unimplemented, "agent registry is not configured on this server")
	}
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "agent id is required")
	}
	a, err := s.store.GetAgent(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "look up agent %q: %v", id, err)
	}
	if a == nil {
		// Refusal rather than an error: removing an absent agent twice is not a
		// second failure, and the reply says what happened.
		return &pb.RemoveAgentReply{Removed: false, Refusal: "agent " + id + " is not registered"}, nil
	}
	if !req.GetForce() && a.ActiveTasks > 0 {
		return &pb.RemoveAgentReply{Removed: false, Refusal: statusMessageForInflight(id, a.ActiveTasks)}, nil
	}
	removed, err := s.store.DeleteAgent(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remove agent %q: %v", id, err)
	}
	if !removed {
		// Lost a race with a concurrent deregister; still not a server fault.
		return &pb.RemoveAgentReply{Removed: false, Refusal: "agent " + id + " was removed by someone else"}, nil
	}
	return &pb.RemoveAgentReply{Removed: true}, nil
}

func statusMessageForInflight(id string, active int) string {
	return "agent " + id + " reports " + strconv.Itoa(active) +
		" in-flight task(s); pass force to remove the record anyway"
}

// validateAgentAddress requires the host:port form the dispatcher dials, and
// rejects an address that merely contains a colon: a record with an unusable
// address would be scheduled against and then fail mid-change.
func validateAgentAddress(addr string) error {
	if addr == "" {
		return status.Error(codes.InvalidArgument, "agent address is required (host:port the agent listens on)")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "agent address %q is not host:port: %v", addr, err)
	}
	if strings.TrimSpace(host) == "" {
		return status.Errorf(codes.InvalidArgument, "agent address %q has an empty host", addr)
	}
	if strings.TrimSpace(port) == "" {
		return status.Errorf(codes.InvalidArgument, "agent address %q has an empty port", addr)
	}
	return nil
}

// normalizeAgentCapabilities trims entries and rejects empty names rather than
// storing them: a capability of "" would match no module yet keep appearing in
// operator output, and a duplicate would make one agent look like two.
func normalizeAgentCapabilities(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		c := strings.TrimSpace(raw)
		if c == "" {
			return nil, status.Error(codes.InvalidArgument,
				"agent capability names must not be empty or whitespace-only")
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, nil
}

// parseAgentStatus validates a caller-supplied status against the registry's
// own vocabulary, so the service cannot be fed a state this package cannot
// produce. The error names the accepted values.
func parseAgentStatus(raw string) (agent.AgentStatus, error) {
	st, err := agent.ParseAgentStatus(raw)
	if err != nil {
		return "", status.Error(codes.InvalidArgument, err.Error())
	}
	return st, nil
}

func agentHasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

func agentRecordToPB(a *state.Agent) *pb.AgentRecord {
	rec := &pb.AgentRecord{
		Id:             a.ID,
		Address:        a.Address,
		Capabilities:   append([]string(nil), a.Capabilities...),
		Status:         a.Status,
		ActiveTasks:    int32(a.ActiveTasks),
		MaxConcurrent:  int32(a.MaxConcurrent),
		CompletedTasks: a.CompletedTasks,
		FailedTasks:    a.FailedTasks,
	}
	if !a.LastHeartbeat.IsZero() {
		rec.LastHeartbeatUnix = a.LastHeartbeat.Unix()
	}
	if !a.RegisteredAt.IsZero() {
		rec.RegisteredAtUnix = a.RegisteredAt.Unix()
	}
	return rec
}
