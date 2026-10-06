package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexus/levee/internal/verify"
)

// GatePlanMarkerPrefix prefixes the synthetic PlanHash a gate approval carries.
// A gate approval must be invisible to the CHANGE approval machinery — the
// settlement path, the apply gate and the retry re-plan gate all select records
// with state.Approval.MatchesPlan(run.PlanHash) — so it is bound to a value that
// can never equal a run's plan hash (those are "v2:<hex>" or legacy 64-hex) and
// therefore never matches. Without this, approving a run would also settle its
// open human gates, and vice versa, conflating two different consents.
const GatePlanMarkerPrefix = "human-gate:"

// GateApprovalID is the deterministic identifier of the approval backing a
// (run, gate) pair. Deterministic so that a resumed gate re-attaches to the
// record it already opened instead of stacking a second one, and so an operator
// can act on it from the CLI given only the run id and gate name they can read
// from the log line.
func GateApprovalID(runID, gate string) string {
	return "gate-" + runID + "-" + gate
}

// GateApprover adapts the approval service to verify.HumanApprover: a workflow
// `human` gate opens a gate-scoped approval record and blocks until a human
// decides it.
//
// It exists so that installing a transport is a one-line wiring step
// (wiring.WithGateApprover) with no second switch to remember: the plan-time
// refusal and the phase-time materialisation both read the same gate runtime.
//
// What the decision carries: the approver identity recorded by whoever decided
// the record. When that surface can authenticate the human (a named token, SSO,
// OIDC) the id is a verifiable subject; the local CLI surface binds the OS actor
// instead, which is the same trust boundary `levee approve` already uses. An
// empty id is passed through as empty rather than filled with a placeholder, so
// an unattributable decision stays visible.
//
// Not yet expressed here: quorum / min_approvers for a gate (single decision
// settles it), and independence rules (exclude_initiator). Both belong to the
// same follow-up that gives gates their own consent UI; the gate MVP is
// deliberately single-approver, matching verify.HumanApprover.
type GateApprover struct {
	svc      *Service
	interval time.Duration
}

// DefaultGatePollInterval is how often the transport re-reads the approval
// record while waiting for a decision.
const DefaultGatePollInterval = 2 * time.Second

// NewGateApprover returns a transport backed by svc.
func NewGateApprover(svc *Service) *GateApprover {
	return &GateApprover{svc: svc, interval: DefaultGatePollInterval}
}

// RequestAndWait opens (or re-attaches to) the approval for this (run, gate) and
// blocks until it is decided or ctx ends. See verify.HumanApprover.
func (g *GateApprover) RequestAndWait(ctx context.Context, req verify.HumanRequest) (verify.HumanDecision, error) {
	if g == nil || g.svc == nil {
		return verify.HumanDecision{}, errors.New("gate approver: no approval service")
	}
	if strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.Gate) == "" {
		return verify.HumanDecision{}, errors.New("gate approver: run id and gate name are required")
	}

	id := GateApprovalID(req.RunID, req.Gate)
	// Rejections are carried on the record's status, not on an error, so the
	// gate can distinguish "a human said no" from "the transport failed".
	if err := g.open(ctx, id, req); err != nil {
		return verify.HumanDecision{}, err
	}

	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		a, err := g.svc.Get(ctx, id)
		if err != nil {
			return verify.HumanDecision{}, fmt.Errorf("gate approver: read %q: %w", id, err)
		}
		switch a.Status {
		case StatusApproved:
			return verify.HumanDecision{Approved: true, Approver: decider(a)}, nil
		case StatusRejected:
			return verify.HumanDecision{Approved: false, Approver: decider(a)}, nil
		}
		select {
		case <-ctx.Done():
			return verify.HumanDecision{}, ctx.Err()
		case <-t.C:
		}
	}
}

// open creates the gate's approval record carrying the workflow's consent shape.
// A record that already exists is fine: the gate is re-attaching (a resumed run,
// or two phases declaring the same name), and the existing record is the one the
// human will decide.
func (g *GateApprover) open(ctx context.Context, id string, req verify.HumanRequest) error {
	minApprovers := req.MinApprovers
	if minApprovers <= 0 {
		minApprovers = 1
	}
	_, err := g.svc.Create(ctx, CreateRequest{
		ID:               id,
		RunID:            req.RunID,
		Level:            LevelStandard,
		MinApprovers:     minApprovers,
		PlanHash:         GatePlanMarkerPrefix + req.Gate,
		Initiator:        req.Initiator,
		ExcludeInitiator: req.ExcludeInitiator,
	})
	if err == nil {
		return nil
	}
	// The only benign failure is "already exists": re-attach to it. Every other
	// error (bad level, store down) must surface rather than be swallowed.
	if existing, getErr := g.svc.Get(ctx, id); getErr == nil && existing != nil {
		return nil
	}
	return fmt.Errorf("gate approver: open %q: %w", id, err)
}

// decider returns the identity behind the decision that settled the record.
//
// A rejection is terminal, so the rejecter is named. An approval settled a
// quorum, so EVERY approver is named (comma-joined) rather than only the last
// vote: with min_approvers > 1 the gate's justification is "these people
// agreed", and reporting one of them would understate it. Empty when the
// deciding surface recorded no identity.
func decider(a *Approval) string {
	if a == nil {
		return ""
	}
	var approved []string
	for _, d := range a.Decisions {
		switch d.Action {
		case ActionReject:
			return d.Approver
		case ActionApprove:
			if d.Approver != "" {
				approved = append(approved, d.Approver)
			}
		}
	}
	return strings.Join(approved, ",")
}
