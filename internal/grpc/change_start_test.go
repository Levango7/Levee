package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/tenant"
)

// startTestEngine blocks until the test releases the accepted execution. The
// channels make "Start returned before settlement" an observation at the RPC
// boundary rather than a timing assumption.
type startTestEngine struct {
	calls    atomic.Int32
	started  chan struct{}
	release  chan struct{}
	finished chan error
	once     sync.Once
	runErr   error
}

func newStartTestEngine() *startTestEngine {
	return &startTestEngine{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan error, 1),
	}
}

func (e *startTestEngine) adapter() *EngineAdapter {
	return &EngineAdapter{Run: func(ctx context.Context, _ string, _ bool, _ int32) (string, bool, string, error) {
		e.calls.Add(1)
		e.once.Do(func() { close(e.started) })
		select {
		case <-e.release:
			e.finished <- e.runErr
			return "exec-start", e.runErr == nil, "", e.runErr
		case <-ctx.Done():
			e.finished <- ctx.Err()
			return "exec-start", false, "", ctx.Err()
		}
	}}
}

func (e *startTestEngine) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-e.started:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted execution did not reach the engine")
	}
}

func createStartableChange(t *testing.T, svc *ChangeService, store state.Store) string {
	t.Helper()
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "start-change"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())
	return created.GetId()
}

func waitForChangeStatus(t *testing.T, svc *ChangeService, changeID, want string) *pb.Change {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		change, err := svc.GetChange(context.Background(), &pb.GetChangeRequest{Id: changeID})
		require.NoError(t, err)
		if change.GetStatus() == want {
			return change
		}
		time.Sleep(10 * time.Millisecond)
	}
	change, err := svc.GetChange(context.Background(), &pb.GetChangeRequest{Id: changeID})
	require.NoError(t, err)
	require.Equal(t, want, change.GetStatus())
	return change
}

func TestStartChangeAcceptsExecutesAndReconnects(t *testing.T) {
	engine := newStartTestEngine()
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	changeID := createStartableChange(t, svc, store)
	req := &pb.StartChangeRequest{
		ChangeId:       changeID,
		AutoApprove:    true,
		MaxConcurrency: 3,
		IdempotencyKey: "request-accepted",
	}

	accepted, err := svc.StartChange(context.Background(), req)
	require.NoError(t, err, "StartChange must return while the engine is still blocked")
	assert.False(t, accepted.GetDuplicate())
	assert.Equal(t, changeID, accepted.GetExecutionId())
	assert.Equal(t, runstatus.StatusRunning, accepted.GetStatus())
	assert.Equal(t, runstatus.StatusRunning, accepted.GetChange().GetStatus())
	engine.waitStarted(t)

	duplicate, err := svc.StartChange(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, duplicate.GetDuplicate())
	assert.Equal(t, changeID, duplicate.GetExecutionId())
	assert.Equal(t, runstatus.StatusRunning, duplicate.GetStatus())
	assert.Equal(t, int32(1), engine.calls.Load(), "a replay must not invoke the engine again")

	close(engine.release)
	waitForChangeStatus(t, svc, changeID, runstatus.StatusCompleted)
	require.NoError(t, <-engine.finished)

	reconnected, err := svc.StartChange(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, reconnected.GetDuplicate())
	assert.Equal(t, runstatus.StatusCompleted, reconnected.GetStatus())
	assert.Equal(t, int32(1), engine.calls.Load())
}

