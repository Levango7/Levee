package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stepNames is the assertion view: the order steps came back in.
func stepNames(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

func step(name string, deps ...string) Step {
	return Step{Name: name, Module: "shell", Action: "exec", DependsOn: deps}
}

func TestOrderStepsByDependsOnReordersOutOfOrder(t *testing.T) {
	// The bug this file closes: b declares it needs a but is written first;
	// serial execution used to run b before a and silently violate the edge.
	ordered, problems := OrderStepsByDependsOn([]Step{
		step("b", "a"),
		step("a"),
		step("c", "b"),
	})
	require.Empty(t, problems)
	assert.Equal(t, []string{"a", "b", "c"}, stepNames(ordered))
}

func TestOrderStepsByDependsOnIsStableForIndependentSteps(t *testing.T) {
	// Independent steps keep declaration order — the reason every shipped
	// example (already written in dependency order) is unaffected.
	ordered, problems := OrderStepsByDependsOn([]Step{
		step("x"),
		step("y", "x"),
		step("z"), // independent of y, declared after it: stays after y
	})
	require.Empty(t, problems)
	assert.Equal(t, []string{"x", "y", "z"}, stepNames(ordered))
}

func TestOrderStepsByDependsOnNoDependenciesUnchanged(t *testing.T) {
	ordered, problems := OrderStepsByDependsOn([]Step{step("a"), step("b"), step("c")})
	require.Empty(t, problems)
	assert.Equal(t, []string{"a", "b", "c"}, stepNames(ordered))
}

func TestOrderStepsByDependsOnEmpty(t *testing.T) {
	ordered, problems := OrderStepsByDependsOn(nil)
	assert.Nil(t, ordered)
	assert.Empty(t, problems)
}

func TestOrderStepsByDependsOnUnknownReference(t *testing.T) {
	// A typo'd dependency must be an error, not a silent no-op.
	_, problems := OrderStepsByDependsOn([]Step{step("a", "nope"), step("b")})
	require.Len(t, problems, 1)
	assert.Equal(t, codeDependencyUnknown, problems[0].Code)
	assert.Contains(t, problems[0].Message, `"nope"`)
}

func TestOrderStepsByDependsOnCycle(t *testing.T) {
	_, problems := OrderStepsByDependsOn([]Step{
		step("a", "b"),
		step("b", "a"),
	})
	require.NotEmpty(t, problems)
	assert.Equal(t, codeDependencyCycle, problems[0].Code)
}

func TestOrderStepsByDependsOnSelfDependency(t *testing.T) {
	_, problems := OrderStepsByDependsOn([]Step{step("a", "a")})
	require.Len(t, problems, 1)
	assert.Equal(t, codeDependencyCycle, problems[0].Code)
	assert.Contains(t, problems[0].Message, "depends on itself")
}

func TestOrderStepsByDependsOnDuplicateEdgeIsOneEdge(t *testing.T) {
	// depends_on: [a, a] is one edge, not indegree 2 (which would never settle
	// because placement only decrements once).
	ordered, problems := OrderStepsByDependsOn([]Step{step("b", "a", "a"), step("a")})
	require.Empty(t, problems)
	assert.Equal(t, []string{"a", "b"}, stepNames(ordered))
}

func TestOrderStepsByDependsOnStillReturnsTotalOrderOnCycle(t *testing.T) {
	// A caller that only wants a preview still gets every step once.
	ordered, problems := OrderStepsByDependsOn([]Step{
		step("a", "b"),
		step("b", "a"),
		step("c"),
	})
	require.NotEmpty(t, problems)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, stepNames(ordered))
}

func TestValidatorReportsDependsOnProblems(t *testing.T) {
	// The compile path (`levee compile`) must surface the same problems the
	// generator fails closed on.
	wf := &Workflow{
		Meta:    WorkflowMeta{Name: "wf"},
		Targets: []TargetGroup{{Name: "all", Hosts: []string{"h1"}}},
		Steps: []Step{
			step("b", "a"),
			step("a", "ghost"),
		},
	}
	v := NewValidator()
	errs := v.Validate(wf)
	codes := map[string]bool{}
	for _, e := range errs {
		codes[e.Code] = true
	}
	assert.True(t, codes[codeDependencyUnknown], "unknown depends_on reference must be reported at compile time")
}
