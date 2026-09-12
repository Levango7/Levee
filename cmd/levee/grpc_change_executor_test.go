// grpc_change_executor_test.go pins the cluster v2 resume semantics: an
// interrupted run is re-driven via RetryChange (resumable batch-skipping),
// while a fresh run goes through ApplyChange.
package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// fakeChangeService records which method was called.
type fakeChangeService struct {
	applyCalled bool
	retryCalled bool
	applyReq    *pb.ApplyChangeRequest
	retryReq    *pb.RetryRequest
	applyResp   *pb.ApplyResponse
	retryResp   *pb.Change
	applyErr    error
	retryErr    error
}

func (f *fakeChangeService) ApplyChange(_ context.Context, req *pb.ApplyChangeRequest) (*pb.ApplyResponse, error) {
	f.applyCalled = true
	f.applyReq = req
	return f.applyResp, f.applyErr
}

func (f *fakeChangeService) RetryChange(_ context.Context, req *pb.RetryRequest) (*pb.Change, error) {
	f.retryCalled = true
	f.retryReq = req
	return f.retryResp, f.retryErr
}

func newExecutorTestStore(t *testing.T, run *state.Run) state.Store {
	t.Helper()
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, ":memory:")
	require.NoError(t, err)
	if run != nil {
		require.NoError(t, store.CreateRun(ctx, run))
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestExecute_InterruptedRun_ResumesViaRetry(t *testing.T) {
	svc := &fakeChangeService{
		retryResp: &pb.Change{Status: "completed"},
	}
	store := newExecutorTestStore(t, &state.Run{ID: "r1", Status: "interrupted"})

	ex := &grpcChangeExecutor{svc: svc, store: store}
	result, err := ex.Execute(context.Background(), "r1")

	require.NoError(t, err)
	assert.Equal(t, "completed", result)
	assert.True(t, svc.retryCalled, "interrupted run must resume via RetryChange")
	assert.False(t, svc.applyCalled, "interrupted run must NOT go through ApplyChange")
	assert.False(t, svc.retryReq.GetReplan(), "resume must not replan")
	assert.Equal(t, "r1", svc.retryReq.GetChangeId())
}

func TestExecute_FreshRun_GoesThroughApply(t *testing.T) {
	svc := &fakeChangeService{
		applyResp: &pb.ApplyResponse{Change: &pb.Change{Status: "completed"}, Success: true},
	}
	store := newExecutorTestStore(t, &state.Run{ID: "r1", Status: "approved"})

	ex := &grpcChangeExecutor{svc: svc, store: store}
	result, err := ex.Execute(context.Background(), "r1")

	require.NoError(t, err)
	assert.Equal(t, "completed", result)
	assert.True(t, svc.applyCalled, "fresh run must go through ApplyChange")
	assert.False(t, svc.retryCalled, "fresh run must NOT go through RetryChange")
	assert.Equal(t, "r1", svc.applyReq.GetChangeId())
	assert.False(t, svc.applyReq.GetAutoApprove(), "assigned runs are already approved")
}

func TestExecute_NilStore_GoesThroughApply(t *testing.T) {
	// When no store is wired (defensive), fall back to ApplyChange.
	svc := &fakeChangeService{
		applyResp: &pb.ApplyResponse{Change: &pb.Change{Status: "completed"}, Success: true},
	}

	ex := &grpcChangeExecutor{svc: svc, store: nil}
	result, err := ex.Execute(context.Background(), "r1")

	require.NoError(t, err)
	assert.Equal(t, "completed", result)
	assert.True(t, svc.applyCalled)
	assert.False(t, svc.retryCalled)
}

func TestExecute_RetryRolledBack(t *testing.T) {
	svc := &fakeChangeService{
		retryResp: &pb.Change{Status: "rolled_back"},
	}
	store := newExecutorTestStore(t, &state.Run{ID: "r1", Status: "interrupted"})

	ex := &grpcChangeExecutor{svc: svc, store: store}
	result, err := ex.Execute(context.Background(), "r1")

	require.NoError(t, err)
	assert.Equal(t, "rolled_back", result)
}
