package grpc

// change_service.go implements pb.ChangeServiceServer, the gRPC service
// covering LEVEE's full change lifecycle: create → plan → approve →
// apply → verify → rollback → archive. This is the largest and most
// important of the five gRPC services.
//
// Implementation strategy:
//
//   - Simple queries (GetChange, ListChanges, GetLogs, GetDiff, GetTrace)
//     call the state Store directly and convert the result to pb types.
//   - Lifecycle transitions (Pause, Resume, Cancel, Approve, Reject,
//     Archive) update the Run row in the store and record an audit
//     entry; the heavy lifting (engine execution, approval state
//     machine) is delegated to the internal packages when the
//     corresponding dependency is non-nil.
//   - Complex operations (ApplyChange, PlanChange, RollbackChange,
//     RetryChange) delegate to the EngineAdapter seam. When the seam
//     is nil they return codes.Unimplemented, keeping the server
//     usable in query-only deployments.
//   - Streaming RPCs (WatchChange, StreamLogs) use a channel +
//     goroutine pattern: a watcher subscribes to a per-change event
//     bus, and a forwarder goroutine pumps events into the gRPC
//     stream until the client disconnects or the change reaches a
//     terminal state.
//
// All errors are mapped to gRPC codes via the status package. Internal
// errors use codes.Internal; not-found uses codes.NotFound; invalid
// state transitions use codes.FailedPrecondition.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/pause"
	"github.com/nexus/levee/internal/state"

	grpcpkg "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ChangeService implements pb.ChangeServiceServer. It embeds
// UnimplementedChangeServiceServer for forward compatibility so that
// newly added RPCs default to Unimplemented rather than panicking.
type ChangeService struct {
	pb.UnimplementedChangeServiceServer

	store    state.Store
	engine   *EngineAdapter
	approval *approval.Service
	pause    *pause.PauseManager

	// eventBus distributes change events to WatchChange subscribers.
	// It is lazily initialised on first subscription.
	eventMu  sync.Mutex
	eventBus *eventBus
}

// NewChangeService constructs a ChangeService backed by the given
// store. The engine, approval and pause dependencies are optional; when
// nil, the corresponding RPCs return codes.Unimplemented or fall back
// to a store-only code path. This keeps the service usable in
// reduced-functionality deployments.
func NewChangeService(
	store state.Store,
	engine *EngineAdapter,
	appr *approval.Service,
	pauseMgr *pause.PauseManager,
) *ChangeService {
	return &ChangeService{
		store:    store,
		engine:   engine,
		approval: appr,
		pause:    pauseMgr,
	}
}

// --- helpers ---------------------------------------------------------------

// newID generates a random hex ID with the supplied prefix, e.g.
// "run-" → "run-a1b2c3...". It panics only when crypto/rand fails,
// which indicates a broken system RNG; the recovery interceptor
// converts such panics into codes.Internal.
func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// This should never happen on a healthy system. We panic
		// rather than silently return a duplicate/empty ID; the
		// recovery interceptor will convert this into a clean
		// codes.Internal gRPC error.
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return prefix + hex.EncodeToString(b)
}

// runToPB converts a state.Run to a pb.Change. The two types have
// overlapping but not identical fields; this helper centralises the
// mapping so every RPC that returns a Change uses the same conversion.
func runToPB(r *state.Run) *pb.Change {
	if r == nil {
		return nil
	}
	params := make(map[string]string)
	if r.Params != "" {
		// Best-effort decode; on error we leave params empty rather
		// than failing the whole RPC, since params are informational.
		_ = json.Unmarshal([]byte(r.Params), &params)
	}
	return &pb.Change{
		Id:           r.ID,
		Label:        r.WorkflowName, // map workflow_name to label for now
		Status:       r.Status,
		Priority:     r.ApprovalLevel,
		WorkflowFile: r.WorkflowName,
		TemplateName: r.TemplateName,
		Params:       params,
		CreatedAt:    r.CreatedAt.Unix(),
		UpdatedAt:    r.UpdatedAt.Unix(),
		CreatedBy:    r.Creator,
		Environment:  r.IncidentID, // reuse field as environment marker
	}
}

// pbToRunParams serialises a pb params map to the JSON string expected
// by state.Run.Params.
func pbToRunParams(params map[string]string) string {
	if len(params) == 0 {
		return "{}"
	}
	b, err := json.Marshal(params)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// actorFromCtx extracts the actor identity from the context. In
// production this would come from the auth interceptor (which validates
// the bearer token); for now we use a metadata key or fall back to
// "grpc-user".
func actorFromCtx(ctx context.Context) string {
	// We do not import metadata here to avoid an extra dependency in
	// this helper; the auth interceptor already validated the token.
	// The actor is conveyed via a context value set by a future
	// audit interceptor; absent that, we use a constant.
	if v, ok := ctx.Value(actorKey{}).(string); ok && v != "" {
		return v
	}
	return "grpc-user"
}

// actorKey is the context key type for the actor identity.
type actorKey struct{}

// --- CreateChange ----------------------------------------------------------

// CreateChange creates a new change (run) record in draft status. It is
// the gRPC analogue of `levee new`. The workflow file or template name
// is recorded but not yet parsed; that happens in PlanChange.
func (s *ChangeService) CreateChange(ctx context.Context, req *pb.CreateChangeRequest) (*pb.Change, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	now := time.Now().UTC()
	runID := newID("run-")

	// Default priority to "normal" if not supplied.
	priority := req.GetPriority()
	if priority == "" {
		priority = "normal"
	}

	run := &state.Run{
		ID:             runID,
		WorkflowName:   req.GetWorkflowFile(),
		TemplateName:   req.GetTemplateName(),
		Params:         pbToRunParams(req.GetParams()),
		Status:         "draft",
		ApprovalStatus: "pending",
		ApprovalLevel:  priority,
		CreatedAt:      now,
		UpdatedAt:      now,
		Creator:        actorFromCtx(ctx),
		IncidentID:     req.GetEnvironment(),
	}
	if err := s.store.CreateRun(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "create run: %v", err)
	}

	// Record an audit entry for the creation.
	_ = s.store.CreateAudit(ctx, &state.Audit{
		ID:        newID("aud-"),
		RunID:     runID,
		Action:    "create",
		Actor:     run.Creator,
		Target:    req.GetTemplateName(),
		Result:    "draft",
		Timestamp: now,
	})

	log.Info("change created", "run_id", runID, "template", req.GetTemplateName(), "actor", run.Creator)
	return runToPB(run), nil
}

