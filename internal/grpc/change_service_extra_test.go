// change_service_extra_test.go covers the ChangeService paths the earlier
// suite left open: PlanChange/ApplyChange (engine delegation, fallbacks and
// guards), bulk PauseAll/ResumeAll, RetryChange/RetryHost, RollbackChange,
// RejectChange, ArchiveChange artifact purging, GetDiff/GetLogs edge cases,
// StreamLogs filtering and the eventBus drop/unsubscribe behaviour. Engine,
// approval and pause dependencies are exercised through small in-package
// stubs so both delegated and fallback code paths run.
package grpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/pause"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// recordingEngine is an EngineAdapter stub that records every invocation so
// tests can assert delegation happened with the right arguments.
type recordingEngine struct {
	runID      string
	runSuccess bool
	runPhase   string
	runErr     error
	plan       *pb.Plan
	planErr    error
	rollbackID string
	rbHosts    []string
	rbErr      error
	retryErr   error

	runCalled      int32
	planCalled     int32
	rollbackCalled int32
	retryCalled    int32

	lastAutoApprove bool
	lastHosts       []string
}

func (e *recordingEngine) adapter() *EngineAdapter {
	return &EngineAdapter{
		Run: func(_ context.Context, _ string, autoApprove bool, _ int32) (string, bool, string, error) {
			atomic.AddInt32(&e.runCalled, 1)
			e.lastAutoApprove = autoApprove
			return e.runID, e.runSuccess, e.runPhase, e.runErr
		},
		Plan: func(_ context.Context, _ string, hosts []string) (*pb.Plan, error) {
			atomic.AddInt32(&e.planCalled, 1)
			e.lastHosts = hosts
			if e.planErr != nil {
				return nil, e.planErr
			}
			return e.plan, nil
		},
		Rollback: func(_ context.Context, _, _ string, _ bool) (string, []string, error) {
			atomic.AddInt32(&e.rollbackCalled, 1)
			if e.rbErr != nil {
				return "", nil, e.rbErr
			}
			return e.rollbackID, e.rbHosts, nil
		},
		Retry: func(_ context.Context, _ string, _ bool, hosts []string) error {
			atomic.AddInt32(&e.retryCalled, 1)
			e.lastHosts = hosts
			return e.retryErr
		},
	}
}

// setRunStatus moves a run to an arbitrary status directly in the store.
func setRunStatus(t *testing.T, store state.Store, runID, status string) {
	t.Helper()
	ctx := context.Background()
	run, err := store.GetRun(ctx, runID)
	require.NoError(t, err)
	require.NotNil(t, run)
	run.Status = status
	require.NoError(t, store.UpdateRun(ctx, run))
}

// newTestChangeServiceWithEngine returns a ChangeService wired to a fresh
// test store and the supplied EngineAdapter stub.
func newTestChangeServiceWithEngine(t *testing.T, engine *EngineAdapter) (*ChangeService, state.Store) {
	t.Helper()
	store := newTestStore(t)
	svc := NewChangeService(store, engine, nil, nil)
	return svc, store
}

// errorStore wraps a real store and injects errors into selected operations
// so the gRPC error-mapping branches can be exercised deterministically.
type errorStore struct {
	state.Store
	getRunErr        error
	listRunsErr      error
	listApprovalsErr error
	// getRunOverride, when non-nil, replaces the default GetRun behaviour
	// (used to keep GetRun working while other methods fail).
	getRunOverride func(ctx context.Context, id string) (*state.Run, error)
}

func (f *errorStore) GetRun(ctx context.Context, id string) (*state.Run, error) {
	if f.getRunOverride != nil {
		return f.getRunOverride(ctx, id)
	}
	if f.getRunErr != nil {
		return nil, f.getRunErr
	}
	return f.Store.GetRun(ctx, id)
}

func (f *errorStore) ListRuns(ctx context.Context, filter state.RunFilter) ([]*state.Run, error) {
	if f.listRunsErr != nil {
		return nil, f.listRunsErr
	}
	return f.Store.ListRuns(ctx, filter)
}

func (f *errorStore) ListApprovals(ctx context.Context, filter state.ApprovalFilter) ([]*state.Approval, error) {
	if f.listApprovalsErr != nil {
		return nil, f.listApprovalsErr
	}
	return f.Store.ListApprovals(ctx, filter)
}

// --- nil-store guard ----------------------------------------------------------

func TestChangeServiceNilStoreReturnsInternal(t *testing.T) {
	svc := NewChangeService(nil, nil, nil, nil)
	ctx := context.Background()

	unary := map[string]func() error{
		"CreateChange":   func() error { _, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{}); return err },
		"CloneChange":    func() error { _, err := svc.CloneChange(ctx, &pb.CloneChangeRequest{}); return err },
		"PlanChange":     func() error { _, err := svc.PlanChange(ctx, &pb.PlanChangeRequest{}); return err },
		"ApplyChange":    func() error { _, err := svc.ApplyChange(ctx, &pb.ApplyChangeRequest{}); return err },
		"PauseChange":    func() error { _, err := svc.PauseChange(ctx, &pb.PauseRequest{}); return err },
		"ResumeChange":   func() error { _, err := svc.ResumeChange(ctx, &pb.PauseRequest{}); return err },
		"PauseAll":       func() error { _, err := svc.PauseAll(ctx, &pb.PauseAllRequest{}); return err },
		"ResumeAll":      func() error { _, err := svc.ResumeAll(ctx, &pb.PauseAllRequest{}); return err },
		"CancelChange":   func() error { _, err := svc.CancelChange(ctx, &pb.CancelRequest{}); return err },
		"RetryChange":    func() error { _, err := svc.RetryChange(ctx, &pb.RetryRequest{}); return err },
		"RetryHost":      func() error { _, err := svc.RetryHost(ctx, &pb.RetryHostRequest{}); return err },
		"RollbackChange": func() error { _, err := svc.RollbackChange(ctx, &pb.RollbackRequest{}); return err },
		"ApproveChange":  func() error { _, err := svc.ApproveChange(ctx, &pb.ApproveRequest{}); return err },
		"RejectChange":   func() error { _, err := svc.RejectChange(ctx, &pb.RejectRequest{}); return err },
		"GetChange":      func() error { _, err := svc.GetChange(ctx, &pb.GetChangeRequest{}); return err },
		"ListChanges":    func() error { _, err := svc.ListChanges(ctx, &pb.ListChangesRequest{}); return err },
		"ArchiveChange":  func() error { _, err := svc.ArchiveChange(ctx, &pb.ArchiveRequest{}); return err },
		"GetLogs":        func() error { _, err := svc.GetLogs(ctx, &pb.GetLogsRequest{}); return err },
		"GetDiff":        func() error { _, err := svc.GetDiff(ctx, &pb.GetDiffRequest{}); return err },
		"GetTrace":       func() error { _, err := svc.GetTrace(ctx, &pb.GetTraceRequest{}); return err },
	}
	for name, fn := range unary {
		t.Run(name, func(t *testing.T) {
			err := fn()
			require.Error(t, err)
			assert.Equal(t, codes.Internal, status.Code(err), "%s should map nil store to Internal", name)
		})
	}

	// Streaming RPCs share the same guard.
	err := svc.WatchChange(&pb.WatchChangeRequest{}, &fakeStream{ctx: ctx})
	assert.Equal(t, codes.Internal, status.Code(err))
	err = svc.StreamLogs(&pb.StreamLogsRequest{}, &fakeLogStream{ctx: ctx})
	assert.Equal(t, codes.Internal, status.Code(err))
}

