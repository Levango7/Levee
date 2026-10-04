// Package authz composes the two authorization models LEVEE already ships into
// one decision, and is the layer the serving process consults before it lets a
// caller act on a change.
//
// Why a composition rather than a choice: the permission matrix and the role
// tree answer different questions, and treating them as rival grant lists
// forces a bad trade — the matrix scopes by environment but its grants are
// flat and duplicated per team; the role tree expresses inheritance and reuse
// but has no notion of environment at all. Picking either one loses what the
// other is good at:
//
//   - matrix only: every team→environment pair needs every action spelled out,
//     and a new action has to be added to every row;
//   - roles only: a role grant follows the actor everywhere, so "operator" in
//     dev is "operator" in prod — the isolation the product sells is gone.
//
// So the two axes are kept orthogonal instead:
//
//	the matrix says WHERE a team may act (environment reachability);
//	the role tree says WHAT its members may do inside the environments
//	their team can already reach.
//
// Concretely: allowed = matrix.Allow(team, env, action)
//
//	|| (role grants action
//	    && team has at least one action in that environment
//	    && the action is not explicitly revoked for that team in that environment)
//
// The last clause is not decoration. The matrix documents that an explicit
// revoke takes precedence over everything, including its own admin super-set;
// a composition that ignored revokes would let a role silently re-authorise an
// action an operator had deliberately denied.
//
// Posture: with no matrix configured there is nothing to judge against, so
// Decide reports Enforced=false and allows — the serving process announces
// that state at startup instead of deciding silently per request. As soon as a
// matrix exists, an unregistered subject is DENIED: "I do not know you" has to
// land on the refusing side, or configuring the matrix would change nothing.
package authz

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexus/levee/internal/identity"
	"github.com/nexus/levee/internal/permission"
)

// Decision is the outcome of one authorisation question, carrying enough
// detail for a log line and for `levee authz explain` to show its work.
type Decision struct {
	// Enforced is false when no permission matrix is configured: the
	// deployment has declared no authorisation policy, so the request is
	// admitted and the caller should be warned at startup rather than here.
	Enforced bool
	Allowed  bool
	// Via names the axis that granted access: ViaMatrix or ViaRole. Empty on
	// denial or when enforcement is off.
	Via string
	// Reason explains a denial (or the absence of enforcement) in one clause.
	Reason string

	Subject string
	Env     string
	Action  string
	// Team and Role are the resolved membership; empty when unregistered.
	Team string
	Role string
}

const (
	ViaMatrix = "matrix"
	ViaRole   = "role"
)

// Authorizer answers authorisation questions against a loaded snapshot of the
// data directory. It is safe for concurrent use: every field is read-only
// after Load.
type Authorizer struct {
	registry   *identity.Registry
	matrix     *permission.PermissionMatrix
	roles      *permission.RoleTree
	defaultEnv string
}

// FileName is the permission matrix file within the LEVEE data directory.
const MatrixFileName = "permissions.yaml"

// RoleTreeFileName is the role tree file within the LEVEE data directory.
const RoleTreeFileName = "roles.yaml"