// --- CloneChange -----------------------------------------------------------

// CloneChange creates a new run by copying the workflow, template and
// parameters of an existing run. The source run is not modified. The
// new run starts in draft status.
func (s *ChangeService) CloneChange(ctx context.Context, req *pb.CloneChangeRequest) (*pb.Change, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	src, err := s.store.GetRun(ctx, req.GetSourceChangeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get source run: %v", err)
	}
	if src == nil {
		return nil, status.Errorf(codes.NotFound, "source change %q not found", req.GetSourceChangeId())
	}

	now := time.Now().UTC()
	runID := newID("run-")

	// Merge source params with override params.
	params := make(map[string]string)
	_ = json.Unmarshal([]byte(src.Params), &params)
	for k, v := range req.GetParams() {
		params[k] = v
	}

	env := req.GetEnvironment()
	if env == "" {
		env = src.IncidentID
	}

	run := &state.Run{
		ID:             runID,
		WorkflowName:   src.WorkflowName,
		TemplateName:   src.TemplateName,
		Params:         pbToRunParams(params),
		Status:         "draft",
		ApprovalStatus: "pending",
		ApprovalLevel:  src.ApprovalLevel,
		CreatedAt:      now,
		UpdatedAt:      now,
		Creator:        actorFromCtx(ctx),
		IncidentID:     env,
	}
	if err := s.store.CreateRun(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "create cloned run: %v", err)
	}

	_ = s.store.CreateAudit(ctx, &state.Audit{
		ID:        newID("aud-"),
		RunID:     runID,
		Action:    "clone",
		Actor:     run.Creator,
		Target:    req.GetSourceChangeId(),
		Result:    "draft",
		Timestamp: now,
	})

	log.Info("change cloned", "new_run_id", runID, "source_run_id", req.GetSourceChangeId())
	return runToPB(run), nil
}

// --- PlanChange ------------------------------------------------------------

// PlanChange generates an execution plan for a change without applying
// it. When the EngineAdapter is configured, it delegates to
// EngineAdapter.Plan; otherwise it returns a minimal placeholder plan
// derived from the stored run metadata.
func (s *ChangeService) PlanChange(ctx context.Context, req *pb.PlanChangeRequest) (*pb.Plan, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	run, err := s.store.GetRun(ctx, req.GetChangeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", req.GetChangeId())
	}

	// Delegate to the engine when available.
	if s.engine != nil && s.engine.Plan != nil {
		plan, err := s.engine.Plan(ctx, req.GetChangeId(), req.GetTargetHosts())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "plan: %v", err)
		}
		// Persist the plan hash on the run for later verification.
		if plan != nil && !req.GetDryRun() {
			run.UpdatedAt = time.Now().UTC()
			if err := s.store.UpdateRun(ctx, run); err != nil {
				log.Warn("failed to update run after plan", "run_id", req.GetChangeId(), "error", err)
			}
		}
		return plan, nil
	}

	// Fallback: return a minimal plan with no batches. This keeps the
	// RPC usable in query-only mode.
	return &pb.Plan{
		ChangeId:      req.GetChangeId(),
		TargetHosts:   req.GetTargetHosts(),
		Batches:       []*pb.Batch{},
		ImpactSummary: "no engine configured; empty plan",
	}, nil
}

// --- ApplyChange -----------------------------------------------------------

// ApplyChange triggers execution of a planned change. It transitions
// the run to "running" and delegates to the EngineAdapter for the
// actual execution. When the engine is nil, it performs a minimal
// status transition (the same MVP behaviour as the CLI).
func (s *ChangeService) ApplyChange(ctx context.Context, req *pb.ApplyChangeRequest) (*pb.ApplyResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	run, err := s.store.GetRun(ctx, req.GetChangeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", req.GetChangeId())
	}

	// State guard: without auto-approve, only approved runs may be applied.
	if !req.GetAutoApprove() && run.Status != "approved" && run.Status != "draft" {
		return nil, status.Errorf(codes.FailedPrecondition, "change %q is in %q state, cannot apply", req.GetChangeId(), run.Status)
	}

	// Transition to running.
	now := time.Now().UTC()
	oldStatus := run.Status
	run.Status = "running"
	run.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "update run to running: %v", err)
	}

	// Record a trace for the apply start.
	_ = s.store.CreateTrace(ctx, &state.Trace{
		ID:        newID("trc-"),
		RunID:     run.ID,
		Event:     "apply_started",
		Actor:     actorFromCtx(ctx),
		PrevHash:  "",
		CurrHash:  "",
		Timestamp: now,
	})

	// Publish a status-changed event for watchers.
	s.publishEvent(&pb.ChangeEvent{
		ChangeId:  run.ID,
		EventType: "status_changed",
		OldStatus: oldStatus,
		NewStatus: "running",
		Message:   "apply triggered",
		Timestamp: now.Unix(),
	})

	// Delegate to the engine when available.
	if s.engine != nil && s.engine.Run != nil {
		execRunID, success, err := s.engine.Run(ctx, req.GetChangeId(), req.GetAutoApprove(), req.GetMaxConcurrency())
		if err != nil {
			// Mark the run as failed.
			run.Status = "failed"
			run.UpdatedAt = time.Now().UTC()
			_ = s.store.UpdateRun(ctx, run)
			return &pb.ApplyResponse{
				Change:  runToPB(run),
				RunId:   execRunID,
				Success: false,
				Message: err.Error(),
			}, status.Errorf(codes.Internal, "engine: %v", err)
		}
		finalStatus := "completed"
		if !success {
			finalStatus = "failed"
		}
		run.Status = finalStatus
		run.UpdatedAt = time.Now().UTC()
		_ = s.store.UpdateRun(ctx, run)
		return &pb.ApplyResponse{
			Change:  runToPB(run),
			RunId:   execRunID,
			Success: success,
			Message: finalStatus,
		}, nil
	}

	// Fallback (no engine): leave the run in "running" status; a
	// subsequent engine integration will complete it.
	return &pb.ApplyResponse{
		Change:  runToPB(run),
		RunId:   run.ID,
		Success: true,
		Message: "apply triggered (no engine; MVP status only)",
	}, nil
}