// --- PlanChange -----------------------------------------------------------------

func TestPlanChange(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		svc, _ := newTestChangeService(t)
		_, err := svc.PlanChange(context.Background(), &pb.PlanChangeRequest{ChangeId: "missing"})
		require.Error(t, err)
		assert.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("fallback plan without engine", func(t *testing.T) {
		svc, _ := newTestChangeService(t)
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "plan-fallback"})
		require.NoError(t, err)

		resp, err := svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
			ChangeId:    created.GetId(),
			TargetHosts: []string{"h1", "h2"},
		})
		require.NoError(t, err)
		assert.Equal(t, created.GetId(), resp.GetChangeId())
		assert.Equal(t, []string{"h1", "h2"}, resp.GetTargetHosts())
		assert.Empty(t, resp.GetBatches())
		assert.Contains(t, resp.GetImpactSummary(), "no engine configured")
	})

	t.Run("delegates to engine and persists update", func(t *testing.T) {
		engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x", ImpactSummary: "engine plan"}}
		svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "plan-engine"})
		require.NoError(t, err)

		resp, err := svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
			ChangeId:    created.GetId(),
			TargetHosts: []string{"h1"},
		})
		require.NoError(t, err)
		assert.Equal(t, "engine plan", resp.GetImpactSummary())
		assert.Equal(t, int32(1), atomic.LoadInt32(&engine.planCalled))

		// Non-dry-run plans refresh UpdatedAt on the stored run.
		run, err := store.GetRun(context.Background(), created.GetId())
		require.NoError(t, err)
		require.NotNil(t, run)
	})

	t.Run("dry run skips store update but still plans", func(t *testing.T) {
		engine := &recordingEngine{plan: &pb.Plan{ImpactSummary: "dry"}}
		svc, _ := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "plan-dry"})
		require.NoError(t, err)

		resp, err := svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
			ChangeId: created.GetId(),
			DryRun:   true,
		})
		require.NoError(t, err)
		assert.Equal(t, "dry", resp.GetImpactSummary())
	})

	t.Run("engine error maps to internal", func(t *testing.T) {
		engine := &recordingEngine{planErr: errors.New("planner offline")}
		svc, _ := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "plan-err"})
		require.NoError(t, err)

		_, err = svc.PlanChange(context.Background(), &pb.PlanChangeRequest{ChangeId: created.GetId()})
		require.Error(t, err)
		assert.Equal(t, codes.Internal, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "planner offline")
	})
}

// --- ApplyChange ------------------------------------------------------------------

func TestApplyChange_NotFound(t *testing.T) {
	svc, _ := newTestChangeService(t)
	_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{ChangeId: "missing"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestApplyChange_StateGuardRejectsCompletedWithoutAutoApprove(t *testing.T) {
	svc, store := newTestChangeService(t)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-guard"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), "completed")

	_, err = svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{ChangeId: created.GetId()})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestApplyChange_DraftRequiresAutoApprove(t *testing.T) {
	svc, store := newTestChangeService(t)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-noeng"})
	require.NoError(t, err)

	// Draft without auto-approve is now rejected: only "approved" runs may
	// be applied without the explicit override.
	_, err = svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{ChangeId: created.GetId()})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "draft", run.Status, "rejected apply must not mutate the run")

	// With auto-approve but NO engine wired, apply is refused
	// (FailedPrecondition) instead of faking a success that would leave
	// the run stuck in "running" forever.
	_, err = svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId:    created.GetId(),
		AutoApprove: true,
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "no engine wired")

	run, err = store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "draft", run.Status, "refused apply must not mutate the run")
}

func TestApplyChange_EngineSuccessCompletes(t *testing.T) {
	engine := &recordingEngine{runID: "exec-1", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-eng"})
	require.NoError(t, err)

	resp, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId:       created.GetId(),
		AutoApprove:    true,
		MaxConcurrency: 4,
	})
	require.NoError(t, err)
	assert.True(t, resp.GetSuccess())
	assert.Equal(t, "exec-1", resp.GetRunId())
	assert.Equal(t, "completed", resp.GetMessage())
	assert.Equal(t, "completed", resp.GetChange().GetStatus())
	assert.True(t, engine.lastAutoApprove)

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "completed", run.Status)
}

func TestApplyChange_EngineFailureMarksFailedAndReturnsInternal(t *testing.T) {
	engine := &recordingEngine{runID: "exec-2", runErr: errors.New("executor crash")}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-fail"})
	require.NoError(t, err)

	resp, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId:    created.GetId(),
		AutoApprove: true,
	})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	require.NotNil(t, resp)
	assert.False(t, resp.GetSuccess())
	assert.Contains(t, resp.GetMessage(), "executor crash")

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "failed", run.Status)
}

// Terminal engine outcomes publish a status_changed event so WatchChange
// subscribers observe completion/failure like any other transition.
func TestApplyChange_EnginePathPublishesTerminalEvent(t *testing.T) {
	t.Run("completed publishes terminal event", func(t *testing.T) {
		engine := &recordingEngine{runID: "exec-3", runSuccess: true}
		svc, _ := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-term"})
		require.NoError(t, err)

		// Subscribe under the real change id before applying.
		bus := svc.getEventBus()
		ch := bus.subscribe(created.GetId())
		defer bus.unsubscribe(created.GetId(), ch)

		_, err = svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
			ChangeId:    created.GetId(),
			AutoApprove: true,
		})
		require.NoError(t, err)

		// The apply-started event is queued first; drain until the
		// terminal transition arrives.
		var terminal *pb.ChangeEvent
		deadline := time.After(2 * time.Second)
		for terminal == nil {
			select {
			case ev := <-ch:
				if ev.GetNewStatus() == "completed" {
					terminal = ev
				}
			case <-deadline:
				t.Fatal("expected a terminal status_changed event")
			}
		}
		assert.Equal(t, "status_changed", terminal.GetEventType())
		assert.Equal(t, "running", terminal.GetOldStatus())
		assert.Equal(t, "completed", terminal.GetNewStatus())
	})

	t.Run("engine error publishes failed event", func(t *testing.T) {
		engine := &recordingEngine{runID: "exec-4", runErr: errors.New("boom")}
		svc, _ := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-term-fail"})
		require.NoError(t, err)

		bus := svc.getEventBus()
		ch := bus.subscribe(created.GetId())
		defer bus.unsubscribe(created.GetId(), ch)

		_, _ = svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
			ChangeId:    created.GetId(),
			AutoApprove: true,
		})

		var failed *pb.ChangeEvent
		deadline := time.After(2 * time.Second)
		for failed == nil {
			select {
			case ev := <-ch:
				if ev.GetNewStatus() == "failed" {
					failed = ev
				}
			case <-deadline:
				t.Fatal("expected a failed status_changed event")
			}
		}
		assert.Equal(t, "failed", failed.GetNewStatus())
	})
}

