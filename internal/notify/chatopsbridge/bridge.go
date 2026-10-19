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
	"github.com/nexus/levee/internal/runstatus"
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

// OnApprovalExpired broadcasts the approval_expired card when the serve-side
// expiry sweeper settles a kickoff approval whose timeout passed without
// quorum. Callers invoke it right after the durable settlement, with the
// settled record.
func (b *ApprovalBridge) OnApprovalExpired(a *approval.Approval) {
	if b == nil || b.bots == nil || a == nil {
		return
	}

	summary := "No quorum before the configured timeout; the approval is closed."
	if !a.ExpiresAt.IsZero() {
		summary = fmt.Sprintf("No quorum before %s; the approval is closed. Re-initiate approval to proceed.",
			a.ExpiresAt.Format("2006-01-02 15:04 MST"))
	}
	evt := chatops.Event{
		Type:      chatops.EventApprovalExpired,
		RunID:     a.RunID,
		ChangeID:  a.ID,
		Title:     fmt.Sprintf("Approval expired: %s run %s", a.Level, a.RunID),
		Summary:   summary,
		Level:     a.Level,
		Timestamp: time.Now().UTC(),
	}
	if err := b.bots.BroadcastEvent(evt); err != nil {
		log.Warn("chatops: broadcast approval_expired failed",
			"run_id", a.RunID, "error", err)
	}
}

// OnRunTransition broadcasts run lifecycle events. The funnel is the run
// event store decorator (wiring.RunEventStore): every successful status
// transition of every run lands here, whatever flow wrote it (apply, retry,
// takeover interrupt, pause/resume).
//
// The chat split is a deliberate product decision; the transitions the room
// stays silent on are not dropped — the outbound webhook mirror
// (notify.RunLifecycleMirror) is installed beside this projection at the same
// funnel and receives the full stream:
//
//   - "running" start and every TERMINAL status go to the room: they are
//     the moments a human must know about without watching the console.
//   - Intermediate hops (paused, pending_approval, ...) stay off chat —
//     a card per hop is noise, and nothing actionable happens between
//     start and end that the approval/gate cards do not already cover.
func (b *ApprovalBridge) OnRunTransition(changeID, from, to string, at time.Time) {
	if b == nil || b.bots == nil || changeID == "" || from == to {
		return
	}

	var typ chatops.EventType
	var title string
	switch {
	case to == "running":
		typ = chatops.EventRunStarted
		title = fmt.Sprintf("Run started: %s", changeID)
	case to == "completed":
		typ = chatops.EventRunCompleted
		title = fmt.Sprintf("Run completed: %s", changeID)
	case runstatus.IsTerminal(to):
		// Every other terminal status is a non-completion outcome: failure,
		// rolled back (wholly or partially), interrupted by takeover. The
		// room gets the real status in the card, not a flattened "failed".
		typ = chatops.EventRunFailed
		title = fmt.Sprintf("Run %s: %s", to, changeID)
	default:
		return
	}

	evt := chatops.Event{
		Type:      typ,
		ChangeID:  changeID,
		RunID:     changeID,
		Title:     title,
		Summary:   fmt.Sprintf("%s → %s", from, to),
		Status:    to,
		Timestamp: at,
	}
	if err := b.bots.BroadcastEvent(evt); err != nil {
		log.Warn("chatops: broadcast run transition failed",
			"change_id", changeID, "to", to, "error", err)
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
