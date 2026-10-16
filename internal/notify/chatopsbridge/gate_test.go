package chatopsbridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/engine"
)

// gateVerdict is the payload OnGateVerdict consumes: one verification-gate
// outcome as the engine produced it, with the change identity already bound by
// wiring.changeBoundGateObserver. These tests cover the bridge's own contract —
// what it announces, what it refuses, and what the card then says.

func failingGate() engine.GateVerdict {
	return engine.GateVerdict{
		ExecRunID: "run-9ebe7dc46fa9fecb",
		ChangeID:  "chg-prod-0417",
		GateName:  "post-apply-health",
		Phase:     "post_apply",
		Stage:     "forward",
		Passed:    false,
		Message:   "systemctl is-active sshd exited 3",
		Latency:   1250 * time.Millisecond,
	}
}

// TestOnGateVerdictAnnouncesOnlyFailures is the scope decision, pinned: a
// passing verdict must NOT reach chat. This is the whole reason the filter
// exists — chatops fans out to every configured webhook (no per-change
// routing), so announcing green gates would bury the asks the room is for.
func TestOnGateVerdictAnnouncesOnlyFailures(t *testing.T) {
	bridge, bot := newBridge()

	green := failingGate()
	green.Passed = true
	red := failingGate()
	red.GateName = "pre-apply-freeze"

	bridge.OnGateVerdict(context.Background(), green)
	bridge.OnGateVerdict(context.Background(), red)
	bridge.OnGateVerdict(context.Background(), green)

	events := drainEvents(t, bot.eventCh, 1)
	assert.Equal(t, chatops.EventGateResult, events[0].Type)
	assert.Equal(t, "pre-apply-freeze", events[0].GateName, "the one broadcast is the failure, by name")
	assert.False(t, events[0].GatePass)

	assert.Empty(t, bot.eventCh, "two green verdicts must produce zero events, not two quieter ones")
}

// TestOnGateVerdictCardActuallyRendersTheGate pins the producer against the
// consumer: chatops.EventGateResult goes through BuildStatusCard, and a card
// whose mandatory fields are blank would be a silent failure of exactly the
// kind this bridge is supposed to remove. Asserting on the RENDERED card is
// what makes "we filled the struct" mean "an operator reads something".
func TestOnGateVerdictCardActuallyRendersTheGate(t *testing.T) {
	bridge, bot := newBridge()

	bridge.OnGateVerdict(context.Background(), failingGate())
	evt := drainEvents(t, bot.eventCh, 1)[0]

	assert.Equal(t, "chg-prod-0417", evt.ChangeID, "the persistent identity, not the engine's ephemeral id")
	assert.Equal(t, "run-9ebe7dc46fa9fecb", evt.RunID, "the exec id stays visible for correlating with batches")
	assert.NotEmpty(t, evt.Title)
	assert.NotEmpty(t, evt.Status, "BuildStatusCard adds 状态 unconditionally — an empty value renders a blank row")
	assert.False(t, evt.Timestamp.IsZero())

	card := chatops.BuildCardForEvent(evt)
	require.NotNil(t, card, "EventGateResult must have a rendering branch")
	assert.Equal(t, "chg-prod-0417", card.ChangeID, "the card's own identity slot is the change, not the exec run")
	assert.Equal(t, "run-9ebe7dc46fa9fecb", card.RunID)
	text := cardText(card)
	for _, want := range []string{"chg-prod-0417", "post-apply-health", "失败", "post_apply", "systemctl is-active sshd exited 3"} {
		assert.Contains(t, text, want, "the card must name the change, the gate, its verdict, its phase and why")
	}
}

// TestOnGateVerdictSummaryDistinguishesRollbackStage: post_apply means the
// forward health check and the "did the undo land healthy" check, and #154
// carries that apart in stage. A summary that flattens the two would read as
// "the forward check failed" when the rollback verification failed instead.
func TestOnGateVerdictSummaryDistinguishesRollbackStage(t *testing.T) {
	v := failingGate()
	v.Stage = "rollback"
	summary := gateSummary(v)
	assert.Contains(t, summary, "post_apply/rollback")
	assert.NotContains(t, summary, "forward")

	// The forward stage is the phase's default reading, so it is not repeated
	// in the line — but batch scoping must be, since post_batch verdicts are
	// per-batch and a card without the batch number cannot say which one.
	b := failingGate()
	b.Phase = "post_batch"
	b.Stage = "forward"
	b.BatchID = "batch-1"
	assert.Contains(t, gateSummary(b), "post_batch")
	assert.Contains(t, gateSummary(b), "batch-1")
	assert.NotContains(t, gateSummary(b), "forward")
}

// TestGateStatusFollowsTheVerdict keeps the status line honest if the
// failure-only filter is ever widened: it must not assert 未通过 for a pass.
func TestGateStatusFollowsTheVerdict(t *testing.T) {
	pass := failingGate()
	pass.Passed = true
	assert.Equal(t, "门禁通过", gateStatus(pass))
	assert.Equal(t, "门禁未通过", gateStatus(failingGate()))
}

// TestOnGateVerdictNilGuards: the serve path installs this observer only when
// notify.chatops.enabled, but the method must also survive being called on a
// nil bridge or a manager with no bots — a notification path that can panic
// would put a verdict-reporting gap in the middle of a run.
func TestOnGateVerdictNilGuards(t *testing.T) {
	var nilBridge *ApprovalBridge
	assert.NotPanics(t, func() { nilBridge.OnGateVerdict(context.Background(), failingGate()) })
	assert.NotPanics(t, func() { NewApprovalBridge(nil).OnGateVerdict(context.Background(), failingGate()) })
	assert.NotPanics(t, func() { NewApprovalBridge(chatops.NewBotManager()).OnGateVerdict(context.Background(), failingGate()) })
}

// errBridgeBot fails every publish, which is how BroadcastEvent reports a
// dead/slow transport.
type errBridgeBot struct{ fakeBridgeBot }

func (b *errBridgeBot) PublishEvent(chatops.Event) error {
	return errors.New("chatops: webhook returned 502")
}

// TestOnGateVerdictSwallowsBroadcastFailure: announcing is observability. A
// failing webhook must not become a run failure and must not panic; the guard
// here is that the call returns at all after the error path ran.
func TestOnGateVerdictSwallowsBroadcastFailure(t *testing.T) {
	mgr := chatops.NewBotManager()
	bot := &errBridgeBot{fakeBridgeBot{
		name:    "broken-slack",
		eventCh: make(chan chatops.Event, 1),
	}}
	require.NoError(t, mgr.Register(bot))

	bridge := NewApprovalBridge(mgr)
	assert.NotPanics(t, func() {
		bridge.OnGateVerdict(context.Background(), failingGate())
	})
}

// cardText flattens a card's title/summary/fields into one string for
// assertions, so the checks read as "what the operator sees".
func cardText(c *chatops.Card) string {
	var b strings.Builder
	b.WriteString(string(c.Kind))
	b.WriteString("\n")
	for _, line := range []string{c.Title, c.Summary} {
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, f := range c.Fields {
		b.WriteString(f.Label)
		b.WriteString("=")
		b.WriteString(f.Value)
		b.WriteString("\n")
	}
	return b.String()
}
