// TargetService implementation for the LEVEE gRPC API.
//
// TargetService manages the set of remote target hosts that LEVEE can act
// on. Targets are persisted in the state store (the same inventory rows the
// InventoryService and the CLI serve), so registrations survive restarts and
// REST/gRPC views match `levee target list`. CheckTarget delegates
// reachability probing to channel.Prechecker when a ChannelFactory is
// available; results are stamped back into the persistent row.
package grpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// CredentialResolver expands a stored, unresolved credential reference (the
// CredentialRef string a target was registered with, e.g. "cred-a1b2c3") into
// transport credentials usable by a channel.Target. Implementations are backed
// by the credential store; TargetService itself never touches secret material.
//
// Resolution failures must NOT abort the RPC: CheckTarget treats them as a
// soft condition, reports them on the response and falls back to an
// unauthenticated probe.
type CredentialResolver interface {
	ResolveTargetCredential(ctx context.Context, ref string) (*channel.CredentialRef, error)
}

// TargetService implements pb.TargetServiceServer on the persistent inventory
// store, optionally probing reachability via a channel.ChannelFactory.
type TargetService struct {
	pb.UnimplementedTargetServiceServer

	store    state.Store
	factory  channel.ChannelFactory
	resolver CredentialResolver // optional; nil disables credential resolution
}

// NewTargetService returns a store-backed TargetService. The optional factory
// enables real reachability probing in CheckTarget; when nil, CheckTarget
// returns the persisted Reachable flag.
func NewTargetService(store state.Store, factory channel.ChannelFactory) *TargetService {
	return &TargetService{
		store:   store,
		factory: factory,
	}
}

// WithCredentialResolver attaches an optional CredentialResolver used by
// CheckTarget to expand a target's stored CredentialRef into real transport
// credentials before probing. A nil resolver disables resolution: probes then
// run unauthenticated (the previous behaviour), with a warning on the response
// when the target carries a CredentialRef. Call this during service setup,
// before the server starts serving.
func (s *TargetService) WithCredentialResolver(r CredentialResolver) *TargetService {
	s.resolver = r
	return s
}

// pbFromState maps a persistent inventory row onto the wire representation.
func pbFromState(t *state.Target) *pb.Target {
	if t == nil {
		return nil
	}
	labels := t.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return &pb.Target{
		Id:            t.ID,
		Hostname:      t.Hostname,
		ChannelType:   t.ChannelType,
		Port:          int32(t.Port),
		CredentialRef: t.CredentialRef,
		Labels:        labels,
		Reachable:     t.Reachable,
		Status:        t.Status,
	}
}

// AddTarget registers a new target host in the persistent inventory. When
// req.Id is empty a random ID is generated. Duplicate IDs and addresses
// (hostname+port owned by another row) yield codes.AlreadyExists.
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
	port := int(req.Port)
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

	if existing, err := s.store.GetTarget(ctx, id); err != nil {
		return nil, status.Errorf(codes.Internal, "get target: %v", err)
	} else if existing != nil {
		return nil, status.Errorf(codes.AlreadyExists, "target %q already exists", id)
	}

	row := &state.Target{
		ID:            id,
		Hostname:      req.Hostname,
		Port:          port,
		ChannelType:   channelType,
		CredentialRef: req.CredentialRef,
		Labels:        req.Labels,
		Status:        state.StatusActive,
	}
	if err := s.store.UpsertTarget(ctx, row); err != nil {
		if errors.Is(err, state.ErrDuplicateTarget) {
			return nil, status.Errorf(codes.AlreadyExists, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "upsert target: %v", err)
	}
	return pbFromState(row), nil
}

