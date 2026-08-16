package dsl

// typechecker_test.go — unit tests for the compile-time type checker.
//
// Coverage:
//   - valid workflow passes type checking
//   - unknown input type is reported
//   - input default type mismatch is reported
//   - step arg type mismatch (shell.command.cmd must be string)
//   - batch.strategy enum membership
//   - approval.level enum membership
//   - rollback.strategy validation
//   - strict vs lenient mode behaviour
//   - CheckStrict returns first error

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validTypedWorkflow returns a workflow that passes full type checking.
func validTypedWorkflow() *Workflow {
	return &Workflow{
		Meta: WorkflowMeta{Name: "typed-wf"},
		Inputs: []InputParam{
			{Name: "pkg", Type: "string", Default: "nginx"},
			{Name: "wait", Type: "duration", Default: "5m"},
		},
		Targets: []TargetGroup{
			{Name: "t", Type: "host", Query: "env=prod"},
		},
		Steps: []Step{
			{
				Name:   "exec",
				Module: "shell",
				Action: "exec",
				Args:   map[string]any{"cmd": "uname -r", "timeout": "30s"},
			},
		},
		Batches:  BatchConfig{Strategy: "percent", Steps: []int{1, 50, 100}},
		Approval: &ApprovalSpec{Level: "high"},
	}
}

// --- Valid workflow --------------------------------------------------------

func TestCheckValidWorkflow(t *testing.T) {
	c := NewTypeChecker(nil, "")
	errs := c.Check(validTypedWorkflow())
	assert.Empty(t, errs, "valid typed workflow should produce no type errors")
}

func TestCheckStrictValidWorkflow(t *testing.T) {
	c := NewTypeChecker(nil, "")
	require.NoError(t, c.CheckStrict(validTypedWorkflow()))
}

// --- Nil workflow ----------------------------------------------------------

func TestCheckNilWorkflow(t *testing.T) {
	c := NewTypeChecker(nil, "")
	errs := c.Check(nil)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Message, "nil")
}

// --- Input type resolution -------------------------------------------------

func TestCheckUnknownInputType(t *testing.T) {
	wf := validTypedWorkflow()
	wf.Inputs = append(wf.Inputs, InputParam{Name: "bad", Type: "nonexistent"})
	c := NewTypeChecker(nil, "")
	errs := c.Check(wf)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Message, "unknown type")
	assert.Contains(t, errs[0].Message, "nonexistent")
}

func TestCheckInputDefaultTypeMismatch(t *testing.T) {
	wf := validTypedWorkflow()
	// duration input with int default — not compatible.
	wf.Inputs = []InputParam{{Name: "wait", Type: "duration", Default: 30}}
	c := NewTypeChecker(nil, "")
	errs := c.Check(wf)
	require.NotEmpty(t, errs)
	found := false
	for _, e := range errs {
		if e.ExpectedType != nil && e.ExpectedType.String() == "duration" {
			found = true
		}
	}
	assert.True(t, found, "should report duration vs int mismatch")
}

func TestCheckInputDefaultIntToFloatOK(t *testing.T) {
	wf := validTypedWorkflow()
	// float input with int default — compatible (widening).
	wf.Inputs = []InputParam{{Name: "ratio", Type: "float", Default: 50}}
	c := NewTypeChecker(nil, "")
	errs := c.Check(wf)
	assert.Empty(t, errs)
}

// --- Step arg type checking ------------------------------------------------

func TestCheckStepArgTypeMismatch(t *testing.T) {
	wf := validTypedWorkflow()
	// shell.exec.timeout expects duration; provide a bool — not compatible.
	wf.Steps[0].Args = map[string]any{"cmd": "uname -r", "timeout": true}
	c := NewTypeChecker(nil, "")
	errs := c.Check(wf)
	require.NotEmpty(t, errs)
	found := false
	for _, e := range errs {
		if e.ExpectedType != nil && e.ExpectedType.String() == "duration" {
			found = true
		}
	}
	assert.True(t, found, "should report duration vs bool mismatch for timeout")
}

func TestCheckStepArgValidTypes(t *testing.T) {
	wf := validTypedWorkflow()
	// shell.exec with correct arg types.
	wf.Steps[0].Args = map[string]any{"cmd": "uname -r", "timeout": "30s"}
	c := NewTypeChecker(nil, "")
	errs := c.Check(wf)
	assert.Empty(t, errs)
}

func TestCheckUnknownActionSkipped(t *testing.T) {
	wf := validTypedWorkflow()
	wf.Steps[0].Module = "unknown"
	wf.Steps[0].Action = "thing"
	wf.Steps[0].Args = map[string]any{"anything": 123}
	c := NewTypeChecker(nil, "")
	errs := c.Check(wf)
	assert.Empty(t, errs, "unknown actions should be skipped")
}

// --- Batch strategy enum ---------------------------------------------------

func TestCheckBatchStrategyValid(t *testing.T) {
	for _, strategy := range []string{"percent", "fixed", "serial"} {
		wf := validTypedWorkflow()
		wf.Batches = BatchConfig{Strategy: strategy, Steps: []int{1, 100}}
		c := NewTypeChecker(nil, "")
		errs := c.Check(wf)
		assert.Empty(t, errs, "strategy %q should be valid", strategy)
	}
}

func TestCheckBatchStrategyInvalid(t *testing.T) {
	wf := validTypedWorkflow()
	wf.Batches = BatchConfig{Strategy: "random", Steps: []int{1}}
	c := NewTypeChecker(nil, "")
	errs := c.Check(wf)
	require.NotEmpty(t, errs)
	found := false
	for _, e := range errs {
		if e.ExpectedType != nil && e.ExpectedType.String() == "enum batch_strategy" {
			found = true
		}
	}
	assert.True(t, found, "should report invalid batch strategy")
}

