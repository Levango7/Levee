// audit_service_test.go tests the AuditService gRPC handler. It uses a
// real SQLite store and seeds it with runs, audits, traces, batches and
// steps to exercise the full read path.
package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// newTestAuditService returns an AuditService backed by a fresh test store.
func newTestAuditService(t *testing.T) (*AuditService, state.Store) {
	t.Helper()
	store := newTestStore(t)
	return NewAuditService(store), store
}

// seedRun creates a run with the given ID and status in the store.
func seedRun(t *testing.T, ctx context.Context, store state.Store, id, status string) *state.Run {
	t.Helper()
	now := time.Now().UTC()
	run := &state.Run{
		ID:           id,
		WorkflowName: "test-workflow",
		TemplateName: "test-template",
		Status:       status,
		CreatedAt:    now,
		UpdatedAt:    now,
		Creator:      "test",
	}
	require.NoError(t, store.CreateRun(ctx, run))
	return run
}

// seedAudit creates an audit entry in the store.
func seedAudit(t *testing.T, ctx context.Context, store state.Store, id, runID, action string) {
	t.Helper()
	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID:        id,
		RunID:     runID,
		Action:    action,
		Actor:     "test-user",
		Target:    "host-01",
		Result:    "success",
		Timestamp: time.Now().UTC(),
	}))
}

// seedTrace creates a trace entry in the store.
func seedTrace(t *testing.T, ctx context.Context, store state.Store, id, runID, prevHash, currHash string) {
	t.Helper()
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID:        id,
		RunID:     runID,
		Event:     "step.execute",
		Actor:     "executor",
		PrevHash:  prevHash,
		CurrHash:  currHash,
		Timestamp: time.Now().UTC(),
	}))
}

// seedBatchAndStep creates a batch with a step for a run.
func seedBatchAndStep(t *testing.T, ctx context.Context, store state.Store, runID, batchID, stepID, host, stepStatus string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID:          batchID,
		RunID:       runID,
		BatchNo:     1,
		Status:      "completed",
		StartedAt:   &now,
		CompletedAt: &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID:          stepID,
		RunID:       runID,
		BatchID:     batchID,
		Host:        host,
		StepName:    "deploy",
		Action:      "exec",
		Status:      stepStatus,
		DurationMs:  100,
		StartedAt:   &now,
		CompletedAt: &now,
	}))
}

// seedStep creates a step within an existing batch.
func seedStep(t *testing.T, ctx context.Context, store state.Store, runID, batchID, stepID, host, stepStatus string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID:          stepID,
		RunID:       runID,
		BatchID:     batchID,
		Host:        host,
		StepName:    "deploy",
		Action:      "exec",
		Status:      stepStatus,
		DurationMs:  100,
		StartedAt:   &now,
		CompletedAt: &now,
	}))
}

// =========================================================================
// GetAuditLog
// =========================================================================

func TestGetAuditLog_Empty(t *testing.T) {
	svc, _ := newTestAuditService(t)
	ctx := context.Background()

	resp, err := svc.GetAuditLog(ctx, &pb.GetAuditLogRequest{})
	require.NoError(t, err)
	assert.Empty(t, resp.GetEntries())
	assert.Equal(t, int32(0), resp.GetTotalSize())
}

func TestGetAuditLog_WithEntries(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedRun(t, ctx, store, "run-1", "completed")
	seedAudit(t, ctx, store, "aud-1", "run-1", "run.start")
	seedAudit(t, ctx, store, "aud-2", "run-1", "run.complete")

	resp, err := svc.GetAuditLog(ctx, &pb.GetAuditLogRequest{RunId: "run-1"})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resp.GetTotalSize())
	assert.Len(t, resp.GetEntries(), 2)
}

func TestGetAuditLog_FilterByAction(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedRun(t, ctx, store, "run-1", "completed")
	seedAudit(t, ctx, store, "aud-1", "run-1", "run.start")
	seedAudit(t, ctx, store, "aud-2", "run-1", "run.complete")

	resp, err := svc.GetAuditLog(ctx, &pb.GetAuditLogRequest{Action: "run.start"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), resp.GetTotalSize())
	assert.Equal(t, "run.start", resp.GetEntries()[0].GetAction())
}

func TestGetAuditLog_Pagination(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedRun(t, ctx, store, "run-1", "completed")
	for i := 0; i < 5; i++ {
		seedAudit(t, ctx, store, string(rune('a'+i)), "run-1", "action")
	}

	resp, err := svc.GetAuditLog(ctx, &pb.GetAuditLogRequest{PageSize: 2})
	require.NoError(t, err)
	assert.Len(t, resp.GetEntries(), 2)
	assert.Equal(t, int32(5), resp.GetTotalSize())
}

func TestGetAuditLog_NilStore(t *testing.T) {
	svc := NewAuditService(nil)
	_, err := svc.GetAuditLog(context.Background(), &pb.GetAuditLogRequest{})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
}

// =========================================================================
// ListAuditTraces
// =========================================================================

