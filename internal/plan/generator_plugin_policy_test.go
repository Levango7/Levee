package plan

// generator_plugin_policy_test.go pins the plan-time half of the three-state
// plugin action classification. The same dsl.PluginActionPolicy feeds the
// compile gate and the executor checker; these tests prove the generator
// stamps the same verdict into the plan, refuses unwhitelisted destructive or
// unclassified actions (LE082), and binds that verdict into the plan hash.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

func cloudPolicy() dsl.PluginActionPolicy {
	return dsl.NewPluginActionPolicy(
		[]string{"cloud.destroy"},
		[]string{"cloud.apply"},
		[]string{"cloud"},
	)
}

// TestGeneratorPluginPolicyThreeStates pins the verdict for each state,
// including the fail-closed default for an unclassified action.
func TestGeneratorPluginPolicyThreeStates(t *testing.T) {
	g := NewGeneratorWithPluginPolicy(cloudPolicy())

	cases := []struct {
		action        string
		whitelist     []string
		wantIrrev     bool
		wantReasonSub string
	}{
		{action: "apply", wantIrrev: false, wantReasonSub: "reversible"},
		{action: "destroy", whitelist: []string{"cloud.destroy"}, wantIrrev: true, wantReasonSub: "whitelist"},
		{action: "probe", whitelist: []string{"cloud.probe"}, wantIrrev: true, wantReasonSub: "not declared reversible"},
	}

	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			wf := irreversibleFixture("cloud", tc.action, false)
			wf.AllowIrreversible = tc.whitelist
			p, err := g.Generate(wf, []string{"h1"})
			require.NoError(t, err)
			step := p.Batches[0].Steps[0]
			assert.Equal(t, tc.wantIrrev, step.Irreversible)
			assert.Contains(t, step.IrreversibleReason, tc.wantReasonSub)
		})
	}
}

// TestGeneratorPluginPolicyRefusesUnclassified pins the gate's fail-closed
// behaviour: an action the manifest left unclassified is refused without the
// whitelist, exactly like a declared destructive one.
func TestGeneratorPluginPolicyRefusesUnclassified(t *testing.T) {
	g := NewGeneratorWithPluginPolicy(cloudPolicy())

	wf := irreversibleFixture("cloud", "probe", false)
	wf.AllowIrreversible = nil
	_, err := g.Generate(wf, []string{"h1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LE082")
	assert.Contains(t, err.Error(), "cloud.probe")
}

// TestGeneratorPluginPolicyReversibleNeedsNoWhitelist is the load-bearing
// exemption: a declared reversible action is the one state that passes without
// allow_irreversible, and it must not be stamped irreversible.
func TestGeneratorPluginPolicyReversibleNeedsNoWhitelist(t *testing.T) {
	g := NewGeneratorWithPluginPolicy(cloudPolicy())

	wf := irreversibleFixture("cloud", "apply", false)
	wf.AllowIrreversible = nil
	p, err := g.Generate(wf, []string{"h1"})
	require.NoError(t, err)
	assert.False(t, p.Batches[0].Steps[0].Irreversible)
}

// TestGeneratorPluginPolicyUnknownModuleStaysReversible is the control: the
// fail-closed default is scoped to installed module plugins, not every module
// name that does not appear in the policy.
func TestGeneratorPluginPolicyUnknownModuleStaysReversible(t *testing.T) {
	g := NewGeneratorWithPluginPolicy(cloudPolicy())

	wf := irreversibleFixture("shell", "exec", false)
	wf.AllowIrreversible = nil
	p, err := g.Generate(wf, []string{"h1"})
	require.NoError(t, err)
	assert.False(t, p.Batches[0].Steps[0].Irreversible)
}

// TestGeneratorPluginPolicyBindsHash pins that the classification is part of
// the plan's identity: the same workflow produces the same hash under the same
// policy (wherever the plan is produced) and a different hash when the action
// is classified differently. Without the second half, a policy that failed to
// reach the generator would go unnoticed.
func TestGeneratorPluginPolicyBindsHash(t *testing.T) {
	wf := irreversibleFixture("cloud", "probe", false)

	withPolicy, err := NewGeneratorWithPluginPolicy(cloudPolicy()).Generate(wf, []string{"h1"})
	require.NoError(t, err)

	again, err := NewGeneratorWithPluginPolicy(cloudPolicy()).Generate(wf, []string{"h1"})
	require.NoError(t, err)
	assert.Equal(t, ComputeHash(withPolicy), ComputeHash(again),
		"identical policies must yield identical hashes")

	reversible := dsl.NewPluginActionPolicy(nil, []string{"cloud.probe"}, []string{"cloud"})
	exempted, err := NewGeneratorWithPluginPolicy(reversible).Generate(wf, []string{"h1"})
	require.NoError(t, err)
	assert.NotEqual(t, ComputeHash(withPolicy), ComputeHash(exempted),
		"a differently classified action must change the plan hash")
}

// TestNewGeneratorCompatStillDestructiveOnly pins the legacy constructor:
// NewGenerator(pairs...) must keep judging only the named destructive pairs.
func TestNewGeneratorCompatStillDestructiveOnly(t *testing.T) {
	g := NewGenerator("cloud.destroy")
	wf := irreversibleFixture("cloud", "destroy", false)
	wf.AllowIrreversible = nil
	_, err := g.Generate(wf, []string{"h1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LE082")

	// Without the declaration the same plugin module is not swept up: the
	// legacy constructor carries no module names, so no fail-closed default.
	g = NewGenerator()
	p, err := g.Generate(irreversibleFixture("cloud", "destroy", false), []string{"h1"})
	require.NoError(t, err)
	assert.False(t, p.Batches[0].Steps[0].Irreversible)
}
