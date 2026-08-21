package autoplanner

// auto_executor_test.go exercises the AutoExecutor implemented in
// auto_executor.go. The tests use a mock WorkflowExecutor and table-driven
// style with testify/require + assert, and target 85%+ coverage of the
// auto_executor.go file.
//
// The mock executor lets each test dial in the exact Execute / Rollback
// outcome pair so that every branch of the execute-with-rollback state
// machine is exercised: success, failure+rollback-success, failure+rollback-
// failure, and the nil-result fallbacks.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/recommend"
)

// --- mock WorkflowExecutor -------------------------------------------------

// mockWorkflowExecutor is a test double for WorkflowExecutor. Each call
// returns the configured result/error pair and records that it was invoked.
type mockWorkflowExecutor struct {
	executeResult  *ExecutionResult
	executeError   error
	rollbackResult *ExecutionResult
	rollbackError  error
	executeCalled  bool
	rollbackCalled bool
	executeCount   int
	rollbackCount  int
}

func (m *mockWorkflowExecutor) Execute(_ context.Context, _ *Workflow) (*ExecutionResult, error) {
	m.executeCalled = true
	m.executeCount++
	return m.executeResult, m.executeError
}

func (m *mockWorkflowExecutor) Rollback(_ context.Context, _ *Workflow) (*ExecutionResult, error) {
	m.rollbackCalled = true
	m.rollbackCount++
	return m.rollbackResult, m.rollbackError
}

// --- test fixtures ---------------------------------------------------------

// newExecWorkflow builds a minimal Workflow with the given risk level and a single
// batch containing one step. It is used by ExecuteWorkflow tests that need to
// bypass the planner.
func newExecWorkflow(risk recommend.RiskLevel) *Workflow {
	return &Workflow{
		ID:        "wf-test-001",
		Name:      "test-workflow",
		YAML:      "name: test\n",
		Batches:   []Batch{{ID: 1, Targets: []string{"host-1"}, Steps: []Step{{Name: "step-1"}}}},
		RiskLevel: risk,
		Target:    "host-1",
		CreatedAt: time.Now().UTC(),
	}
}

// newExecutor returns an AutoExecutor wired with the given mock executor and
// default planner / assessor / logger.
func newExecutor(m WorkflowExecutor) *AutoExecutor {
	return NewAutoExecutor(AutoExecutorConfig{Executor: m})
}

// --- NewAutoExecutor -------------------------------------------------------

func TestNewAutoExecutor_Defaults(t *testing.T) {
	e := newExecutor(&mockWorkflowExecutor{})
	require.NotNil(t, e)

	// The default planner / assessor / logger should be wired. We verify by
	// running a dry-run through Execute, which exercises the planner.
	rec := newRec(recommend.RiskLow)
	res, err := e.Execute(context.Background(), rec, ModeDryRun)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Success, "dry run should succeed")
	assert.Greater(t, res.StepsTotal, 0, "dry run should count steps")
}

func TestNewAutoExecutor_CustomConfig(t *testing.T) {
	planner := NewAutoPlanner(AutoPlannerConfig{})
	assessor := NewRiskAssessor()
	e := NewAutoExecutor(AutoExecutorConfig{
		Planner:  planner,
		Assessor: assessor,
		Executor: &mockWorkflowExecutor{},
	})
	require.NotNil(t, e)
	// Sanity: dry run still works with custom wiring.
	rec := newRec(recommend.RiskLow)
	res, err := e.Execute(context.Background(), rec, ModeDryRun)
	require.NoError(t, err)
	require.NotNil(t, res)
}

func TestNewAutoExecutor_NilExecutor(t *testing.T) {
	e := NewAutoExecutor(AutoExecutorConfig{}) // Executor is nil
	require.NotNil(t, e)

	wf := newExecWorkflow(recommend.RiskLow)
	res, err := e.ExecuteWorkflow(context.Background(), wf, ModeAuto)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNilExecutor)
	// A nil executor is detected before any execution, so no result is
	// produced.
	assert.Nil(t, res)
}

// --- Execute ---------------------------------------------------------------

func TestExecute_NilRecommendation(t *testing.T) {
	e := newExecutor(&mockWorkflowExecutor{})
	res, err := e.Execute(context.Background(), nil, ModeAuto)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrExecNilRecommendation)
	assert.Nil(t, res)
}

func TestExecute_PlanFailure(t *testing.T) {
	m := &mockWorkflowExecutor{}
	e := newExecutor(m)
	rec := newRec(recommend.RiskLow)
	rec.WorkflowDraft = "" // empty draft -> Plan returns ErrEmptyWorkflowDraft

	res, err := e.Execute(context.Background(), rec, ModeAuto)
	require.Error(t, err)
	assert.Nil(t, res)
	assert.False(t, m.executeCalled, "executor must not run when planning fails")
}

