// subject.go separates WHO CALLED from WHO SAYS THEY CALLED.
//
// The actor carried under actorKey (internal/grpc/change_service.go) is an
// audit label: over gRPC it comes from the client-supplied "x-actor"
// metadata, over REST from the X-Acting-As header, and the auth code
// itself documents that a legacy shared token authenticates nobody in
// particular. Labels are fine for logs and useless for governance: an
// approval chain whose votes are attributed to a name the voter types is
// not an approval chain.
//
// A Subject is an identity LEVEE verified: the subject a named token is
// bound to, the identity inside a verified SSO session token, the OIDC
// subject, or — in process — the caller's own assertion through
// ContextWithSubject. Nothing on the wire can produce one without
// presenting the matching credential.
//
// Governance decisions (who approved what) consume SubjectFromContext.
// When it is empty the caller has no verifiable identity, and the
// operation is refused rather than attributed to a guess.

package grpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// subjectKey carries a verified identity. It is deliberately a distinct
// key from actorKey so that an audit label can never be read as
// authentication, in either direction.
type subjectKey struct{}

// SubjectFromContext returns the verified identity of the caller, or ""
// when the caller presented an unverifiable credential: a legacy shared
// token (which authenticates the deployment, not a person), or no
// credential at all (development mode).
func SubjectFromContext(ctx context.Context) string {
	s, _ := ctx.Value(subjectKey{}).(string)
	return s
}

// ContextWithSubject marks an in-process caller's identity as verified.
//
// Local-mode commands (levee --local, the CLI's direct service calls)
// live inside the trust boundary: there is no remote caller to fake them,
// and the process itself is the authority on who typed the command. It
// must NOT be called on any path that consumes remote input — a gRPC or
// REST handler gets its subject from the auth layer, never from here.
func ContextWithSubject(ctx context.Context, subject string) context.Context {
	if subject == "" {
		return ctx
	}
	ctx = context.WithValue(ctx, subjectKey{}, subject)
	// Keep the audit label in step with the verified identity so a
	// governance action is never recorded under a different name than the
	// one that authorised it.
	return context.WithValue(ctx, actorKey{}, subject)
}

// admitAssertedIdentity adopts a client-asserted name as the subject, but
// ONLY in a deployment that configured no credential source at all
// (authentication disabled).
//
// The line is drawn at whether a trust boundary exists, not at whether a
// name was verified:
//
//   - no credentials configured — every caller can already invoke every
//     RPC, so there is no attacker to withhold attribution from and the
//     asserted name is the best available record of who ran the command.
//   - a credential IS configured but authenticates nobody in particular
//     (the legacy shared --token) — refusing is the whole point. That
//     token proves "someone inside the deployment", which is exactly the
//     claim an approval must not rest on: any holder could approve as any
//     name.
//
// It must be called from the auth layer, which is the only place that
// knows which case applies.
func admitAssertedIdentity(ctx context.Context, asserted string) context.Context {
	if SubjectFromContext(ctx) != "" || asserted == "" {
		return ctx
	}
	return ContextWithSubject(ctx, asserted)
}

// creatorFromCtx resolves the identity a newly created change is OWNED by.
//
// The answer must be the verified subject when one exists, because
// run.Creator feeds the approval chain's Initiator (see kickoffApproval) and
// that field drives exclude_initiator — the independence rule that stops an
// author from approving their own high-tier change. Sourcing it from
// actorFromCtx alone would let a caller name someone else in the "x-actor"
// header and then vote their own change through, or hide behind a name that no
// subject can ever match so that the exclusion excludes nobody.
//
// Unverifiable credentials (legacy shared token, development mode, the CLI's
// local mode) keep the asserted label: there is no subject to prefer, and
// inventing a constant there would replace "who typed this" with a channel
// name.
//
// Every creation path must use it. A path that does not is not untidy — it
// puts the change into a different governance universe for the rest of its
// life (see the three call sites: CreateChange, CloneChange,
// InstantiateTemplate).
func creatorFromCtx(ctx context.Context) string {
	if subj := SubjectFromContext(ctx); subj != "" {
		return subj
	}
	return actorFromCtx(ctx)
}

// requireSubject returns the verified identity, or an Unauthenticated
// status telling the caller exactly which credential kinds LEVEE accepts
// for this operation. Governance writes use it before any state change.
func requireSubject(ctx context.Context, op string) (string, error) {
	if s := SubjectFromContext(ctx); s != "" {
		return s, nil
	}
	return "", status.Errorf(codes.Unauthenticated,
		"%s requires a verifiable identity: approve/reject votes are bound to the authenticated "+
			"subject, and this credential carries none. Use a named token (server.auth_tokens), an "+
			"SSO session, or OIDC; a shared --token authenticates the deployment, not a person.", op)
}
