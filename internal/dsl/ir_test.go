package dsl

// ir_test.go — unit tests for IR generation and JSON round-trip.
//
// Coverage:
//   - GenerateIR produces a versioned IR from a workflow AST
//   - IR carries input/target/batch/step/approval/rollback data
//   - MarshalJSON / UnmarshalJSON round-trip preserves data
//   - Unsupported ir_version is rejected on unmarshal
//   - Missing ir_version is rejected on unmarshal
//   - nil AST returns an error

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// irSampleWorkflow returns a workflow exercising every IR-relevant field.
func irSampleWorkflow() *Workflow {
	return &Workflow{
		Meta: WorkflowMeta{Name: "ir-wf", Version: "1.0", Description: "test"},
		Inputs: []InputParam{
			{Name: "pkg", Type: "string", Required: true, Default: "nginx"},
			{Name: "wait", Type: "duration"},
		},
		Targets: []TargetGroup{
			{Name: "t1", Type: "host", Hosts: []string{"h1", "h2"}, MinCount: 1, MaxCount: 10},
		},
		Batches: BatchConfig{Strategy: "percent", MaxConcurrency: 5, Steps: []int{1, 50, 100}},
		Steps: []Step{
			{Name: "s1", Module: "shell", Action: "exec", DependsOn: []string{"s0"}, Idempotent: true},
		},
		Rollback: &RollbackSpec{
			Strategy:    "snapshot",
			OnFailure:   "auto",
			VerifyAfter: true,
			Steps:       []Step{{Name: "rb1", Module: "shell", Action: "exec"}},
		},
		Approval: &ApprovalSpec{Level: "high", Approvers: []string{"alice"}, MinApprovers: 1},
	}
}

// --- GenerateIR basics -----------------------------------------------------

func TestGenerateIRVersion(t *testing.T) {
	ir, err := GenerateIR(irSampleWorkflow(), nil)
	require.NoError(t, err)
	assert.Equal(t, IRVersion, ir.IRVersion)
	assert.Equal(t, "1.0", ir.IRVersion)
}

func TestGenerateIRNilWorkflow(t *testing.T) {
	ir, err := GenerateIR(nil, nil)
	require.Error(t, err)
	assert.Nil(t, ir)
}

func TestGenerateIRWorkflowMeta(t *testing.T) {
	ir, err := GenerateIR(irSampleWorkflow(), nil)
	require.NoError(t, err)
	assert.Equal(t, "ir-wf", ir.Workflow.Name)
	assert.Equal(t, "1.0", ir.Workflow.Version)
	assert.Equal(t, "test", ir.Workflow.Description)
}

func TestGenerateIRInputs(t *testing.T) {
	ir, err := GenerateIR(irSampleWorkflow(), nil)
	require.NoError(t, err)
	require.Len(t, ir.Inputs, 2)
	assert.Equal(t, "pkg", ir.Inputs[0].Name)
	assert.Equal(t, "string", ir.Inputs[0].Type)
	assert.True(t, ir.Inputs[0].Required)
	assert.True(t, ir.Inputs[0].HasDefault)
	assert.Equal(t, "nginx", ir.Inputs[0].Default)
	assert.False(t, ir.Inputs[1].HasDefault)
}

func TestGenerateIRTargets(t *testing.T) {
	ir, err := GenerateIR(irSampleWorkflow(), nil)
	require.NoError(t, err)
	require.Len(t, ir.Targets, 1)
	assert.Equal(t, "t1", ir.Targets[0].Name)
	assert.Equal(t, []string{"h1", "h2"}, ir.Targets[0].Hosts)
	assert.Equal(t, 1, ir.Targets[0].MinCount)
	assert.Equal(t, 10, ir.Targets[0].MaxCount)
}

func TestGenerateIRBatches(t *testing.T) {
	ir, err := GenerateIR(irSampleWorkflow(), nil)
	require.NoError(t, err)
	assert.Equal(t, "percent", ir.Batches.Strategy)
	assert.Equal(t, 5, ir.Batches.MaxConcurrency)
	assert.Equal(t, []int{1, 50, 100}, ir.Batches.Steps)
}

func TestGenerateIRSteps(t *testing.T) {
	ir, err := GenerateIR(irSampleWorkflow(), nil)
	require.NoError(t, err)
	require.Len(t, ir.Steps, 1)
	assert.Equal(t, "s1", ir.Steps[0].Name)
	assert.Equal(t, "exec", ir.Steps[0].Action)
	assert.Equal(t, "shell", ir.Steps[0].Module)
	assert.Equal(t, []string{"s0"}, ir.Steps[0].DependsOn)
	assert.True(t, ir.Steps[0].Idempotent)
}

