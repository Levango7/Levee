// TargetService implementation for the LEVEE gRPC API.
//
// TargetService manages the set of remote target hosts that LEVEE can act
// on. Targets are held in an in-memory registry (guarded by a RWMutex) keyed
// by ID. CheckTarget delegates reachability probing to the channel.Prechecker
// when a ChannelFactory is available; otherwise it returns the cached
// Reachable flag.
package grpc

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/grpc/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// TargetService implements pb.TargetServiceServer. It maintains an in-memory
// registry of target hosts and optionally probes reachability via a
// channel.ChannelFactory.
type TargetService struct {
	pb.UnimplementedTargetServiceServer

	store   stateStore // reserved for future persistence; currently unused
	factory channel.ChannelFactory

	mu      sync.RWMutex
	targets map[string]*pb.Target // keyed by Id
}

// stateStore is a minimal store interface alias to avoid importing state when
// not needed. It matches state.Store but is kept here for clarity; the field
// is reserved for future persistence without forcing callers to wire a full
// store today.
type stateStore interface{}

// NewTargetService returns a TargetService with an empty in-memory target
// registry. The optional factory enables real reachability probing in
// CheckTarget; when nil, CheckTarget returns the cached Reachable flag.
func NewTargetService(factory channel.ChannelFactory) *TargetService {
	return &TargetService{
		factory: factory,
		targets: make(map[string]*pb.Target),
	}
}

// NewTargetServiceWithStore is like NewTargetService but also accepts a
// state.Store for future persistence. The store is currently retained but not
// queried; target CRUD is in-memory.
func NewTargetServiceWithStore(store interface{}, factory channel.ChannelFactory) *TargetService {
	s := NewTargetService(factory)
	s.store = store
	return s
}

// AddTarget registers a new target host. When req.Id is empty a random ID is
// generated. Duplicate IDs yield codes.AlreadyExists.
func (s *TargetService) AddTarget(ctx context.Context, req *pb.AddTargetRequest) (*pb.Target, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}
	if strings.TrimSpace(req.Hostname) == "" {
		return nil, status.Error(codes.InvalidArgument, "hostname is required")
	}
	channelType := req.ChannelType
	if channelType == "" {
		channelType = "ssh"
	}
	port := req.Port
	if port == 0 {
		switch channelType {
		case "winrm":
			port = 5985
		default:
			port = 22
		}
	}

	id := req.Id
	if id == "" {
		var err error
		id, err = generateID("tgt-")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "generate target id: %v", err)
		}
	}

	tgt := &pb.Target{
		Id:            id,
		Hostname:      req.Hostname,
		ChannelType:   channelType,
		Port:          port,
		CredentialRef: req.CredentialRef,
		Labels:        req.Labels,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.targets[id]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "target %q already exists", id)
	}
	s.targets[id] = tgt
	return cloneTarget(tgt), nil
}

