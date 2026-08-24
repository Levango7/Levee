package verify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubApprover is a scripted HumanApprover: it records the request it
// received and answers with the configured decision / error after an
// optional delay driven by its own view of the context.
type stubApprover struct {
	decision bool
	err      error

	// waitForContext, when true, blocks until the passed context is done and
	// then returns that context's error (used for timeout / cancel tests).
	waitForContext bool

	gotCtx     context.Context
	gotRunID   string
	gotSubject string
	gotReason  string
	calls      int
}

func (a *stubApprover) RequestAndWait(ctx context.Context, runID, subject, reason string) (bool, error) {
	a.calls++
	a.gotCtx = ctx
	a.gotRunID = runID
	a.gotSubject = subject
	a.gotReason = reason

	if a.waitForContext {
		<-ctx.Done()
		return false, ctx.Err()
	}
	if a.err != nil {
		return false, a.err
	}
	return a.decision, nil
}

func TestHumanGateImplementsGateInterface(t *testing.T) {
	var _ Gate = (*HumanGate)(nil)

	g := NewHumanGate("change-window", "post_apply", &stubApprover{decision: true}, nil)
	assert.Equal(t, "change-window", g.Name())
	assert.Equal(t, PhasePostApply, g.Phase())
	require.NoError(t, g.ParamsError())
}

func TestHumanGateApproved(t *testing.T) {
	ap := &stubApprover{decision: true}
	g := NewHumanGate("approve-cutover", "post_batch", ap, map[string]any{
		"reason": "cutover window 02:00-04:00",
	})

	res, err := g.Check(context.Background(), GateInput{RunID: "run-42"})
	require.NoError(t, err)
	assert.True(t, res.Passed, "message: %s", res.Message)
	assert.Equal(t, 1, ap.calls)
	assert.Equal(t, "run-42", ap.gotRunID)
	assert.Equal(t, "approve-cutover", ap.gotSubject)
	assert.Equal(t, "cutover window 02:00-04:00", ap.gotReason)
	assert.Equal(t, "approved", res.Details["decision"])
}

func TestHumanGateRejected(t *testing.T) {
	ap := &stubApprover{decision: false}
	g := NewHumanGate("approve-cutover", "post_apply", ap, nil)

	res, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, res.Message, "rejected")
	assert.Equal(t, "rejected", res.Details["decision"])
}

func TestHumanGateTimeoutFailsWithoutTerminalError(t *testing.T) {
	ap := &stubApprover{waitForContext: true}
	g := NewHumanGate("slow-signoff", "pre_apply", ap, map[string]any{
		"timeout_seconds": 1,
	})

	start := time.Now()
	res, err := g.Check(context.Background(), GateInput{})
	took := time.Since(start)

	require.NoError(t, err, "timeout is a failed gate, not a terminal error")
	assert.False(t, res.Passed)
	assert.Contains(t, res.Message, "timed out")
	assert.Less(t, took, 5*time.Second, "the derived deadline must bound the wait")
	assert.GreaterOrEqual(t, took, time.Second, "the wait must honour timeout_seconds")
}

func TestHumanGateParentCancelPropagates(t *testing.T) {
	ap := &stubApprover{waitForContext: true}
	g := NewHumanGate("cancellable", "post_apply", ap, map[string]any{"timeout_seconds": 600})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	res, err := g.Check(ctx, GateInput{})
	require.NoError(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, res.Message, "cancelled")
}

func TestHumanGateAlreadyCancelledContextSkipsApprover(t *testing.T) {
	ap := &stubApprover{decision: true}
	g := NewHumanGate("late", "post_apply", ap, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := g.Check(ctx, GateInput{})
	require.NoError(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, res.Message, "cancelled before run")
	assert.Equal(t, 0, ap.calls, "must not contact the approver on a dead context")
}

func TestHumanGateApproverTransportError(t *testing.T) {
	ap := &stubApprover{err: errors.New("chat webhook down")}
	g := NewHumanGate("signoff", "post_apply", ap, nil)

	res, err := g.Check(context.Background(), GateInput{})
	require.Error(t, err, "transport failure stays visible as an error")
	assert.False(t, res.Passed)
	assert.Contains(t, res.Message, "approver error")
}

func TestHumanGateMissingApproverFailsClosed(t *testing.T) {
	g := NewHumanGate("unwired", "post_apply", nil, nil)

	res, err := g.Check(context.Background(), GateInput{})
	require.Error(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, res.Message, "no approver configured")
}

func TestHumanGateUnknownParamFailsClosed(t *testing.T) {
	g := NewHumanGate("bad-params", "post_apply", &stubApprover{decision: true}, map[string]any{
		"karma": 1,
	})

	res, err := g.Check(context.Background(), GateInput{})
	require.Error(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, err.Error(), `"karma"`)
	for _, k := range validHumanParamKeys {
		assert.Contains(t, err.Error(), k, "error must list every valid key")
	}
}

func TestHumanGateBadTimeoutParamFailsClosed(t *testing.T) {
	g := NewHumanGate("bad-timeout", "post_apply", &stubApprover{decision: true}, map[string]any{
		"timeout_seconds": -5,
	})

	res, err := g.Check(context.Background(), GateInput{})
	require.Error(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, err.Error(), `must be > 0`)
}

func TestHumanGateRegisteredWithManager(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewHumanGate("gatekeeper", "post_batch", &stubApprover{decision: true}, map[string]any{
		"reason": "batch gate",
	}))

	results := gm.RunPhase(context.Background(), PhasePostBatch, GateInput{BatchID: "b1"})
	require.Len(t, results, 1)
	assert.True(t, results[0].Passed)
}
