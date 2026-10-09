// resource_authz.go is the policy surface for the resources that are NOT
// changes.
//
// The change-scoped RPCs have judged every request against the permission
// matrix since the authz layer was wired (see internal/authz). Everything else
// that a token can reach — host inventory, groups, the template library, the
// audit trail, the system surface — was authenticated and otherwise unpoliced,
// which makes a configured matrix describe half of the API. A team pinned to
// dev could still enumerate prod's hostnames and credential references, freeze
// a prod host, delete a template another team builds changes from, read the
// whole audit log and dump the deployment's config.
//
// The rule, stated once here so the five services cannot drift apart:
//
//   - Scope comes from what the resource itself declares. A target says which
//     environment it belongs to with `labels["env"]`; a change says it with its
//     environment column. Groups, templates and config declare nothing, and
//     those are deployment-wide: they are judged in permission.default_env.
//     Deciding their environment by parsing a group name like "prod/db" would
//     hang a security judgement on a naming habit, so it is not done.
//   - Fleet-wide writes require `admin`. Who LEVEE is allowed to reach is not a
//     side effect of being allowed to change something inside it.
//   - Reads require `view`, and a list narrows to the caller's visible
//     environments instead of refusing the page — refusing would hide the rows
//     the caller IS entitled to, turning a permissions question into an empty
//     dashboard (the same reasoning ListChanges follows).
//   - An unattributable caller (shared token, no named credential) passes reads
//     and is refused writes. That exemption is inherited, not invented here;
//     see authz.AdmitsRead for why filtering every holder of one token hides
//     nothing and only darkens the console.
package grpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/permission"
	"github.com/nexus/levee/internal/state"
)

// targetEnvLabelKey is the label a target uses to state which environment it
// belongs to. It is the same key the ABAC condition vocabulary reads
// (internal/permission/abac.go matches on target labels), so one label serves
// both "which hosts does this policy talk about" and "who may touch them".
const targetEnvLabelKey = "env"

// envOfTarget is the environment a host inventory row declares. Empty means
// undeclared, which authz resolves to permission.default_env — and refuses when
// the deployment has a matrix and no default, rather than guessing.
func envOfTarget(t *state.Target) string {
	if t == nil {
		return ""
	}
	return t.Labels[targetEnvLabelKey]
}

// authorizeResource is the write gate. A nil authorizer short-circuits: the
// deployment configured no policy, so there is nothing to decide against, and
// that state is announced at startup rather than replayed per request.
func authorizeResource(ctx context.Context, a *authz.Authorizer, env, action, rpc string) error {
	if a == nil {
		return nil
	}
	d := a.Decide(SubjectFromContext(ctx), env, action)
	if d.Allowed {
		return nil
	}
	// Refusals are persisted before they are returned (SA-007): a refusal that
	// only exists in the client's error message is unauditable — the client can
	// drop it, and a caller that logs nothing leaves no trace that we said no.
	// Recorded here, at the request-level gate, and not inside Decide (see
	// Authorizer.RecordDenial for why).
	a.RecordDenial(ctx, d)
	if d.Subject == "" {
		return status.Errorf(codes.Unauthenticated,
			"%s requires an identity the policy can judge: %s", rpc, d.Reason)
	}
	return status.Errorf(codes.PermissionDenied,
		"%s denied for subject %q on env %q action %q: %s (see `levee authz explain --subject %s --env %s --action %s`)",
		rpc, d.Subject, d.Env, action, d.Reason, d.Subject, d.Env, action)
}

// authorizeResourceRead is the read gate: `view`, with the unattributable
// caller admitted by design rather than by omission.
func authorizeResourceRead(ctx context.Context, a *authz.Authorizer, env, rpc string) error {
	if a == nil {
		return nil
	}
	subject := SubjectFromContext(ctx)
	allowed, reason := a.AdmitsRead(subject, env)
	if allowed {
		return nil
	}
	// Decide is called again for the message only: it is the function that
	// applies the default-env fallback, and an operator reading "env \"\"" out
	// of a refusal would go looking for an empty environment instead of the
	// default one that was actually judged. RecordDenial reuses that same
	// decision, so the audited environment is the one that was judged too.
	d := a.Decide(subject, env, permission.ActionView)
	a.RecordDenial(ctx, d)
	return status.Errorf(codes.PermissionDenied,
		"%s denied for subject %q on env %q action %q: %s (see `levee authz explain --subject %s --env %s --action %s`)",
		rpc, subject, d.Env, permission.ActionView, reason, subject, d.Env, permission.ActionView)
}

// countVisibleRuns is the aggregate form of the read rule: a count is a read
// too, and "2 changes are running in your prod right now" is information about
// somebody else's change. A nil predicate counts everything, which is what an
// unfiltered caller (no matrix, or no provable identity) is entitled to.
func countVisibleRuns(runs []*state.Run, visible func(env string) bool) int {
	if visible == nil {
		return len(runs)
	}
	n := 0
	for _, r := range runs {
		if visible(envOf(r)) {
			n++
		}
	}
	return n
}

// resourceVisibility answers with nil when this caller may see every
// environment, or with a predicate over the environment a row declares. It is
// the list counterpart of authorizeResourceRead, and the "is filtering active
// at all" rule lives in one place because a list that filters when it cannot
// change the answer is just a slower list.
//
// The predicate does NOT record refusals (SA-007), deliberately: it runs once
// per row, and it answers "may this caller see this environment" rather than
// "is this request refused". Recording here would write one denial row per
// hidden row of a list — a page of 200 changes would bury the audit chain in
// 200 rows for one question. Refusals that turn a request away are recorded by
// the two gates above, one row per refused request.
func resourceVisibility(ctx context.Context, a *authz.Authorizer) func(env string) bool {
	if a == nil {
		return nil
	}
	subject := SubjectFromContext(ctx)
	if !a.ReadFilteringActive(subject) {
		return nil
	}
	return func(env string) bool {
		allowed, _ := a.AdmitsRead(subject, env)
		return allowed
	}
}