func TestExecute_DryRun(t *testing.T) {
	m := &mockWorkflowExecutor{}
	e := newExecutor(m)
	rec := newRec(recommend.RiskLow)

	res, err := e.Execute(context.Background(), rec, ModeDryRun)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.False(t, m.executeCalled, "dry run must not call executor")
	assert.False(t, m.rollbackCalled, "dry run must not call rollback")
	assert.Greater(t, res.StepsTotal, 0)
}

func TestExecute_AutoLowRisk(t *testing.T) {
	m := &mockWorkflowExecutor{
		executeResult: &ExecutionResult{Success: true, StepsTotal: 2, Duration: 5 * time.Second},
	}
	e := newExecutor(m)
	rec := newRec(recommend.RiskLow)

	res, err := e.Execute(context.Background(), rec, ModeAuto)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.True(t, m.executeCalled)
	assert.False(t, m.rollbackCalled, "rollback should not run on success")
}

func TestExecute_AutoLowRisk_NilResult(t *testing.T) {
	// Executor returns (nil, nil): the AutoExecutor should synthesise a
	// success result so that the caller never receives a nil pointer on a
	// nil error.
	m := &mockWorkflowExecutor{}
	e := newExecutor(m)
	rec := newRec(recommend.RiskLow)

	res, err := e.Execute(context.Background(), rec, ModeAuto)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.NotEmpty(t, res.WorkflowID)
}

func TestExecute_AutoMediumRisk(t *testing.T) {
	m := &mockWorkflowExecutor{}
	e := newExecutor(m)
	rec := newRec(recommend.RiskMedium)

	res, err := e.Execute(context.Background(), rec, ModeAuto)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRequiresApproval)
	assert.False(t, m.executeCalled, "executor must not run when approval required")
	require.NotNil(t, res)
	assert.False(t, res.Success)
}

func TestExecute_AutoHighRisk(t *testing.T) {
	m := &mockWorkflowExecutor{}
	e := newExecutor(m)
	rec := newRec(recommend.RiskHigh)

	res, err := e.Execute(context.Background(), rec, ModeAuto)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRequiresApproval)
	assert.False(t, m.executeCalled, "executor must not run when approval required")
	require.NotNil(t, res)
	assert.False(t, res.Success)
}

func TestExecute_AutoCriticalRisk(t *testing.T) {
	m := &mockWorkflowExecutor{}
	e := newExecutor(m)
	rec := newRec(recommend.RiskCritical)

	res, err := e.Execute(context.Background(), rec, ModeAuto)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRequiresApproval)
	assert.False(t, m.executeCalled)
	require.NotNil(t, res)
	assert.False(t, res.Success)
}

func TestExecute_AutoExecutionFailure(t *testing.T) {
	m := &mockWorkflowExecutor{
		executeError:   errors.New("exec boom"),
		rollbackResult: &ExecutionResult{Success: true, RollbackUsed: true},
	}
	e := newExecutor(m)
	rec := newRec(recommend.RiskLow)

	res, err := e.Execute(context.Background(), rec, ModeAuto)
	require.Error(t, err)
	assert.True(t, m.executeCalled)
	assert.True(t, m.rollbackCalled, "rollback should be attempted on failure")
	_ = res
}

func TestExecute_AutoRollbackSuccess(t *testing.T) {
	m := &mockWorkflowExecutor{
		executeError:   errors.New("exec boom"),
		rollbackResult: &ExecutionResult{Success: true, RollbackUsed: true},
	}
	e := newExecutor(m)
	rec := newRec(recommend.RiskLow)

	res, err := e.Execute(context.Background(), rec, ModeAuto)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrExecutionFailed)
	require.NotNil(t, res)
	assert.True(t, res.RollbackUsed, "result should report rollback was used")
	assert.False(t, res.Success)
}

func TestExecute_AutoRollbackFailure(t *testing.T) {
	m := &mockWorkflowExecutor{
		executeError:  errors.New("exec boom"),
		rollbackError: errors.New("rollback boom"),
	}
	e := newExecutor(m)
	rec := newRec(recommend.RiskLow)

	res, err := e.Execute(context.Background(), rec, ModeAuto)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRollbackFailed)
	require.NotNil(t, res)
	assert.True(t, res.RollbackUsed)
	assert.False(t, res.Success)
}

func TestExecute_ForceMode(t *testing.T) {
	m := &mockWorkflowExecutor{
		executeResult: &ExecutionResult{Success: true, StepsTotal: 2},
	}
	e := newExecutor(m)
	rec := newRec(recommend.RiskHigh) // high risk, but force bypasses approval

	res, err := e.Execute(context.Background(), rec, ModeForce)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.True(t, m.executeCalled)
	assert.False(t, m.rollbackCalled, "rollback should not run on success")
}

func TestExecute_ForceMode_Failure(t *testing.T) {
	m := &mockWorkflowExecutor{
		executeError:   errors.New("exec boom"),
		rollbackResult: &ExecutionResult{Success: true},
	}
	e := newExecutor(m)
	rec := newRec(recommend.RiskHigh)

	res, err := e.Execute(context.Background(), rec, ModeForce)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrExecutionFailed)
	assert.True(t, m.rollbackCalled, "force mode should still rollback on failure")
	require.NotNil(t, res)
	assert.True(t, res.RollbackUsed)
}