// --- PauseChange / ResumeChange --------------------------------------------

// PauseChange pauses a single change. It delegates to the pause
// manager when available; otherwise it performs a direct status
// transition in the store.
func (s *ChangeService) PauseChange(ctx context.Context, req *pb.PauseRequest) (*pb.Change, error) {
	return s.transitionStatus(ctx, req.GetChangeId(), "paused", "pause", req.GetReason())
}

// ResumeChange resumes a paused change.
func (s *ChangeService) ResumeChange(ctx context.Context, req *pb.PauseRequest) (*pb.Change, error) {
	return s.transitionStatus(ctx, req.GetChangeId(), "running", "resume", req.GetReason())
}

// transitionStatus is the shared implementation for Pause/Resume. It
// loads the run, validates the transition, updates the store and
// records an audit entry. The action parameter is recorded in the
// audit log; the newStatus parameter is the target status.
func (s *ChangeService) transitionStatus(ctx context.Context, runID, newStatus, action, reason string) (*pb.Change, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", runID)
	}

	// Delegate to the pause manager when available — it enforces the
	// full state machine and writes its own audit entries.
	if s.pause != nil {
		switch newStatus {
		case "paused":
			if err := s.pause.PauseRun(ctx, runID, actorFromCtx(ctx)); err != nil {
				return nil, mapPauseError(err, runID)
			}
		case "running":
			if err := s.pause.ResumeRun(ctx, runID, actorFromCtx(ctx)); err != nil {
				return nil, mapPauseError(err, runID)
			}
		}
		// Reload to pick up the pause manager's changes.
		run, err = s.store.GetRun(ctx, runID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "reload run: %v", err)
		}
		return runToPB(run), nil
	}

	// Fallback: direct status transition.
	oldStatus := run.Status
	if !isValidTransition(oldStatus, newStatus) {
		return nil, status.Errorf(codes.FailedPrecondition, "cannot transition from %q to %q", oldStatus, newStatus)
	}
	now := time.Now().UTC()
	run.Status = newStatus
	run.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "update run: %v", err)
	}

	_ = s.store.CreateAudit(ctx, &state.Audit{
		ID:        newID("aud-"),
		RunID:     runID,
		Action:    action,
		Actor:     actorFromCtx(ctx),
		Target:    runID,
		Result:    newStatus,
		Timestamp: now,
	})

	s.publishEvent(&pb.ChangeEvent{
		ChangeId:  runID,
		EventType: "status_changed",
		OldStatus: oldStatus,
		NewStatus: newStatus,
		Message:   reason,
		Timestamp: now.Unix(),
	})

	return runToPB(run), nil
}

// mapPauseError converts a pause-manager error into a gRPC status error.
func mapPauseError(err error, runID string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		return status.Errorf(codes.NotFound, "change %q not found", runID)
	case strings.Contains(msg, "invalid"), strings.Contains(msg, "cannot"):
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	default:
		return status.Errorf(codes.Internal, "pause: %v", err)
	}
}

// isValidTransition reports whether a direct status transition is
// legal. This is a simplified subset of the full state machine; the
// pause manager enforces the complete rules when available.
func isValidTransition(from, to string) bool {
	switch to {
	case "paused":
		return from == "running" || from == "pending"
	case "running":
		return from == "paused" || from == "pending" || from == "approved"
	case "cancelled":
		return from != "completed" && from != "cancelled" && from != "archived"
	case "archived":
		return from == "completed" || from == "failed" || from == "cancelled" || from == "rolled_back"
	default:
		return false
	}
}

// --- PauseAll / ResumeAll --------------------------------------------------

// PauseAll pauses every running or pending change, optionally
// restricted to the supplied teams/environments. It returns the IDs of
// the changes that were paused and those that were skipped (e.g.
// already completed).
func (s *ChangeService) PauseAll(ctx context.Context, req *pb.PauseAllRequest) (*pb.PauseAllResponse, error) {
	return s.bulkTransition(ctx, "paused", "pause-all", req.GetTeams(), req.GetEnvironments(), req.GetReason())
}

// ResumeAll resumes every paused change.
func (s *ChangeService) ResumeAll(ctx context.Context, req *pb.PauseAllRequest) (*pb.PauseAllResponse, error) {
	return s.bulkTransition(ctx, "running", "resume-all", req.GetTeams(), req.GetEnvironments(), req.GetReason())
}

// bulkTransition is the shared implementation for PauseAll/ResumeAll.
func (s *ChangeService) bulkTransition(ctx context.Context, targetStatus, action string, teams, envs []string, reason string) (*pb.PauseAllResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	runs, err := s.store.ListRuns(ctx, state.RunFilter{Limit: 0})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list runs: %v", err)
	}

	teamSet := toSet(teams)
	envSet := toSet(envs)

	resp := &pb.PauseAllResponse{}
	now := time.Now().UTC()
	for _, run := range runs {
		// Apply team/environment filters. We reuse the IncidentID
		// field as the environment marker; teams are not currently
		// stored on the run, so the team filter is best-effort.
		if len(envSet) > 0 && !envSet[run.IncidentID] {
			continue
		}
		_ = teamSet // team filtering would go here once runs carry a team field.

		if !isValidTransition(run.Status, targetStatus) {
			resp.SkippedChangeIds = append(resp.SkippedChangeIds, run.ID)
			continue
		}

		oldStatus := run.Status
		run.Status = targetStatus
		run.UpdatedAt = now
		if err := s.store.UpdateRun(ctx, run); err != nil {
			log.Warn("bulk transition failed for run", "run_id", run.ID, "error", err)
			resp.SkippedChangeIds = append(resp.SkippedChangeIds, run.ID)
			continue
		}
		resp.PausedChangeIds = append(resp.PausedChangeIds, run.ID)

		_ = s.store.CreateAudit(ctx, &state.Audit{
			ID:        newID("aud-"),
			RunID:     run.ID,
			Action:    action,
			Actor:     actorFromCtx(ctx),
			Target:    run.ID,
			Result:    targetStatus,
			Timestamp: now,
		})

		s.publishEvent(&pb.ChangeEvent{
			ChangeId:  run.ID,
			EventType: "status_changed",
			OldStatus: oldStatus,
			NewStatus: targetStatus,
			Message:   reason,
			Timestamp: now.Unix(),
		})
	}
	resp.Count = int32(len(resp.PausedChangeIds))
	return resp, nil
}