func TestStartChangeRejectsSameKeyWithDifferentParameters(t *testing.T) {
	engine := newStartTestEngine()
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	changeID := createStartableChange(t, svc, store)
	req := &pb.StartChangeRequest{ChangeId: changeID, AutoApprove: true, IdempotencyKey: "request-parameters"}
	_, err := svc.StartChange(context.Background(), req)
	require.NoError(t, err)
	engine.waitStarted(t)

	for name, mutate := range map[string]func(*pb.StartChangeRequest){
		"auto approve":    func(r *pb.StartChangeRequest) { r.AutoApprove = false },
		"max concurrency": func(r *pb.StartChangeRequest) { r.MaxConcurrency = 2 },
		"dry run":         func(r *pb.StartChangeRequest) { r.DryRun = true },
	} {
		t.Run(name, func(t *testing.T) {
			conflicting := &pb.StartChangeRequest{
				ChangeId: req.ChangeId, AutoApprove: req.AutoApprove, MaxConcurrency: req.MaxConcurrency,
				DryRun: req.DryRun, IdempotencyKey: req.IdempotencyKey,
			}
			mutate(conflicting)
			_, err := svc.StartChange(context.Background(), conflicting)
			require.Error(t, err)
			assert.Equal(t, codes.AlreadyExists, status.Code(err))
		})
	}
	assert.Equal(t, int32(1), engine.calls.Load())
	close(engine.release)
	waitForChangeStatus(t, svc, changeID, runstatus.StatusCompleted)
	require.NoError(t, <-engine.finished)
}

func TestStartChangeFailureSettlesInBackground(t *testing.T) {
	engine := newStartTestEngine()
	engine.runErr = errors.New("executor unavailable")
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	changeID := createStartableChange(t, svc, store)

	accepted, err := svc.StartChange(context.Background(), &pb.StartChangeRequest{
		ChangeId: changeID, AutoApprove: true, IdempotencyKey: "request-failure",
	})
	require.NoError(t, err)
	assert.Equal(t, runstatus.StatusRunning, accepted.GetStatus())
	engine.waitStarted(t)
	close(engine.release)
	waitForChangeStatus(t, svc, changeID, runstatus.StatusFailed)
	require.Error(t, <-engine.finished)
}

// startReplayBarrierStore holds the initial idempotency reads until every
// caller has observed "no key". It deterministically exercises the race between
// the running CAS and durable key insertion without changing the public RPC.
type startReplayBarrierStore struct {
	state.Store
	waiters atomic.Int32
	ready   chan struct{}
}

func (s *startReplayBarrierStore) GetChangeIdempotency(ctx context.Context, changeID, key string) (*state.ChangeIdempotency, error) {
	if remaining := s.waiters.Add(-1); remaining >= 0 {
		if remaining == 0 {
			close(s.ready)
		}
		<-s.ready
	}
	return s.Store.(state.ChangeIdempotencyStore).GetChangeIdempotency(ctx, changeID, key)
}

func (s *startReplayBarrierStore) ClaimChangeExecution(ctx context.Context, record *state.ChangeIdempotency, fromStatus, toStatus string, updatedAt time.Time) (bool, bool, error) {
	return s.Store.(state.ChangeIdempotencyStore).ClaimChangeExecution(ctx, record, fromStatus, toStatus, updatedAt)
}

func TestStartChangeConcurrentSameKeyExecutesOnce(t *testing.T) {
	const callers = 12
	engine := newStartTestEngine()
	store := &startReplayBarrierStore{Store: newTestStore(t), ready: make(chan struct{})}
	store.waiters.Store(callers)
	svc := NewChangeService(store, engine.adapter(), nil, nil)
	changeID := createStartableChange(t, svc, store)
	begin := make(chan struct{})
	type result struct {
		response *pb.StartChangeResponse
		err      error
	}
	results := make(chan result, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			<-begin
			response, err := svc.StartChange(context.Background(), &pb.StartChangeRequest{
				ChangeId: changeID, AutoApprove: true, MaxConcurrency: 2, IdempotencyKey: "request-concurrent",
			})
			results <- result{response: response, err: err}
		}()
	}
	close(begin)
	wg.Wait()
	close(results)

	accepted, duplicates := 0, 0
	for result := range results {
		require.NoError(t, result.err)
		require.NotNil(t, result.response)
		if result.response.GetDuplicate() {
			duplicates++
		} else {
			accepted++
		}
		assert.Equal(t, changeID, result.response.GetExecutionId())
	}
	assert.Equal(t, 1, accepted, "exactly one caller may accept new work")
	assert.Equal(t, callers-1, duplicates, "every other caller must observe the durable replay")
	assert.Equal(t, int32(1), engine.calls.Load())
	close(engine.release)
	waitForChangeStatus(t, svc, changeID, runstatus.StatusCompleted)
	require.NoError(t, <-engine.finished)
}

