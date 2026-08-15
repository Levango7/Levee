// change_service_test.go tests the ChangeService gRPC handler. It uses
// a real SQLite in-memory store so the tests exercise the full
// store-backed code path. The engine, approval and pause dependencies
// are left nil so the service uses its fallback code paths; this keeps
// the tests hermetic and avoids mocking the entire engine stack.
package grpc

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpcpkg "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// newTestStore returns a fresh SQLite store backed by a temp file. Each
// test gets its own database so concurrent tests do not interfere.
func newTestStore(t *testing.T) state.Store {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "levee-grpc-test.db")
	store, err := state.NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestChangeService returns a ChangeService backed by a fresh test
// store. The engine, approval and pause dependencies are nil.
func newTestChangeService(t *testing.T) (*ChangeService, state.Store) {
	t.Helper()
	store := newTestStore(t)
	svc := NewChangeService(store, nil, nil, nil)
	return svc, store
}

// =========================================================================
// CreateChange
// =========================================================================

func TestCreateChange_Success(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	req := &pb.CreateChangeRequest{
		Label:        "test-change",
		Priority:     "high",
		WorkflowFile: "workflow.levee",
		TemplateName: "deploy-web",
		Params:       map[string]string{"version": "1.2.3"},
		Team:         "platform",
		Environment:  "staging",
	}

	resp, err := svc.CreateChange(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// The returned change should have a generated ID and draft status.
	assert.NotEmpty(t, resp.GetId())
	assert.Equal(t, "draft", resp.GetStatus())
	assert.Equal(t, "high", resp.GetPriority())
	assert.Equal(t, "deploy-web", resp.GetTemplateName())
	assert.Equal(t, "staging", resp.GetEnvironment())

	// The run should be persisted in the store.
	run, err := store.GetRun(ctx, resp.GetId())
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, "draft", run.Status)
	assert.Equal(t, "pending", run.ApprovalStatus)
}

func TestCreateChange_DefaultPriority(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	req := &pb.CreateChangeRequest{
		Label:        "no-priority",
		WorkflowFile: "workflow.levee",
	}

	resp, err := svc.CreateChange(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, "normal", resp.GetPriority())
}

func TestCreateChange_AuditEntryCreated(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	req := &pb.CreateChangeRequest{
		Label:        "audited-change",
		TemplateName: "deploy",
	}

	resp, err := svc.CreateChange(ctx, req)
	require.NoError(t, err)

	audits, err := store.ListAudits(ctx, state.AuditFilter{RunID: resp.GetId()})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	assert.Equal(t, "create", audits[0].Action)
	assert.Equal(t, "draft", audits[0].Result)
}

// =========================================================================
// GetChange
// =========================================================================

func TestGetChange_Success(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	// Create a change first.
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "get-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Fetch it.
	resp, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: created.GetId()})
	require.NoError(t, err)
	assert.Equal(t, created.GetId(), resp.GetId())
	assert.Equal(t, "draft", resp.GetStatus())
}

func TestGetChange_NotFound(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	_, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: "nonexistent"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

// =========================================================================
// ListChanges
// =========================================================================

func TestListChanges_Empty(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	resp, err := svc.ListChanges(ctx, &pb.ListChangesRequest{})
	require.NoError(t, err)
	assert.Empty(t, resp.GetChanges())
	assert.Equal(t, int32(0), resp.GetTotalSize())
}

func TestListChanges_WithChanges(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	// Create three changes.
	for i := 0; i < 3; i++ {
		_, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
			Label:        "list-test",
			TemplateName: "deploy",
		})
		require.NoError(t, err)
	}

	resp, err := svc.ListChanges(ctx, &pb.ListChangesRequest{})
	require.NoError(t, err)
	assert.Len(t, resp.GetChanges(), 3)
	assert.Equal(t, int32(3), resp.GetTotalSize())
}

func TestListChanges_FilterByStatus(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	// Create a change (starts in draft).
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "filter-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Filter by draft status — should find the change.
	resp, err := svc.ListChanges(ctx, &pb.ListChangesRequest{
		Statuses: []string{"draft"},
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetChanges(), 1)
	assert.Equal(t, created.GetId(), resp.GetChanges()[0].GetId())

	// Filter by running status — should find nothing.
	resp, err = svc.ListChanges(ctx, &pb.ListChangesRequest{
		Statuses: []string{"running"},
	})
	require.NoError(t, err)
	assert.Empty(t, resp.GetChanges())
}

func TestListChanges_Pagination(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	// Create 5 changes.
	for i := 0; i < 5; i++ {
		_, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
			Label:        "page-test",
			TemplateName: "deploy",
		})
		require.NoError(t, err)
	}

	// Page size 2 — first page.
	resp, err := svc.ListChanges(ctx, &pb.ListChangesRequest{PageSize: 2})
	require.NoError(t, err)
	assert.Len(t, resp.GetChanges(), 2)
	assert.NotEmpty(t, resp.GetNextPageToken())
}

