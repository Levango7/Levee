package main

import (
	"context"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/runstatus"
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
	// Case labels are run statuses, taken from runstatus so this mapping
	// cannot drift from the vocabulary the server writes. The returned
	// strings are the *assignment result* vocabulary
	// (completed | failed | rolled_back — design-cluster-dispatch.md), a
	// deliberately smaller, frozen set — not run statuses.
	case runstatus.StatusRolledBack:
		return state.AssignResultRolledBack, nil
	case runstatus.StatusRolledBackPartial, runstatus.StatusRollbackIncomplete:
		// D-2 v2: the run row keeps the precise verdict; the assignment
		// result vocabulary (completed | failed | rolled_back) records
		// anything that is not a clean outcome as "failed" so dispatch
		// and operators treat it as needing attention.
		return state.AssignResultFailed, nil
	case runstatus.StatusCompleted, "done": // "done" is legacy, not a run status
		return state.AssignResultCompleted, nil
	case runstatus.StatusFailed:
		return state.AssignResultFailed, nil
	default:
		// The resume returned a status that is not a terminal outcome:
		// still running, interrupted again, or something this switch has
		// not seen. The assignment-result vocabulary is frozen at
		// completed|failed|rolled_back and `state=done` requires one of
		// them, so leaking the run status here would put a value the
		// console and the terminal-state recognition do not know into the
		// column. "failed" is the only honest terminal value; the precise
		// status stays on the run row and is named in this log so the two
		// can be joined.
		log.Warn("dispatch: resume returned a non-terminal run status; recording assignment as failed",
			"run_id", runID, "status", resp.GetStatus())
		return state.AssignResultFailed, nil
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
	case resp.GetChange().GetStatus() == runstatus.StatusRolledBack:
		return state.AssignResultRolledBack, nil
	case resp.GetSuccess():
		return state.AssignResultCompleted, nil
	default:
		return state.AssignResultFailed, nil
	}
}
