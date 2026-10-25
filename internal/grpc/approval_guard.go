package grpc

import (
	"context"
	"fmt"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/state"
)

// ApprovalDecisionGuard adapts an *authz.Authorizer to
// approval.DecisionGuard, bringing the gateway's permission judgement to
// every surface that casts approval votes directly against the domain
// service: CLI (--as), ChatOps commands, human-gate decisions and mobile
// deep links. Those paths do not pass through ChangeService.ApproveChange,
// which is where the matrix judgement used to live exclusively — so until
// this adapter, a deployment with a permissions.yaml granted or refused
// approval votes over gRPC while the very same vote over `levee approve`
// was judged by the Approvers roster alone.
//
// The judged subject is the VOTER passed to Approve/Reject, not the context
// subject: the gateway path already proves both are the same identity
// (ApproveChange refuses a claimed approver that disagrees with the
// credential), and on the direct paths (CLI, deep link) the context has no
// subject at all — the vote's attribution IS its identity.
type ApprovalDecisionGuard struct {
	// Auth is the policy registry. A nil Auth (or nil receiver) admits
	// everything: no policy configured, nothing to decide — the same
	// posture as authorizeResource.
	Auth *authz.Authorizer

	// Store resolves the run an approval belongs to, so the vote is judged
	// in the change's environment rather than a guess. Must be non-nil
	// whenever Auth is.
	Store state.Store
}

// AuthorizeDecision implements approval.DecisionGuard. The action arrives
// as approval.ActionApprove / approval.ActionReject, which spell the same
// tokens as permission.ActionApprove / ActionReject — one vocabulary, so a
// matrix row that grants "approve" governs both the RPC and the vote
// behind it.
func (g *ApprovalDecisionGuard) AuthorizeDecision(ctx context.Context, subject, action string, a *approval.Approval) error {
	if g == nil || g.Auth == nil {
		return nil
	}
	if a == nil {
		return fmt.Errorf("approval decision guard: no approval record to judge")
	}
	run, err := g.Store.GetRun(ctx, a.RunID)
	if err != nil {
		// Fail closed: a vote we cannot place in an environment is a vote
		// we cannot judge, and an unreadable store is not a policy grant.
		return fmt.Errorf("resolve run %s for policy: %w", a.RunID, err)
	}
	env := envOf(run)
	d := g.Auth.Decide(subject, env, action)
	if d.Allowed {
		return nil
	}
	// Same audit trail as the request-level gate (SA-007): a refusal that
	// exists only in the caller's error message is unauditable.
	g.Auth.RecordDenial(ctx, d)
	if d.Subject == "" {
		return fmt.Errorf("approval vote requires an identity the policy can judge: %s", d.Reason)
	}
	return fmt.Errorf("approval vote denied for subject %q on env %q action %q: %s (see `levee authz explain --subject %s --env %s --action %s`)",
		d.Subject, d.Env, action, d.Reason, d.Subject, d.Env, action)
}