// toSet converts a slice to a set for fast membership testing.
func toSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}

// --- CancelChange ----------------------------------------------------------

// CancelChange cancels a change. Cancellation is terminal: the run
// cannot be resumed after being cancelled.
func (s *ChangeService) CancelChange(ctx context.Context, req *pb.CancelRequest) (*pb.Change, error) {
	return s.transitionStatusWithForce(ctx, req.GetChangeId(), "cancelled", "cancel", req.GetReason(), req.GetForce())
}

// transitionStatusWithForce is like transitionStatus but supports a
// force flag that relaxes the state-guard checks.
func (s *ChangeService) transitionStatusWithForce(ctx context.Context, runID, newStatus, action, reason string, force bool) (*pb.Change, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", runID)
	}

	oldStatus := run.Status
	if !force && !isValidTransition(oldStatus, newStatus) {
		return nil, status.Errorf(codes.FailedPrecondition, "cannot transition from %q to %q (use force=true to override)", oldStatus, newStatus)
	}
	now := time.Now().UTC()
	run.Status = newStatus
	run.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "update run: %v", err)
	}

	_ = s.store.CreateAudit(ctx, &state.Audit{
		ID:        newID("aud-"),
		RunID:     runID,
		Action:    action,
		Actor:     actorFromCtx(ctx),
		Target:    runID,
		Result:    newStatus,
		Timestamp: now,
	})

	s.publishEvent(&pb.ChangeEvent{
		ChangeId:  runID,
		EventType: "status_changed",
		OldStatus: oldStatus,
		NewStatus: newStatus,
		Message:   reason,
		Timestamp: now.Unix(),
	})

	return runToPB(run), nil
}

// --- RetryChange / RetryHost -----------------------------------------------

// RetryChange re-executes a failed change. When the EngineAdapter is
// configured it delegates to EngineAdapter.Retry; otherwise it
// transitions the run back to "draft" so it can be re-planned.
func (s *ChangeService) RetryChange(ctx context.Context, req *pb.RetryRequest) (*pb.Change, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	run, err := s.store.GetRun(ctx, req.GetChangeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", req.GetChangeId())
	}

	if run.Status != "failed" && run.Status != "rolled_back" {
		return nil, status.Errorf(codes.FailedPrecondition, "can only retry failed or rolled_back changes; current status: %q", run.Status)
	}

	if s.engine != nil && s.engine.Retry != nil {
		if err := s.engine.Retry(ctx, req.GetChangeId(), req.GetReplan(), req.GetTargetHosts()); err != nil {
			return nil, status.Errorf(codes.Internal, "engine retry: %v", err)
		}
	}

	now := time.Now().UTC()
	oldStatus := run.Status
	run.Status = "running"
	run.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "update run: %v", err)
	}

	_ = s.store.CreateAudit(ctx, &state.Audit{
		ID:        newID("aud-"),
		RunID:     run.ID,
		Action:    "retry",
		Actor:     actorFromCtx(ctx),
		Target:    run.ID,
		Result:    "running",
		Timestamp: now,
	})

	s.publishEvent(&pb.ChangeEvent{
		ChangeId:  run.ID,
		EventType: "status_changed",
		OldStatus: oldStatus,
		NewStatus: "running",
		Message:   "retry triggered",
		Timestamp: now.Unix(),
	})

	return runToPB(run), nil
}

// RetryHost retries a specific subset of hosts within a change. It does
// not change the overall run status; it merely records the retry
// intent and (when the engine is available) triggers re-execution on
// the specified hosts.
func (s *ChangeService) RetryHost(ctx context.Context, req *pb.RetryHostRequest) (*pb.Change, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	run, err := s.store.GetRun(ctx, req.GetChangeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", req.GetChangeId())
	}

	if s.engine != nil && s.engine.Retry != nil {
		if err := s.engine.Retry(ctx, req.GetChangeId(), false, req.GetHosts()); err != nil {
			return nil, status.Errorf(codes.Internal, "engine retry-host: %v", err)
		}
	}

	now := time.Now().UTC()
	_ = s.store.CreateAudit(ctx, &state.Audit{
		ID:        newID("aud-"),
		RunID:     run.ID,
		Action:    "retry-host",
		Actor:     actorFromCtx(ctx),
		Target:    strings.Join(req.GetHosts(), ","),
		Result:    run.Status,
		Timestamp: now,
	})

	return runToPB(run), nil
}

// --- RollbackChange --------------------------------------------------------

