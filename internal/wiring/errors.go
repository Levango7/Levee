// errors.go — the refusal vocabulary of the wiring layer.
//
// Why this file exists: a refusal the operator can act on (the request named
// no targets, the plan artifact drifted) used to travel as a bare
// fmt.Errorf, so the layer above could only classify it as "internal" — the
// operator saw a 500 for a request that was simply wrong. The house rule is
// the opposite, and it is written down in internal/grpc/change_service.go
// (PlanChange): "A closed change window, an unexecutable declared gate and a
// calendar freeze are all refusals the operator can act on, not server
// faults: report them like the approval-quorum refusal below instead of
// burying them in Internal." A missing target list belongs to that family.
//
// Two properties, one error value:
//
//   - errors.Is(err, ErrTargetHostsRequired) finds the CAUSE, so callers that
//     do import wiring (the CLI, the tests) can branch on the reason;
//   - status.FromError(err) reads the gRPC CODE, so the gateway's
//     writeGRPCError maps it to 400/412 instead of 500.
//
// The second property cannot be achieved by importing wiring from the gateway
// package: wiring already imports internal/grpc for its EngineAdapter seam
// (adapter.go), so internal/grpc importing internal/wiring would be an import
// cycle. Carrying the code on the error itself is what lets the classification
// travel upward without the dependency turning backwards.
package wiring

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The refusals worth naming. Each message is the text the caller sees, so
// the sentinel text IS the user-facing text: an operator reading the error
// learns which rule refused them and what to change.
var (
	// ErrTargetHostsRequired — the plan request named no targets. The
	// request is malformed (a required field is absent), not the state.
	ErrTargetHostsRequired = errors.New("wiring: target_hosts is required to plan a change")

	// ErrTargetsNotFound — the request named hosts that are not in the
	// inventory. Also a malformed request: the names cannot be resolved.
	ErrTargetsNotFound = errors.New("wiring: targets not found in inventory")

	// ErrTargetsRetired — the hosts exist but were retired. This one is a
	// STATE refusal: the same request would plan fine against an active
	// host, so it is FailedPrecondition, not InvalidArgument.
	ErrTargetsRetired = errors.New("wiring: targets are retired")

	// ErrNoPersistedPlan — apply/approve reached a run with no plan
	// artifact. The operator can fix it by planning (and re-approving).
	ErrNoPersistedPlan = errors.New("wiring: change has no persisted plan; plan (and re-approve) before executing")

	// ErrPlanDrift — the stored plan artifact does not match the plan hash
	// recorded on the run. Drift or corruption; re-planning is the remedy.
	ErrPlanDrift = errors.New("wiring: stored plan does not match its plan hash (drift or corruption); re-plan required")
)

// classified is an error that carries BOTH a sentinel cause and a gRPC code.
//
// GRPCStatus makes status.FromError (the gateway's writeGRPCError, and the
// gRPC server's own conversion) read the code; Unwrap makes errors.Is reach
// the sentinel. One value, two consumers, no import cycle.
type classified struct {
	code codes.Code
	err  error
}

func (e *classified) Error() string { return e.err.Error() }
func (e *classified) Unwrap() error { return e.err }
func (e *classified) GRPCStatus() *status.Status {
	return status.New(e.code, e.err.Error())
}

// invalidArgument marks a MALFORMED REQUEST: the request itself cannot be
// honoured no matter what the state is (HTTP 400 / gRPC InvalidArgument).
func invalidArgument(err error) error {
	return &classified{code: codes.InvalidArgument, err: err}
}

// failedPrecondition marks a STATE refusal: the request is well-formed but
// the current state forbids it, and the operator can change that state
// (HTTP 412 / gRPC FailedPrecondition).
func failedPrecondition(err error) error {
	return &classified{code: codes.FailedPrecondition, err: err}
}