// =========================================================================
// CloneChange
// =========================================================================

func TestCloneChange_Success(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	// Create a source change.
	src, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "source",
		TemplateName: "deploy",
		Params:       map[string]string{"version": "1.0.0"},
		Environment:  "staging",
	})
	require.NoError(t, err)

	// Clone it.
	resp, err := svc.CloneChange(ctx, &pb.CloneChangeRequest{
		SourceChangeId: src.GetId(),
		Label:          "cloned",
		Params:         map[string]string{"version": "2.0.0"},
	})
	require.NoError(t, err)
	assert.NotEqual(t, src.GetId(), resp.GetId())
	assert.Equal(t, "draft", resp.GetStatus())
}

func TestCloneChange_SourceNotFound(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	_, err := svc.CloneChange(ctx, &pb.CloneChangeRequest{
		SourceChangeId: "nonexistent",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.NotFound, st.Code())
}

// =========================================================================
// PauseChange / ResumeChange
// =========================================================================

func TestPauseChange_Success(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	// Create a change and transition it to running.
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "pause-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Manually set to running via the store (since ApplyChange without
	// engine leaves it in running).
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	run.Status = "running"
	require.NoError(t, store.UpdateRun(ctx, run))

	// Pause it.
	resp, err := svc.PauseChange(ctx, &pb.PauseRequest{
		ChangeId: created.GetId(),
		Reason:   "testing",
	})
	require.NoError(t, err)
	assert.Equal(t, "paused", resp.GetStatus())
}

func TestResumeChange_Success(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "resume-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Set to paused via the store.
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	run.Status = "paused"
	require.NoError(t, store.UpdateRun(ctx, run))

	// Resume it.
	resp, err := svc.ResumeChange(ctx, &pb.PauseRequest{
		ChangeId: created.GetId(),
		Reason:   "resuming",
	})
	require.NoError(t, err)
	assert.Equal(t, "running", resp.GetStatus())
}

func TestPauseChange_NotFound(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	_, err := svc.PauseChange(ctx, &pb.PauseRequest{ChangeId: "nonexistent"})
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.NotFound, st.Code())
}

// =========================================================================
// CancelChange
// =========================================================================

func TestCancelChange_Success(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "cancel-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	resp, err := svc.CancelChange(ctx, &pb.CancelRequest{
		ChangeId: created.GetId(),
		Reason:   "no longer needed",
	})
	require.NoError(t, err)
	assert.Equal(t, "cancelled", resp.GetStatus())
}

func TestCancelChange_Force(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "force-cancel-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Set to completed (normally can't cancel completed without force).
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	run.Status = "completed"
	require.NoError(t, store.UpdateRun(ctx, run))

	// Force cancel should work even from completed.
	resp, err := svc.CancelChange(ctx, &pb.CancelRequest{
		ChangeId: created.GetId(),
		Reason:   "force",
		Force:    true,
	})
	require.NoError(t, err)
	assert.Equal(t, "cancelled", resp.GetStatus())
}

// =========================================================================
// ArchiveChange
// =========================================================================

func TestArchiveChange_Success(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "archive-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	resp, err := svc.ArchiveChange(ctx, &pb.ArchiveRequest{
		ChangeId: created.GetId(),
	})
	require.NoError(t, err)
	assert.Equal(t, "archived", resp.GetStatus())
}

func TestArchiveChange_Idempotent(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "archive-idempotent",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Archive twice — second call should succeed without error.
	_, err = svc.ArchiveChange(ctx, &pb.ArchiveRequest{ChangeId: created.GetId()})
	require.NoError(t, err)
	resp, err := svc.ArchiveChange(ctx, &pb.ArchiveRequest{ChangeId: created.GetId()})
	require.NoError(t, err)
	assert.Equal(t, "archived", resp.GetStatus())
}

// =========================================================================
// GetTrace
// =========================================================================