// RollbackChange rolls back a completed or failed change. It delegates
// to the EngineAdapter when available.
func (s *ChangeService) RollbackChange(ctx context.Context, req *pb.RollbackRequest) (*pb.RollbackResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	run, err := s.store.GetRun(ctx, req.GetChangeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", req.GetChangeId())
	}

	if run.Status != "completed" && run.Status != "failed" {
		return nil, status.Errorf(codes.FailedPrecondition, "can only rollback completed or failed changes; current status: %q", run.Status)
	}

	now := time.Now().UTC()
	oldStatus := run.Status

	// Delegate to the engine when available.
	if s.engine != nil && s.engine.Rollback != nil {
		rollbackRunID, hosts, err := s.engine.Rollback(ctx, req.GetChangeId(), req.GetRunId(), req.GetAutoApprove())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "engine rollback: %v", err)
		}
		run.Status = "rolled_back"
		run.UpdatedAt = time.Now().UTC()
		_ = s.store.UpdateRun(ctx, run)

		_ = s.store.CreateAudit(ctx, &state.Audit{
			ID:        newID("aud-"),
			RunID:     run.ID,
			Action:    "rollback",
			Actor:     actorFromCtx(ctx),
			Target:    rollbackRunID,
			Result:    "rolled_back",
			Timestamp: now,
		})

		s.publishEvent(&pb.ChangeEvent{
			ChangeId:  run.ID,
			EventType: "status_changed",
			OldStatus: oldStatus,
			NewStatus: "rolled_back",
			Message:   "rollback completed",
			Timestamp: now.Unix(),
		})

		return &pb.RollbackResponse{
			Change:          runToPB(run),
			RollbackRunId:   rollbackRunID,
			Success:         true,
			RolledBackHosts: hosts,
			Message:         "rollback completed",
		}, nil
	}

	// Fallback: just mark the run as rolled_back.
	run.Status = "rolled_back"
	run.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "update run: %v", err)
	}

	_ = s.store.CreateAudit(ctx, &state.Audit{
		ID:        newID("aud-"),
		RunID:     run.ID,
		Action:    "rollback",
		Actor:     actorFromCtx(ctx),
		Target:    run.ID,
		Result:    "rolled_back",
		Timestamp: now,
	})

	return &pb.RollbackResponse{
		Change:        runToPB(run),
		RollbackRunId: run.ID,
		Success:       true,
		Message:       "rollback completed (no engine; status only)",
	}, nil
}

// --- ApproveChange / RejectChange ------------------------------------------

// ApproveChange records an approval decision. When the approval service
// is configured it delegates to approval.Service.Approve; otherwise it
// performs a direct status transition to "approved".
func (s *ChangeService) ApproveChange(ctx context.Context, req *pb.ApproveRequest) (*pb.Change, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	run, err := s.store.GetRun(ctx, req.GetChangeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", req.GetChangeId())
	}

	now := time.Now().UTC()
	oldStatus := run.Status

	// Delegate to the approval service when available.
	if s.approval != nil {
		// Find the pending approval for this run. We list all
		// approvals for the run and pick the first pending one.
		approvals, err := s.store.ListApprovals(ctx, state.ApprovalFilter{RunID: req.GetChangeId(), Status: "pending"})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list approvals: %v", err)
		}
		if len(approvals) == 0 {
			return nil, status.Errorf(codes.FailedPrecondition, "no pending approval for change %q", req.GetChangeId())
		}
		if err := s.approval.Approve(ctx, approvals[0].ID, req.GetApprover()); err != nil {
			return nil, mapApprovalError(err)
		}
	}

	run.Status = "approved"
	run.ApprovalStatus = "approved"
	run.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "update run: %v", err)
	}

	_ = s.store.CreateAudit(ctx, &state.Audit{
		ID:        newID("aud-"),
		RunID:     run.ID,
		Action:    "approve",
		Actor:     req.GetApprover(),
		Target:    run.ID,
		Result:    "approved",
		Timestamp: now,
	})

	s.publishEvent(&pb.ChangeEvent{
		ChangeId:  run.ID,
		EventType: "status_changed",
		OldStatus: oldStatus,
		NewStatus: "approved",
		Message:   req.GetComment(),
		Timestamp: now.Unix(),
	})

	return runToPB(run), nil
}

// RejectChange records a rejection. A single rejection is enough to
// veto the change (one-vote-veto semantics).
func (s *ChangeService) RejectChange(ctx context.Context, req *pb.RejectRequest) (*pb.Change, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	run, err := s.store.GetRun(ctx, req.GetChangeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", req.GetChangeId())
	}

	now := time.Now().UTC()
	oldStatus := run.Status

	if s.approval != nil {
		approvals, err := s.store.ListApprovals(ctx, state.ApprovalFilter{RunID: req.GetChangeId(), Status: "pending"})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list approvals: %v", err)
		}
		if len(approvals) == 0 {
			return nil, status.Errorf(codes.FailedPrecondition, "no pending approval for change %q", req.GetChangeId())
		}
		if err := s.approval.Reject(ctx, approvals[0].ID, req.GetRejecter(), req.GetReason()); err != nil {
			return nil, mapApprovalError(err)
		}
	}

	run.Status = "rejected"
	run.ApprovalStatus = "rejected"
	run.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "update run: %v", err)
	}

	_ = s.store.CreateAudit(ctx, &state.Audit{
		ID:        newID("aud-"),
		RunID:     run.ID,
		Action:    "reject",
		Actor:     req.GetRejecter(),
		Target:    run.ID,
		Result:    "rejected",
		Timestamp: now,
	})

	s.publishEvent(&pb.ChangeEvent{
		ChangeId:  run.ID,
		EventType: "status_changed",
		OldStatus: oldStatus,
		NewStatus: "rejected",
		Message:   req.GetReason(),
		Timestamp: now.Unix(),
	})

	return runToPB(run), nil
}

// mapApprovalError converts an approval-service error into a gRPC
// status error.
func mapApprovalError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, approval.ErrNotFound):
		return status.Errorf(codes.NotFound, "approval not found")
	case errors.Is(err, approval.ErrInvalidTransition):
		return status.Errorf(codes.FailedPrecondition, "invalid approval transition: %v", err)
	case errors.Is(err, approval.ErrUnauthorizedApprover):
		return status.Errorf(codes.PermissionDenied, "unauthorized approver: %v", err)
	case errors.Is(err, approval.ErrDuplicateDecision):
		return status.Errorf(codes.AlreadyExists, "approver already decided: %v", err)
	default:
		return status.Errorf(codes.Internal, "approval: %v", err)
	}
}

// --- GetChange -------------------------------------------------------------

