// apply_cas_test.go pins the B1 terminal-write discipline: every settle
// of a running run goes through a compare-and-set from "running", so a
// late executor (a zombie whose lease a failover takeover invalidated, or
// a concurrent pause) can never overwrite the state another actor already
// wrote. These tests stage the race deterministically: the engine stub
// invokes its duringRun hook before returning, and that hook transitions
// the run out from under the executor — so the terminal CAS in the
// handler must observe the loss and defer to the winner.
package grpc

import (
	"context"
	"errors"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// racingEngine is an EngineAdapter stub whose Run closure first invokes
// duringRun (deterministically simulating "another actor moved the run
// while the engine was executing") and then returns the configured
// outcome. The hook may be (re)assigned any time before ApplyChange.
type racingEngine struct {
	runID      string
	runSuccess bool
	runPhase   string
	runErr     error
	duringRun  func(ctx context.Context, changeID string)
}

func (e *racingEngine) adapter() *EngineAdapter {
	return &EngineAdapter{
		Run: func(ctx context.Context, changeID string, autoApprove bool, maxConcurrency int32) (string, bool, string, error) {
			if e.duringRun != nil {
				e.duringRun(ctx, changeID)
			}
			return e.runID, e.runSuccess, e.runPhase, e.runErr
		},
	}
}

func TestApplyChange_TerminalSupersededByTakeoverOnSuccess(t *testing.T) {
	// The engine executes to completion, but while it ran a failover
	// takeover settled the run to "interrupted". The executor's
	// "completed" verdict must NOT overwrite it.
	engine := &racingEngine{runID: "exec-r1", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "cas-sup-succ"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())
	engine.duringRun = func(_ context.Context, changeID string) {
		setRunStatus(t, store, changeID, "interrupted")
	}

	resp, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId:    created.GetId(),
		AutoApprove: true,
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "concurrently changed")
	require.NotNil(t, resp)
	assert.Equal(t, "interrupted", resp.GetChange().GetStatus(),
		"the response must report the takeover's state, not the executor's verdict")

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "interrupted", run.Status,
		"the takeover's interrupted state must survive the executor's late completed verdict")
}

func TestApplyChange_TerminalSupersededByTakeoverOnError(t *testing.T) {
	// The engine returns a fatal error, but a takeover already settled
	// the run to "interrupted" while it executed. The executor's error
	// path must not overwrite the winner either.
	engine := &racingEngine{runID: "exec-r2", runErr: errors.New("disk on fire")}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "cas-sup-err"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())
	engine.duringRun = func(_ context.Context, changeID string) {
		setRunStatus(t, store, changeID, "interrupted")
	}

	resp, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId:    created.GetId(),
		AutoApprove: true,
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.NotNil(t, resp)
	assert.Equal(t, "interrupted", resp.GetChange().GetStatus())

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "interrupted", run.Status)
}

func TestApplyChange_ConcurrentPauseDuringExecutionKeepsPaused(t *testing.T) {
	// A pause slips in while the engine executes. The executor finishing
	// with "completed" must not stomp the pause.
	engine := &racingEngine{runID: "exec-r3", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "cas-pause"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())
	engine.duringRun = func(_ context.Context, changeID string) {
		setRunStatus(t, store, changeID, "paused")
	}

	_, err = svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId:    created.GetId(),
		AutoApprove: true,
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "paused", run.Status, "the pause must survive the executor's late completed verdict")
}