// --- PauseAll / ResumeAll ----------------------------------------------------------

func TestPauseAllAndResumeAll(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	running, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "bulk-running"})
	require.NoError(t, err)
	setRunStatus(t, store, running.GetId(), "running")

	pending, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "bulk-pending"})
	require.NoError(t, err)
	// Created runs are in "draft", which bulkTransition skips; promote the
	// second run to a literal "pending" status so it is pausable.
	setRunStatus(t, store, pending.GetId(), "pending")

	done, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "bulk-done"})
	require.NoError(t, err)
	setRunStatus(t, store, done.GetId(), "completed")

	draft, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "bulk-draft"})
	require.NoError(t, err)
	_ = draft // stays in "draft": ResumeAll must never touch it

	// Environment filter that matches nothing pauses nothing.
	filtered, err := svc.PauseAll(ctx, &pb.PauseAllRequest{Environments: []string{"prod-only"}})
	require.NoError(t, err)
	assert.Equal(t, int32(0), filtered.GetCount())

	// PauseAll transitions running+pending; completed is skipped.
	paused, err := svc.PauseAll(ctx, &pb.PauseAllRequest{Reason: "freeze"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{running.GetId(), pending.GetId()}, paused.GetPausedChangeIds())
	assert.Equal(t, int32(2), paused.GetCount())

	for _, id := range paused.GetPausedChangeIds() {
		run, err := store.GetRun(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "paused", run.Status, "run %s", id)
	}

	// ResumeAll resumes ONLY paused runs. The completed run is skipped
	// (completed → running is not legal) and — critically — the still-
	// drafted run is skipped too even though draft→running became a legal
	// transition for other flows; "resume" means un-pause, nothing else.
	resumed, err := svc.ResumeAll(ctx, &pb.PauseAllRequest{})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resumed.GetCount())
	assert.ElementsMatch(t, []string{running.GetId(), pending.GetId()}, resumed.GetPausedChangeIds())
	assert.Contains(t, resumed.GetSkippedChangeIds(), done.GetId())
	assert.Contains(t, resumed.GetSkippedChangeIds(), draft.GetId())
}

func TestBulkTransition_ListRunsErrorIsInternal(t *testing.T) {
	fs := &errorStore{Store: newTestStore(t), listRunsErr: errors.New("db down")}
	svc := NewChangeService(fs, nil, nil, nil)

	_, err := svc.PauseAll(context.Background(), &pb.PauseAllRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

// --- ListChanges pagination ---------------------------------------------------------

// TestListChanges_MultiStatusPaginatesWithoutSkipping seeds runs across two
// statuses interleaved with noise and pages through a multi-status filter.
// The store can only filter one status, so the service must scan client-side;
// the old implementation fetched only the first status and silently dropped
// the rest.
func TestListChanges_MultiStatusPaginatesWithoutSkipping(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	seedRun := func(id string, status string) {
		t.Helper()
		now := time.Now().UTC().Add(-time.Duration(len(id)) * time.Second)
		require.NoError(t, store.CreateRun(ctx, &state.Run{
			ID: id, WorkflowName: "wf", Status: status,
			CreatedAt: now, UpdatedAt: now, Creator: "test",
		}))
	}
	for i := 0; i < 3; i++ {
		seedRun(fmt.Sprintf("run-done-%02d", i), "completed")
		seedRun(fmt.Sprintf("run-fail-%02d", i), "failed")
	}

	// The expected full ordering comes from the store itself so the test
	// asserts pagination completeness, not store sort details.
	allRuns, err := store.ListRuns(ctx, state.RunFilter{})
	require.NoError(t, err)
	var want []string
	for _, r := range allRuns {
		if r.Status == "failed" || r.Status == "completed" {
			want = append(want, r.ID)
		}
	}
	require.Len(t, want, 6)

	// Page size 2 with statuses [failed completed]: every page must carry
	// exactly pageSize matches until exhaustion — no gaps from unfiltered
	// rows consuming page slots.
	var got []string
	token := ""
	pages := 0
	for {
		resp, err := svc.ListChanges(ctx, &pb.ListChangesRequest{
			Statuses:  []string{"failed", "completed"},
			PageSize:  2,
			PageToken: token,
		})
		require.NoError(t, err)
		pages++
		for _, c := range resp.GetChanges() {
			got = append(got, c.GetId())
		}
		if resp.GetNextPageToken() == "" {
			break
		}
		token = resp.GetNextPageToken()
		require.Less(t, pages, 10, "pagination did not terminate")
	}
	require.Equal(t, want, got)

	// Exhausted filtered scans know their full match count.
	final, err := svc.ListChanges(ctx, &pb.ListChangesRequest{
		Statuses: []string{"failed", "completed"},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(6), final.GetTotalSize())
}

func TestListChanges_SingleStatusUsesOffsetPagination(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		require.NoError(t, store.CreateRun(ctx, &state.Run{
			ID: fmt.Sprintf("run-d-%02d", i), WorkflowName: "wf", Status: "draft",
			CreatedAt: now.Add(time.Duration(i) * time.Second),
			UpdatedAt: now, Creator: "test",
		}))
	}

	first, err := svc.ListChanges(ctx, &pb.ListChangesRequest{Statuses: []string{"draft"}, PageSize: 2})
	require.NoError(t, err)
	require.Len(t, first.GetChanges(), 2)
	require.NotEmpty(t, first.GetNextPageToken())

	second, err := svc.ListChanges(ctx, &pb.ListChangesRequest{
		Statuses: []string{"draft"}, PageSize: 2, PageToken: first.GetNextPageToken(),
	})
	require.NoError(t, err)
	require.Len(t, second.GetChanges(), 2)

	// No overlap between pages.
	assert.NotEqual(t,
		first.GetChanges()[0].GetId()+first.GetChanges()[1].GetId(),
		second.GetChanges()[0].GetId()+second.GetChanges()[1].GetId())
}

func TestListChanges_LabelFilterScansUntilEnoughMatches(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Only runs whose workflow name (mapped to label) contains "needle"
	// should be returned, regardless of how many non-matching rows sit
	// in front of them in the store ordering.
	for i := 0; i < 4; i++ {
		require.NoError(t, store.CreateRun(ctx, &state.Run{
			ID: fmt.Sprintf("run-noise-%02d", i), WorkflowName: "noise", Status: "running",
			CreatedAt: now.Add(time.Duration(i) * time.Second),
			UpdatedAt: now, Creator: "test",
		}))
	}
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-hit-1", WorkflowName: "find-the-needle-here", Status: "running",
		CreatedAt: now, UpdatedAt: now, Creator: "test",
	}))
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-hit-2", WorkflowName: "another needle", Status: "running",
		CreatedAt: now.Add(-time.Second), UpdatedAt: now, Creator: "test",
	}))

	resp, err := svc.ListChanges(ctx, &pb.ListChangesRequest{LabelContains: "needle"})
	require.NoError(t, err)
	ids := []string{}
	for _, c := range resp.GetChanges() {
		ids = append(ids, c.GetId())
	}
	assert.ElementsMatch(t, []string{"run-hit-1", "run-hit-2"}, ids)
}

