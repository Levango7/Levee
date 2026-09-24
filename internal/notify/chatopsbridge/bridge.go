// Package chatopsbridge wires approval decisions into the ChatOps layer.
//
// The approval package deliberately exposes only a DecisionObserver hook
// (no chatops dependency) so the dependency graph stays acyclic; this
// bridge is the composition point: it converts observer callbacks into
// chatops events and broadcasts them through the BotManager, and it
// renders the "please approve" card when a new approval is created.
//
// Wire-up lives in the serve wiring (cmd/levee); calling NewApprovalBridge
// gives serve a one-liner:
//
//	svc := approval.NewService(store)
//	bridge := chatopsbridge.NewApprovalBridge(botMgr)
//	svc.WithDecisionObserver(bridge.OnDecision)
//	// on approval create: bridge.OnApprovalCreated(a)
package chatopsbridge

import (
	"fmt"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/log"
)

// ApprovalBridge fans approval lifecycle moments out to ChatOps.
// A nil BotManager turns every method into a logged no-op so callers
// never need nil checks. The bridge is stateless and safe for
// concurrent use.
type ApprovalBridge struct {
	bots *chatops.BotManager
}

// NewApprovalBridge returns a bridge broadcasting to mgr. mgr may be nil
// (methods become logged no-ops).
func NewApprovalBridge(mgr *chatops.BotManager) *ApprovalBridge {
	return &ApprovalBridge{bots: mgr}
}

// OnApprovalCreated broadcasts the approval_requested card for a freshly
// created approval so approvers see the ask in their chat client. Callers
// invoke it right after approval.Service.Create succeeds.
func (b *ApprovalBridge) OnApprovalCreated(a *approval.Approval) {
	if b == nil || b.bots == nil || a == nil {
		return
	}

	evt := chatops.Event{
		Type:      chatops.EventApprovalRequested,
		RunID:     a.RunID,
		ChangeID:  a.ID,
		Title:     fmt.Sprintf("Approval required: %s run %s", a.Level, a.RunID),
		Summary:   fmt.Sprintf("Waiting for %d approver(s) at level %q. One reject vetoes.", a.MinApprovers, a.Level),
		Level:     a.Level,
		Timestamp: time.Now().UTC(),
	}
	if err := b.bots.BroadcastEvent(evt); err != nil {
		log.Warn("chatops: broadcast approval_requested failed",
			"run_id", a.RunID, "error", err)
	}
}

// OnDecision is the DecisionObserver installed via WithDecisionObserver.
// It broadcasts the approval_decision event after every durably recorded
// decision — including partial multi-approver decisions that keep the
// record pending (so the room sees "1/2 approved" progress).
func (b *ApprovalBridge) OnDecision(a *approval.Approval, action string) {
	if b == nil || b.bots == nil || a == nil {
		return
	}

	evt := chatops.Event{
		Type:      chatops.EventApprovalDecision,
		RunID:     a.RunID,
		ChangeID:  a.ID,
		Title:     fmt.Sprintf("Approval %s: run %s", a.Status, a.RunID),
		Summary:   decisionSummary(a, action),
		Level:     a.Level,
		Timestamp: time.Now().UTC(),
	}
	if err := b.bots.BroadcastEvent(evt); err != nil {
		log.Warn("chatops: broadcast approval_decision failed",
			"run_id", a.RunID, "error", err)
	}
}

// decisionSummary renders a one-line summary of the decision state,
// e.g. "approved by alice (2/2) — run may proceed" or
// "approved by bob (1/2) — waiting for another approver".
func decisionSummary(a *approval.Approval, action string) string {
	approved := 0
	var lastApprover string
	for _, d := range a.Decisions {
		if d.Action == approval.ActionApprove {
			approved++
			lastApprover = d.Approver
		}
	}

	switch a.Status {
	case approval.StatusApproved:
		return fmt.Sprintf("approved by %s (%d/%d) — run may proceed", lastApprover, approved, a.MinApprovers)
	case approval.StatusRejected:
		return fmt.Sprintf("rejected (%d/%d approvals collected) — one-vote veto applied", approved, a.MinApprovers)
	default: // still pending
		return fmt.Sprintf("%s recorded by %s (%d/%d) — waiting for more approvers",
			action, lastApprover, approved, a.MinApprovers)
	}
}
