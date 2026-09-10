package main

import (
	"context"

	"github.com/nexus/levee/internal/grpc/pb"
)

// grpcChangeExecutor adapts the in-process gRPC ChangeService to the
// dispatch.RunExecutor interface, so a worker loop executes assigned runs
// through the exact same apply path every node uses (CAS → engine → evidence
// → terminal CAS). No gRPC wire round-trip: this is the in-process service
// instance the server already constructed.
type grpcChangeExecutor struct {
	svc interface {
		ApplyChange(ctx context.Context, req *pb.ApplyChangeRequest) (*pb.ApplyResponse, error)
	}
}

// Execute applies the run via the ChangeService and maps the gRPC response to
// a terminal result string the dispatch layer persists. A rolled-back apply
// is an outcome (result "rolled_back"), not an error; a hard engine error is
// surfaced as an error and the caller marks the assignment "failed".
func (e *grpcChangeExecutor) Execute(ctx context.Context, runID string) (string, error) {
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