func TestExecute_ForceMode_RollbackFailure(t *testing.T) {
	m := &mockWorkflowExecutor{
		executeError:  errors.New("exec boom"),
		rollbackError: errors.New("rollback boom"),
	}
	e := newExecutor(m)
	rec := newRec(recommend.RiskHigh)

	res, err := e.Execute(context.Background(), rec, ModeForce)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRollbackFailed)
	require.NotNil(t, res)
	assert.True(t, res.RollbackUsed)
}

// --- ExecuteWorkflow -------------------------------------------------------

func TestExecuteWorkflow_NilWorkflow(t *testing.T) {
	e := newExecutor(&mockWorkflowExecutor{})
	res, err := e.ExecuteWorkflow(context.Background(), nil, ModeAuto)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrExecNilWorkflow)
	assert.Nil(t, res)
}

func TestExecuteWorkflow_DryRun(t *testing.T) {
	m := &mockWorkflowExecutor{}
	e := newExecutor(m)
	wf := newExecWorkflow(recommend.RiskHigh)

	res, err := e.ExecuteWorkflow(context.Background(), wf, ModeDryRun)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.False(t, m.executeCalled)
	assert.Equal(t, 1, res.StepsTotal, "newExecWorkflow has one step")
	require.Len(t, res.BatchResults, 1)
	assert.True(t, res.BatchResults[0].Success)
}

func TestExecuteWorkflow_UnknownMode(t *testing.T) {
	m := &mockWorkflowExecutor{}
	e := newExecutor(m)
	wf := newExecWorkflow(recommend.RiskLow)

	res, err := e.ExecuteWorkflow(context.Background(), wf, ExecutionMode(99))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownExecutionMode)
	assert.Nil(t, res)
	assert.False(t, m.executeCalled)
}

func TestExecuteWorkflow_NilContext(t *testing.T) {
	// A nil context is replaced with context.Background so the call should
	// not panic.
	m := &mockWorkflowExecutor{
		executeResult: &ExecutionResult{Success: true},
	}
	e := newExecutor(m)
	wf := newExecWorkflow(recommend.RiskLow)

	res, err := e.ExecuteWorkflow(nil, wf, ModeAuto)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Success)
}

// --- ShouldAutoExecute -----------------------------------------------------

func TestShouldAutoExecute_LowRisk(t *testing.T) {
	e := newExecutor(&mockWorkflowExecutor{})
	rec := newRec(recommend.RiskLow)
	assert.True(t, e.ShouldAutoExecute(rec))
}

func TestShouldAutoExecute_MediumRisk(t *testing.T) {
	e := newExecutor(&mockWorkflowExecutor{})
	rec := newRec(recommend.RiskMedium)
	assert.False(t, e.ShouldAutoExecute(rec))
}

func TestShouldAutoExecute_HighRisk(t *testing.T) {
	e := newExecutor(&mockWorkflowExecutor{})
	rec := newRec(recommend.RiskHigh)
	assert.False(t, e.ShouldAutoExecute(rec))
}

func TestShouldAutoExecute_CriticalRisk(t *testing.T) {
	e := newExecutor(&mockWorkflowExecutor{})
	rec := newRec(recommend.RiskCritical)
	assert.False(t, e.ShouldAutoExecute(rec))
}

func TestShouldAutoExecute_Nil(t *testing.T) {
	e := newExecutor(&mockWorkflowExecutor{})
	assert.False(t, e.ShouldAutoExecute(nil))
}

// --- helpers ---------------------------------------------------------------

func TestCountSteps(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		assert.Equal(t, 0, countSteps(&Workflow{}))
	})
	t.Run("single batch single step", func(t *testing.T) {
		assert.Equal(t, 1, countSteps(newExecWorkflow(recommend.RiskLow)))
	})
	t.Run("multi batch", func(t *testing.T) {
		wf := &Workflow{
			Batches: []Batch{
				{ID: 1, Steps: []Step{{Name: "a"}, {Name: "b"}}},
				{ID: 2, Steps: []Step{{Name: "c"}}},
			},
		}
		assert.Equal(t, 3, countSteps(wf))
	})
}

func TestBuildBatchResults(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		wf := &Workflow{}
		assert.Nil(t, buildBatchResults(wf, true))
	})
	t.Run("success", func(t *testing.T) {
		wf := newExecWorkflow(recommend.RiskLow)
		out := buildBatchResults(wf, true)
		require.Len(t, out, 1)
		assert.True(t, out[0].Success)
		assert.Nil(t, out[0].FailedTargets)
		assert.Empty(t, out[0].Error)
	})
	t.Run("failure", func(t *testing.T) {
		wf := newExecWorkflow(recommend.RiskLow)
		out := buildBatchResults(wf, false)
		require.Len(t, out, 1)
		assert.False(t, out[0].Success)
		assert.NotNil(t, out[0].FailedTargets)
		assert.NotEmpty(t, out[0].Error)
	})
}