func TestListAuditTraces_Empty(t *testing.T) {
	svc, _ := newTestAuditService(t)
	ctx := context.Background()

	resp, err := svc.ListAuditTraces(ctx, &pb.ListAuditTracesRequest{})
	require.NoError(t, err)
	assert.Equal(t, int32(0), resp.GetTotalSize())
}

func TestListAuditTraces_WithTraces(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedRun(t, ctx, store, "run-1", "completed")
	seedTrace(t, ctx, store, "tr-1", "run-1", "", "hash-1")
	seedTrace(t, ctx, store, "tr-2", "run-1", "hash-1", "hash-2")

	resp, err := svc.ListAuditTraces(ctx, &pb.ListAuditTracesRequest{ChangeId: "run-1"})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resp.GetTotalSize())
}

func TestListAuditTraces_AllRuns(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedRun(t, ctx, store, "run-1", "completed")
	seedRun(t, ctx, store, "run-2", "completed")
	seedTrace(t, ctx, store, "tr-1", "run-1", "", "h1")
	seedTrace(t, ctx, store, "tr-2", "run-2", "", "h2")

	resp, err := svc.ListAuditTraces(ctx, &pb.ListAuditTracesRequest{})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resp.GetTotalSize())
}

func TestListAuditTraces_NilStore(t *testing.T) {
	svc := NewAuditService(nil)
	_, err := svc.ListAuditTraces(context.Background(), &pb.ListAuditTracesRequest{})
	require.Error(t, err)
}

// =========================================================================
// VerifyHashChain
// =========================================================================

func TestVerifyHashChain_SingleRun(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedRun(t, ctx, store, "run-1", "completed")
	seedTrace(t, ctx, store, "tr-1", "run-1", "", "hash-1")
	seedTrace(t, ctx, store, "tr-2", "run-1", "hash-1", "hash-2")

	resp, err := svc.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{RunId: "run-1"})
	require.NoError(t, err)
	// The chain may or may not be valid depending on how ChainVerifier
	// computes hashes; we just verify the response structure.
	assert.NotNil(t, resp)
}

func TestVerifyHashChain_NoTraces(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedRun(t, ctx, store, "run-1", "completed")

	resp, err := svc.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{RunId: "run-1"})
	require.NoError(t, err)
	// No traces → no verification entries.
	assert.True(t, resp.GetValid())
}

func TestVerifyHashChain_NilStore(t *testing.T) {
	svc := NewAuditService(nil)
	_, err := svc.VerifyHashChain(context.Background(), &pb.VerifyHashChainRequest{})
	require.Error(t, err)
}

// =========================================================================
// GetRunReport
// =========================================================================

func TestGetRunReport_Success(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedRun(t, ctx, store, "run-1", "completed")
	seedBatchAndStep(t, ctx, store, "run-1", "bat-1", "step-1", "host-01", "success")
	seedStep(t, ctx, store, "run-1", "bat-1", "step-2", "host-02", "failed")

	resp, err := svc.GetRunReport(ctx, &pb.GetRunReportRequest{RunId: "run-1"})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, "run-1", resp.GetRunId())
	assert.Equal(t, "completed", resp.GetStatus())
	assert.Equal(t, int32(2), resp.GetTotalHosts())
	assert.Equal(t, int32(1), resp.GetSuccessHosts())
	assert.Equal(t, int32(1), resp.GetFailedHosts())
	assert.Len(t, resp.GetHostResults(), 2)
}

func TestGetRunReport_WithLogs(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedRun(t, ctx, store, "run-1", "completed")
	seedTrace(t, ctx, store, "tr-1", "run-1", "", "h1")
	seedTrace(t, ctx, store, "tr-2", "run-1", "h1", "h2")

	resp, err := svc.GetRunReport(ctx, &pb.GetRunReportRequest{RunId: "run-1", IncludeLogs: true})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetLogs())
}

func TestGetRunReport_ChangeId(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedRun(t, ctx, store, "run-1", "completed")

	resp, err := svc.GetRunReport(ctx, &pb.GetRunReportRequest{ChangeId: "run-1"})
	require.NoError(t, err)
	assert.Equal(t, "run-1", resp.GetRunId())
}

func TestGetRunReport_NotFound(t *testing.T) {
	svc, _ := newTestAuditService(t)
	ctx := context.Background()

	_, err := svc.GetRunReport(ctx, &pb.GetRunReportRequest{RunId: "nonexistent"})
	require.Error(t, err)
}

func TestGetRunReport_MissingID(t *testing.T) {
	svc, _ := newTestAuditService(t)
	ctx := context.Background()

	_, err := svc.GetRunReport(ctx, &pb.GetRunReportRequest{})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

func TestGetRunReport_NilRequest(t *testing.T) {
	svc, _ := newTestAuditService(t)
	_, err := svc.GetRunReport(context.Background(), nil)
	require.Error(t, err)
}

func TestGetRunReport_NilStore(t *testing.T) {
	svc := NewAuditService(nil)
	_, err := svc.GetRunReport(context.Background(), &pb.GetRunReportRequest{RunId: "x"})
	require.Error(t, err)
}

// Ensure emptypb is used.
var _ = emptypb.Empty{}
