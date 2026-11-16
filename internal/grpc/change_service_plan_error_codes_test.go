// change_service_plan_error_codes_test.go — a refusal the engine already
// classified must reach the caller with that classification.
//
// Why this matters: the engine layer (internal/wiring) knows whether a plan
// request was malformed (InvalidArgument) or whether the state forbids it
// (FailedPrecondition). PlanChange used to bury EVERY engine error in
// codes.Internal, so an operator asking for a plan without targets got a 500
// — a server fault for a request that was simply wrong. The house rule, one
// screen up in the same file, says the opposite: "A closed change window, an
// unexecutable declared gate and a calendar freeze are all refusals the
// operator can act on, not server faults."
//
// The classified error is DEFINED HERE rather than imported from
// internal/wiring: wiring imports internal/grpc (the EngineAdapter seam), so
// internal/grpc cannot import wiring — that is why the classification travels
// on the error value (see internal/wiring/errors.go) instead of through a
// dependency. This test pins the mechanism on the gateway side; the wiring
// side pins the codes it produces (internal/wiring/errors_test.go).
package grpc

import (
	"context"
	"errors"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/risk"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// classifiedRefusal is the shape internal/wiring produces: a cause plus a
// gRPC code, reachable through status.FromError and errors.Is.
type classifiedRefusal struct {
	code codes.Code
	err  error
}

func (e *classifiedRefusal) Error() string { return e.err.Error() }
func (e *classifiedRefusal) Unwrap() error { return e.err }
func (e *classifiedRefusal) GRPCStatus() *status.Status {
	return status.New(e.code, e.err.Error())
}

func TestPlanChange_KeepsTheClassifiedRefusal(t *testing.T) {
	cases := []struct {
		name string
		code codes.Code
	}{
		{"malformed request", codes.InvalidArgument},
		{"state refusal", codes.FailedPrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cause := errors.New("wiring: target_hosts is required to plan a change")
			engine := &recordingEngine{planErr: &classifiedRefusal{code: tc.code, err: cause}}
			svc, _ := newTestChangeServiceWithEngine(t, engine.adapter())
			created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "plan-code"})
			require.NoError(t, err)

			_, err = svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
				ChangeId:    created.GetId(),
				TargetHosts: []string{"web-1"},
			})
			require.Error(t, err)
			assert.Equal(t, tc.code, status.Code(err), "the refusal must keep the code the engine gave it")
			assert.ErrorIs(t, err, cause, "the cause must stay reachable")
		})
	}
}

// TestApplyChange_KeepsTheClassifiedRefusal pins the same rule on the apply
// path: a plan artifact that is missing or has drifted is a state the
// operator can fix by re-planning (412), not a server fault (500).
func TestApplyChange_KeepsTheClassifiedRefusal(t *testing.T) {
	cause := errors.New("wiring: stored plan does not match its plan hash (drift or corruption); re-plan required")
	rec := &recordingEngine{
		runID:  "exec-drift",
		runErr: &classifiedRefusal{code: codes.FailedPrecondition, err: cause},
	}
	store := newTestStore(t)
	svc := NewChangeService(store, rec.adapter(), nil, nil)

	run := &state.Run{
		ID: "run-drift", WorkflowName: "wf", Status: "draft",
		Creator: "alice", CreatedAt: timeNowUTC(),
		PlanHash: "hash-v2", ApprovalLevel: risk.LevelStandard,
	}
	require.NoError(t, store.CreateRun(context.Background(), run))
	persistPlanOnRun(t, store, "run-drift")

	_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId: "run-drift", AutoApprove: true,
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.ErrorIs(t, err, cause)
}

// TestApplyChange_UnclassifiedEngineErrorStaysInternal guards the other side
// of the rule: an engine error that carries NO classification is a server
// fault and must not be laundered into a 412.
func TestApplyChange_UnclassifiedEngineErrorStaysInternal(t *testing.T) {
	rec := &recordingEngine{
		runID:  "exec-boom",
		runErr: errors.New("engine: the disk caught fire"),
	}
	store := newTestStore(t)
	svc := NewChangeService(store, rec.adapter(), nil, nil)

	run := &state.Run{
		ID: "run-boom", WorkflowName: "wf", Status: "draft",
		Creator: "alice", CreatedAt: timeNowUTC(),
		PlanHash: "hash-v2", ApprovalLevel: risk.LevelStandard,
	}
	require.NoError(t, store.CreateRun(context.Background(), run))
	persistPlanOnRun(t, store, "run-boom")

	_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId: "run-boom", AutoApprove: true,
	})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}