// RemoveTarget removes a target by ID. Unknown IDs yield codes.NotFound.
// The Force flag is accepted but currently a no-op.
func (s *TargetService) RemoveTarget(ctx context.Context, req *pb.RemoveTargetRequest) (*emptypb.Empty, error) {
	if req == nil || strings.TrimSpace(req.Id) == "" {
		return nil, status.Error(codes.InvalidArgument, "target id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.targets[req.Id]; !ok {
		return nil, status.Errorf(codes.NotFound, "target %q not found", req.Id)
	}
	delete(s.targets, req.Id)
	return &emptypb.Empty{}, nil
}

// ListTargets returns targets matching the given filters, with pagination.
// LabelSelector matches targets whose labels contain all the given key=value
// pairs. ChannelType filters by transport. ReachableOnly filters by the cached
// Reachable flag.

func (s *TargetService) ListTargets(ctx context.Context, req *pb.ListTargetsRequest) (*pb.ListTargetsResponse, error) {
	if req == nil {
		req = &pb.ListTargetsRequest{}
	}

	pageSize := int(req.PageSize)
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	offset := parsePageToken(req.PageToken)

	s.mu.RLock()
	var matched []*pb.Target
	for _, t := range s.targets {
		if !matchLabelSelector(t.Labels, req.LabelSelector) {
			continue
		}
		if req.ChannelType != "" && t.ChannelType != req.ChannelType {
			continue
		}
		if req.ReachableOnly && !t.Reachable {
			continue
		}
		matched = append(matched, cloneTarget(t))
	}
	s.mu.RUnlock()

	sortTargetsByID(matched)

	total := len(matched)
	if offset > total {
		offset = total
	}
	end := offset + pageSize
	if end > total {
		end = total
	}
	page := matched[offset:end]

	return &pb.ListTargetsResponse{
		Targets:       page,
		TotalSize:     int32(total),
		NextPageToken: buildPageToken(end, total),
	}, nil
}

// GetTarget returns a single target by ID.
func (s *TargetService) GetTarget(ctx context.Context, req *pb.GetTargetRequest) (*pb.Target, error) {
	if req == nil || strings.TrimSpace(req.Id) == "" {
		return nil, status.Error(codes.InvalidArgument, "target id is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.targets[req.Id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "target %q not found", req.Id)
	}
	return cloneTarget(t), nil
}

// CheckTarget probes target reachability. When Fresh is true and a
// ChannelFactory is configured, a real connection probe is performed via
// channel.Prechecker. Otherwise the cached Reachable flag is returned.
func (s *TargetService) CheckTarget(ctx context.Context, req *pb.CheckTargetRequest) (*pb.CheckTargetResponse, error) {
	if req == nil || strings.TrimSpace(req.Id) == "" {
		return nil, status.Error(codes.InvalidArgument, "target id is required")
	}

	s.mu.RLock()
	t, ok := s.targets[req.Id]
	s.mu.RUnlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "target %q not found", req.Id)
	}

	resp := &pb.CheckTargetResponse{
		Target:    cloneTarget(t),
		Reachable: t.Reachable,
		CheckedAt: time.Now().UTC().Unix(),
	}

	if !req.Fresh {
		return resp, nil
	}

	// Fresh probe: use Prechecker when a factory is available.
	if s.factory == nil {
		// No factory — return cached state with a note.
		if !t.Reachable {
			resp.Error = "no channel factory configured; returning cached state"
		}
		return resp, nil
	}

	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = channel.DefaultNoopTimeout
	}

	tgt := &grpcTarget{
		host:        t.Hostname,
		port:        int(t.Port),
		channelType: t.ChannelType,
		cred:        channel.CredentialRef{},
	}

	prechecker := channel.NewPrechecker(nil, nil,
		channel.WithChannelFactory(s.factory),
		channel.WithNoopTimeout(timeout),
	)
	report := prechecker.Check(ctx, []channel.Target{tgt})
	if len(report.Results) == 0 {
		resp.Error = "no precheck result returned"
		return resp, nil
	}

	result := report.Results[0]
	resp.Reachable = result.Reachable
	resp.LatencyMs = result.Latency.Milliseconds()
	resp.Error = result.Error

	// Update the cached Reachable flag.
	s.mu.Lock()
	if cached, ok := s.targets[req.Id]; ok {
		cached.Reachable = result.Reachable
	}
	s.mu.Unlock()
	resp.Target = func() *pb.Target {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return cloneTarget(s.targets[req.Id])
	}()

	return resp, nil
}

// --- helpers -----------------------------------------------------------------

// matchLabelSelector returns true when target labels contain every key=value
// pair from the selector. An empty selector matches everything.
func matchLabelSelector(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// cloneTarget returns a shallow copy of a pb.Target so callers cannot mutate
// the registry entry.
func cloneTarget(t *pb.Target) *pb.Target {
	if t == nil {
		return nil
	}
	cp := &pb.Target{
		Id:            t.Id,
		Hostname:      t.Hostname,
		ChannelType:   t.ChannelType,
		Port:          t.Port,
		CredentialRef: t.CredentialRef,
		Reachable:     t.Reachable,
	}
	if t.Labels != nil {
		cp.Labels = make(map[string]string, len(t.Labels))
		for k, v := range t.Labels {
			cp.Labels[k] = v
		}
	}
	return cp
}

// sortTargetsByID sorts a slice of pb.Target by Id ascending.
func sortTargetsByID(ts []*pb.Target) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j-1].Id > ts[j].Id; j-- {
			ts[j-1], ts[j] = ts[j], ts[j-1]
		}
	}
}

// grpcTarget adapts a pb.Target to the channel.Target interface for precheck.
type grpcTarget struct {
	host        string
	port        int
	channelType string
	cred        channel.CredentialRef
}

func (t *grpcTarget) Host() string                       { return t.host }
func (t *grpcTarget) Port() int                          { return t.port }
func (t *grpcTarget) Type() string                       { return t.channelType }
func (t *grpcTarget) Credentials() channel.CredentialRef { return t.cred }
