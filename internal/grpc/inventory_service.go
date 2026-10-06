// inventory_service.go — gRPC InventoryService implementation: hierarchical
// target groups, bulk YAML import, target lifecycle status and per-target
// change history. Backed entirely by state.Store (persistent across
// restarts, unlike the legacy in-memory TargetService registry).

package grpc

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/inventory"
	"github.com/nexus/levee/internal/permission"
	"github.com/nexus/levee/internal/state"
)

// InventoryService serves asset-management RPCs on the persistent store.
type InventoryService struct {
	pb.UnimplementedInventoryServiceServer

	store    state.Store
	importer *inventory.Importer
	authz    *authz.Authorizer // optional; nil means no policy configured
}

// WithAuthorizer installs the policy authorizer. Groups and bulk imports are
// fleet-wide writes, so they are judged in permission.default_env — see
// resource_authz.go for the whole posture.
func (s *InventoryService) WithAuthorizer(a *authz.Authorizer) *InventoryService {
	s.authz = a
	return s
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
	// Groups declare no environment — a name like "prod/db" is a naming habit,
	// not a fact the policy can rest on — so they are judged as the
	// deployment-wide namespace they are: permission.default_env.
	if err := authorizeResourceRead(ctx, s.authz, "", "ListGroups"); err != nil {
		return nil, err
	}
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
	// Group names are unique table-wide (two tenants may not both claim
	// "prod/db"), so creating one claims space in everyone's namespace: fleet
	// write, not a change action.
	if err := authorizeResource(ctx, s.authz, "", permission.ActionAdmin, "CreateGroup"); err != nil {
		return nil, err
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
	if err := authorizeResource(ctx, s.authz, "", permission.ActionAdmin, "DeleteGroup"); err != nil {
		return nil, err
	}
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
	// An inventory file is a pile of AddTarget requests, so it must not be
	// authorisable in a weaker scope than one of them is: judge every
	// environment the file declares, not just the one the caller happens to
	// default to. Refusal comes before any write, so a file that contains one
	// host out of reach imports none of its hosts rather than half of them.
	envs := map[string]bool{}
	for _, td := range f.Targets {
		envs[td.Labels[targetEnvLabelKey]] = true
	}
	for env := range envs {
		if err := authorizeResource(ctx, s.authz, env, permission.ActionAdmin, "ImportTargets"); err != nil {
			return nil, err
		}
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
		return nil, status.Errorf(codes.InvalidArgument, "invalid status %q (active|frozen|retired)", req.GetStatus())
	}
	// Frozen hosts are refused at plan AND apply time, so this one call can
	// stop another team's change from running at all — that is a fleet write on
	// the host's own environment, decided before the row is touched.
	existing, err := s.store.GetTarget(ctx, req.GetTargetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get target: %v", err)
	}
	if existing == nil {
		return nil, status.Errorf(codes.NotFound, "target %q not found", req.GetTargetId())
	}
	if err := authorizeResource(ctx, s.authz, envOfTarget(existing), permission.ActionAdmin, "SetTargetStatus"); err != nil {
		return nil, err
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
	// Each entry carries the run's workflow document and creator, so this is a
	// read of change data and it narrows to the environments the caller may
	// see. The rows are already fetched per run to render them, so the filter
	// costs no additional store traffic.
	visible := resourceVisibility(ctx, s.authz)
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
		if visible != nil && !visible(envOf(run)) {
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
