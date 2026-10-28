package dsl

// plugin_policy_test.go pins the three-state plugin action classification:
// declared destructive -> irreversible, declared reversible -> the only state
// that passes the whitelist gate, unclassified action of an installed module
// -> irreversible (fail-closed). The same type feeds the compile gate, the
// plan-time gate, and the executor checker, so these tests anchor the
// semantics all three must share.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPluginActionPolicyClassifiesThreeStates is the core table: one action per
// state, plus the unknown-module control that keeps the fail-closed default
// from spilling onto modules this deployment never installed.
func TestPluginActionPolicyClassifiesThreeStates(t *testing.T) {
	p := NewPluginActionPolicy(
		[]string{"cloud.destroy"},
		[]string{"cloud.apply"},
		[]string{"cloud"},
	)

	assert.True(t, p.JudgesIrreversible("cloud", "destroy"), "declared destructive must be irreversible")
	assert.False(t, p.JudgesIrreversible("cloud", "apply"), "declared reversible is the only exemption")
	assert.True(t, p.JudgesIrreversible("cloud", "probe"),
		"an unclassified action of an installed module plugin must be irreversible (fail-closed)")
	assert.False(t, p.JudgesIrreversible("other", "anything"),
		"a module this deployment never installed must not be swept up")
}

// TestPluginActionPolicyZeroValueClaimsNothing pins that the zero value is
// usable and inert: callers that never touch plugins need no special case.
func TestPluginActionPolicyZeroValueClaimsNothing(t *testing.T) {
	var p PluginActionPolicy
	assert.True(t, p.IsEmpty())
	assert.False(t, p.JudgesIrreversible("cloud", "destroy"))
	assert.Empty(t, p.Destructive())
	assert.Empty(t, p.Reversible())
	assert.Empty(t, p.Modules())
}

// TestPluginActionPolicyNormalizes pins the input hygiene: whitespace is
// trimmed, duplicates collapse, dot-less entries are dropped (they name no
// step), and a module name must not itself contain a dot.
func TestPluginActionPolicyNormalizes(t *testing.T) {
	p := NewPluginActionPolicy(
		[]string{" cloud.destroy ", "cloud.destroy", "nodot", ".", "cloud."},
		[]string{"cloud.apply", "cloud.apply"},
		[]string{"cloud", " cloud ", "", "bad.name"},
	)

	assert.Equal(t, []string{"cloud.destroy"}, p.Destructive())
	assert.Equal(t, []string{"cloud.apply"}, p.Reversible())
	assert.Equal(t, []string{"cloud"}, p.Modules())
	assert.False(t, p.JudgesIrreversible("bad", "name"), "a dotted module name is not a module")
}

// TestPluginActionPolicyDestructiveWinsOverlap pins the corrupt-record reading:
// install-time validation refuses an action in both lists, but if a record
// still carries it the destructive reading must win — silently choosing the
// safe side would be the one failure mode this feature exists to prevent.
func TestPluginActionPolicyDestructiveWinsOverlap(t *testing.T) {
	p := NewPluginActionPolicy([]string{"cloud.destroy"}, []string{"cloud.destroy"}, []string{"cloud"})
	assert.True(t, p.JudgesIrreversible("cloud", "destroy"))
}

// TestValidatorWithPluginPolicyThreeStates drives the compile gate (V14/LE082)
// through all three states. The whitelist cases are the load-bearing half: a
// destructive or unclassified action is accepted exactly when the workflow
// names it in allow_irreversible.
func TestValidatorWithPluginPolicyThreeStates(t *testing.T) {
	policy := NewPluginActionPolicy(
		[]string{"cloud.destroy"},
		[]string{"cloud.apply"},
		[]string{"cloud"},
	)
	v := NewValidatorWithPluginPolicy(policy)

	cases := []struct {
		name      string
		action    string
		whitelist []string
		wantLE082 bool
	}{
		{name: "reversible passes without whitelist", action: "apply"},
		{name: "destructive refused without whitelist", action: "destroy", wantLE082: true},
		{name: "destructive accepted when whitelisted", action: "destroy", whitelist: []string{"cloud.destroy"}},
		{name: "unclassified refused without whitelist", action: "probe", wantLE082: true},
		{name: "unclassified accepted when whitelisted", action: "probe", whitelist: []string{"cloud.probe"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := validWorkflow()
			wf.Steps = []Step{{Name: "s", Module: "cloud", Action: tc.action}}
			wf.AllowIrreversible = tc.whitelist

			_, found := findErr(v.Validate(wf), codeIrreversibleNotWhitelisted)
			assert.Equal(t, tc.wantLE082, found)
		})
	}
}

// TestValidatorWithPluginPolicyUnknownModulePasses is the control: a module
// outside every installed plugin's declaration must stay reversible, proving
// the fail-closed default is scoped to installed plugins rather than flagging
// every unknown module.
func TestValidatorWithPluginPolicyUnknownModulePasses(t *testing.T) {
	v := NewValidatorWithPluginPolicy(NewPluginActionPolicy(nil, nil, []string{"cloud"}))
	wf := validWorkflow()
	wf.Steps = []Step{{Name: "s", Module: "shell", Action: "exec"}}
	assert.Empty(t, v.Validate(wf))
}

// TestNewValidatorCompatKeepsDestructiveOnly pins that the legacy constructor
// still enforces exactly the destructive declarations it was given.
func TestNewValidatorCompatKeepsDestructiveOnly(t *testing.T) {
	v := NewValidator("cloud.destroy")
	wf := validWorkflow()
	wf.Steps = []Step{{Name: "s", Module: "cloud", Action: "destroy"}}

	errs := v.Validate(wf)
	require.Len(t, errs, 1)
	assert.Equal(t, codeIrreversibleNotWhitelisted, errs[0].Code)
}