// Load reads the registry, the permission matrix and the role tree from
// dataDir. Missing files are not errors — an unconfigured deployment is a
// normal state, reported through Enforced/Registered rather than a failure to
// start.
func Load(dataDir, defaultEnv string) (*Authorizer, error) {
	reg, err := identity.LoadForDataDir(dataDir)
	if err != nil {
		return nil, err
	}

	// Missing files are a normal state here, but a malformed one is not: a
	// policy the operator wrote and LEVEE cannot parse must stop the load
	// rather than silently downgrade the deployment to "not enforced".
	matrix := permission.NewPermissionMatrix()
	matrixPath := filepath.Join(dataDir, MatrixFileName)
	if _, statErr := os.Stat(matrixPath); statErr == nil {
		if err := matrix.LoadFromYAML(matrixPath); err != nil {
			return nil, fmt.Errorf("load permission matrix: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("stat permission matrix: %w", statErr)
	}

	roles := permission.NewRoleTree()
	if err := roles.LoadFromYAML(filepath.Join(dataDir, RoleTreeFileName)); err != nil {
		return nil, fmt.Errorf("load role tree: %w", err)
	}

	return &Authorizer{
		registry:   reg,
		matrix:     matrix,
		roles:      roles,
		defaultEnv: defaultEnv,
	}, nil
}

// Enforced reports whether a permission matrix is configured. It is the
// switch between "this deployment has a policy" and "this deployment has only
// authentication", and the serving process logs it at startup.
func (a *Authorizer) Enforced() bool {
	return a != nil && len(a.matrix.Teams()) > 0
}

// AdmitsRead answers the read version of Decide, and the answer is deliberately
// not "the same as writes".
//
// A write by an unattributable caller (shared --token: the actor name is
// something the client asserts, so there is no principal to bind a decision to)
// is refused, because refusing costs nothing the caller needed. A read is the
// opposite: every holder of that token is the SAME principal, so denying them
// buys no isolation at all — it only turns dashboards and status pages dark, and
// does so in the one configuration (matrix + shared token) where an operator
// believes they have policy. So an unattributable caller passes the read gate,
// the serving process says so out loud at startup, and `/system/status` keeps
// saying it; a deployment that wants per-team visibility enables named tokens,
// SSO or OIDC, which is the same migration the approval path already requires.
//
// Once the caller IS attributable, reads are policed exactly like writes:
// matrix grant, role grant bounded by reachability, explicit revoke honoured.
func (a *Authorizer) AdmitsRead(subject, env string) (allowed bool, reason string) {
	if !a.Enforced() {
		return true, "no permission matrix configured (read visibility not filtered)"
	}
	if subject == "" {
		return true, "caller has no provable identity: a shared token makes every " +
			"client one principal, so filtering it would deny everyone equally"
	}
	d := a.Decide(subject, env, permission.ActionView)
	if d.Allowed {
		return true, fmt.Sprintf("granted via %s", d.Via)
	}
	return false, d.Reason
}

// ReadFilteringActive reports whether AdmitsRead can ever answer "no" for this
// caller. List endpoints need it as a separate question, because filtering a
// list means abandoning the store's offset paging and scanning with a cap — a
// cost worth paying only when it can actually change the answer. It restates no
// policy: the two facts are exactly the two AdmitsRead checks that admit
// everything.
func (a *Authorizer) ReadFilteringActive(subject string) bool {
	return a.Enforced() && subject != ""
}

// Registered returns the subject names the registry knows, for startup
// reconciliation against configured credentials.
func (a *Authorizer) Registered() []string {
	if a == nil {
		return nil
	}
	return a.registry.Names()
}

// Knows reports whether the registry has an entry for subject. The serving
// process uses it at startup to reconcile configured credentials against the
// registry: a named token whose subject is not registered can never be
// authorised once a matrix exists, and discovering that on the first denied
// request is too late. The reverse direction (a registered subject no
// credential can authenticate) is not checkable at startup — SSO identities
// are not enumerable there.
func (a *Authorizer) Knows(subject string) bool {
	if a == nil || subject == "" {
		return false
	}
	_, ok := a.registry.Lookup(subject)
	return ok
}

// Decide answers whether subject may perform action in env.
//
// env is the change's environment; when empty the configured default is used,
// and with no default the request is refused — an authorisation question with
// no environment cannot be answered against an environment-keyed policy, and
// guessing "any environment" is the one guess that must never be made.
func (a *Authorizer) Decide(subject, env, action string) Decision {
	if !a.Enforced() {
		return Decision{
			Enforced: false, Allowed: true,
			Reason:  "no permission matrix configured (authorization not in effect)",
			Subject: subject, Env: env, Action: action,
		}
	}

	d := Decision{Enforced: true, Subject: subject, Env: env, Action: action}
	if env == "" {
		env = a.defaultEnv
		d.Env = env
	}
	if env == "" {
		d.Reason = "no environment given and permission.default_env is unset"
		return d
	}
	if subject == "" {
		d.Reason = "no authenticated subject"
		return d
	}

	member, ok := a.registry.Lookup(subject)
	if !ok {
		d.Reason = "subject is not registered in users.yaml"
		return d
	}
	d.Team, d.Role = member.Team, member.Role
	if member.Team == "" {
		d.Reason = "registered without a team"
		return d
	}

	// Axis 1: the matrix may allow the action outright.
	if a.matrix.Allow(member.Team, env, action) {
		d.Allowed, d.Via = true, ViaMatrix
		return d
	}

	// Axis 2: a role grant, bounded by the environments the team can reach.
	if len(a.matrix.ActionsFor(member.Team, env)) == 0 {
		d.Reason = fmt.Sprintf("team %q has no grant in environment %q", member.Team, env)
		return d
	}
	if a.matrix.Revoked(member.Team, env, action) {
		d.Reason = fmt.Sprintf("action %q is explicitly revoked for team %q in %q", action, member.Team, env)
		return d
	}
	granted, err := a.roles.EffectivePermissions(member.Role)
	switch {
	case err != nil:
		// Unknown or empty role: no role grants. Not an error the caller can
		// act on, and not a reason to deny when the matrix already allowed.
		d.Reason = fmt.Sprintf("role %q is not declared in %s and the matrix does not grant %q", member.Role, RoleTreeFileName, action)
		return d
	case containsAction(granted, action):
		d.Allowed, d.Via = true, ViaRole
		return d
	}
	d.Reason = fmt.Sprintf("neither the matrix nor role %q grants %q in %q", member.Role, action, env)
	return d
}

// Explain renders a decision as an indented report for `levee authz explain`.
func (a *Authorizer) Explain(subject, env, action string) string {
	d := a.Decide(subject, env, action)
	var b strings.Builder
	fmt.Fprintf(&b, "subject : %s\n", orDash(subject))
	fmt.Fprintf(&b, "env     : %s%s\n", orDash(d.Env), envFallbackNote(env, d.Env))
	fmt.Fprintf(&b, "action  : %s\n", orDash(action))
	fmt.Fprintf(&b, "enforced: %v\n", d.Enforced)
	if d.Enforced {
		fmt.Fprintf(&b, "team    : %s\n", orDash(d.Team))
		fmt.Fprintf(&b, "role    : %s\n", orDash(d.Role))
	}
	fmt.Fprintf(&b, "allowed : %v\n", d.Allowed)
	if d.Via != "" {
		fmt.Fprintf(&b, "via     : %s\n", d.Via)
	}
	fmt.Fprintf(&b, "reason  : %s\n", d.Reason)
	return b.String()
}

func envFallbackNote(given, used string) string {
	if given == "" && used != "" {
		return " (from permission.default_env)"
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func containsAction(actions []string, action string) bool {
	for _, a := range actions {
		if a == action {
			return true
		}
	}
	return false
}

// ErrNotEnforced is returned by callers that require a policy to exist.
var ErrNotEnforced = errors.New("authz: no permission matrix configured")
