package approval

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/push"
)

// MobileApprovalService extends the core ApprovalService with mobile-first
// capabilities: it sends push notifications when an approval is requested and
// supports one-tap approve / reject via deep links. It does not modify the
// existing service.go; all mobile-specific state lives in this file.
//
// The service maintains an in-memory approval history per user so that mobile
// clients can render a recent-decisions list without hitting the audit log.
// A production deployment would back this with the audit store; the in-memory
// map is sufficient for the CLI / single-binary form factor.
type MobileApprovalService struct {
	approvalSvc *Service
	pushMgr     *push.PushManager
	deeplink    *push.DeepLinkGenerator

	mu      sync.RWMutex
	history map[string][]ApprovalRecord // key: user ID
}

// ApprovalRecord is a single mobile-visible approval decision. It is a
// projection of the core Decision plus the run id and approval id so the
// mobile client can deep-link back to the change.
type ApprovalRecord struct {
	ApprovalID string    `json:"approval_id"`
	RunID      string    `json:"run_id"`
	Approver   string    `json:"approver"`
	Action     string    `json:"action"`
	Reason     string    `json:"reason,omitempty"`
	At         time.Time `json:"at"`
	Source     string    `json:"source,omitempty"` // "mobile" or "cli"
}

// NewMobileApprovalService wires a MobileApprovalService with the core
// approval service, a push manager and a deep-link generator. Any of the
// push / deeplink dependencies may be nil when mobile support is disabled;
// the methods degrade gracefully and return a descriptive error.
func NewMobileApprovalService(approvalSvc *Service, pushMgr *push.PushManager, deeplink *push.DeepLinkGenerator) *MobileApprovalService {
	return &MobileApprovalService{
		approvalSvc: approvalSvc,
		pushMgr:     pushMgr,
		deeplink:    deeplink,
		history:     make(map[string][]ApprovalRecord),
	}
}

// RequestApproval creates a pending approval for the given run and notifies
// the user's mobile devices. The push notification carries a deep link that
// allows one-tap approve / reject. The approval is created via the core
// service; the push delivery is best-effort and does not block the approval
// creation.
//
// The approval is created at the standard level with a single approver and a
// 24h expiry. Callers needing finer control should use the core Service.Create
// directly and then call NotifyApprovalRequest separately.
func (s *MobileApprovalService) RequestApproval(ctx context.Context, runID, userID string) error {
	if runID == "" {
		return ErrEmptyRunID
	}
	if userID == "" {
		return fmt.Errorf("approval: mobile: empty user id")
	}

	a, err := s.approvalSvc.Create(ctx, CreateRequest{
		RunID:     runID,
		Level:     LevelStandard,
		Approvers: []string{userID},
		ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	})
	if err != nil {
		return fmt.Errorf("approval: mobile: create: %w", err)
	}

	// Best-effort push notification. We generate the deep link first so the
	// notification payload can carry it. When the push manager or deeplink
	// generator is nil we skip silently.
	if s.pushMgr == nil || s.deeplink == nil {
		log.InfoCtx(ctx, "approval: mobile: push disabled; skipping notification",
			"run_id", runID, "user_id", userID)
		return nil
	}

	link, err := s.deeplink.GenerateApprovalLink(runID, userID)
	if err != nil {
		log.WarnCtx(ctx, "approval: mobile: generate deeplink failed",
			"run_id", runID, "err", err)
		return nil // approval already created; push is best-effort
	}

	msg := push.PushMessage{
		UserID:   userID,
		Title:    "审批请求",
		Body:     fmt.Sprintf("变更 %s 待审批", runID),
		Category: "APPROVE_CATEGORY",
		Data: map[string]string{
			"run_id":      runID,
			"approval_id": a.ID,
			"action":      push.ActionApprove,
			"deeplink":    link.URL,
			"token":       link.Token,
		},
	}
	if err := s.pushMgr.Send(ctx, msg); err != nil {
		log.WarnCtx(ctx, "approval: mobile: push send failed",
			"run_id", runID, "user_id", userID, "err", err)
		// Do not return the push error: the approval was created successfully.
	}
	return nil
}

