// trace_chain_seal_test.go pins the trace-chain wiring: every funnel that
// settles a run into a terminal status (ApplyChange success, ApplyChange
// engine error, CancelChange) must seal the per-run trace hash chain after
// its last trace write, so /audit/verify's trace half has a real, closed
// chain to verify. Until this wiring existed HashChainBuilder.Build had no
// production callers — removing any seal site below turns the matching test
// red (Verify reports the run's rows as empty_hash), which is the mutation
// net for the wiring.
package grpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifySealedTraceChain asserts the run's trace chain exists, is non-empty
// and verifies. Shared by every seal-site test in this file.
func verifySealedTraceChain(t *testing.T, store state.Store, runID string) {
	t.Helper()
	b, err := audit.NewHashChainBuilder(store)
	require.NoError(t, err)
	count, err := b.Verify(context.Background(), runID)
	require.NoError(t, err, "a sealed chain must verify without error")
	assert.GreaterOrEqual(t, count, 1, "the run must carry at least one chained trace record")
}

func TestApplyChange_SealsTraceChainOnSuccess(t *testing.T) {
	engine := &racingEngine{runID: "exec-seal-succ", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "chain-seal-succ"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())

	_, err = svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId:    created.GetId(),
		AutoApprove: true,
	})
	require.NoError(t, err)

	verifySealedTraceChain(t, store, created.GetId())
}

func TestApplyChange_SealsTraceChainOnEngineError(t *testing.T) {
	// The engine returns a fatal error: the run settles to "failed", which
	// is equally terminal — the failure branch's seal must fire too.
	engine := &racingEngine{runID: "exec-seal-err", runErr: errors.New("disk on fire")}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "chain-seal-err"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())

	_, err = svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId:    created.GetId(),
		AutoApprove: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk on fire")

	verifySealedTraceChain(t, store, created.GetId())
}

func TestCancelChange_SealsTraceChain(t *testing.T) {
	// A run whose apply already started carries an apply_started trace;
	// cancelling it settles to a terminal state, so the shared
	// direct-transition path must seal the chain — otherwise every
	// cancelled run shows up as empty_hash at verification.
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "chain-seal-cancel",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID: "trc-seal-cancel-1", RunID: created.GetId(), Event: "apply_started",
		Actor: "executor", Timestamp: time.Now().UTC(),
	}))
	setRunStatus(t, store, created.GetId(), "running")

	resp, err := svc.CancelChange(ctx, &pb.CancelRequest{
		ChangeId: created.GetId(),
		Reason:   "no longer needed",
	})
	require.NoError(t, err)
	assert.Equal(t, "cancelled", resp.GetStatus())

	verifySealedTraceChain(t, store, created.GetId())
}

func TestCancelChange_BeforeAnyTraceLeavesNoChain(t *testing.T) {
	// A run cancelled before apply has no traces: the seal is a no-op
	// (ErrNoTraces maps to nil), and no chain must appear — Verify has
	// nothing to say about it.
	svc, store := newTestChangeService(t)
	ctx := context.Background()

	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{
		Label:        "chain-seal-preapply",
		TemplateName: "deploy",
	})
	require.NoError(t, err)

	resp, err := svc.CancelChange(ctx, &pb.CancelRequest{
		ChangeId: created.GetId(),
		Reason:   "never started",
	})
	require.NoError(t, err)
	assert.Equal(t, "cancelled", resp.GetStatus())

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: created.GetId()})
	require.NoError(t, err)
	assert.Empty(t, traces, "a pre-apply cancel must not write traces")
}
