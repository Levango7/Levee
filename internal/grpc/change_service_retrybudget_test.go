package grpc

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/retrybudget"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

// The retry cap is a documented product promise (docs/levee-api.md:
// 重试次数有上限（默认 3），超限升级人工) and these RPCs did not enforce it at
// all — they were the way around the limit the CLI enforces. The assertions
// that matter are two: the refusal code (ResourceExhausted ⇒ REST 429) and
// that the ENGINE WAS NOT CALLED, because a refused retry must not execute.

// seedRetryRows writes n retry audit rows for one host (target <run>/<host>),
// the shape both entry points now write.
func seedRetryRows(t *testing.T, store state.Store, runID, host string, n int) {
	t.Helper()
	now := time.Now().UTC()
	for i := range n {
		require.NoError(t, store.CreateAudit(context.Background(), &state.Audit{
			ID:     fmt.Sprintf("seed-%s-%s-%d", runID, host, i),
			RunID:  runID,
			Action: state.AuditActionRetryHost,
			Actor:  "operator",
			Target: state.AuditTargetHost(runID, host),
			Result: state.AuditResultSuccess, Timestamp: now,
		}))
	}
}

func seedRunRetryRows(t *testing.T, store state.Store, runID string, n int) {
	t.Helper()
	now := time.Now().UTC()
	for i := range n {
		require.NoError(t, store.CreateAudit(context.Background(), &state.Audit{
			ID:     fmt.Sprintf("seed-run-%s-%d", runID, i),
			RunID:  runID,
			Action: state.AuditActionRetry,
			Actor:  "operator",
			Target: runID,
			Result: state.AuditResultSuccess, Timestamp: now,
		}))
	}
}

func TestRetryChangeRefusesAtTheCapWithoutExecuting(t *testing.T) {
	ctx := context.Background()

	t.Run("under the cap it retries", func(t *testing.T) {
		engine := &recordingEngine{}
		svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "budget-ok"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), runstatus.StatusFailed)
		seedRunRetryRows(t, store, created.GetId(), retrybudget.MaxAttempts-1)

		_, err = svc.RetryChange(ctx, &pb.RetryRequest{ChangeId: created.GetId()})
		require.NoError(t, err)
		assert.Equal(t, int32(1), atomic.LoadInt32(&engine.retryCalled), "a retry under the cap must execute")
	})

	t.Run("at the cap it refuses and does not execute", func(t *testing.T) {
		engine := &recordingEngine{}
		svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "budget-full"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), runstatus.StatusFailed)
		seedRunRetryRows(t, store, created.GetId(), retrybudget.MaxAttempts)

		_, err = svc.RetryChange(ctx, &pb.RetryRequest{ChangeId: created.GetId()})
		require.Error(t, err)
		assert.Equal(t, codes.ResourceExhausted, status.Code(err),
			"REST maps ResourceExhausted to 429; the cap is a rate/quota refusal, not a precondition")
		assert.Contains(t, err.Error(), fmt.Sprintf("(%d/%d)", retrybudget.MaxAttempts, retrybudget.MaxAttempts))
		assert.Zero(t, atomic.LoadInt32(&engine.retryCalled),
			"the refusal must come BEFORE the engine call: a refused retry cannot execute")
	})

	t.Run("a replan does not bypass the cap", func(t *testing.T) {
		// replan re-generates a plan and re-executes it, so exempting it
		// would make "retry with replan" the new way around the limit.
		engine := &recordingEngine{}
		svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "budget-replan"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), runstatus.StatusFailed)
		seedRunRetryRows(t, store, created.GetId(), retrybudget.MaxAttempts)

		_, err = svc.RetryChange(ctx, &pb.RetryRequest{ChangeId: created.GetId(), Replan: true})
		require.Error(t, err)
		assert.Equal(t, codes.ResourceExhausted, status.Code(err))
		assert.Zero(t, atomic.LoadInt32(&engine.retryCalled))
	})
}

func TestRetryHostRefusesAtThePerHostCapWithoutExecuting(t *testing.T) {
	ctx := context.Background()

	t.Run("under the cap it retries", func(t *testing.T) {
		engine := &recordingEngine{}
		svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "host-budget-ok"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), runstatus.StatusFailed)
		seedRetryRows(t, store, created.GetId(), "h1", retrybudget.MaxAttempts-1)

		_, err = svc.RetryHost(ctx, &pb.RetryHostRequest{ChangeId: created.GetId(), Hosts: []string{"h1"}})
		require.NoError(t, err)
		assert.Equal(t, int32(1), atomic.LoadInt32(&engine.retryCalled))
	})

	t.Run("at the cap it refuses, names the host, and does not execute", func(t *testing.T) {
		engine := &recordingEngine{}
		svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "host-budget-full"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), runstatus.StatusFailed)
		seedRetryRows(t, store, created.GetId(), "h1", retrybudget.MaxAttempts)

		_, err = svc.RetryHost(ctx, &pb.RetryHostRequest{
			ChangeId: created.GetId(),
			Hosts:    []string{"h1", "h2"}, // h2 has budget; h1 does not
		})
		require.Error(t, err)
		assert.Equal(t, codes.ResourceExhausted, status.Code(err))
		assert.Contains(t, err.Error(), "h1", "the refusal must name the exhausted host")
		assert.NotContains(t, err.Error(), "h2,", "and not blame a host that still has budget")
		assert.Zero(t, atomic.LoadInt32(&engine.retryCalled),
			"a batch containing an exhausted host must not execute: partial execution would be a silent overrun")
	})

	t.Run("another host keeps its own budget", func(t *testing.T) {
		engine := &recordingEngine{}
		svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "host-budget-other"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), runstatus.StatusFailed)
		seedRetryRows(t, store, created.GetId(), "h1", retrybudget.MaxAttempts)

		_, err = svc.RetryHost(ctx, &pb.RetryHostRequest{ChangeId: created.GetId(), Hosts: []string{"h2"}})
		require.NoError(t, err, "the cap is per host; h2 must not inherit h1's exhausted budget")
		assert.Equal(t, int32(1), atomic.LoadInt32(&engine.retryCalled))
	})

	t.Run("an empty host list is refused rather than treated as all hosts", func(t *testing.T) {
		engine := &recordingEngine{}
		svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
		created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "host-budget-empty"})
		require.NoError(t, err)
		setRunStatus(t, store, created.GetId(), runstatus.StatusFailed)

		_, err = svc.RetryHost(ctx, &pb.RetryHostRequest{ChangeId: created.GetId()})
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Zero(t, atomic.LoadInt32(&engine.retryCalled))
	})
}