// ApproveViaDeepLink consumes a one-tap approval token and records an approve
// decision on the bound approval. The token is single-use; a second call with
// the same token returns ErrInvalidToken from the push package.
//
// It returns the runID the token was bound to, so the caller (the REST
// gateway) can settle the run from the approval chain state — mirroring
// a completed quorum onto the run is ChangeService territory and must
// not live in this package (approval is a layer below grpc).
func (s *MobileApprovalService) ApproveViaDeepLink(ctx context.Context, token string) (string, error) {
	runID, userID, action, err := s.deeplink.ValidateToken(token)
	if err != nil {
		return "", fmt.Errorf("approval: mobile: validate token: %w", err)
	}
	if action != push.ActionApprove {
		return "", fmt.Errorf("approval: mobile: token action %q is not approve", action)
	}

	approvalID, err := s.findPendingApprovalID(ctx, runID)
	if err != nil {
		return runID, err
	}
	if err := s.approvalSvc.Approve(ctx, approvalID, userID); err != nil {
		return runID, fmt.Errorf("approval: mobile: approve: %w", err)
	}
	s.recordHistory(userID, ApprovalRecord{
		ApprovalID: approvalID,
		RunID:      runID,
		Approver:   userID,
		Action:     ActionApprove,
		At:         time.Now().UTC(),
		Source:     "mobile",
	})
	log.InfoCtx(ctx, "approval: mobile: approved via deep link",
		"run_id", runID, "user_id", userID)
	return runID, nil
}

// RejectViaDeepLink consumes a one-tap reject token and records a reject
// decision on the bound approval. Returns the runID (see
// ApproveViaDeepLink for the settlement contract).
func (s *MobileApprovalService) RejectViaDeepLink(ctx context.Context, token string) (string, error) {
	runID, userID, action, err := s.deeplink.ValidateToken(token)
	if err != nil {
		return "", fmt.Errorf("approval: mobile: validate token: %w", err)
	}
	if action != push.ActionReject {
		return "", fmt.Errorf("approval: mobile: token action %q is not reject", action)
	}

	approvalID, err := s.findPendingApprovalID(ctx, runID)
	if err != nil {
		return runID, err
	}
	reason := "rejected via mobile deep link"
	if err := s.approvalSvc.Reject(ctx, approvalID, userID, reason); err != nil {
		return runID, fmt.Errorf("approval: mobile: reject: %w", err)
	}
	s.recordHistory(userID, ApprovalRecord{
		ApprovalID: approvalID,
		RunID:      runID,
		Approver:   userID,
		Action:     ActionReject,
		Reason:     reason,
		At:         time.Now().UTC(),
		Source:     "mobile",
	})
	log.InfoCtx(ctx, "approval: mobile: rejected via deep link",
		"run_id", runID, "user_id", userID)
	return runID, nil
}

// DeeplinkState is the evidence a one-tap link leaves behind once it can no
// longer be applied. The token is single-use, so after a failed tap the
// operator's real question is not "was the token well-formed" but "what has
// already happened to the change it pointed at": a second tap must be
// answerable with "already approved by X" rather than "invalid token",
// because the second reading sends people back to click again.
type DeeplinkState struct {
	RunID  string `json:"run_id"`
	Action string `json:"action"` // what the link offered: approve | reject
	// Decided is true only for a terminal record (approved / rejected). A
	// still-pending or unknown record is reported with Decided=false, and
	// the caller keeps the flat "invalid or expired token" answer.
	Decided bool      `json:"decided"`
	Status  string    `json:"status"` // approved | rejected | pending | expired | unknown
	By      string    `json:"by,omitempty"`
	At      time.Time `json:"at,omitzero"`
	// Votes and MinApprovers describe a record that is still pending: how
	// many approvals it has collected against how many it needs. They say
	// nothing about a decided record, so they stay zero there.
	Votes        int `json:"votes,omitempty"`
	MinApprovers int `json:"min_approvers,omitempty"`
}

