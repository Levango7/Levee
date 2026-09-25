package recommend

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

// TestGenerate_ProducesParseableWorkflow is the invariant that keeps the AI
// path inside the governed path. A draft is only useful if dsl can parse it —
// that parser is the single entry to plan generation, plan_hash and approval.
// The previous generator emitted a private dialect that failed to parse, and
// internal/autoplanner was the shadow parser that hid the breakage.
func TestGenerate_ProducesParseableWorkflow(t *testing.T) {
	cases := []struct {
		name  string
		steps []FixStep
	}{
		{"single reversible step", []FixStep{{Name: "restart-nginx", Module: "svc", Action: "restart"}}},
		{"multiple steps", []FixStep{
			{Name: "stop-app", Module: "svc", Action: "stop"},
			{Name: "bump-config", Module: "file", Action: "write", Args: map[string]string{"path": "/etc/app.conf"}},
		}},
		{"unknown action has no reverse", []FixStep{{Name: "mystery", Module: "custom", Action: "frobnicate"}}},
		{"no args", []FixStep{{Name: "bare", Action: "install"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y, err := NewWorkflowGenerator().Generate("web-1", tc.steps)
			require.NoError(t, err)

			wf, perr := dsl.NewParser().ParseBytes([]byte(y))
			require.NoError(t, perr, "generated draft must parse: %s", y)
			assert.Len(t, wf.Steps, len(tc.steps))
			assert.Equal(t, "host", wf.Targets[0].Type)
			assert.Equal(t, []string{"web-1"}, wf.Targets[0].Hosts)
		})
	}
}

// TestGenerate_RollbackIsPerStepAndNeverNoop pins the compensation shape.
// A top-level rollback list has nowhere to attach, and a "noop" reverse makes
// an uncovered step look covered — which is exactly the false-clean verdict
// D-2 exists to prevent.
func TestGenerate_RollbackIsPerStepAndNeverNoop(t *testing.T) {
	y, err := NewWorkflowGenerator().Generate("web-1", []FixStep{
		{Name: "restart-nginx", Module: "svc", Action: "restart"},
		{Name: "mystery", Module: "custom", Action: "frobnicate"},
	})
	require.NoError(t, err)

	wf, perr := dsl.NewParser().ParseBytes([]byte(y))
	require.NoError(t, perr)

	require.Len(t, wf.Steps, 2)
	assert.NotNil(t, wf.Steps[0].Rollback, "a reversible step must declare its compensation")
	require.Len(t, wf.Steps[0].Rollback.Steps, 1)
	assert.Equal(t, "restart", wf.Steps[0].Rollback.Steps[0].Action)

	assert.Nil(t, wf.Steps[1].Rollback,
		"an underivable reverse must be an honest compensation gap, not a noop")

	assert.NotContains(t, y, "noop", "noop compensation must never be emitted")
	assert.NotContains(t, y, "\nrollback:", "rollback must be step-scoped, not a top-level list")
}

// TestReverseAction_NoInventedReverses documents the fail-closed behaviour:
// an action we cannot reverse yields no reverse at all.
func TestReverseAction_NoInventedReverses(t *testing.T) {
	rev, ok := reverseAction("restart")
	assert.True(t, ok)
	assert.Equal(t, "restart", rev)

	_, ok = reverseAction("frobnicate")
	assert.False(t, ok, "an unknown action must not yield an invented reverse")

	_, ok = reverseAction("")
	assert.False(t, ok)
}