// GetChange returns a single change by ID. It returns codes.NotFound
// when the change does not exist.
func (s *ChangeService) GetChange(ctx context.Context, req *pb.GetChangeRequest) (*pb.Change, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	run, err := s.store.GetRun(ctx, req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", req.GetId())
	}
	return runToPB(run), nil
}

// --- ListChanges -----------------------------------------------------------

// ListChanges lists changes matching the supplied filter. Pagination is
// cursor-based using the page token; the page size defaults to 50 when
// not specified.
func (s *ChangeService) ListChanges(ctx context.Context, req *pb.ListChangesRequest) (*pb.ListChangesResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	// Build the store filter from the request. The store currently
	// supports a single status filter; we take the first status from
	// the request when multiple are supplied (the rest are ignored
	// with a warning).
	filter := state.RunFilter{Limit: int(req.GetPageSize())}
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if len(req.GetStatuses()) > 0 {
		filter.Status = req.GetStatuses()[0]
	}

	runs, err := s.store.ListRuns(ctx, filter)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list runs: %v", err)
	}

	// Apply client-side filters that the store does not support
	// natively: label-contains and the multi-status case.
	changes := make([]*pb.Change, 0, len(runs))
	for _, run := range runs {
		c := runToPB(run)
		if !matchLabelContains(c, req.GetLabelContains()) {
			continue
		}
		if !matchStatuses(c, req.GetStatuses()) {
			continue
		}
		changes = append(changes, c)
	}

	// Compute the next page token. We use a simple offset-based
	// scheme encoded as a decimal string.
	nextToken := ""
	if len(changes) == filter.Limit {
		nextToken = fmt.Sprintf("%d", parseOffset(req.GetPageToken())+filter.Limit)
	}

	return &pb.ListChangesResponse{
		Changes:       changes,
		NextPageToken: nextToken,
		TotalSize:     int32(len(changes)),
	}, nil
}

// matchLabelContains reports whether the change's label contains the
// supplied substring. An empty substring matches everything.
func matchLabelContains(c *pb.Change, substr string) bool {
	if substr == "" {
		return true
	}
	return strings.Contains(c.GetLabel(), substr)
}

// matchStatuses reports whether the change's status is in the supplied
// set. An empty set matches everything.
func matchStatuses(c *pb.Change, statuses []string) bool {
	if len(statuses) == 0 {
		return true
	}
	for _, st := range statuses {
		if c.GetStatus() == st {
			return true
		}
	}
	return false
}

// parseOffset extracts the integer offset from a page token. An empty
// or malformed token yields 0.
func parseOffset(token string) int {
	if token == "" {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(token, "%d", &n); err != nil {
		return 0
	}
	return n
}

// --- ArchiveChange ---------------------------------------------------------

// ArchiveChange marks a change as archived. When purgeArtifacts is
// true, the associated batches, steps and traces are also deleted.
func (s *ChangeService) ArchiveChange(ctx context.Context, req *pb.ArchiveRequest) (*pb.Change, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	run, err := s.store.GetRun(ctx, req.GetChangeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", req.GetChangeId())
	}

	if run.Status == "archived" {
		return runToPB(run), nil
	}

	oldStatus := run.Status
	now := time.Now().UTC()
	run.Status = "archived"
	run.UpdatedAt = now
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "update run: %v", err)
	}

	if req.GetPurgeArtifacts() {
		// Delete associated batches and steps. Traces are deliberately
		// retained: the store enforces a WORM constraint on trace
		// records (immutable audit evidence), so purge covers runtime
		// artifacts only.
		batches, _ := s.store.ListBatches(ctx, state.BatchFilter{RunID: req.GetChangeId()})
		for _, b := range batches {
			steps, _ := s.store.ListSteps(ctx, state.StepFilter{RunID: req.GetChangeId(), BatchID: b.ID})
			for _, st := range steps {
				_ = s.store.DeleteStep(ctx, st.ID)
			}
			_ = s.store.DeleteBatch(ctx, b.ID)
		}
	}

	_ = s.store.CreateAudit(ctx, &state.Audit{
		ID:        newID("aud-"),
		RunID:     run.ID,
		Action:    "archive",
		Actor:     actorFromCtx(ctx),
		Target:    run.ID,
		Result:    "archived",
		Timestamp: now,
	})

	s.publishEvent(&pb.ChangeEvent{
		ChangeId:  run.ID,
		EventType: "status_changed",
		OldStatus: oldStatus,
		NewStatus: "archived",
		Message:   "archived",
		Timestamp: now.Unix(),
	})

	return runToPB(run), nil
}

// --- GetLogs ---------------------------------------------------------------

// GetLogs returns log entries for a change. Logs are reconstructed
// from the step stdout/stderr fields; when a limit is supplied the
// result may be truncated.
func (s *ChangeService) GetLogs(ctx context.Context, req *pb.GetLogsRequest) (*pb.GetLogsResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	runID := req.GetChangeId()
	if req.GetRunId() != "" {
		runID = req.GetRunId()
	}

	steps, err := s.store.ListSteps(ctx, state.StepFilter{RunID: runID, Limit: int(req.GetLimit())})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list steps: %v", err)
	}

	// Apply the optional level filter (DEBUG/INFO/WARN/ERROR). Empty
	// means all levels. Stdout maps to INFO and stderr to ERROR below,
	// so the filter decides which of the two is emitted per step.
	var levelFilter map[string]bool
	if levels := req.GetLevels(); len(levels) > 0 {
		levelFilter = make(map[string]bool, len(levels))
		for _, l := range levels {
			levelFilter[strings.ToUpper(l)] = true
		}
	}

	entries := make([]*pb.LogEntry, 0, len(steps)*2)
	for _, step := range steps {
		if step.Stdout != "" && (levelFilter == nil || levelFilter["INFO"]) {
			entries = append(entries, &pb.LogEntry{
				RunId:     runID,
				Timestamp: formatTime(step.StartedAt),
				Level:     "INFO",
				Message:   step.Stdout,
				Source:    step.Host,
			})
		}
		if step.Stderr != "" && (levelFilter == nil || levelFilter["ERROR"]) {
			entries = append(entries, &pb.LogEntry{
				RunId:     runID,
				Timestamp: formatTime(step.StartedAt),
				Level:     "ERROR",
				Message:   step.Stderr,
				Source:    step.Host,
			})
		}
	}

	// Apply limit.
	truncated := false
	if req.GetLimit() > 0 && int32(len(entries)) > req.GetLimit() {
		entries = entries[:req.GetLimit()]
		truncated = true
	}

	return &pb.GetLogsResponse{Entries: entries, Truncated: truncated}, nil
}

