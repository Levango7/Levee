package executor

// irreversible_policy_test.go pins RegisterPolicy — the one call that makes
// the checker agree with dsl.NewValidatorWithPluginPolicy and
// plan.NewGeneratorWithPluginPolicy. The three-state semantics are asserted
// here on the checker itself so a divergence between the layers fails in the
// owning package rather than only at an end-to-end gate.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nexus/levee/internal/dsl"
)

func TestRegisterPolicyThreeStates(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterPolicy(dsl.NewPluginActionPolicy(
		[]string{"cloud.destroy"},
		[]string{"cloud.apply"},
		[]string{"cloud"},
	))

	destructive := c.Check(Step{Module: "cloud", Action: "destroy"})
	assert.True(t, destructive.Irreversible)
	assert.Contains(t, destructive.Reason, "whitelist")

	reversible := c.Check(Step{Module: "cloud", Action: "apply"})
	assert.False(t, reversible.Irreversible)

	unclassified := c.Check(Step{Module: "cloud", Action: "probe"})
	assert.True(t, unclassified.Irreversible,
		"an unclassified action of a registered plugin module must be irreversible")
	assert.Contains(t, unclassified.Reason, "not declared reversible")

	unknownModule := c.Check(Step{Module: "shell", Action: "exec"})
	assert.False(t, unknownModule.Irreversible,
		"modules outside the policy must stay reversible")
}

// TestRegisterPolicyExplicitDeclarationStillWins pins the documented priority:
// the author's explicit mark beats the policy's reversible declaration.
func TestRegisterPolicyExplicitDeclarationStillWins(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterPolicy(dsl.NewPluginActionPolicy(nil, []string{"cloud.apply"}, []string{"cloud"}))

	res := c.Check(Step{Module: "cloud", Action: "apply", Irreversible: true})
	assert.True(t, res.Irreversible)
	assert.Contains(t, res.Reason, "explicitly marked")
}

// TestRegisterPolicyZeroValueIsInert pins that installing an empty policy
// changes nothing — the common "no plugins installed" deployment.
func TestRegisterPolicyZeroValueIsInert(t *testing.T) {
	c := NewIrreversibleChecker()
	c.RegisterPolicy(dsl.PluginActionPolicy{})

	assert.Empty(t, c.Whitelist())
	assert.False(t, c.Check(Step{Module: "cloud", Action: "destroy"}).Irreversible)
}
