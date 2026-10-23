package authz

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/identity"
	"github.com/nexus/levee/internal/permission"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture summary (sre can only VIEW prod; dba can apply in prod):
//
//	alice  sre / operator(apply, rollback ← viewer(view))  → dev 可 apply（矩阵），prod 靠角色
//	bob    dba / viewer(view)                              → prod 可 view
//	ghost  sre / phantom（roles.yaml 里没有这个角色）
const (
	matrixYAML = `
teams:
  - name: sre
    environments:
      - name: dev
        actions: [plan, apply, approve, rollback, view]
      - name: prod
        actions: [view]
  - name: dba
    environments:
      - name: prod
        actions: [apply, rollback, view]
`
	rolesYAML = `
roles:
  - name: viewer
    permissions: [view]
  - name: operator
    parent: viewer
    permissions: [apply, rollback]
`
	usersYAML = `
users:
  - name: alice
    team: sre
    role: operator
  - name: bob
    team: dba
    role: viewer
  - name: ghost
    team: sre
    role: phantom
`
)

func writeFixture(t *testing.T, dir, matrix string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, MatrixFileName), []byte(matrix), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, RoleTreeFileName), []byte(rolesYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "users.yaml"), []byte(usersYAML), 0o600))
}

func loadFixture(t *testing.T, defaultEnv string) (*Authorizer, string) {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, dir, matrixYAML)
	a, err := Load(dir, defaultEnv)
	require.NoError(t, err)
	return a, dir
}

func TestNoMatrixIsNotEnforced(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "users.yaml"), []byte(usersYAML), 0o600))

	a, err := Load(dir, "dev")
	require.NoError(t, err)
	require.False(t, a.Enforced(), "no matrix means no policy to enforce")

	d := a.Decide("nobody", "prod", "apply")
	assert.True(t, d.Allowed, "an unconfigured deployment must not start denying")
	assert.False(t, d.Enforced)
	assert.Contains(t, d.Reason, "not in effect")
}

func TestMatrixAxisDecidesOnItsOwn(t *testing.T) {
	a, _ := loadFixture(t, "dev")

	// alice's team is granted apply in dev outright.
	d := a.Decide("alice", "dev", "apply")
	require.True(t, d.Allowed)
	assert.Equal(t, ViaMatrix, d.Via, "a direct matrix grant must not need the role axis")
	assert.Equal(t, "sre", d.Team)
	assert.Equal(t, "operator", d.Role)

	// An action nobody grants in that environment stays denied, and the reason
	// says which axis failed.
	d = a.Decide("bob", "prod", "approve")
	assert.False(t, d.Allowed)
	assert.Contains(t, d.Reason, "neither the matrix nor role")
}

func TestRoleExtendsInsideAReachableEnvironment(t *testing.T) {
	a, _ := loadFixture(t, "dev")

	// sre may only view prod, so the environment is reachable; alice's role
	// carries apply — this is precisely the extension the composition exists
	// for (the role supplies the action, the matrix supplies the environment).
	d := a.Decide("alice", "prod", "apply")
	require.True(t, d.Allowed)
	assert.Equal(t, ViaRole, d.Via)

	// The inheritance path works too: operator inherits view from viewer.
	d = a.Decide("alice", "prod", "view")
	require.True(t, d.Allowed)
	assert.Equal(t, ViaMatrix, d.Via, "the matrix grants view directly, so it wins")
}

func TestRoleCannotGrantInAnUnreachableEnvironment(t *testing.T) {
	a, _ := loadFixture(t, "dev")

	// dba exists only for prod. bob's role grants nothing anyway, but the point
	// is the refusal reason: dev is not reachable for his team, so no role
	// grant could carry him there even if it did.
	d := a.Decide("bob", "dev", "apply")
	assert.False(t, d.Allowed)
	assert.Contains(t, d.Reason, "no grant in environment")
	assert.Empty(t, d.Via)
}

func TestExplicitRevokeBeatsRoleGrant(t *testing.T) {
	// LoadFromYAML resets revokes by design, so this fixture is built in code.
	m := permission.NewPermissionMatrix()
	require.NoError(t, m.Grant("sre", "prod", permission.ActionView))
	require.NoError(t, m.Revoke("sre", "prod", permission.ActionApply))

	roles := permission.NewRoleTree()
	require.NoError(t, roles.AddRole("operator", ""))
	require.NoError(t, roles.GrantPermission("operator", permission.ActionApply))

	a := &Authorizer{
		registry:   &identity.Registry{Users: []identity.User{{Name: "alice", Team: "sre", Role: "operator"}}},
		matrix:     m,
		roles:      roles,
		defaultEnv: "dev",
	}

	d := a.Decide("alice", "prod", "apply")
	assert.False(t, d.Allowed, "an explicit revoke outranks a role grant")
	assert.Contains(t, d.Reason, "explicitly revoked")
}

func TestUnknownSubjectIsDeniedWhenEnforced(t *testing.T) {
	a, _ := loadFixture(t, "dev")
	d := a.Decide("mallory", "dev", "apply")
	assert.False(t, d.Allowed)
	assert.Contains(t, d.Reason, "not registered")
}

