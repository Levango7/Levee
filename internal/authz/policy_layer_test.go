package authz

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/permission"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadFixtureWithPolicies builds the standard fixture and adds a policies.yaml.
// A malformed policies file must fail the load (see TestMalformedPolicies...).
func loadFixtureWithPolicies(t *testing.T, defaultEnv, policiesYAML string) *Authorizer {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, dir, matrixYAML)
	require.NoError(t, os.WriteFile(filepath.Join(dir, PoliciesFileName), []byte(policiesYAML), 0o600))
	a, err := Load(dir, defaultEnv)
	require.NoError(t, err)
	return a
}

// alice is sre/operator. The matrix grants sre only `view` in prod, so an
// `apply` in prod passes through the ROLE axis — the layer must narrow that
// path too, not just the matrix path.
const denyProdApplyPolicy = `
policies:
  - id: no-prod-apply
    effect: deny
    resource: "change:*"
    action: "apply"
    condition: "target.env = prod"
`

func TestConditionalPolicyDeniesMatchingRequest(t *testing.T) {
	a := loadFixtureWithPolicies(t, "dev", denyProdApplyPolicy)

	base := a.Decide("alice", "dev", permission.ActionApply)
	require.True(t, base.Allowed, "control: apply in dev is unaffected by the prod-only deny")

	d := a.Decide("alice", "prod", permission.ActionApply)
	assert.False(t, d.Allowed, "the matching deny policy must refuse an otherwise-allowed apply")
	assert.Empty(t, d.Via, "Via names the granting axis and stays empty on a refusal")
	assert.Contains(t, d.Reason, "conditional policy")
}

func TestConditionalPolicyLeavesNonMatchingRequestAlone(t *testing.T) {
	// The same deny, conditioned on the OTHER environment: evaluating it in prod
	// must yield "no match", which defers to the base decision rather than
	// denying. This is what distinguishes ErrNoMatch from a deny.
	a := loadFixtureWithPolicies(t, "dev", `
policies:
  - id: no-dev-apply
    effect: deny
    resource: "change:*"
    action: "apply"
    condition: "target.env = dev"
`)

	d := a.Decide("alice", "prod", permission.ActionApply)
	assert.True(t, d.Allowed, "a non-matching policy must not refuse the request")
	assert.Equal(t, ViaRole, d.Via, "the role axis is still the granting axis")
}

func TestConditionalAllowMarksPolicyAxis(t *testing.T) {
	a := loadFixtureWithPolicies(t, "dev", `
policies:
  - id: allow-prod-apply
    effect: allow
    resource: "change:*"
    action: "apply"
    condition: "target.env = prod"
`)
	d := a.Decide("alice", "prod", permission.ActionApply)
	assert.True(t, d.Allowed)
	assert.Equal(t, ViaPolicy, d.Via)
}

// The layer is consulted only on the allow path: it may narrow, never widen.
func TestConditionalPolicyCannotWiden(t *testing.T) {
	// ghost has an undeclared role and no matrix grant, so the base decision
	// refuses before the layer is reached; a matching allow policy must not
	// resurrect it.
	a := loadFixtureWithPolicies(t, "dev", `
policies:
  - id: allow-everything
    effect: allow
    resource: "*"
    action: "*"
`)
	d := a.Decide("ghost", "prod", permission.ActionApply)
	assert.False(t, d.Allowed, "an allow policy must not grant what the matrix and role axes refused")
}

func TestConditionalPolicyAbsentKeepsBaseAxes(t *testing.T) {
	// No policies.yaml at all: the two axes answer exactly as before.
	a, _ := loadFixture(t, "dev")
	d := a.Decide("alice", "prod", permission.ActionApply)
	assert.True(t, d.Allowed)
	assert.Equal(t, ViaRole, d.Via)
}

func TestMalformedPoliciesFailLoad(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, matrixYAML)
	// A condition that cannot be parsed must stop the load rather than silently
	// drop the constraint.
	require.NoError(t, os.WriteFile(filepath.Join(dir, PoliciesFileName), []byte(`
policies:
  - id: broken
    effect: deny
    resource: "change:*"
    action: "apply"
    condition: "target.env =~ prod"
`), 0o600))
	_, err := Load(dir, "dev")
	require.Error(t, err, "a malformed policies.yaml must fail the load")
}
