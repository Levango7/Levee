// Package approval implements the approval service for LEVEE's change
// pipeline. It provides a state machine over approval records with four
// states — pending, approved, rejected, expired — and enforces the
// legal transitions defined by the design document (section 4.4.3).
//
// The only legal transitions are:
//
//	pending -> approved
//	pending -> rejected
//	pending -> expired
//
// All transitions out of a terminal state (approved, rejected, expired)
// are rejected.
//
// The service supports multi-approver workflows: when MinApprovers is
// greater than one, the approval only transitions to approved once
// enough distinct approvers have recorded an "approve" decision. A
// single "reject" decision immediately transitions the record to
// rejected (one-vote-veto semantics), matching the design red line R4.
//
// The CheckExpiry method scans all pending approvals and marks those
// whose ExpiresAt is in the past as expired, returning their IDs so
// the caller can notify or escalate as needed (section 4.4.3.2).
package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Status -----------------------------------------------------------------

// Status is the lifecycle state of an approval record.
type Status string

const (
	// StatusPending indicates the approval is waiting for decisions.
	StatusPending Status = "pending"

	// StatusApproved indicates the approval has been approved (enough
	// approvers have recorded an "approve" decision).
	StatusApproved Status = "approved"

	// StatusRejected indicates the approval has been rejected.
	StatusRejected Status = "rejected"

	// StatusExpired indicates the approval timed out before a decision
	// was reached.
	StatusExpired Status = "expired"
)

// canTransition reports whether a transition from one status to another
// is legal. The only legal source state is pending; the only legal
// target states are approved, rejected and expired.
func canTransition(from, to Status) bool {
	if from != StatusPending {
		return false
	}
	switch to {
	case StatusApproved, StatusRejected, StatusExpired:
		return true
	default:
		return false
	}
}

// --- Decision ---------------------------------------------------------------

// ActionApprove and ActionReject are the two possible decision actions
// recorded by an approver.
const (
	ActionApprove = "approve"
	ActionReject  = "reject"
)

// Decision is a single approver's decision on an approval record. A
// reject decision should carry a non-empty Reason; an approve decision
// may carry an optional comment in Reason.
type Decision struct {
	Approver string    `json:"approver"`
	Action   string    `json:"action"` // "approve" or "reject"
	Reason   string    `json:"reason"` // required for reject, optional for approve
	At       time.Time `json:"at"`
}

// --- Approval ---------------------------------------------------------------