func TestUndeclaredRoleGrantsNothingButMatrixStillApplies(t *testing.T) {
	a, _ := loadFixture(t, "dev")

	// The matrix grants per TEAM, so being an sre member is enough in dev —
	// the role only matters where the matrix alone would not carry the action.
	d := a.Decide("ghost", "dev", "apply")
	require.True(t, d.Allowed)
	assert.Equal(t, ViaMatrix, d.Via)

	// prod is view-only for sre: reaching it is allowed, acting there needs a
	// role, and ghost's role is not declared — so this is where the refusal
	// shows up, naming the reason.
	d = a.Decide("ghost", "prod", "apply")
	assert.False(t, d.Allowed, "a role the tree does not declare grants nothing")
	assert.Contains(t, d.Reason, "not declared")
}

func TestEnvironmentFallsBackToDefaultAndThenRefusesToGuess(t *testing.T) {
	a, dir := loadFixture(t, "dev")
	d := a.Decide("alice", "", "view")
	require.True(t, d.Allowed)
	assert.Equal(t, "dev", d.Env, "the configured default environment fills in when the change has none")

	// Same policy, no default env: the question cannot be answered, and
	// guessing "any environment" is the one guess that must never be made.
	noDefault, err := Load(dir, "")
	require.NoError(t, err)
	require.True(t, noDefault.Enforced())
	d = noDefault.Decide("alice", "", "view")
	assert.False(t, d.Allowed)
	assert.Contains(t, d.Reason, "default_env is unset")
}

func TestExplainShowsBothAxes(t *testing.T) {
	a, _ := loadFixture(t, "dev")
	out := a.Explain("alice", "dev", "apply")
	for _, want := range []string{
		"subject : alice", "env     : dev", "action  : apply",
		"team    : sre", "role    : operator", "via     : matrix",
	} {
		assert.Contains(t, out, want, "explain must show its work:\n%s", out)
	}

	denied := a.Explain("mallory", "dev", "apply")
	assert.Contains(t, denied, "allowed : false")
	assert.Contains(t, denied, "not registered")
}

// --- default subject (local mode) -------------------------------------------
//
// Local mode has one operator and no per-request identity: every command passes
// a bare context.Background() (see cmd/levee), so the subject reaching the
// authorizer is always "". WithDefaultSubject is how those commands get judged
// at all. These cases pin the two properties that make it safe: it substitutes
// ONLY when the context supplied nothing, and an empty default is exactly the
// behaviour every deployment had before the option existed.

func TestDefaultSubjectSubstitutesOnlyWhenTheContextSuppliesNone(t *testing.T) {
	a, _ := loadFixture(t, "dev")
	a = a.WithDefaultSubject("alice") // sre → operator, so apply on prod is granted

	d := a.Decide("", "prod", "apply")
	assert.True(t, d.Allowed, "the default subject must actually be judged: %s", d.Reason)
	assert.Equal(t, "alice", d.Subject, "the decision must name the identity it judged")

	// A real subject wins: the default is a fallback, not an override. If it
	// overrode, a named-token caller could be silently granted somebody else's
	// rights.
	//
	// bob is dba, and the fixture gives dba prod only — so dev is the action/env
	// pair where the two subjects differ. (Using prod/apply would have proved
	// nothing: the fixture grants dba apply on prod outright.)
	d = a.Decide("bob", "dev", "apply")
	assert.False(t, d.Allowed, "the default must not lift bob into an environment he cannot reach")
	assert.Equal(t, "bob", d.Subject, "the decision must name the subject that was actually judged")
}

func TestDefaultSubjectUnsetIsTheOldBehaviour(t *testing.T) {
	a, _ := loadFixture(t, "dev")

	// No default: an identity-less write is refused, exactly as before this
	// option existed. This is the assertion that keeps the option from being a
	// silent change for every deployment that never sets it.
	d := a.Decide("", "prod", "apply")
	assert.False(t, d.Allowed, "without a default there is nobody to judge")
}

func TestDefaultSubjectReachesTheReadPathsToo(t *testing.T) {
	a, _ := loadFixture(t, "dev")
	a = a.WithDefaultSubject("bob") // viewer

	// AdmitsRead and ReadFilteringActive must answer for the substituted
	// subject. If only Decide substituted, one local request could be judged as
	// bob for its write while its read was treated as unattributable (admitted
	// wholesale) — the same request answering two different questions about who
	// is asking.
	allowed, reason := a.AdmitsRead("", "prod")
	assert.True(t, allowed, "bob can view prod: %s", reason)
	assert.True(t, a.ReadFilteringActive(""), "bob is attributable, so lists filter for him")

	// And a subject that cannot reach an environment IS refused — something the
	// pre-substitution path (unattributable ⇒ admit) could never say.
	//
	// bob is dba, whose fixture entry names prod and nothing else, so dev is the
	// refusal to assert. (ghost would not work as the example: ghost is sre, and
	// the MATRIX grants sre view on prod regardless of the unknown role.)
	narrow, _ := loadFixture(t, "dev")
	narrow = narrow.WithDefaultSubject("bob")
	allowed, _ = narrow.AdmitsRead("", "dev")
	assert.False(t, allowed, "bob cannot reach dev, so the read must be refused")
}

func TestDefaultSubjectOnANilAuthorizerIsSafe(t *testing.T) {
	// The local command path may hold a nil authorizer (no --as). Setting a
	// default on it must not panic: WithAuthorizer(nil) is how the ungated
	// posture is expressed everywhere else.
	var a *Authorizer
	assert.Nil(t, a.WithDefaultSubject("alice"))
}