// --- ArchiveChange state guard ------------------------------------------------------

func TestArchiveChange_RejectsRunningAndPaused(t *testing.T) {
	for _, st := range []string{"running", "paused"} {
		t.Run(st, func(t *testing.T) {
			svc, store := newTestChangeService(t)
			created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "arch-guard"})
			require.NoError(t, err)
			setRunStatus(t, store, created.GetId(), st)

			_, err = svc.ArchiveChange(context.Background(), &pb.ArchiveRequest{ChangeId: created.GetId()})
			require.Error(t, err)
			assert.Equal(t, codes.FailedPrecondition, status.Code(err))

			run, err := store.GetRun(context.Background(), created.GetId())
			require.NoError(t, err)
			assert.Equal(t, st, run.Status, "rejected archive must not mutate the run")
		})
	}

	t.Run("terminal states still archiveable", func(t *testing.T) {
		for _, st := range []string{"completed", "failed", "cancelled", "rejected", "rolled_back"} {
			svc, store := newTestChangeService(t)
			created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "arch-ok"})
			require.NoError(t, err)
			setRunStatus(t, store, created.GetId(), st)

			resp, err := svc.ArchiveChange(context.Background(), &pb.ArchiveRequest{ChangeId: created.GetId()})
			require.NoError(t, err, "status %q must be archivable", st)
			assert.Equal(t, "archived", resp.GetStatus())
		}
	})
}

// --- RetryChange / RetryHost --------------------------------------------------------

func TestRetryChange(t *testing.T) {
	makeSvc := func(t *testing.T, engine *recordingEngine) (*ChangeService, state.Store, string) {
		var svc *ChangeService
		var store state.Store
		if engine == nil {
			svc, store = newTestChangeService(t)
		} else {
			svc, store = newTestChangeServiceWithEngine(t, engine.adapter())
		}
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "retry-me"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), "failed")
		return svc, store, created.GetId()
	}

	t.Run("guard rejects running change", func(t *testing.T) {
		svc, store, id := makeSvc(t, nil)
		setRunStatus(t, store, id, "running")
		_, err := svc.RetryChange(context.Background(), &pb.RetryRequest{ChangeId: id})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("not found", func(t *testing.T) {
		svc, _, _ := makeSvc(t, nil)
		_, err := svc.RetryChange(context.Background(), &pb.RetryRequest{ChangeId: "missing"})
		assert.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("without engine resets to draft", func(t *testing.T) {
		svc, store, id := makeSvc(t, nil)
		resp, err := svc.RetryChange(context.Background(), &pb.RetryRequest{ChangeId: id})
		require.NoError(t, err)
		// No engine means nothing will execute the retry; the run resets
		// to "draft" so it can be re-planned instead of being stuck in a
		// "running" state no executor would ever finish.
		assert.Equal(t, "draft", resp.GetStatus())
		run, err := store.GetRun(context.Background(), id)
		require.NoError(t, err)
		assert.Equal(t, "draft", run.Status)

		audits, err := store.ListAudits(context.Background(), state.AuditFilter{RunID: id})
		require.NoError(t, err)
		found := false
		for _, a := range audits {
			if a.Action == "retry" && a.Result == "draft" {
				found = true
			}
		}
		assert.True(t, found, "expected retry audit entry with draft result")
	})

	t.Run("delegates to engine retry", func(t *testing.T) {
		engine := &recordingEngine{}
		svc, _, id := makeSvc(t, engine)
		resp, err := svc.RetryChange(context.Background(), &pb.RetryRequest{
			ChangeId:    id,
			Replan:      true,
			TargetHosts: []string{"h1"},
		})
		require.NoError(t, err)
		assert.Equal(t, "running", resp.GetStatus())
		assert.Equal(t, int32(1), atomic.LoadInt32(&engine.retryCalled))
		assert.Equal(t, []string{"h1"}, engine.lastHosts)
	})

	t.Run("engine error maps to internal", func(t *testing.T) {
		engine := &recordingEngine{retryErr: errors.New("retry refused")}
		svc, _, id := makeSvc(t, engine)
		_, err := svc.RetryChange(context.Background(), &pb.RetryRequest{ChangeId: id})
		require.Error(t, err)
		assert.Equal(t, codes.Internal, status.Code(err))
	})
}

func TestRetryHost(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		svc, _ := newTestChangeService(t)
		_, err := svc.RetryHost(context.Background(), &pb.RetryHostRequest{ChangeId: "missing", Hosts: []string{"h1"}})
		require.Error(t, err)
		assert.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("records audit without changing status", func(t *testing.T) {
		svc, store := newTestChangeService(t)
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "host-retry"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), "failed")

		resp, err := svc.RetryHost(context.Background(), &pb.RetryHostRequest{
			ChangeId: created.GetId(),
			Hosts:    []string{"h1", "h2"},
		})
		require.NoError(t, err)
		assert.Equal(t, "failed", resp.GetStatus(), "overall status must not change")

		audits, err := store.ListAudits(context.Background(), state.AuditFilter{RunID: created.GetId()})
		require.NoError(t, err)
		found := false
		for _, a := range audits {
			if a.Action == "retry-host" {
				found = true
				assert.Equal(t, "h1,h2", a.Target)
			}
		}
		assert.True(t, found, "expected a retry-host audit entry")
	})

	t.Run("delegates to engine with host subset", func(t *testing.T) {
		engine := &recordingEngine{}
		svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "host-retry-eng"})
		require.NoError(t, err)
		_ = store

		_, err = svc.RetryHost(context.Background(), &pb.RetryHostRequest{
			ChangeId: created.GetId(),
			Hosts:    []string{"h9"},
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), atomic.LoadInt32(&engine.retryCalled))
		assert.Equal(t, []string{"h9"}, engine.lastHosts)
	})

	t.Run("engine error maps to internal", func(t *testing.T) {
		engine := &recordingEngine{retryErr: errors.New("hosts unreachable")}
		svc, _ := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "host-retry-err"})
		require.NoError(t, err)

		_, err = svc.RetryHost(context.Background(), &pb.RetryHostRequest{ChangeId: created.GetId(), Hosts: []string{"h1"}})
		require.Error(t, err)
		assert.Equal(t, codes.Internal, status.Code(err))
	})
}

// --- RollbackChange -------------------------------------------------------------------

