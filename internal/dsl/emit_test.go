package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarshalWorkflow_RoundTripsSupportedShape(t *testing.T) {
	wf := &Workflow{
		Meta: WorkflowMeta{Name: "restart-nginx", Description: "become=true"},
		Targets: []TargetGroup{
			{Name: "web", Type: "host", Hosts: []string{"web-1", "web-2"}},
		},
		Window:  ChangeWindow{Start: "02:00", End: "04:00", Timezone: "UTC", Days: []string{"Sat"}},
		Batches: BatchConfig{Strategy: "fixed", Steps: []int{1, 2}},
		Steps: []Step{
			{Name: "restart", Module: "svc", Action: "restart", Args: map[string]any{"name": "nginx"}},
			{Name: "check", Module: "shell", Action: "exec", Args: map[string]any{"cmd": "true"}, Idempotent: true},
		},
	}

	out, err := MarshalWorkflow(wf)
	require.NoError(t, err)

	reparsed, err := NewParser().ParseBytes(out)
	require.NoError(t, err, "emitted YAML must parse:\n%s", string(out))
	require.Len(t, reparsed.Steps, 2)
	assert.Equal(t, "restart-nginx", reparsed.Meta.Name)
	assert.Equal(t, "svc", reparsed.Steps[0].Module)
	assert.Equal(t, "restart", reparsed.Steps[0].Action)
	assert.Equal(t, "nginx", reparsed.Steps[0].Args["name"])
	assert.True(t, reparsed.Steps[1].Idempotent)
	assert.Equal(t, []string{"web-1", "web-2"}, reparsed.Targets[0].Hosts)
	assert.Equal(t, "fixed", reparsed.Batches.Strategy)
	assert.Equal(t, "02:00", reparsed.Window.Start)

	// And the validator must accept it — the same second door `levee compile`
	// strict mode applies, so translated output is guaranteed compilable.
	assert.Empty(t, NewValidator().Validate(reparsed))
}

func TestMarshalWorkflow_DeterministicArgs(t *testing.T) {
	wf := &Workflow{
		Steps: []Step{{Name: "s", Module: "shell", Action: "exec",
			Args: map[string]any{"z": 1, "a": 2, "m": 3}}},
	}
	out1, err := MarshalWorkflow(wf)
	require.NoError(t, err)
	out2, err := MarshalWorkflow(wf)
	require.NoError(t, err)
	assert.Equal(t, string(out1), string(out2), "emitted YAML must be byte-stable for diffing")

	s := string(out1)
	ia, im, iz := strings.Index(s, "a:"), strings.Index(s, "m:"), strings.Index(s, "z:")
	require.Positive(t, ia)
	assert.Less(t, ia, im)
	assert.Less(t, im, iz, "args must be emitted in sorted-key order:\n%s", s)
}

func TestMarshalWorkflow_FailClosedOnUnsupportedFields(t *testing.T) {
	cases := []struct {
		name    string
		wf      *Workflow
		wantMsg string
	}{
		{"inputs", &Workflow{Inputs: []InputParam{{Name: "x"}}}, "input parameter"},
		{"workflow rollback", &Workflow{Rollback: &RollbackSpec{}}, "workflow-level rollback"},
		{"workflow approval", &Workflow{Approval: &ApprovalSpec{}}, "workflow-level approval"},
		{"workflow gate", &Workflow{Gate: &GateSpec{}}, "workflow-level gates"},
		{"snapshot", &Workflow{Snapshot: &RunSnapshotSpec{}}, "snapshot"},
		{"step rollback", &Workflow{Steps: []Step{{Name: "s", Module: "shell", Action: "exec", Rollback: &RollbackSpec{}}}}, "rollback declaration"},
		{"step approval", &Workflow{Steps: []Step{{Name: "s", Module: "shell", Action: "exec", Approval: &ApprovalSpec{}}}}, "approval declaration"},
		{"step gate", &Workflow{Steps: []Step{{Name: "s", Module: "shell", Action: "exec", Gate: &GateSpec{}}}}, "gate declaration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MarshalWorkflow(tc.wf)
			require.Error(t, err, "unemittable fields must fail loudly, never be dropped")
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

func TestMarshalWorkflow_NilIsRefused(t *testing.T) {
	_, err := MarshalWorkflow(nil)
	require.Error(t, err)
}