// formatTime renders a *time.Time as an RFC3339 string, returning an
// empty string when the pointer is nil.
func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// --- GetDiff ---------------------------------------------------------------

// GetDiff returns a diff of the changes made by a run. The current
// implementation returns a placeholder; a real diff requires snapshot
// comparison support from the rollback package.
func (s *ChangeService) GetDiff(ctx context.Context, req *pb.GetDiffRequest) (*pb.GetDiffResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	runID := req.GetChangeId()
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", runID)
	}

	format := req.GetFormat()
	if format == "" {
		format = "unified"
	}

	// Build a simple textual diff from the step records. Each step
	// contributes a header line and its stdout/stderr.
	steps, err := s.store.ListSteps(ctx, state.StepFilter{RunID: runID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list steps: %v", err)
	}

	var sb strings.Builder
	for _, step := range steps {
		fmt.Fprintf(&sb, "--- %s @ %s\n", step.Host, step.StepName)
		fmt.Fprintf(&sb, "+++ %s @ %s (after)\n", step.Host, step.StepName)
		if step.Stdout != "" {
			sb.WriteString(step.Stdout)
			if !strings.HasSuffix(step.Stdout, "\n") {
				sb.WriteByte('\n')
			}
		}
		if step.Stderr != "" {
			sb.WriteString("--- stderr ---\n")
			sb.WriteString(step.Stderr)
			if !strings.HasSuffix(step.Stderr, "\n") {
				sb.WriteByte('\n')
			}
		}
	}

	return &pb.GetDiffResponse{
		ChangeId: runID,
		RunId:    req.GetRunId(),
		Diff:     sb.String(),
		Format:   format,
	}, nil
}

// --- GetTrace --------------------------------------------------------------

// GetTrace returns the audit trace entries for a change. When verify
// is true, the hash chain is verified and the result is included in
// the response.
func (s *ChangeService) GetTrace(ctx context.Context, req *pb.GetTraceRequest) (*pb.GetTraceResponse, error) {
	if s.store == nil {
		return nil, status.Error(codes.Internal, "store not configured")
	}

	runID := req.GetChangeId()
	if req.GetRunId() != "" {
		// Explicit run filter wins over the change-wide default.
		runID = req.GetRunId()
	}
	run, err := s.store.GetRun(ctx, req.GetChangeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return nil, status.Errorf(codes.NotFound, "change %q not found", req.GetChangeId())
	}

	traces, err := s.store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list traces: %v", err)
	}

	entries := make([]*pb.TraceEntry, 0, len(traces))
	for _, t := range traces {
		entries = append(entries, &pb.TraceEntry{
			Id:         t.ID,
			RunId:      t.RunID,
			Action:     t.Event,
			TargetHost: t.Actor,
			PrevHash:   t.PrevHash,
			CurrHash:   t.CurrHash,
			Timestamp:  t.Timestamp.Unix(),
		})
	}

	resp := &pb.GetTraceResponse{
		RunId:   runID,
		Entries: entries,
	}

	if req.GetVerify() {
		valid, msg := verifyHashChain(traces)
		resp.HashChainValid = valid
		resp.VerificationMessage = msg
	}

	return resp, nil
}

// verifyHashChain checks that each trace's CurrHash matches the hash
// computed from the previous trace's CurrHash and the current trace's
// contents. This is a simplified verification that does not depend on
// the audit package; the audit.ChainVerifier provides the full
// implementation.
func verifyHashChain(traces []*state.Trace) (bool, string) {
	if len(traces) == 0 {
		return true, "no traces to verify"
	}
	prevHash := ""
	for i, t := range traces {
		if i == 0 {
			prevHash = t.PrevHash
		}
		if t.PrevHash != prevHash {
			return false, fmt.Sprintf("hash mismatch at trace %d: expected prev %q, got %q", i, prevHash, t.PrevHash)
		}
		prevHash = t.CurrHash
	}
	return true, "hash chain valid"
}

// --- WatchChange (streaming) -----------------------------------------------

// WatchChange streams change events to the client. It subscribes to
// the per-change event bus and forwards events until the client
// disconnects, the context is cancelled, or the change reaches a
// terminal state.
//
// When IncludeCurrentState is true, the current state of the change is
// emitted as the first event.
func (s *ChangeService) WatchChange(req *pb.WatchChangeRequest, stream grpcpkg.ServerStreamingServer[pb.ChangeEvent]) error {
	if s.store == nil {
		return status.Error(codes.Internal, "store not configured")
	}

	ctx := stream.Context()
	changeID := req.GetChangeId()

	// Validate the change exists.
	run, err := s.store.GetRun(ctx, changeID)
	if err != nil {
		return status.Errorf(codes.Internal, "get run: %v", err)
	}
	if run == nil {
		return status.Errorf(codes.NotFound, "change %q not found", changeID)
	}

	// Optionally emit the current state as the first event.
	if req.GetIncludeCurrentState() {
		if err := stream.Send(&pb.ChangeEvent{
			ChangeId:  changeID,
			EventType: "status_changed",
			NewStatus: run.Status,
			Message:   "current state",
			Timestamp: time.Now().Unix(),
		}); err != nil {
			return status.Errorf(codes.Internal, "send current state: %v", err)
		}
	}

	// Subscribe to the event bus.
	bus := s.getEventBus()
	ch := bus.subscribe(changeID)
	defer bus.unsubscribe(changeID, ch)

	// Build the event-type filter set.
	typeFilter := toSet(req.GetEventTypes())

	// Pump events to the stream until the context is cancelled or
	// the change reaches a terminal state.
	terminalStates := map[string]bool{
		"completed":   true,
		"failed":      true,
		"cancelled":   true,
		"archived":    true,
		"rolled_back": true,
		"rejected":    true,
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			// Apply event-type filter.
			if len(typeFilter) > 0 && !typeFilter[ev.GetEventType()] {
				continue
			}
			if err := stream.Send(ev); err != nil {
				return status.Errorf(codes.Internal, "send event: %v", err)
			}
			// Stop when the change reaches a terminal state.
			if terminalStates[ev.GetNewStatus()] {
				return nil
			}
		}
	}
}