// Approval is a single approval record within a multi-level approval
// chain. An approval collects decisions from one or more approvers
// until the minimum number of approvals is reached or a rejection is
// recorded.
type Approval struct {
	ID           string     `json:"id"`
	RunID        string     `json:"run_id"`
	Level        string     `json:"level"` // standard / high / emergency
	Status       Status     `json:"status"`
	Approvers    []string   `json:"approvers"`
	MinApprovers int        `json:"min_approvers"`
	Decisions    []Decision `json:"decisions"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	// PlanHash is the plan artifact this approval attests to. Empty means
	// "legacy record" (created before D-1 v2): it still settles regardless
	// of plan, preserving behaviour for pre-migration rows.
	PlanHash string `json:"plan_hash"`
	// Revision is the optimistic-lock version used by the concurrent decision
	// CAS. It is carried by the store round-trip so the CAS compares the exact
	// record the caller read.
	Revision int64 `json:"revision"`
}

// --- CreateRequest ----------------------------------------------------------

// CreateRequest is the input to Service.Create. The service generates
// the ID and CreatedAt; the caller supplies everything else.
type CreateRequest struct {
	RunID        string
	Level        string // standard / high / emergency
	Approvers    []string
	MinApprovers int
	ExpiresAt    time.Time
	// PlanHash is the plan artifact the new approval attests to. Leave empty
	// to create a legacy (plan-agnostic) approval record.
	PlanHash string
}

// --- Store ------------------------------------------------------------------

// Store is the persistence abstraction for approval records.
// Implementations must be safe for concurrent use.
//
// Conventions:
//   - Create inserts a new approval; the ID must be set by the caller.
//   - Get returns (nil, nil) when the approval does not exist.
//   - Update overwrites all mutable columns; the ID is the key.
//   - UpdateIfPending is a compare-and-set variant used by the decision
//     path so that two concurrent decisions can never both overwrite the
//     record after it has left StatusPending.
//   - ListPending returns all approvals currently in StatusPending.
type Store interface {
	Create(ctx context.Context, a *Approval) error
	Get(ctx context.Context, id string) (*Approval, error)
	Update(ctx context.Context, a *Approval) error
	// UpdateIfPending applies the update only when the stored record is
	// still in StatusPending. It returns true when the update was applied
	// and false when the record was concurrently decided (or vanished),
	// letting the caller retry from a fresh read instead of overwriting a
	// terminal decision.
	UpdateIfPending(ctx context.Context, a *Approval) (bool, error)
	ListPending(ctx context.Context) ([]*Approval, error)
}

// --- Sentinel errors --------------------------------------------------------

// Sentinel errors returned by the service. Callers may use errors.Is to
// match on them regardless of the wrapped message.
var (
	ErrNotFound             = errors.New("approval: not found")
	ErrInvalidTransition    = errors.New("approval: invalid status transition")
	ErrDuplicateDecision    = errors.New("approval: approver already decided")
	ErrUnauthorizedApprover = errors.New("approval: approver not in approvers list")
	ErrInvalidLevel         = errors.New("approval: invalid level")
	ErrEmptyRunID           = errors.New("approval: empty run id")
	ErrMinApproversTooLarge = errors.New("approval: min_approvers exceeds approvers")
	// ErrConflict is returned when the compare-and-set update could not be
	// applied because another actor kept winning the pending->decided
	// transition for casMaxAttempts consecutive attempts. It signals a
	// heavily contended approval, not a decision failure; retrying later is
	// safe.
	ErrConflict = errors.New("approval: concurrently modified, decision not recorded")
)

// casMaxAttempts bounds the compare-and-set retry loop in decide. The
// revision guard means concurrent partial votes now actively conflict (that is
// the point: no silent overwrite), so the budget covers one initial attempt
// plus retries for several approvers deciding at once. Exhausting it surfaces
// ErrConflict (nothing is lost — the caller may retry).
const casMaxAttempts = 4

// casRetryBackoff spaces out retries with a small, attempt-proportional delay
// so a burst of simultaneous decisions does not spin on the same revision.
func casRetryBackoff(attempt int) {
	if attempt <= 0 {
		return
	}
	time.Sleep(time.Duration(attempt) * 2 * time.Millisecond)
}

// validLevel reports whether the given approval level is one of the
// three legal tiers defined by the LEVEELang spec (standard / high /
// emergency).
func validLevel(level string) bool {
	switch level {
	case "standard", "high", "emergency":
		return true
	default:
		return false
	}
}

// --- Service ----------------------------------------------------------------

// Service is the approval service. It drives the approval state machine
// over a Store. A Service is safe for concurrent use provided the
// underlying Store is.
type Service struct {
	store Store

	// onDecision, when non-nil, is invoked after every successfully
	// recorded decision (approve or reject, including partial decisions
	// that keep the record pending). It receives the freshly updated
	// approval plus the action verb. The hook exists so callers (serve
	// wiring, notification / ChatOps fan-out) can observe decisions without
	// the approval package importing them — keeping the dependency graph
	// acyclic. Implementations must be safe for concurrent use and should
	// return quickly; errors are the observer's to handle (they are
	// ignored by the service so a failing side channel can never fail the
	// decision itself).
	onDecision func(a *Approval, action string)
}

// DecisionObserver is the signature of the optional decision hook
// installable via WithDecisionObserver.
type DecisionObserver func(a *Approval, action string)

// WithDecisionObserver installs a post-decision observer on the service.
// Pass nil to remove a previously installed observer. The observer fires
// only after the decision has been durably recorded (UpdateIfPending
// succeeded), never on speculative reads or failed attempts.
func (s *Service) WithDecisionObserver(fn DecisionObserver) *Service {
	s.onDecision = fn
	return s
}

// NewService returns a ready-to-use approval Service backed by the
// given Store. The Store must be non-nil.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// Create creates a new approval record in StatusPending. The ID is
// generated by the service; CreatedAt is set to the current UTC time.
//
// Returns an error when:
//   - req.RunID is empty;
//   - req.Level is not one of standard / high / emergency;
//   - req.MinApprovers (after defaulting to 1) exceeds the number of
//     approvers when the approvers list is non-empty.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Approval, error) {
	if req.RunID == "" {
		return nil, ErrEmptyRunID
	}
	if !validLevel(req.Level) {
		return nil, fmt.Errorf("%w: %q (allowed: standard, high, emergency)", ErrInvalidLevel, req.Level)
	}
	if req.MinApprovers <= 0 {
		req.MinApprovers = 1
	}
	if len(req.Approvers) > 0 && req.MinApprovers > len(req.Approvers) {
		return nil, fmt.Errorf("%w: %d > %d", ErrMinApproversTooLarge, req.MinApprovers, len(req.Approvers))
	}

	now := time.Now().UTC()
	id, err := newID()
	if err != nil {
		return nil, err
	}
	a := &Approval{
		ID:           id,
		RunID:        req.RunID,
		Level:        req.Level,
		Status:       StatusPending,
		Approvers:    req.Approvers,
		MinApprovers: req.MinApprovers,
		Decisions:    nil,
		CreatedAt:    now,
		ExpiresAt:    req.ExpiresAt,
		PlanHash:     req.PlanHash,
	}
	if err := s.store.Create(ctx, a); err != nil {
		return nil, fmt.Errorf("approval: create: %w", err)
	}
	log.InfoCtx(ctx, "approval created",
		"id", a.ID, "run_id", a.RunID, "level", a.Level, "min_approvers", a.MinApprovers)
	return a, nil
}

// Approve records an "approve" decision from the given approver. When
// the number of distinct "approve" decisions reaches MinApprovers, the
// approval transitions to StatusApproved. Otherwise it stays in
// StatusPending so that further approvers may still decide.
//
// The decision is applied with a compare-and-set (UpdateIfPending): if a
// concurrent decision wins the pending->decided transition first, the
// record is re-read once and the decision re-attempted; if the CAS fails
// again, ErrConflict is returned rather than overwriting the other actor's
// decision.
//
// Returns an error when:
//   - the approval does not exist;
//   - the approval is no longer pending (illegal transition);
//   - the approver is not in the Approvers list (when the list is
//     non-empty);
//   - the approver has already recorded a decision;
//   - the record kept changing concurrently (ErrConflict).
func (s *Service) Approve(ctx context.Context, id string, approver string) error {
	return s.decide(ctx, id, approver, ActionApprove, "", StatusApproved)
}

// Reject records a "reject" decision from the given approver and
// immediately transitions the approval to StatusRejected (one-vote-veto
// semantics). The reason is stored with the decision for audit.
//
// Uses the same compare-and-set flow as Approve.
//
// Returns an error under the same conditions as Approve (with rejected
// as the target state).
func (s *Service) Reject(ctx context.Context, id string, approver string, reason string) error {
	return s.decide(ctx, id, approver, ActionReject, reason, StatusRejected)
}

// decide is the shared compare-and-set implementation behind Approve and
// Reject. Each attempt reloads the record, validates the decision against
// the freshly-read state and applies it via Store.UpdateIfPending so two
// racing decisions can never both overwrite a terminal state. A false
// return from UpdateIfPending triggers at most one retry; persistent
// contention surfaces as ErrConflict instead of a silent lost update.
func (s *Service) decide(ctx context.Context, id string, approver string, action string, reason string, target Status) error {
	var lastErr error
	for attempt := 0; attempt < casMaxAttempts; attempt++ {
		a, err := s.store.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("approval: get: %w", err)
		}
		if a == nil {
			return ErrNotFound
		}
		if !canTransition(a.Status, target) {
			return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, a.Status, target)
		}
		// Defensive: a legacy or malformed record with MinApprovers<=0 must
		// not auto-approve on the first vote (countApproves(1) >= 0).
		// Well-formed records are clamped to 1 at Create; this clamps rows
		// written before the clamp existed.
		if a.MinApprovers <= 0 {
			a.MinApprovers = 1
		}
		if !isAuthorized(a.Approvers, approver) {
			return fmt.Errorf("%w: %s", ErrUnauthorizedApprover, approver)
		}
		if hasDecided(a.Decisions, approver) {
			return fmt.Errorf("%w: %s", ErrDuplicateDecision, approver)
		}

		a.Decisions = append(a.Decisions, Decision{
			Approver: approver,
			Action:   action,
			Reason:   reason,
			At:       time.Now().UTC(),
		})
		switch target {
		case StatusRejected:
			a.Status = StatusRejected
		default: // StatusApproved
			if countApproves(a.Decisions) >= a.MinApprovers {
				a.Status = StatusApproved
			}
		}

		ok, err := s.store.UpdateIfPending(ctx, a)
		if err != nil {
			return fmt.Errorf("approval: update if pending: %w", err)
		}
		if ok {
			log.InfoCtx(ctx, "approval decision recorded",
				"id", id, "approver", approver, "action", action, "status", a.Status,
				"attempt", attempt+1)
			if s.onDecision != nil {
				s.onDecision(a, action)
			}
			return nil
		}
		// The record was decided or modified concurrently between our read
		// and write; back off briefly, loop back, re-read and retry.
		lastErr = fmt.Errorf("%w: approval %s for %s", ErrConflict, id, approver)
		log.WarnCtx(ctx, "approval compare-and-set lost, retrying",
			"id", id, "approver", approver, "attempt", attempt+1)
		casRetryBackoff(attempt + 1)
	}
	return lastErr
}

// Get returns the approval record with the given ID. Returns (nil, nil)
// when the approval does not exist, matching the Store convention.
func (s *Service) Get(ctx context.Context, id string) (*Approval, error) {
	a, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("approval: get: %w", err)
	}
	return a, nil
}

// CheckExpiry scans all pending approvals and marks those whose
// ExpiresAt is in the past as expired. It returns the IDs of the
// approvals that were expired by this call. Approvals with a zero
// ExpiresAt are skipped (no timeout configured).
func (s *Service) CheckExpiry(ctx context.Context) ([]string, error) {
	pending, err := s.store.ListPending(ctx)
	if err != nil {
		return nil, fmt.Errorf("approval: list pending: %w", err)
	}
	now := time.Now().UTC()
	var expired []string
	for _, a := range pending {
		if a.ExpiresAt.IsZero() || !now.After(a.ExpiresAt) {
			continue
		}
		if !canTransition(a.Status, StatusExpired) {
			continue
		}
		a.Status = StatusExpired
		if err := s.store.Update(ctx, a); err != nil {
			return nil, fmt.Errorf("approval: update expiry: %w", err)
		}
		expired = append(expired, a.ID)
		log.InfoCtx(ctx, "approval expired", "id", a.ID, "expires_at", a.ExpiresAt)
	}
	return expired, nil
}

// --- helpers ----------------------------------------------------------------

// isAuthorized reports whether the approver is allowed to decide on the
// approval. When the approvers list is empty, any approver is allowed
// (the caller is responsible for permission checks). When the list is
// non-empty, the approver must appear in it.
func isAuthorized(approvers []string, approver string) bool {
	if len(approvers) == 0 {
		return true
	}
	for _, a := range approvers {
		if a == approver {
			return true
		}
	}
	return false
}

// hasDecided reports whether the approver has already recorded a
// decision (approve or reject) on this approval.
func hasDecided(decisions []Decision, approver string) bool {
	for _, d := range decisions {
		if d.Approver == approver {
			return true
		}
	}
	return false
}

// countApproves returns the number of "approve" decisions recorded so
// far. Each approver can contribute at most one (enforced by
// hasDecided before appending).
func countApproves(decisions []Decision) int {
	n := 0
	for _, d := range decisions {
		if d.Action == ActionApprove {
			n++
		}
	}
	return n
}

// newID generates a unique approval identifier of the form
// "approval-<16-hex-chars>". The approval ID is what decisions reference,
// so it is uniqueness-critical: a crypto/rand failure is an error, never a
// timestamp fallback (SA-012).
func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("approval: generate id: %w", err)
	}
	return "approval-" + hex.EncodeToString(b), nil
}
