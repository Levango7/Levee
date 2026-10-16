package chatopsbridge

import (
	"context"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/engine"
	"github.com/nexus/levee/internal/log"
)

// OnGateVerdict announces a failed verification gate to chat. It is the
// sender that `chatops.EventGateResult` never had: the event type was defined
// (bot.go) and the card branch renders it (card.go), but nothing in the
// production path ever produced one, so the card existed on paper only.
//
// Scope is deliberate — FAILURES ONLY. ChatOps has no per-change routing: the
// three adapters all discard the `channel` argument and post to the bot's
// configured webhook, so "who to send to" can only ever be "every configured
// bot" (there is no room/channel table in state to key a target off). With
// that fan-out, announcing every verdict would put N gates × batches × stages
// of routine green noise into the same room that carries approval asks — and
// this repo's closest precedent (rollback escalation, rollback_grade.go) also
// announces only the unhealthy case. A passing gate is still fully recorded:
// it lands in the run's audit trail as a gate_check row (PR #154), which is
// the surface that answers "which gates ran and what did each decide".
//
// Announcing is observability, never a safety property, so the posture mirrors
// the audit sink: a nil bridge or a nil BotManager is a no-op, and a failed
// broadcast — including the drop-on-full that BotManager's channel does by
// design — is logged and swallowed. Nothing here can change a verdict.
func (b *ApprovalBridge) OnGateVerdict(_ context.Context, v engine.GateVerdict) {
	if b == nil || b.bots == nil || !shouldAnnounceGate(v) {
		return
	}

	evt := chatops.Event{
		Type:      chatops.EventGateResult,
		ChangeID:  v.ChangeID,
		RunID:     v.ExecRunID,
		Title:     v.ChangeID,
		Summary:   gateSummary(v),
		Status:    gateStatus(v),
		GateName:  v.GateName,
		GatePass:  v.Passed,
		Timestamp: time.Now().UTC(),
	}
	if err := b.bots.BroadcastEvent(evt); err != nil {
		log.Warn("chatops: broadcast gate_result failed (gate verdict unaffected)",
			"change_id", v.ChangeID, "gate", v.GateName, "phase", v.Phase, "error", err)
	}
}

// shouldAnnounceGate is the whole failure-only policy, kept as one predicate so
// a later widening (a config switch, or announcing the rollback stage only) has
// exactly one place to change and one place to be tested.
func shouldAnnounceGate(v engine.GateVerdict) bool { return !v.Passed }

// gateStatus renders the 状态 field from the verdict rather than hard-coding
// "门禁未通过": the broadcast is failure-only today, but a status line that
// asserts failure independently of v.Passed would start lying the day the
// filter widens.
func gateStatus(v engine.GateVerdict) string {
	if v.Passed {
		return "门禁通过"
	}
	return "门禁未通过"
}

// gateSummary names where in the run the gate ran, because the same phase
// token means two different things depending on the path that reached it:
// post_apply is the forward health check normally, and "did the undo land
// healthy" when stage=rollback (the distinction #154 introduced and the audit
// row carries in its detail). Batch scoping is shown only when the producer
// had one.
func gateSummary(v engine.GateVerdict) string {
	where := v.Phase
	if v.Stage != "" && v.Stage != "forward" {
		where += "/" + v.Stage
	}
	if v.BatchID != "" {
		where += " · " + v.BatchID
	}
	summary := fmt.Sprintf("门禁 %q 未通过（%s）", v.GateName, where)
	if d := v.Latency.Milliseconds(); d > 0 {
		summary += fmt.Sprintf("，耗时 %dms", d)
	}
	if v.Message != "" {
		summary += "：" + v.Message
	}
	return summary
}
