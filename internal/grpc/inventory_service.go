// inventory_service.go — gRPC InventoryService implementation: hierarchical
// target groups, bulk YAML import, target lifecycle status and per-target
// change history. Backed entirely by state.Store (persistent across
// restarts, unlike the legacy in-memory TargetService registry).

package grpc

import (
	"context"
	"strings"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/inventory"
	"github.com/nexus/levee/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// InventoryService serves asset-management RPCs on the persistent store.
type InventoryService struct {
	pb.UnimplementedInventoryServiceServer

	store    state.Store
	importer *inventory.Importer
}

// NewInventoryService builds the service on top of store.
func NewInventoryService(store state.Store) *InventoryService {
	return &InventoryService{
		store:    store,
		importer: inventory.NewImporter(store),
	}
}

var validTargetStatuses = map[string]bool{
	state.StatusActive: true, state.StatusFrozen: true, state.StatusRetired: true,
}

// isUniqueViolation reports whether err is a UNIQUE-constraint failure from
// the underlying database (dialect-agnostic substring check).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint")
}

func groupToPB(g *state.InventoryGroup) *pb.Group {
	return &pb.Group{Id: g.ID, Name: g.Name, ParentId: g.ParentID}
}

func (s *InventoryService) ListGroups(ctx context.Context, _ *pb.ListGroupsRequest) (*pb.ListGroupsResponse, error) {
	groups, err := s.store.ListInventoryGroups(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list groups: %v", err)
	}
	out := make([]*pb.Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, groupToPB(g))
	}
	return &pb.ListGroupsResponse{Groups: out}, nil
}

func (s *InventoryService) CreateGroup(ctx context.Context, req *pb.CreateGroupRequest) (*pb.Group, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "group name is required")
	}
	g := &state.InventoryGroup{ID: newID("grp-"), Name: req.GetName(), ParentID: req.GetParentId()}
	if err := s.store.UpsertInventoryGroup(ctx, g); err != nil {
		if isUniqueViolation(err) {
			return nil, status.Errorf(codes.AlreadyExists, "group %q already exists", req.GetName())
		}
		return nil, status.Errorf(codes.Internal, "create group: %v", err)
	}
	return groupToPB(g), nil
}

func (s *InventoryService) DeleteGroup(ctx context.Context, req *pb.DeleteGroupRequest) (*pb.DeleteGroupResponse, error) {
	n, err := s.store.CountTargetsInGroup(ctx, req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "count targets in group: %v", err)
	}
	if n > 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"group %q still contains %d target(s); move or delete them first", req.GetId(), n)
	}
	if err := s.store.DeleteInventoryGroup(ctx, req.GetId()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete group: %v", err)
	}
	return &pb.DeleteGroupResponse{}, nil
}

func (s *InventoryService) ImportTargets(ctx context.Context, req *pb.ImportTargetsRequest) (*pb.ImportTargetsResponse, error) {
	f, err := inventory.ParseYAML([]byte(req.GetYamlContent()))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse inventory yaml: %v", err)
	}
	sum, err := s.importer.Import(ctx, f, req.GetDefaultGroup())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "import: %v", err)
	}
	return &pb.ImportTargetsResponse{
		Created: int32(sum.Created),
		Updated: int32(sum.Updated),
		Failed:  int32(sum.Failed),
		Errors:  sum.Errors,
	}, nil
}

func (s *InventoryService) SetTargetStatus(ctx context.Context, req *pb.SetTargetStatusRequest) (*pb.SetTargetStatusResponse, error) {
	st := req.GetStatus()
	if !validTargetStatuses[st] {
		return nil, status.Errorf(codes.InvalidArgument, "invalid status %q (active|frozen|retired)", st)
	}
	if err := s.store.UpdateTargetStatus(ctx, req.GetTargetId(), st); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, status.Errorf(codes.NotFound, "target %q not found", req.GetTargetId())
		}
		return nil, status.Errorf(codes.Internal, "set status: %v", err)
	}
	return &pb.SetTargetStatusResponse{Id: req.GetTargetId(), Status: st}, nil
}

func (s *InventoryService) TargetHistory(ctx context.Context, req *pb.TargetHistoryRequest) (*pb.TargetHistoryResponse, error) {
	host := req.GetHost()
	if host == "" {
		return nil, status.Error(codes.InvalidArgument, "host is required")
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 20
	}

	steps, err := s.store.ListSteps(ctx, state.StepFilter{Host: host, Limit: limit * 10})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list steps: %v", err)
	}

	seen := map[string]bool{}
	var out []*pb.TargetHistoryEntry
	for _, st := range steps {
		if seen[st.RunID] {
			continue
		}
		seen[st.RunID] = true
		run, err := s.store.GetRun(ctx, st.RunID)
		if err != nil || run == nil {
			continue
		}
		out = append(out, &pb.TargetHistoryEntry{
			RunId:        run.ID,
			WorkflowName: run.WorkflowName,
			Status:       run.Status,
			Creator:      run.Creator,
			CreatedAt:    run.CreatedAt.Unix(),
		})
		if len(out) >= limit {
			break
		}
	}
	return &pb.TargetHistoryResponse{Entries: out}, nil
}

// Compile-time interface assertion.
var _ pb.InventoryServiceServer = (*InventoryService)(nil)