// RemoveTarget removes a target by ID. Unknown IDs yield codes.NotFound.
// The Force flag is accepted but currently a no-op.
func (s *TargetService) RemoveTarget(ctx context.Context, req *pb.RemoveTargetRequest) (*emptypb.Empty, error) {
	if req == nil || strings.TrimSpace(req.Id) == "" {
		return nil, status.Error(codes.InvalidArgument, "target id is required")
	}
	existing, err := s.store.GetTarget(ctx, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get target: %v", err)
	}
	if existing == nil {
		return nil, status.Errorf(codes.NotFound, "target %q not found", req.Id)
	}
	if err := s.store.DeleteTarget(ctx, req.Id); err != nil {
		return nil, status.Errorf(codes.Internal, "delete target: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// ListTargets returns persisted targets matching the given filters, with
// pagination. LabelSelector matches targets whose labels contain all the
// given key=value pairs (pushed down to the store); ChannelType and
// ReachableOnly are filtered after the fetch.

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
	offset, err := parsePageToken(req.PageToken)
	if err != nil {
		return nil, err
	}

	filter := state.TargetFilter{Labels: req.LabelSelector}
	rows, err := s.store.ListTargets(ctx, filter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list targets: %v", err)
	}

	matched := make([]*pb.Target, 0, len(rows))
	for _, row := range rows {
		if req.ChannelType != "" && row.ChannelType != req.ChannelType {
			continue
		}
		if req.ReachableOnly && !row.Reachable {
			continue
		}
		matched = append(matched, pbFromState(row))
	}

	total := len(matched)
	if offset > total {
		offset = total
	}
	end := offset + pageSize
	if end > total {
		end = total
	}
	page := matched[offset:end]

	nextToken := ""
	if end < total {
		nextToken = fmt.Sprintf("%d", end)
	}
	return &pb.ListTargetsResponse{
		Targets:       page,
		TotalSize:     int32(total),
		NextPageToken: nextToken,
	}, nil
}

// GetTarget returns a single target by ID.
func (s *TargetService) GetTarget(ctx context.Context, req *pb.GetTargetRequest) (*pb.Target, error) {
	if req == nil || strings.TrimSpace(req.Id) == "" {
		return nil, status.Error(codes.InvalidArgument, "target id is required")
	}
	row, err := s.store.GetTarget(ctx, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get target: %v", err)
	}
	if row == nil {
		return nil, status.Errorf(codes.NotFound, "target %q not found", req.Id)
	}
	return pbFromState(row), nil
}

// CheckTarget probes target reachability. When Fresh is true and a
// ChannelFactory is configured, a real connection probe is performed via
// channel.Prechecker and the result is stamped into the persistent row.
// Otherwise the stored Reachable flag is returned.
func (s *TargetService) CheckTarget(ctx context.Context, req *pb.CheckTargetRequest) (*pb.CheckTargetResponse, error) {
	if req == nil || strings.TrimSpace(req.Id) == "" {
		return nil, status.Error(codes.InvalidArgument, "target id is required")
	}

	row, err := s.store.GetTarget(ctx, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get target: %v", err)
	}
	if row == nil {
		return nil, status.Errorf(codes.NotFound, "target %q not found", req.Id)
	}

	resp := &pb.CheckTargetResponse{
		Target:    pbFromState(row),
		Reachable: row.Reachable,
		CheckedAt: time.Now().UTC().Unix(),
	}

	if !req.Fresh {
		return resp, nil
	}

	// Fresh probe: use Prechecker when a factory is available.
	if s.factory == nil {
		// No factory — return persisted state with a note.
		if !row.Reachable {
			resp.Error = "no channel factory configured; returning cached state"
		}
		return resp, nil
	}

	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = channel.DefaultNoopTimeout
	}

	// Resolve the target's stored credential reference so the probe exercises
	// the same authentication path as real execution. Resolution is
	// best-effort: a missing resolver or a resolution failure degrades to an
	// unauthenticated probe and is reported as a warning on the response
	// (CheckTargetResponse has no dedicated warnings field, so warnings ride
	// on Error behind a "warning:" prefix); it never hard-fails the RPC.
	var warnings []string
	tgt := &grpcTarget{
		host:          row.Hostname,
		port:          row.Port,
		channelType:   row.ChannelType,
		credentialRef: row.CredentialRef,
	}
	if ref := row.CredentialRef; ref != "" {
		if s.resolver == nil {
			warnings = append(warnings, "probed without credentials (no resolver configured)")
		} else if resolved, rerr := s.resolver.ResolveTargetCredential(ctx, ref); rerr != nil {
			warnings = append(warnings, fmt.Sprintf("credential %q could not be resolved (%v); probed without credentials", ref, rerr))
		} else if resolved == nil {
			warnings = append(warnings, fmt.Sprintf("credential %q resolved to no usable material; probed without credentials", ref))
		} else {
			tgt.cred = *resolved
		}
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
	resp.Error = mergeProbeWarnings(result.Error, warnings)

	// Stamp reachability into the persistent row and refresh the response.
	now := time.Now().UTC()
	if err := s.store.SetTargetReachability(ctx, row.ID, result.Reachable, now); err != nil {
		resp.Error = mergeProbeWarnings(resp.Error, []string{fmt.Sprintf("persisting reachability failed: %v", err)})
	}
	row.Reachable = result.Reachable
	v := now
	row.LastCheckedAt = &v
	resp.Target = pbFromState(row)

	return resp, nil
}

// --- helpers -----------------------------------------------------------------

// mergeProbeWarnings folds soft credential warnings into the probe error so
// they reach the caller on CheckTargetResponse.Error (the response message has
// no dedicated warnings field). Warnings are prefixed with "warning:" so
// clients can distinguish them from hard probe failures; an existing probe
// error is preserved verbatim after the warnings.
func mergeProbeWarnings(probeErr string, warnings []string) string {
	if len(warnings) == 0 {
		return probeErr
	}
	joined := "warning: " + strings.Join(warnings, "; ")
	if probeErr == "" {
		return joined
	}
	return joined + "; " + probeErr
}

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

	// credentialRef carries the stored, UNRESOLVED credential reference
	// (e.g. "cred-a1b2c3") the target was registered with. The registry
	// deliberately never holds secret material. When TargetService has a
	// CredentialResolver configured, CheckTarget expands the reference into
	// cred before probing; without a resolver (or on resolution failure) cred
	// stays empty and the probe runs unauthenticated, with a warning on the
	// CheckTarget response. The raw reference is also kept available for
	// ChannelFactory implementations that resolve references themselves.
	credentialRef string
}

func (t *grpcTarget) Host() string                       { return t.host }
func (t *grpcTarget) Port() int                          { return t.port }
func (t *grpcTarget) Type() string                       { return t.channelType }
func (t *grpcTarget) Credentials() channel.CredentialRef { return t.cred }

// StoredCredentialRef returns the stored, unresolved credential reference.
func (t *grpcTarget) StoredCredentialRef() string { return t.credentialRef }