func TestCheckBatchStrategyEmpty(t *testing.T) {
	wf := validTypedWorkflow()
	wf.Batches = BatchConfig{}
	c := NewTypeChecker(nil, "")
	errs := c.Check(wf)
	assert.Empty(t, errs, "empty strategy should be skipped")
}

// --- Approval level enum ---------------------------------------------------

func TestCheckApprovalLevelValid(t *testing.T) {
	for _, level := range []string{"standard", "high", "emergency"} {
		wf := validTypedWorkflow()
		wf.Approval = &ApprovalSpec{Level: level}
		c := NewTypeChecker(nil, "")
		errs := c.Check(wf)
		assert.Empty(t, errs, "level %q should be valid", level)
	}
}

func TestCheckApprovalLevelInvalid(t *testing.T) {
	wf := validTypedWorkflow()
	wf.Approval = &ApprovalSpec{Level: "god-mode"}
	c := NewTypeChecker(nil, "")
	errs := c.Check(wf)
	require.NotEmpty(t, errs)
}

func TestCheckApprovalNil(t *testing.T) {
	wf := validTypedWorkflow()
	wf.Approval = nil
	c := NewTypeChecker(nil, "")
	errs := c.Check(wf)
	assert.Empty(t, errs)
}

// --- Rollback strategy -----------------------------------------------------

func TestCheckRollbackStrategyValid(t *testing.T) {
	for _, strategy := range []string{"snapshot", "undo-action", "config-revert"} {
		wf := validTypedWorkflow()
		wf.Rollback = &RollbackSpec{Strategy: strategy}
		c := NewTypeChecker(nil, "")
		errs := c.Check(wf)
		assert.Empty(t, errs, "rollback strategy %q should be valid", strategy)
	}
}

func TestCheckRollbackStrategyInvalid(t *testing.T) {
	wf := validTypedWorkflow()
	wf.Rollback = &RollbackSpec{Strategy: "magic"}
	c := NewTypeChecker(nil, "")
	errs := c.Check(wf)
	require.NotEmpty(t, errs)
}

// --- Strict vs lenient mode ------------------------------------------------

func TestCheckWithModeLenient(t *testing.T) {
	wf := validTypedWorkflow()
	wf.Approval = &ApprovalSpec{Level: "invalid"}
	c := NewTypeChecker(nil, "")
	// Lenient mode still returns the errors (caller decides).
	errs := c.CheckWithMode(wf, ModeLenient)
	assert.NotEmpty(t, errs)
}

func TestCheckWithModeRestoresMode(t *testing.T) {
	c := NewTypeChecker(nil, "")
	c.SetMode(ModeStrict)
	_ = c.CheckWithMode(validTypedWorkflow(), ModeLenient)
	assert.Equal(t, ModeStrict, c.mode, "mode should be restored after CheckWithMode")
}

func TestCheckStrictReturnsFirstError(t *testing.T) {
	wf := validTypedWorkflow()
	// Two errors: invalid approval + invalid batch strategy.
	wf.Approval = &ApprovalSpec{Level: "bad"}
	wf.Batches = BatchConfig{Strategy: "bad"}
	c := NewTypeChecker(nil, "")
	err := c.CheckStrict(wf)
	require.Error(t, err)
	// Should be one of the errors.
	assert.NotEmpty(t, err.Error())
}

// --- Registry integration --------------------------------------------------

func TestCheckWithRegistryAlias(t *testing.T) {
	r := NewTypeRegistry()
	require.NoError(t, r.RegisterAlias("port", TypeInt{}))
	wf := validTypedWorkflow()
	wf.Inputs = []InputParam{{Name: "p", Type: "port", Default: 8080}}
	c := NewTypeChecker(r, "")
	errs := c.Check(wf)
	assert.Empty(t, errs, "alias 'port' should resolve to int")
}

func TestCheckWithRegistryEnum(t *testing.T) {
	r := NewTypeRegistry()
	require.NoError(t, r.RegisterEnum("status", []string{"ok", "warn", "crit"}))
	wf := validTypedWorkflow()
	wf.Inputs = []InputParam{{Name: "s", Type: "status", Default: "ok"}}
	c := NewTypeChecker(r, "")
	errs := c.Check(wf)
	assert.Empty(t, errs)
}

// --- TypeError formatting --------------------------------------------------

func TestTypeErrorErrorFormat(t *testing.T) {
	e := TypeError{
		File:         "wf.yaml",
		Line:         10,
		Column:       3,
		Message:      "type mismatch",
		ExpectedType: TypeString{},
		ActualType:   TypeInt{},
	}
	s := e.Error()
	assert.Contains(t, s, "wf.yaml:10:3")
	assert.Contains(t, s, "expected=string")
	assert.Contains(t, s, "actual=int")
}

func TestTypeErrorErrorNoLocation(t *testing.T) {
	e := TypeError{Message: "something wrong"}
	s := e.Error()
	assert.Contains(t, s, "<input>")
	assert.Contains(t, s, "something wrong")
}

// --- SetMode / Registry accessors ------------------------------------------

func TestTypeCheckerSetMode(t *testing.T) {
	c := NewTypeChecker(nil, "")
	c.SetMode(ModeLenient)
	assert.Equal(t, ModeLenient, c.mode)
}

func TestTypeCheckerRegistry(t *testing.T) {
	r := NewTypeRegistry()
	c := NewTypeChecker(r, "")
	assert.Same(t, r, c.Registry())
}

func TestTypeCheckerNilRegistryCreatesNew(t *testing.T) {
	c := NewTypeChecker(nil, "")
	assert.NotNil(t, c.Registry())
}