func TestRollbackChange(t *testing.T) {
	t.Run("guard rejects draft change", func(t *testing.T) {
		svc, _ := newTestChangeService(t)
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "rb-guard"})
		require.NoError(t, err)

		_, err = svc.RollbackChange(context.Background(), &pb.RollbackRequest{ChangeId: created.GetId()})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("not found", func(t *testing.T) {
		svc, _ := newTestChangeService(t)
		_, err := svc.RollbackChange(context.Background(), &pb.RollbackRequest{ChangeId: "missing"})
		assert.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("fallback marks rolled_back without engine", func(t *testing.T) {
		svc, store := newTestChangeService(t)
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "rb-fallback"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), "completed")

		// The fallback must publish the terminal event like every other
		// transition does.
		bus := svc.getEventBus()
		ch := bus.subscribe(created.GetId())
		defer bus.unsubscribe(created.GetId(), ch)

		resp, err := svc.RollbackChange(context.Background(), &pb.RollbackRequest{
			ChangeId: created.GetId(),
			RunId:    "orig-exec-run", // caller-supplied trace scoping is honoured
		})
		require.NoError(t, err)
		assert.True(t, resp.GetSuccess())
		assert.Equal(t, "orig-exec-run", resp.GetRollbackRunId())
		assert.Equal(t, "rolled_back", resp.GetChange().GetStatus())

		select {
		case ev := <-ch:
			assert.Equal(t, "status_changed", ev.GetEventType())
			assert.Equal(t, "completed", ev.GetOldStatus())
			assert.Equal(t, "rolled_back", ev.GetNewStatus())
		default:
			t.Fatal("expected rollback status_changed event from fallback path")
		}
	})

	t.Run("engine rollback returns hosts", func(t *testing.T) {
		engine := &recordingEngine{rollbackID: "rb-exec-1", rbHosts: []string{"h1", "h3"}}
		svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "rb-engine"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), "failed")

		resp, err := svc.RollbackChange(context.Background(), &pb.RollbackRequest{
			ChangeId:    created.GetId(),
			RunId:       "orig-run",
			AutoApprove: true,
		})
		require.NoError(t, err)
		assert.True(t, resp.GetSuccess())
		assert.Equal(t, "rb-exec-1", resp.GetRollbackRunId())
		assert.Equal(t, []string{"h1", "h3"}, resp.GetRolledBackHosts())
		assert.Equal(t, "rolled_back", resp.GetChange().GetStatus())
		assert.Equal(t, int32(1), atomic.LoadInt32(&engine.rollbackCalled))
	})

	t.Run("engine error maps to internal", func(t *testing.T) {
		engine := &recordingEngine{rbErr: errors.New("snapshot missing")}
		svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "rb-err"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), "completed")

		_, err = svc.RollbackChange(context.Background(), &pb.RollbackRequest{ChangeId: created.GetId()})
		require.Error(t, err)
		assert.Equal(t, codes.Internal, status.Code(err))
	})
}

// --- Approve / Reject with approval service ---------------------------------------------

// stubApprovalStore satisfies approval.Store without persisting anything;
// the tests only need the approval.Service handle to be non-nil.
type stubApprovalStore struct{}

func (stubApprovalStore) Create(context.Context, *approval.Approval) error { return nil }

func (stubApprovalStore) Get(_ context.Context, id string) (*approval.Approval, error) {
	return nil, fmt.Errorf("%w: %s", approval.ErrNotFound, id)
}

func (stubApprovalStore) Update(context.Context, *approval.Approval) error { return nil }

func (stubApprovalStore) UpdateIfPending(context.Context, *approval.Approval) (bool, error) {
	return true, nil
}

func (stubApprovalStore) ListPending(context.Context) ([]*approval.Approval, error) {
	return nil, nil
}

func TestApproveChange_NoPendingApprovalFailsPrecondition(t *testing.T) {
	store := newTestStore(t)
	svc := NewChangeService(store, nil, approval.NewService(stubApprovalStore{}), nil)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "appr-none"})
	require.NoError(t, err)

	_, err = svc.ApproveChange(context.Background(), &pb.ApproveRequest{ChangeId: created.GetId(), Approver: "alice"})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestApproveChange_ListApprovalsErrorMapped(t *testing.T) {
	store := newTestStore(t)
	fs := &errorStore{
		Store:            store,
		listApprovalsErr: errors.New("approvals table corrupt"),
		getRunOverride: func(ctx context.Context, id string) (*state.Run, error) {
			return store.GetRun(ctx, id)
		},
	}
	svc := NewChangeService(fs, nil, approval.NewService(stubApprovalStore{}), nil)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "appr-err2"})
	require.NoError(t, err)

	_, err = svc.ApproveChange(context.Background(), &pb.ApproveRequest{ChangeId: created.GetId(), Approver: "a"})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

func TestRejectChange_TransitionsToRejected(t *testing.T) {
	svc, store := newTestChangeService(t)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "reject-me"})
	require.NoError(t, err)

	resp, err := svc.RejectChange(context.Background(), &pb.RejectRequest{
		ChangeId: created.GetId(),
		Rejecter: "bob",
		Reason:   "risky",
	})
	require.NoError(t, err)
	assert.Equal(t, "rejected", resp.GetStatus())

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "rejected", run.Status)
	assert.Equal(t, "rejected", run.ApprovalStatus)

	audits, err := store.ListAudits(context.Background(), state.AuditFilter{RunID: created.GetId()})
	require.NoError(t, err)
	found := false
	for _, a := range audits {
		if a.Action == "reject" && a.Actor == "bob" {
			found = true
		}
	}
	assert.True(t, found, "expected reject audit entry by bob")
}

func TestRejectChange_StoreErrorMapsToInternal(t *testing.T) {
	store := newTestStore(t)
	fs := &errorStore{
		Store:            store,
		listApprovalsErr: errors.New("reject lookup failed"),
		getRunOverride: func(ctx context.Context, id string) (*state.Run, error) {
			return store.GetRun(ctx, id)
		},
	}
	svc := NewChangeService(fs, nil, approval.NewService(stubApprovalStore{}), nil)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "reject-err"})
	require.NoError(t, err)

	_, err = svc.RejectChange(context.Background(), &pb.RejectRequest{ChangeId: created.GetId(), Rejecter: "bob"})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

// --- error mapping helpers ------------------------------------------------------------

func TestMapApprovalErrorTable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"nil error", nil, codes.OK},
		{"not found", approval.ErrNotFound, codes.NotFound},
		{"invalid transition", fmt.Errorf("wrapped: %w", approval.ErrInvalidTransition), codes.FailedPrecondition},
		{"unauthorized approver", fmt.Errorf("wrapped: %w", approval.ErrUnauthorizedApprover), codes.PermissionDenied},
		{"duplicate decision", fmt.Errorf("wrapped: %w", approval.ErrDuplicateDecision), codes.AlreadyExists},
		{"unknown error", errors.New("mystery"), codes.Internal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapApprovalError(tc.err)
			assert.Equal(t, tc.want, status.Code(got))
		})
	}
}

func TestMapPauseErrorTable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"nil error", nil, codes.OK},
		{"run not found", pause.ErrRunNotFound, codes.NotFound},
		{"not pausable", pause.ErrNotPausable, codes.FailedPrecondition},
		{"not resumable", fmt.Errorf("wrapped: %w", pause.ErrNotResumable), codes.FailedPrecondition},
		{"permission denied", fmt.Errorf("wrapped: %w", pause.ErrPermissionDenied), codes.PermissionDenied},
		{"empty run id", pause.ErrEmptyRunID, codes.InvalidArgument},
		{"empty actor", pause.ErrEmptyActor, codes.InvalidArgument},
		{"unknown error", errors.New("disk on fire"), codes.Internal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapPauseError(tc.err, "run-1")
			assert.Equal(t, tc.want, status.Code(got))
		})
	}
}

// --- pause-manager delegation path -------------------------------------------------------