func TestStartChangeValidatesIdempotencyKey(t *testing.T) {
	svc := NewChangeService(nil, nil, nil, nil)
	for name, key := range map[string]string{
		"empty":      "",
		"blank":      "   ",
		"too long":   strings.Repeat("k", maxStartIdempotencyKeyLen+1),
		"utf8 bytes": strings.Repeat("界", 43),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.StartChange(context.Background(), &pb.StartChangeRequest{IdempotencyKey: key})
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}
}

func TestStartChangeDisconnectDoesNotCancelAcceptedExecution(t *testing.T) {
	engine := newStartTestEngine()
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	changeID := createStartableChange(t, svc, store)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := svc.StartChange(ctx, &pb.StartChangeRequest{
		ChangeId: changeID, AutoApprove: true, IdempotencyKey: "request-disconnect",
	})
	require.NoError(t, err)
	engine.waitStarted(t)
	cancel()
	select {
	case err := <-engine.finished:
		t.Fatalf("client cancellation stopped accepted execution: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(engine.release)
	waitForChangeStatus(t, svc, changeID, runstatus.StatusCompleted)
	require.NoError(t, <-engine.finished)
}

func TestStartChangeReplaySurvivesProcessRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "start-restart.db")
	store, err := state.NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	engine := newStartTestEngine()
	svc := NewChangeService(store, engine.adapter(), nil, nil)
	changeID := createStartableChange(t, svc, store)
	req := &pb.StartChangeRequest{ChangeId: changeID, AutoApprove: true, IdempotencyKey: "request-restart"}
	_, err = svc.StartChange(ctx, req)
	require.NoError(t, err)
	engine.waitStarted(t)
	close(engine.release)
	waitForChangeStatus(t, svc, changeID, runstatus.StatusCompleted)
	require.NoError(t, <-engine.finished)
	require.NoError(t, store.Close())

	reopened, err := state.NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := NewChangeService(reopened, engine.adapter(), nil, nil)
	replay, err := restarted.StartChange(ctx, req)
	require.NoError(t, err)
	assert.True(t, replay.GetDuplicate())
	assert.Equal(t, runstatus.StatusCompleted, replay.GetStatus())
	assert.Equal(t, int32(1), engine.calls.Load(), "restart replay must not execute again")
}

type startWithoutIdempotencyStore struct{ state.Store }