func TestGetTrace_Success(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "trace-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Add a trace entry directly via the store.
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID:        "trc-1",
		RunID:     created.GetId(),
		Event:     "apply_started",
		Actor:     "test",
		PrevHash:  "",
		CurrHash:  "abc123",
		Timestamp: time.Now().UTC(),
	}))

	resp, err := svc.GetTrace(ctx, &pb.GetTraceRequest{
		ChangeId: created.GetId(),
	})
	require.NoError(t, err)
	assert.Equal(t, created.GetId(), resp.GetRunId())
	require.Len(t, resp.GetEntries(), 1)
	assert.Equal(t, "apply_started", resp.GetEntries()[0].GetAction())
}

func TestGetTrace_VerifyHashChain(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "verify-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Add a trace with a valid hash chain.
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID:        "trc-1",
		RunID:     created.GetId(),
		Event:     "start",
		PrevHash:  "",
		CurrHash:  "hash1",
		Timestamp: time.Now().UTC(),
	}))
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID:        "trc-2",
		RunID:     created.GetId(),
		Event:     "end",
		PrevHash:  "hash1",
		CurrHash:  "hash2",
		Timestamp: time.Now().UTC(),
	}))

	resp, err := svc.GetTrace(ctx, &pb.GetTraceRequest{
		ChangeId: created.GetId(),
		Verify:   true,
	})
	require.NoError(t, err)
	assert.True(t, resp.GetHashChainValid())
	assert.Contains(t, resp.GetVerificationMessage(), "valid")
}

// =========================================================================
// GetLogs
// =========================================================================

func TestGetLogs_Success(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "logs-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Add a batch first (steps have an FK on batches).
	now := time.Now().UTC()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID:         "batch-1",
		RunID:      created.GetId(),
		BatchNo:    1,
		Status:     "completed",
		TotalHosts: 1,
		Succeeded:  1,
		StartedAt:  &now,
	}))

	// Add a step with stdout/stderr.
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID:        "step-1",
		RunID:     created.GetId(),
		BatchID:   "batch-1",
		Host:      "host1",
		StepName:  "deploy",
		Status:    "completed",
		Stdout:    "deployment successful",
		Stderr:    "",
		StartedAt: &now,
	}))

	resp, err := svc.GetLogs(ctx, &pb.GetLogsRequest{
		ChangeId: created.GetId(),
	})
	require.NoError(t, err)
	require.Len(t, resp.GetEntries(), 1)
	assert.Equal(t, "INFO", resp.GetEntries()[0].GetLevel())
	assert.Equal(t, "deployment successful", resp.GetEntries()[0].GetMessage())
}

// =========================================================================
// GetDiff
// =========================================================================

func TestGetDiff_Success(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "diff-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	// Add a batch first (steps have an FK on batches).
	now := time.Now().UTC()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID:         "batch-1",
		RunID:      created.GetId(),
		BatchNo:    1,
		Status:     "completed",
		TotalHosts: 1,
		Succeeded:  1,
		StartedAt:  &now,
	}))

	// Add a step.
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID:        "step-1",
		RunID:     created.GetId(),
		BatchID:   "batch-1",
		Host:      "host1",
		StepName:  "deploy",
		Status:    "completed",
		Stdout:    "changed line",
		StartedAt: &now,
	}))

	resp, err := svc.GetDiff(ctx, &pb.GetDiffRequest{
		ChangeId: created.GetId(),
	})
	require.NoError(t, err)
	assert.Equal(t, created.GetId(), resp.GetChangeId())
	assert.Equal(t, "unified", resp.GetFormat())
	assert.Contains(t, resp.GetDiff(), "changed line")
}

// =========================================================================
// WatchChange (streaming)
// =========================================================================

// fakeStream is a test double for grpc.ServerStreamingServer[pb.ChangeEvent].
// It captures sent events and allows the test to control the context.
type fakeStream struct {
	ctx     context.Context
	events  []*pb.ChangeEvent
	sendErr error
}

func (f *fakeStream) Context() context.Context { return f.ctx }

func (f *fakeStream) SendMsg(m interface{}) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	if ev, ok := m.(*pb.ChangeEvent); ok {
		f.events = append(f.events, ev)
	}
	return nil
}

func (f *fakeStream) RecvMsg(m interface{}) error { return nil }

func (f *fakeStream) Send(ev *pb.ChangeEvent) error {
	return f.SendMsg(ev)
}