// DeeplinkState reports the current state of the approval a deep-link token
// points at. It never consumes anything: it is the read that answers "did my
// tap register?" after the tap itself failed.
//
// The token's binding is all it may disclose, and holding that binding
// requires a token this generator actually issued — spent or still live. A
// token the server never saw returns (zero, false, nil): a forged string
// must not become a way to probe which runs exist and what became of them.
func (s *MobileApprovalService) DeeplinkState(ctx context.Context, token string) (DeeplinkState, bool, error) {
	if s.deeplink == nil {
		return DeeplinkState{}, false, fmt.Errorf("approval: mobile: deeplink state: %w", ErrMobilePushDisabled)
	}
	b, ok := s.deeplink.LookupBinding(token)
	if !ok || b.RunID == "" {
		return DeeplinkState{}, false, nil
	}
	if b.Action != push.ActionApprove && b.Action != push.ActionReject {
		// show / list links are non-mutating; they carry no decision to
		// report evidence about.
		return DeeplinkState{}, false, nil
	}
	a, err := s.approvalSvc.ApprovalForRun(ctx, b.RunID)
	if err != nil {
		return DeeplinkState{}, false, err
	}
	st := DeeplinkState{RunID: b.RunID, Action: b.Action}
	if a == nil {
		st.Status = "unknown"
		return st, true, nil
	}
	st.Status = string(a.Status)
	switch a.Status {
	case StatusApproved, StatusRejected:
		st.Decided = true
		if last := lastDecision(a); last != nil {
			st.By, st.At = last.Approver, last.At
		}
	case StatusPending:
		votes := 0
		for _, d := range a.Decisions {
			if d.Action == ActionApprove {
				votes++
			}
		}
		st.Votes, st.MinApprovers = votes, a.MinApprovers
	}
	return st, true, nil
}

// lastDecision returns the most recent decision on the record by recorded
// time, or nil when none was cast. It names who closed (or is closing) the
// approval — the name the operator needs in "already approved by X".
func lastDecision(a *Approval) *Decision {
	var last *Decision
	for i := range a.Decisions {
		if last == nil || !a.Decisions[i].At.Before(last.At) {
			last = &a.Decisions[i]
		}
	}
	return last
}

// GetApprovalHistory returns the recent mobile approval decisions for the
// given user, most-recent first. The returned slice is a copy and may be
// modified freely by the caller.
func (s *MobileApprovalService) GetApprovalHistory(ctx context.Context, userID string) ([]ApprovalRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.history[userID]
	if len(src) == 0 {
		return nil, nil
	}
	out := make([]ApprovalRecord, len(src))
	copy(out, src)
	// Reverse so most-recent is first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// --- internal helpers -------------------------------------------------------

// findPendingApprovalID returns the id of the single pending approval for the
// given run. When zero or more than one pending approval exists, an error is
// returned. The method relies on the core service's ListPending.
func (s *MobileApprovalService) findPendingApprovalID(ctx context.Context, runID string) (string, error) {
	pending, err := s.approvalSvc.store.ListPending(ctx)
	if err != nil {
		return "", fmt.Errorf("approval: mobile: list pending: %w", err)
	}
	var match string
	for _, a := range pending {
		if a.RunID == runID {
			if match != "" {
				return "", fmt.Errorf("approval: mobile: multiple pending approvals for run %q", runID)
			}
			match = a.ID
		}
	}
	if match == "" {
		return "", fmt.Errorf("approval: mobile: no pending approval for run %q: %w",
			runID, ErrNotFound)
	}
	return match, nil
}

// recordHistory appends a decision to the user's history. The history is
// capped at maxHistoryEntries per user to avoid unbounded growth.
func (s *MobileApprovalService) recordHistory(userID string, rec ApprovalRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.history[userID]
	entries = append(entries, rec)
	if len(entries) > maxHistoryEntries {
		entries = entries[len(entries)-maxHistoryEntries:]
	}
	s.history[userID] = entries
}

// maxHistoryEntries caps the per-user in-memory history. 200 entries is
// roughly a month of active mobile approvals for a single user.
const maxHistoryEntries = 200

// ErrMobilePushDisabled is returned when a mobile operation requires push
// delivery but the push manager or deeplink generator was not configured.
var ErrMobilePushDisabled = errors.New("approval: mobile: push or deeplink not configured")