func TestStartChangeUnsupportedStoreFailsClosed(t *testing.T) {
	engine := newStartTestEngine()
	store := startWithoutIdempotencyStore{Store: newTestStore(t)}
	svc := NewChangeService(store, engine.adapter(), nil, nil)
	changeID := createStartableChange(t, svc, store)
	_, err := svc.StartChange(context.Background(), &pb.StartChangeRequest{
		ChangeId: changeID, AutoApprove: true, IdempotencyKey: "request-unsupported",
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Equal(t, int32(0), engine.calls.Load())
	change, err := svc.GetChange(context.Background(), &pb.GetChangeRequest{Id: changeID})
	require.NoError(t, err)
	assert.Equal(t, runstatus.StatusDraft, change.GetStatus())
}

type startClaimErrorStore struct {
	state.Store
	claimErr error
}

func (s *startClaimErrorStore) GetChangeIdempotency(ctx context.Context, changeID, key string) (*state.ChangeIdempotency, error) {
	return s.Store.(state.ChangeIdempotencyStore).GetChangeIdempotency(ctx, changeID, key)
}

func (s *startClaimErrorStore) ClaimChangeExecution(ctx context.Context, record *state.ChangeIdempotency, fromStatus, toStatus string, updatedAt time.Time) (bool, bool, error) {
	if s.claimErr != nil {
		return false, false, s.claimErr
	}
	return s.Store.(state.ChangeIdempotencyStore).ClaimChangeExecution(ctx, record, fromStatus, toStatus, updatedAt)
}

func TestStartChangeClaimFailureLeavesRunRetryable(t *testing.T) {
	engine := newStartTestEngine()
	store := &startClaimErrorStore{Store: newTestStore(t), claimErr: errors.New("database full")}
	svc := NewChangeService(store, engine.adapter(), nil, nil)
	changeID := createStartableChange(t, svc, store)
	req := &pb.StartChangeRequest{ChangeId: changeID, AutoApprove: true, IdempotencyKey: "request-claim-error"}
	_, err := svc.StartChange(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, err.Error(), "database full")
	assert.Equal(t, int32(0), engine.calls.Load())
	change, err := svc.GetChange(context.Background(), &pb.GetChangeRequest{Id: changeID})
	require.NoError(t, err)
	assert.Equal(t, runstatus.StatusDraft, change.GetStatus())

	store.claimErr = nil
	_, err = svc.StartChange(context.Background(), req)
	require.NoError(t, err, "a failed claim must not leave a key or running state behind")
	engine.waitStarted(t)
	close(engine.release)
	waitForChangeStatus(t, svc, changeID, runstatus.StatusCompleted)
	require.NoError(t, <-engine.finished)
}

func TestStartChangeDryRunRefusalDoesNotConsumeKey(t *testing.T) {
	engine := newStartTestEngine()
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	changeID := createStartableChange(t, svc, store)
	req := &pb.StartChangeRequest{ChangeId: changeID, AutoApprove: true, DryRun: true, IdempotencyKey: "request-dry-run"}
	_, err := svc.StartChange(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Equal(t, int32(0), engine.calls.Load())

	req.DryRun = false
	_, err = svc.StartChange(context.Background(), req)
	require.NoError(t, err)
	engine.waitStarted(t)
	close(engine.release)
	waitForChangeStatus(t, svc, changeID, runstatus.StatusCompleted)
	require.NoError(t, <-engine.finished)
}

func TestStartChangeTenantScopeSurvivesBackgroundExecution(t *testing.T) {
	engine := newStartTestEngine()
	base := newTestStore(t)
	store := tenant.NewTenantStore(base, tenant.NewResolver(true), nil)
	svc := NewChangeService(store, engine.adapter(), nil, nil)
	ctx := tenant.ContextWithTenant(context.Background(), "acme")
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "tenant-start"})
	require.NoError(t, err)
	changeID := created.GetId()
	p := testPlan()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	run, err := store.GetRun(ctx, changeID)
	require.NoError(t, err)
	run.PlanJSON = string(raw)
	run.PlanHash = plan.ComputeHash(p)
	run.UpdatedAt = time.Now().UTC()
	require.NoError(t, store.UpdateRun(ctx, run))

	req := &pb.StartChangeRequest{ChangeId: changeID, AutoApprove: true, IdempotencyKey: "tenant-request"}
	_, err = svc.StartChange(ctx, req)
	require.NoError(t, err)
	engine.waitStarted(t)
	close(engine.release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		change, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: changeID})
		require.NoError(t, err)
		if change.GetStatus() == runstatus.StatusCompleted {
			replay, err := svc.StartChange(ctx, req)
			require.NoError(t, err)
			assert.True(t, replay.GetDuplicate())
			assert.Equal(t, runstatus.StatusCompleted, replay.GetStatus())
			require.NoError(t, <-engine.finished)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	change, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: changeID})
	require.NoError(t, err)
	assert.Equal(t, runstatus.StatusCompleted, change.GetStatus())
}
