package main

import (
	"context"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// grpcChangeExecutor adapts the in-process gRPC ChangeService to the
// dispatch.RunExecutor interface, so a worker loop executes assigned runs
// through the exact same apply path every node uses (CAS → engine → evidence
// → terminal CAS). No gRPC wire round-trip: this is the in-process service
// instance the server already constructed.
//
// Cluster v2 (batch resumption): when a run was previously interrupted by the
// failover takeover and is being re-dispatched, Execute calls RetryChange
// (replan=false) instead of ApplyChange. The engine's executePlan(resumable=true)
// then skips batches whose every (step, host) combination already has a
// "success" row, resuming from where the dead worker broke. Fresh runs and
// non-interrupted statuses go through the normal ApplyChange path.
type grpcChangeExecutor struct {
	svc   stateChangeService
	store state.Store
}

// stateChangeService is the subset of the gRPC ChangeService the executor
// calls. Kept narrow so tests can stub it.
type stateChangeService interface {
	ApplyChange(ctx context.Context, req *pb.ApplyChangeRequest) (*pb.ApplyResponse, error)
	RetryChange(ctx context.Context, req *pb.RetryRequest) (*pb.Change, error)
}

// Execute applies the run via the ChangeService and maps the gRPC response to
// a terminal result string the dispatch layer persists. A rolled-back apply
// is an outcome (result "rolled_back"), not an error; a hard engine error is
// surfaced as an error and the caller marks the assignment "failed".
func (e *grpcChangeExecutor) Execute(ctx context.Context, runID string) (string, error) {
	// Cluster v2: check if the run was previously interrupted. If so, resume
	// via RetryChange (which uses the engine's resumable batch-skipping path)
	// instead of re-applying the whole plan from scratch.
	if e.store != nil {
		run, err := e.store.GetRun(ctx, runID)
		if err != nil {
			return "", err
		}
		if run != nil && run.Status == "interrupted" {
			return e.retry(ctx, runID)
		}
	}
	return e.apply(ctx, runID)
}

// retry calls RetryChange (replan=false) to resume an interrupted run.
func (e *grpcChangeExecutor) retry(ctx context.Context, runID string) (string, error) {
	resp, err := e.svc.RetryChange(ctx, &pb.RetryRequest{
		ChangeId: runID,
		Replan:   false, // resume from the persisted plan; don't re-derive
	})
	if err != nil {
		return "", err
	}
	switch resp.GetStatus() {
	case "rolled_back":
		return "rolled_back", nil
	case "completed", "done":
		return "completed", nil
	case "failed":
		return "failed", nil
	default:
		// interrupted again or still running: surface as status for the
		// assignment layer to handle.
		return resp.GetStatus(), nil
	}
}

// apply calls ApplyChange for a fresh run.
func (e *grpcChangeExecutor) apply(ctx context.Context, runID string) (string, error) {
	resp, err := e.svc.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:    runID,
		AutoApprove: false, // assigned runs are already approved
	})
	if err != nil {
		return "", err
	}
	// The apply succeeded at the RPC level; map the outcome to a result.
	switch {
	case resp.GetChange().GetStatus() == "rolled_back":
		return "rolled_back", nil
	case resp.GetSuccess():
		return "completed", nil
	default:
		return "failed", nil
	}
}