// SetHeader, SendHeader and SetTrailer satisfy grpc.ServerStream.
// They are no-ops in tests.
func (f *fakeStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeStream) SetTrailer(metadata.MD)       {}

func TestWatchChange_IncludeCurrentState(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "watch-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	stream := &fakeStream{ctx: ctx}

	// Run WatchChange in a goroutine; cancel the context after a short
	// delay to end the stream.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err = svc.WatchChange(&pb.WatchChangeRequest{
		ChangeId:            created.GetId(),
		IncludeCurrentState: true,
	}, stream)
	require.NoError(t, err)

	// The first event should be the current state.
	require.NotEmpty(t, stream.events)
	assert.Equal(t, created.GetId(), stream.events[0].GetChangeId())
	assert.Equal(t, "status_changed", stream.events[0].GetEventType())
	assert.Equal(t, "draft", stream.events[0].GetNewStatus())
}

func TestWatchChange_NotFound(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	stream := &fakeStream{ctx: ctx}
	err := svc.WatchChange(&pb.WatchChangeRequest{
		ChangeId: "nonexistent",
	}, stream)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.NotFound, st.Code())
}

func TestWatchChange_ReceivesPublishedEvents(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "watch-pub-test",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	stream := &fakeStream{ctx: ctx}

	// Run WatchChange in a goroutine.
	watchDone := make(chan error, 1)
	go func() {
		watchDone <- svc.WatchChange(&pb.WatchChangeRequest{
			ChangeId: created.GetId(),
		}, stream)
	}()

	// Give the watcher a moment to subscribe.
	time.Sleep(50 * time.Millisecond)

	// Publish an event.
	svc.publishEvent(&pb.ChangeEvent{
		ChangeId:  created.GetId(),
		EventType: "step_completed",
		NewStatus: "running",
		Message:   "step 1 done",
		Timestamp: time.Now().Unix(),
	})

	// Wait a moment for the event to be delivered.
	time.Sleep(50 * time.Millisecond)

	// Cancel to end the stream.
	cancel()
	<-watchDone

	// We should have received the published event.
	require.NotEmpty(t, stream.events)
	found := false
	for _, ev := range stream.events {
		if ev.GetEventType() == "step_completed" {
			found = true
			assert.Equal(t, "running", ev.GetNewStatus())
		}
	}
	assert.True(t, found, "expected to receive step_completed event")
}

// =========================================================================
// Auth interceptor
// =========================================================================

func TestAuthInterceptor_DisabledWhenTokenEmpty(t *testing.T) {
	interceptor := AuthInterceptor("")
	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return "ok", nil
	}
	info := &grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}
	resp, err := interceptor(context.Background(), "req", info, handler)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp)
	assert.True(t, called)
}

func TestAuthInterceptor_RejectsMissingMetadata(t *testing.T) {
	interceptor := AuthInterceptor("secret-token")
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		t.Fatal("handler should not be called")
		return nil, nil
	}
	info := &grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}
	_, err := interceptor(context.Background(), "req", info, handler)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

// grpcServerInfo is retained for backward compatibility but no longer
// used; tests use grpcpkg.UnaryServerInfo directly.

// =========================================================================
// Server construction
// =========================================================================

func TestNewServer_Defaults(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store)
	assert.NotNil(t, srv)
	assert.Equal(t, DefaultListenAddr, srv.listenAddr)
	assert.NotNil(t, srv.grpcServer)
	assert.NotNil(t, srv.changeService)
}

func TestNewServer_WithOptions(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store,
		WithListenAddr(":9999"),
		WithAuthToken("my-token"),
	)
	assert.Equal(t, ":9999", srv.listenAddr)
	assert.Equal(t, "my-token", srv.authToken)
}

func TestServer_StartStop(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store, WithListenAddr(":0")) // :0 = random free port

	// Start in a goroutine (Start blocks).
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(":0")
	}()

	// Give it a moment to bind.
	time.Sleep(100 * time.Millisecond)

	// Stop the server.
	require.NoError(t, srv.Stop())

	// The Start goroutine should return.
	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop within 2 seconds")
	}
}

func TestServer_DoubleStart(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store, WithListenAddr(":0"))

	// Start once.
	go func() { _ = srv.Start(":0") }()
	time.Sleep(100 * time.Millisecond)

	// Second start should fail.
	err := srv.Start(":0")
	require.Error(t, err)

	_ = srv.Stop()
}