func TestPauseChange_DelegatesToPauseManager(t *testing.T) {
	store := newTestStore(t)
	svc := NewChangeService(store, nil, nil, pause.NewPauseManager(store))
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "pause-mgr"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), "running")

	resp, err := svc.PauseChange(ctx, &pb.PauseRequest{ChangeId: created.GetId(), Reason: "maintenance"})
	require.NoError(t, err)
	assert.Equal(t, "paused", resp.GetStatus())

	// Resume via the manager too.
	resumed, err := svc.ResumeChange(ctx, &pb.PauseRequest{ChangeId: created.GetId()})
	require.NoError(t, err)
	assert.Equal(t, "running", resumed.GetStatus())
}

func TestPauseChange_PauseManagerRejectsDraftAsPrecondition(t *testing.T) {
	store := newTestStore(t)
	svc := NewChangeService(store, nil, nil, pause.NewPauseManager(store))

	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "pause-mgr-draft"})
	require.NoError(t, err)

	// Draft is not a pausable state; the pause manager rejects it with
	// ErrNotPausable and mapPauseError surfaces codes.FailedPrecondition.
	_, err = svc.PauseChange(context.Background(), &pb.PauseRequest{ChangeId: created.GetId()})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
}

// --- ArchiveChange purge -------------------------------------------------------------------

func TestArchiveChange_PurgeArtifactsDeletesChildren(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "purge-me"})
	require.NoError(t, err)
	id := created.GetId()

	now := time.Now().UTC()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "batch-p1", RunID: id, BatchNo: 1, Status: "completed",
		TotalHosts: 1, Succeeded: 1, StartedAt: &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "step-p1", RunID: id, BatchID: "batch-p1", Host: "h1",
		StepName: "deploy", Status: "completed", Stdout: "out", StartedAt: &now,
	}))
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID: "trc-p1", RunID: id, Event: "start", PrevHash: "", CurrHash: "c1", Timestamp: now,
	}))

	_, err = svc.ArchiveChange(ctx, &pb.ArchiveRequest{ChangeId: id, PurgeArtifacts: true})
	require.NoError(t, err)

	batches, err := store.ListBatches(ctx, state.BatchFilter{RunID: id})
	require.NoError(t, err)
	assert.Empty(t, batches, "batches must be purged")

	steps, err := store.ListSteps(ctx, state.StepFilter{RunID: id})
	require.NoError(t, err)
	assert.Empty(t, steps, "steps must be purged")

	// Traces are deliberately retained: the SQLite store enforces a WORM
	// constraint on the trace table ("trace records cannot be deleted"),
	// so purge covers runtime artifacts (batches/steps) only and trace
	// records survive as immutable audit evidence.
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: id})
	require.NoError(t, err)
	assert.Len(t, traces, 1, "traces are WORM-protected and survive archive purge")
	if len(traces) == 1 {
		assert.Equal(t, "trc-p1", traces[0].ID)
	}
}

// --- GetDiff / GetLogs edges ------------------------------------------------------------------

func TestGetDiff_CustomFormatAndStderr(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "diff-err"})
	require.NoError(t, err)

	now := time.Now().UTC()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "batch-d2", RunID: created.GetId(), BatchNo: 1, Status: "failed",
		TotalHosts: 1, StartedAt: &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "step-d2", RunID: created.GetId(), BatchID: "batch-d2", Host: "h2",
		StepName: "verify", Status: "failed", Stderr: "exit code 1", StartedAt: &now,
	}))

	resp, err := svc.GetDiff(ctx, &pb.GetDiffRequest{ChangeId: created.GetId(), Format: "json"})
	require.NoError(t, err)
	assert.Equal(t, "json", resp.GetFormat(), "explicit format overrides default")
	assert.Contains(t, resp.GetDiff(), "--- stderr ---")
	assert.Contains(t, resp.GetDiff(), "exit code 1")
	assert.Contains(t, resp.GetDiff(), "h2 @ verify")
}

func TestGetDiff_NotFound(t *testing.T) {
	svc, _ := newTestChangeService(t)
	_, err := svc.GetDiff(context.Background(), &pb.GetDiffRequest{ChangeId: "missing"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestGetLogs_TruncationAndLevels(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "logs-trunc"})
	require.NoError(t, err)
	id := created.GetId()

	now := time.Now().UTC()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "batch-l2", RunID: id, BatchNo: 1, Status: "completed",
		TotalHosts: 1, Succeeded: 1, StartedAt: &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "step-l2", RunID: id, BatchID: "batch-l2", Host: "h1",
		StepName: "deploy", Status: "completed", Stdout: "fine", Stderr: "warned", StartedAt: &now,
	}))

	t.Run("stderr yields ERROR entry", func(t *testing.T) {
		resp, err := svc.GetLogs(ctx, &pb.GetLogsRequest{ChangeId: id})
		require.NoError(t, err)
		require.Len(t, resp.GetEntries(), 2)
		assert.Equal(t, "INFO", resp.GetEntries()[0].GetLevel())
		assert.Equal(t, "ERROR", resp.GetEntries()[1].GetLevel())
		assert.Equal(t, "h1", resp.GetEntries()[0].GetSource())
		assert.NotEmpty(t, resp.GetEntries()[0].GetTimestamp())
	})

	t.Run("limit truncates and flags response", func(t *testing.T) {
		resp, err := svc.GetLogs(ctx, &pb.GetLogsRequest{ChangeId: id, Limit: 1})
		require.NoError(t, err)
		require.Len(t, resp.GetEntries(), 1)
		assert.True(t, resp.GetTruncated())
	})

	t.Run("run_id overrides change_id", func(t *testing.T) {
		resp, err := svc.GetLogs(ctx, &pb.GetLogsRequest{ChangeId: "ignored", RunId: id})
		require.NoError(t, err)
		require.NotEmpty(t, resp.GetEntries())
		assert.Equal(t, id, resp.GetEntries()[0].GetRunId())
	})

	t.Run("unsupported level is rejected", func(t *testing.T) {
		// DEBUG/WARN do not exist in the stdout/stderr projection; they
		// must be a client error rather than silently matching nothing.
		for _, lvl := range []string{"DEBUG", "WARN"} {
			_, err := svc.GetLogs(ctx, &pb.GetLogsRequest{ChangeId: id, Levels: []string{lvl}})
			require.Error(t, err, "level %s", lvl)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "level %s", lvl)
			assert.Contains(t, status.Convert(err).Message(), "unsupported level")
		}
	})

	t.Run("limit applies after filtering", func(t *testing.T) {
		// The step has 1 INFO + 1 ERROR entry. Requesting only ERROR with
		// limit=1 must return that one entry (the old pre-limit on
		// ListSteps would have truncated steps before filtering).
		resp, err := svc.GetLogs(ctx, &pb.GetLogsRequest{ChangeId: id, Levels: []string{"ERROR"}, Limit: 5})
		require.NoError(t, err)
		require.Len(t, resp.GetEntries(), 1)
		assert.False(t, resp.GetTruncated())
		assert.Equal(t, "ERROR", resp.GetEntries()[0].GetLevel())
	})

	t.Run("since filters entries older than the timestamp", func(t *testing.T) {
		old := time.Now().UTC().Add(-2 * time.Hour)
		require.NoError(t, store.CreateBatch(ctx, &state.Batch{
			ID: "batch-old", RunID: id, BatchNo: 2, Status: "completed",
			TotalHosts: 1, Succeeded: 1, StartedAt: &old,
		}))
		require.NoError(t, store.CreateStep(ctx, &state.Step{
			ID: "step-old", RunID: id, BatchID: "batch-old", Host: "h9",
			StepName: "old-deploy", Status: "completed", Stdout: "ancient", StartedAt: &old,
		}))

		since := time.Now().UTC().Add(-time.Minute).Unix()
		resp, err := svc.GetLogs(ctx, &pb.GetLogsRequest{ChangeId: id, Since: since})
		require.NoError(t, err)
		for _, e := range resp.GetEntries() {
			assert.NotEqual(t, "ancient", e.GetMessage(), "entries before ?since must be filtered")
		}
		require.NotEmpty(t, resp.GetEntries())
	})
}

