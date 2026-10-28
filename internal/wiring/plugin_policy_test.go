package wiring

// plugin_policy_test.go proves WithPluginActionPolicy actually reaches the
// plan-time gate. Without this, the option could be stored on the Engine and
// never read (the failure mode is invisible: plans still generate, just with
// every plugin action judged reversible), and the CLI/serve paths would
// disagree with the compile gate while all their unit tests stayed green.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
)

func cloudWorkflowYAML(action string) string {
	return `name: plugin-policy-test
target:
  type: host
  query: "env=test"
steps:
  - name: s
    action: cloud.` + action + `
`
}

func cloudWorkflowWithWhitelist(action string) string {
	return cloudWorkflowYAML(action) + `allow_irreversible:
  - cloud.` + action + `
`
}

func cloudPolicy() dsl.PluginActionPolicy {
	return dsl.NewPluginActionPolicy(
		[]string{"cloud.destroy"},
		[]string{"cloud.apply"},
		[]string{"cloud"},
	)
}

// TestGeneratePlanUsesPluginActionPolicy walks the three states through the
// real plan funnel.
func TestGeneratePlanUsesPluginActionPolicy(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name      string
		workflow  string
		wantErr   bool
		wantIrrev bool
	}{
		{name: "reversible action plans clean", workflow: cloudWorkflowYAML("apply")},
		{name: "unclassified action refused", workflow: cloudWorkflowYAML("probe"), wantErr: true},
		{name: "destructive action refused", workflow: cloudWorkflowYAML("destroy"), wantErr: true},
		{name: "destructive action whitelisted", workflow: cloudWorkflowWithWhitelist("destroy"), wantIrrev: true},
		{name: "unclassified action whitelisted", workflow: cloudWorkflowWithWhitelist("probe"), wantIrrev: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, store := newTestEngine(t, "web-1")
			eng := NewEngine(store, WithPluginActionPolicy(cloudPolicy()))
			seedRun(t, store, "run-policy", tc.workflow)

			_, stored, err := eng.GeneratePlan(ctx, "run-policy", []string{"web-1"})
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "LE082")
				assert.Nil(t, stored)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, stored)

			var p plan.Plan
			require.NoError(t, json.Unmarshal([]byte(stored.JSON), &p))
			require.Len(t, p.Batches, 1)
			assert.Equal(t, tc.wantIrrev, p.Batches[0].Steps[0].Irreversible)
		})
	}
}

// TestGeneratePlanWithoutPluginPolicyIsTheControl pins that the same
// unclassified step plans reversibly when no policy was installed — the
// refusal above must come from the policy, not from an unknown-module
// fallback in the generator.
func TestGeneratePlanWithoutPluginPolicyIsTheControl(t *testing.T) {
	_, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-no-policy", cloudWorkflowYAML("probe"))

	_, stored, err := NewEngine(store).GeneratePlan(context.Background(), "run-no-policy", []string{"web-1"})
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.NotContains(t, stored.JSON, `"irreversible":true`)
}

// TestWithInherentIrreversibleIsDestructiveOnlyCompat pins the legacy option:
// it still installs the named destructive pairs, and it does not accidentally
// claim module names (which would turn on the fail-closed default).
func TestWithInherentIrreversibleIsDestructiveOnlyCompat(t *testing.T) {
	_, store := newTestEngine(t, "web-1")
	eng := NewEngine(store, WithInherentIrreversible([]string{"cloud.destroy"}))

	seedRun(t, store, "run-legacy-refused", cloudWorkflowYAML("destroy"))
	_, _, err := eng.GeneratePlan(context.Background(), "run-legacy-refused", []string{"web-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LE082")

	seedRun(t, store, "run-legacy-unclassified", cloudWorkflowYAML("probe"))
	_, stored, err := eng.GeneratePlan(context.Background(), "run-legacy-unclassified", []string{"web-1"})
	require.NoError(t, err, "legacy option must not turn on the module fail-closed default")
	assert.NotContains(t, stored.JSON, `"irreversible":true`)
}