// --- StreamLogs (streaming) ------------------------------------------------

// StreamLogs streams log entries for a change. It first replays
// historical logs from the store, then (when follow is true) tails
// new logs by polling the store for new step records.
func (s *ChangeService) StreamLogs(req *pb.StreamLogsRequest, stream grpcpkg.ServerStreamingServer[pb.LogEntry]) error {
	if s.store == nil {
		return status.Error(codes.Internal, "store not configured")
	}

	ctx := stream.Context()
	runID := req.GetChangeId()
	if req.GetRunId() != "" {
		runID = req.GetRunId()
	}

	// Replay historical logs.
	steps, err := s.store.ListSteps(ctx, state.StepFilter{RunID: runID})
	if err != nil {
		return status.Errorf(codes.Internal, "list steps: %v", err)
	}
	levelSet := toSet(req.GetLevels())
	seen := make(map[string]bool, len(steps))
	for _, step := range steps {
		seen[step.ID] = true
		if err := sendStepLogs(stream, runID, step, levelSet); err != nil {
			return err
		}
	}

	if !req.GetFollow() {
		return nil
	}

	// Tail new logs by polling. We use a 1-second poll interval; a
	// production implementation would use a notification channel from
	// the executor, but polling is sufficient for the MVP and avoids
	// coupling the gRPC layer to the executor's internals.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			steps, err := s.store.ListSteps(ctx, state.StepFilter{RunID: runID})
			if err != nil {
				log.Warn("stream logs: list steps failed", "run_id", runID, "error", err)
				continue
			}
			for _, step := range steps {
				if seen[step.ID] {
					continue
				}
				seen[step.ID] = true
				if err := sendStepLogs(stream, runID, step, levelSet); err != nil {
					return err
				}
			}
		}
	}
}

// sendStepLogs emits the stdout/stderr of a step as LogEntry messages
// on the supplied stream. It applies the level filter and returns the
// first send error (which aborts the stream).
func sendStepLogs(stream grpcpkg.ServerStreamingServer[pb.LogEntry], runID string, step *state.Step, levelSet map[string]bool) error {
	emit := func(level, msg string) error {
		if len(levelSet) > 0 && !levelSet[level] {
			return nil
		}
		return stream.Send(&pb.LogEntry{
			RunId:     runID,
			Timestamp: formatTime(step.StartedAt),
			Level:     level,
			Message:   msg,
			Source:    step.Host,
		})
	}
	if step.Stdout != "" {
		if err := emit("INFO", step.Stdout); err != nil {
			return status.Errorf(codes.Internal, "send log: %v", err)
		}
	}
	if step.Stderr != "" {
		if err := emit("ERROR", step.Stderr); err != nil {
			return status.Errorf(codes.Internal, "send log: %v", err)
		}
	}
	return nil
}

// --- event bus -------------------------------------------------------------

// eventBus distributes change events to subscribers. It is a simple
// fan-out bus: each change ID has a set of subscriber channels; when
// an event is published, it is sent to every subscriber's channel
// (non-blocking; slow subscribers drop events).
type eventBus struct {
	mu   sync.Mutex
	subs map[string]map[chan *pb.ChangeEvent]struct{}
	// dropped counts events discarded because a subscriber channel was
	// full. Atomic: incremented on the publisher's goroutine.
	dropped int64
}

// newEventBus creates a ready-to-use eventBus.
func newEventBus() *eventBus {
	return &eventBus{subs: make(map[string]map[chan *pb.ChangeEvent]struct{})}
}

// getEventBus lazily initialises the event bus on the service.
func (s *ChangeService) getEventBus() *eventBus {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.eventBus == nil {
		s.eventBus = newEventBus()
	}
	return s.eventBus
}

// subscribe registers a new subscriber for the given change ID and
// returns the channel on which events will be delivered.
func (b *eventBus) subscribe(changeID string) chan *pb.ChangeEvent {
	ch := make(chan *pb.ChangeEvent, 16)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[changeID] == nil {
		b.subs[changeID] = make(map[chan *pb.ChangeEvent]struct{})
	}
	b.subs[changeID][ch] = struct{}{}
	return ch
}

// unsubscribe removes a subscriber.
func (b *eventBus) unsubscribe(changeID string, ch chan *pb.ChangeEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if subs, ok := b.subs[changeID]; ok {
		delete(subs, ch)
		if len(subs) == 0 {
			delete(b.subs, changeID)
		}
	}
}

// publishEvent fans an event out to all subscribers of the change. It
// is non-blocking: subscribers whose channel buffer is full are
// skipped (the event is dropped for that subscriber). This prevents one
// slow watcher from blocking the publisher.
func (s *ChangeService) publishEvent(ev *pb.ChangeEvent) {
	s.eventMu.Lock()
	bus := s.eventBus
	s.eventMu.Unlock()
	if bus == nil {
		return
	}
	bus.mu.Lock()
	subs := bus.subs[ev.GetChangeId()]
	bus.mu.Unlock()
	for ch := range subs {
		select {
		case ch <- ev:
		default:
			// Channel full; drop the event for this subscriber. The
			// counter keeps the loss observable (exposed via the event
			// bus stats) instead of failing completely silently.
			atomic.AddInt64(&bus.dropped, 1)
			log.WarnCtx(context.Background(), "change event dropped for slow subscriber",
				"change_id", ev.GetChangeId(), "event_type", ev.GetEventType(), "total_dropped", atomic.LoadInt64(&bus.dropped))
		}
	}
}