// --- conversion helpers --------------------------------------------------------------------

func TestRunToPB_NilReturnsNil(t *testing.T) {
	assert.Nil(t, runToPB(nil))
}

func TestRunToPB_InvalidParamsJSONYieldsEmptyMap(t *testing.T) {
	now := time.Now().UTC()
	c := runToPB(&state.Run{
		ID: "run-x", Params: "{not json", WorkflowName: "wf", TemplateName: "tpl",
		ApprovalLevel: "high", CreatedAt: now, UpdatedAt: now, Creator: "op", IncidentID: "prod",
	})
	require.NotNil(t, c)
	assert.Empty(t, c.GetParams())
	assert.Equal(t, "wf", c.GetLabel())
	assert.Equal(t, "wf", c.GetWorkflowFile())
	assert.Equal(t, "high", c.GetPriority())
	assert.Equal(t, "prod", c.GetEnvironment())
	assert.EqualValues(t, now.Unix(), c.GetCreatedAt())
}

func TestPbToRunParams(t *testing.T) {
	assert.Equal(t, "{}", pbToRunParams(nil))
	assert.JSONEq(t, `{"k":"v"}`, pbToRunParams(map[string]string{"k": "v"}))
}

func TestFormatTime(t *testing.T) {
	assert.Empty(t, formatTime(nil))
	now := time.Date(2025, 6, 1, 12, 30, 0, 0, time.UTC)
	assert.Equal(t, "2025-06-01T12:30:00Z", formatTime(&now))
}

func TestActorFromCtx(t *testing.T) {
	ctx := context.WithValue(context.Background(), actorKey{}, "alice")
	assert.Equal(t, "alice", actorFromCtx(ctx))
	assert.Equal(t, "grpc-user", actorFromCtx(context.Background()))
	assert.Equal(t, "grpc-user", actorFromCtx(context.WithValue(context.Background(), actorKey{}, "")))
}

func TestParsePageTokenTable(t *testing.T) {
	tests := []struct {
		token    string
		want     int
		wantCode codes.Code // codes.OK means no error expected
	}{
		{"", 0, codes.OK},
		{"7", 7, codes.OK},
		{"not-a-number", 0, codes.InvalidArgument},
		{"-3", 0, codes.InvalidArgument},
	}
	for _, tc := range tests {
		got, err := parsePageToken(tc.token)
		assert.Equal(t, tc.want, got, "parsePageToken(%q) offset", tc.token)
		assert.Equal(t, tc.wantCode, status.Code(err), "parsePageToken(%q) error", tc.token)
		if tc.wantCode == codes.OK {
			assert.NoError(t, err, "parsePageToken(%q)", tc.token)
		}
	}
}

// --- StreamLogs ------------------------------------------------------------------------------

// fakeLogStream is a test double for grpc.ServerStreamingServer[pb.LogEntry].
type fakeLogStream struct {
	ctx     context.Context
	entries []*pb.LogEntry
	sendErr error
}

func (f *fakeLogStream) Context() context.Context { return f.ctx }

func (f *fakeLogStream) SendMsg(m interface{}) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	if e, ok := m.(*pb.LogEntry); ok {
		f.entries = append(f.entries, e)
	}
	return nil
}

func (f *fakeLogStream) RecvMsg(interface{}) error { return nil }
func (f *fakeLogStream) Send(e *pb.LogEntry) error { return f.SendMsg(e) }
func (f *fakeLogStream) SetHeader(metadata.MD) error {
	return nil
}
func (f *fakeLogStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeLogStream) SetTrailer(metadata.MD)       {}

// seedLogFixture creates a run, a batch and two steps (one stdout-only, one
// with both stdout and stderr) under the fixed run id "run-sl".
func seedLogFixture(t *testing.T, store state.Store) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-sl", WorkflowName: "wf", Status: "running",
		CreatedAt: now, UpdatedAt: now, Creator: "test",
	}))
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "batch-sl", RunID: "run-sl", BatchNo: 1, Status: "completed",
		TotalHosts: 2, Succeeded: 2, StartedAt: &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "step-sl-1", RunID: "run-sl", BatchID: "batch-sl", Host: "h1",
		StepName: "deploy", Status: "completed", Stdout: "stdout line", StartedAt: &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "step-sl-2", RunID: "run-sl", BatchID: "batch-sl", Host: "h2",
		StepName: "verify", Status: "completed", Stdout: "both out", Stderr: "both err", StartedAt: &now,
	}))
	return "run-sl"
}

func TestStreamLogs_ReplayOnlyWithoutFollow(t *testing.T) {
	svc, store := newTestChangeService(t)
	runID := seedLogFixture(t, store)

	stream := &fakeLogStream{ctx: context.Background()}
	err := svc.StreamLogs(&pb.StreamLogsRequest{ChangeId: runID, Follow: false}, stream)
	require.NoError(t, err)
	assert.Len(t, stream.entries, 3, "one stdout-only step + stdout/stderr pair")
	assert.Equal(t, "INFO", stream.entries[0].GetLevel())
	assert.Equal(t, "stdout line", stream.entries[0].GetMessage())
}

func TestStreamLogs_LevelFilter(t *testing.T) {
	svc, store := newTestChangeService(t)
	runID := seedLogFixture(t, store)

	stream := &fakeLogStream{ctx: context.Background()}
	err := svc.StreamLogs(&pb.StreamLogsRequest{ChangeId: runID, Levels: []string{"ERROR"}}, stream)
	require.NoError(t, err)
	require.Len(t, stream.entries, 1)
	assert.Equal(t, "ERROR", stream.entries[0].GetLevel())
	assert.Equal(t, "both err", stream.entries[0].GetMessage())
}