func TestGenerateIRRollback(t *testing.T) {
	ir, err := GenerateIR(irSampleWorkflow(), nil)
	require.NoError(t, err)
	require.NotNil(t, ir.Rollback)
	assert.Equal(t, "snapshot", ir.Rollback.Strategy)
	assert.True(t, ir.Rollback.VerifyAfter)
	// StepMap should map s1 -> [rb1].
	require.Contains(t, ir.Rollback.StepMap, "s1")
	assert.Equal(t, []string{"rb1"}, ir.Rollback.StepMap["s1"])
}

func TestGenerateIRApproval(t *testing.T) {
	ir, err := GenerateIR(irSampleWorkflow(), nil)
	require.NoError(t, err)
	require.NotNil(t, ir.Approval)
	assert.Equal(t, "high", ir.Approval.Level)
	assert.Equal(t, []string{"alice"}, ir.Approval.Approvers)
	assert.Equal(t, 1, ir.Approval.MinApprovers)
}

func TestGenerateIRNoRollback(t *testing.T) {
	wf := irSampleWorkflow()
	wf.Rollback = nil
	ir, err := GenerateIR(wf, nil)
	require.NoError(t, err)
	assert.Nil(t, ir.Rollback)
}

func TestGenerateIRNoApproval(t *testing.T) {
	wf := irSampleWorkflow()
	wf.Approval = nil
	ir, err := GenerateIR(wf, nil)
	require.NoError(t, err)
	assert.Nil(t, ir.Approval)
}

// --- Registry-driven type normalisation ------------------------------------

func TestGenerateIRNormalisesInputType(t *testing.T) {
	r := NewTypeRegistry()
	require.NoError(t, r.RegisterAlias("port", TypeInt{}))
	wf := &Workflow{
		Meta:   WorkflowMeta{Name: "w"},
		Inputs: []InputParam{{Name: "p", Type: "port"}},
	}
	ir, err := GenerateIR(wf, r)
	require.NoError(t, err)
	// The alias's String() is its name, so the IR carries "port".
	assert.Equal(t, "port", ir.Inputs[0].Type)
}

func TestGenerateIRBasicTypeCanonicalName(t *testing.T) {
	wf := &Workflow{
		Meta:   WorkflowMeta{Name: "w"},
		Inputs: []InputParam{{Name: "p", Type: "int"}},
	}
	ir, err := GenerateIR(wf, nil)
	require.NoError(t, err)
	assert.Equal(t, "int", ir.Inputs[0].Type)
}

// --- JSON round-trip -------------------------------------------------------

func TestIRMarshalUnmarshalRoundTrip(t *testing.T) {
	ir1, err := GenerateIR(irSampleWorkflow(), nil)
	require.NoError(t, err)

	data, err := json.Marshal(ir1)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"ir_version":"1.0"`)

	var ir2 IR
	require.NoError(t, json.Unmarshal(data, &ir2))
	assert.Equal(t, ir1.IRVersion, ir2.IRVersion)
	assert.Equal(t, ir1.Workflow.Name, ir2.Workflow.Name)
	require.Len(t, ir2.Inputs, len(ir1.Inputs))
	assert.Equal(t, ir1.Inputs[0].Name, ir2.Inputs[0].Name)
	require.Len(t, ir2.Steps, len(ir1.Steps))
	assert.Equal(t, ir1.Steps[0].Name, ir2.Steps[0].Name)
}

func TestIRUnmarshalMissingVersion(t *testing.T) {
	data := []byte(`{"workflow":{"name":"x"},"inputs":[]}`)
	var ir IR
	err := json.Unmarshal(data, &ir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing ir_version")
}

func TestIRUnmarshalUnsupportedVersion(t *testing.T) {
	data := []byte(`{"ir_version":"9.9","workflow":{"name":"x"},"inputs":[]}`)
	var ir IR
	err := json.Unmarshal(data, &ir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported ir_version")
}

func TestIRMarshalNil(t *testing.T) {
	var ir *IR
	data, err := ir.MarshalJSON()
	require.NoError(t, err)
	assert.Equal(t, "null", string(data))
}

func TestIRUnmarshalNilReceiver(t *testing.T) {
	var ir *IR
	err := ir.UnmarshalJSON([]byte(`{"ir_version":"1.0"}`))
	require.Error(t, err)
}

// --- isSupportedIRVersion --------------------------------------------------

func TestIsSupportedIRVersion(t *testing.T) {
	assert.True(t, isSupportedIRVersion("1.0"))
	assert.False(t, isSupportedIRVersion("2.0"))
	assert.False(t, isSupportedIRVersion(""))
}

func TestSupportedIRVersionsContains1_0(t *testing.T) {
	versions := supportedIRVersions()
	assert.Contains(t, versions, "1.0")
}