func TestStreamLogs_SendFailureAbortsStream(t *testing.T) {
	svc, store := newTestChangeService(t)
	runID := seedLogFixture(t, store)

	stream := &fakeLogStream{ctx: context.Background(), sendErr: errors.New("client gone")}
	err := svc.StreamLogs(&pb.StreamLogsRequest{ChangeId: runID}, stream)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

func TestStreamLogs_EmptyRunYieldsNothing(t *testing.T) {
	svc, _ := newTestChangeService(t)
	stream := &fakeLogStream{ctx: context.Background()}
	err := svc.StreamLogs(&pb.StreamLogsRequest{ChangeId: "run-empty", Follow: false}, stream)
	require.NoError(t, err)
	assert.Empty(t, stream.entries)
}

// --- WatchChange filters and terminal states -----------------------------------------------

func TestWatchChange_TerminalEventEndsStream(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "watch-terminal"})
	require.NoError(t, err)

	stream := &fakeStream{ctx: ctx}
	watchDone := make(chan error, 1)
	go func() {
		watchDone <- svc.WatchChange(&pb.WatchChangeRequest{ChangeId: created.GetId()}, stream)
	}()

	// Give the watcher time to subscribe, then push a terminal event.
	time.Sleep(50 * time.Millisecond)
	svc.publishEvent(&pb.ChangeEvent{
		ChangeId:  created.GetId(),
		EventType: "status_changed",
		NewStatus: "completed",
		Timestamp: time.Now().Unix(),
	})

	select {
	case err := <-watchDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("WatchChange did not terminate on terminal-state event")
	}
	require.NotEmpty(t, stream.events)
	assert.Equal(t, "completed", stream.events[len(stream.events)-1].GetNewStatus())
}

func TestWatchChange_EventTypeFilterDropsOtherEvents(t *testing.T) {
	svc, _ := newTestChangeService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "watch-filter"})
	require.NoError(t, err)

	stream := &fakeStream{ctx: ctx}
	watchDone := make(chan error, 1)
	go func() {
		watchDone <- svc.WatchChange(&pb.WatchChangeRequest{
			ChangeId:   created.GetId(),
			EventTypes: []string{"step_completed"},
		}, stream)
	}()
	time.Sleep(50 * time.Millisecond)

	// This event does not match the filter and must not reach the stream.
	svc.publishEvent(&pb.ChangeEvent{ChangeId: created.GetId(), EventType: "status_changed", Timestamp: time.Now().Unix()})
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-watchDone

	for _, ev := range stream.events {
		assert.Equal(t, "step_completed", ev.GetEventType(), "filtered-out event leaked to subscriber")
	}
}

func TestWatchChange_GetRunErrorIsInternal(t *testing.T) {
	store := newTestStore(t)
	fs := &errorStore{Store: store, getRunErr: errors.New("watch lookup failed")}
	svc := NewChangeService(fs, nil, nil, nil)

	stream := &fakeStream{ctx: context.Background()}
	err := svc.WatchChange(&pb.WatchChangeRequest{ChangeId: "any"}, stream)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

// --- event bus --------------------------------------------------------------------------------

func TestEventBusSubscribeUnsubscribeLifecycle(t *testing.T) {
	bus := newEventBus()

	chA := bus.subscribe("run-a")
	chB := bus.subscribe("run-a")
	require.Len(t, bus.subs["run-a"], 2, "two subscribers expected")

	bus.unsubscribe("run-a", chA)
	require.Len(t, bus.subs["run-a"], 1)

	// Unsubscribing the last subscriber removes the change entry entirely.
	bus.unsubscribe("run-a", chB)
	assert.NotContains(t, bus.subs, "run-a")

	// Unsubscribing an unknown channel is a no-op.
	bus.unsubscribe("run-a", chB)

	// The surviving channel still receives publishes.
	svc := &ChangeService{}
	svc.eventMu.Lock()
	svc.eventBus = bus
	svc.eventMu.Unlock()

	chC := bus.subscribe("run-b")
	svc.publishEvent(&pb.ChangeEvent{ChangeId: "run-b", EventType: "step_started"})
	select {
	case ev := <-chC:
		assert.Equal(t, "run-b", ev.GetChangeId())
	default:
		t.Fatal("expected published event on subscriber channel")
	}
}

func TestPublishEventWithoutBusIsNoOp(t *testing.T) {
	svc := &ChangeService{}
	assert.NotPanics(t, func() {
		svc.publishEvent(&pb.ChangeEvent{ChangeId: "run-x", EventType: "status_changed"})
	})
}

func TestPublishEventDropsForSlowSubscriber(t *testing.T) {
	svc := &ChangeService{}
	bus := svc.getEventBus()
	ch := bus.subscribe("run-slow")

	// Fill the bounded buffer (capacity 16), then publish more.
	for i := 0; i < 16; i++ {
		svc.publishEvent(&pb.ChangeEvent{ChangeId: "run-slow", EventType: "fill"})
	}
	before := atomic.LoadInt64(&bus.dropped)
	svc.publishEvent(&pb.ChangeEvent{ChangeId: "run-slow", EventType: "overflow"})

	assert.Greater(t, atomic.LoadInt64(&bus.dropped), before, "overflow publish must be counted as dropped")

	// Drain and confirm only the buffered events arrived.
	drained := 0
	for len(ch) > 0 {
		<-ch
		drained++
	}
	assert.Equal(t, 16, drained)
}

// TestApplyChange_EngineRolledBackPersistsStatus verifies the phase
// propagation: when the engine reports phase "rolled_back" the run ends in
// "rolled_back" (not "failed"), so a successful rollback is distinguishable
// from a hard failure for retry/rollback tooling.
func TestApplyChange_EngineRolledBackPersistsStatus(t *testing.T) {
	engine := &recordingEngine{runID: "exec-rb-1", runSuccess: false, runPhase: "rolled_back"}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-rb"})
	require.NoError(t, err)

	resp, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId:    created.GetId(),
		AutoApprove: true,
	})
	require.NoError(t, err)
	assert.False(t, resp.GetSuccess())
	assert.Equal(t, "rolled_back", resp.GetMessage())
	assert.Equal(t, "rolled_back", resp.GetChange().GetStatus())

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "rolled_back", run.Status)
}

// TestApplyChange_ConcurrentDoubleApplyIsSerialised verifies the CAS guard:
// two racing ApplyChange calls on the same run cannot both transition it.
// The first caller wins the "approved -> running" transition; the second
// observes a FailedPrecondition with the current (already terminal) state.
func TestApplyChange_ConcurrentDoubleApplyIsSerialised(t *testing.T) {
	// The stub engine is deliberately slow so both callers pass the
	// advisory state guard before either reaches the store.
	release := make(chan struct{})
	engine := &EngineAdapter{
		Run: func(_ context.Context, _ string, _ bool, _ int32) (string, bool, string, error) {
			<-release
			return "exec-race", true, "completed", nil
		},
	}
	svc, store := newTestChangeServiceWithEngine(t, engine)

	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-race"})
	require.NoError(t, err)
	// Approve the run so both goroutines take the non-auto-approve path and
	// both see status "approved" before the CAS decides the winner.
	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	run.Status = "approved"
	require.NoError(t, store.UpdateRun(context.Background(), run))

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{ChangeId: created.GetId()})
			results[idx] = err
		}(i)
	}
	close(start)
	// Let both goroutines reach the store, then unblock the engine.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
			continue
		}
		// The loser must surface a precondition failure, not an internal
		// error, and must never have seen a bogus double-execution.
		assert.Equal(t, codes.FailedPrecondition, status.Code(err), "loser error: %v", err)
	}
	assert.Equal(t, 1, successes, "exactly one apply must win the CAS race")

	run, err = store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "completed", run.Status)
}
