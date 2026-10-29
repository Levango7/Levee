// inputs_test.go pins LEVEELang input resolution (dsl.ResolveInputs): values
// are taken from the run's parameters (or the declared default), checked
// against the declared type, and substituted into every reference — and a
// reference can never survive as literal text for a module to execute.
package dsl

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withInputs returns a minimal workflow declaring the given inputs and one
// shell step carrying args (the reference surface these tests exercise).
func withInputs(inputs []InputParam, args map[string]any) *Workflow {
	wf := validWorkflow()
	wf.Inputs = inputs
	wf.Steps = []Step{{Name: "s1", Module: "shell", Action: "exec", Args: args}}
	return wf
}

func TestResolveInputsSubstitutesSuppliedValue(t *testing.T) {
	wf := withInputs(
		[]InputParam{{Name: "table", Type: "string"}},
		map[string]any{"cmd": "DESCRIBE {{input.table}}"},
	)
	got, err := ResolveInputs(wf, map[string]string{"table": "orders"})
	require.NoError(t, err)
	assert.Equal(t, "DESCRIBE orders", got.Steps[0].Args["cmd"])
}

func TestResolveInputsUsesDeclaredDefault(t *testing.T) {
	wf := withInputs(
		[]InputParam{{Name: "table", Type: "string", Default: "orders"}},
		map[string]any{"cmd": "DESCRIBE {{input.table}}"},
	)
	got, err := ResolveInputs(wf, nil)
	require.NoError(t, err)
	assert.Equal(t, "DESCRIBE orders", got.Steps[0].Args["cmd"])
}

func TestResolveInputsAllowsWhitespaceInReference(t *testing.T) {
	// The spec writes both `{{input.x}}` and `{{ input.table }}`.
	wf := withInputs(
		[]InputParam{{Name: "table", Type: "string"}},
		map[string]any{"cmd": "DESCRIBE {{ input.table }}"},
	)
	got, err := ResolveInputs(wf, map[string]string{"table": "orders"})
	require.NoError(t, err)
	assert.Equal(t, "DESCRIBE orders", got.Steps[0].Args["cmd"])
}

func TestResolveInputsMissingRequired(t *testing.T) {
	wf := withInputs(
		[]InputParam{{Name: "table", Type: "string"}},
		map[string]any{"cmd": "DESCRIBE {{input.table}}"},
	)
	_, err := ResolveInputs(wf, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInputMissing), "want ErrInputMissing, got %v", err)
	assert.Contains(t, err.Error(), "table")
}

func TestResolveInputsUndeclaredReference(t *testing.T) {
	// No declaration at all, but the args reference one: this is the typo that
	// must not become a literal command.
	wf := withInputs(nil, map[string]any{"cmd": "DESCRIBE {{input.typo}}"})
	_, err := ResolveInputs(wf, map[string]string{"table": "orders"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInputUndeclared), "want ErrInputUndeclared, got %v", err)
	assert.Contains(t, err.Error(), "typo")
}

func TestResolveInputsTypeMismatch(t *testing.T) {
	cases := []struct {
		name  string
		typ   string
		value string
	}{
		{"int", "int", "not-a-number"},
		{"bool", "bool", "perhaps"},
		{"duration", "duration", "soon"},
		{"percent", "percent", "140"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := withInputs(
				[]InputParam{{Name: "x", Type: tc.typ}},
				map[string]any{"cmd": "echo {{input.x}}"},
			)
			_, err := ResolveInputs(wf, map[string]string{"x": tc.value})
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInputType), "want ErrInputType, got %v", err)
		})
	}
}

func TestResolveInputsAcceptsValidScalars(t *testing.T) {
	wf := withInputs(
		[]InputParam{
			{Name: "n", Type: "int"},
			{Name: "flag", Type: "bool"},
			{Name: "wait", Type: "duration"},
		},
		map[string]any{"cmd": "sleep {{input.wait}} # {{input.n}} {{input.flag}}"},
	)
	got, err := ResolveInputs(wf, map[string]string{"n": "3", "flag": "true", "wait": "5m"})
	require.NoError(t, err)
	assert.Equal(t, "sleep 5m # 3 true", got.Steps[0].Args["cmd"])
}

func TestResolveInputsRefusesCompositeValues(t *testing.T) {
	// A map/list value needs a wire format the language has not chosen; better
	// to refuse than to substitute a string that looks like a map.
	wf := withInputs(
		[]InputParam{{Name: "m", Type: "map<string,string>"}},
		map[string]any{"cmd": "echo {{input.m}}"},
	)
	_, err := ResolveInputs(wf, map[string]string{"m": `{"a":"b"}`})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInputType), "want ErrInputType, got %v", err)
}

func TestResolveInputsIsNoopWithoutInputs(t *testing.T) {
	wf := withInputs(nil, map[string]any{"cmd": "echo hi"})
	got, err := ResolveInputs(wf, map[string]string{"unrelated": "x"})
	require.NoError(t, err)
	assert.Same(t, wf, got, "a workflow with no inputs must be returned untouched")
	assert.Equal(t, "echo hi", got.Steps[0].Args["cmd"])
}

// TestResolveInputsCoversEveryReferenceSurface pins that substitution reaches
// the SAME places (and more) than the reference walk can see: nested args,
// rollback step args, gate params and snapshot paths. A surface the walk sees
// but substitution misses is exactly the silent-literal bug this feature
// removes.
func TestResolveInputsCoversEveryReferenceSurface(t *testing.T) {
	wf := validWorkflow()
	wf.Inputs = []InputParam{{Name: "table", Type: "string"}}
	wf.Steps = []Step{{
		Name: "s1", Module: "shell", Action: "exec",
		Args: map[string]any{
			"cmd":  "DESCRIBE {{input.table}}",
			"env":  map[string]any{"TARGET": "t-{{input.table}}"},
			"list": []any{"{{input.table}}", "literal"},
		},
		Rollback: &RollbackSpec{Steps: []Step{{
			Name: "undo", Module: "shell", Action: "exec",
			Args: map[string]any{"cmd": "DROP IF EXISTS {{input.table}}"},
		}}},
		Gate: &GateSpec{Post: []GateCheck{{
			Type: "cmd", Command: "check {{input.table}}",
			Params: map[string]any{"label": "g-{{input.table}}"},
		}}},
	}}
	wf.Gate = &GateSpec{Pre: []GateCheck{{Type: "cmd", Command: "pre {{input.table}}"}}}
	wf.Batches.Gate = &GateSpec{Batch: []GateCheck{{Type: "cmd", Command: "batch {{input.table}}"}}}
	wf.Rollback = &RollbackSpec{SnapshotPaths: []string{"/etc/{{input.table}}.conf"}}

	got, err := ResolveInputs(wf, map[string]string{"table": "orders"})
	require.NoError(t, err)

	assert.Equal(t, "DESCRIBE orders", got.Steps[0].Args["cmd"])
	assert.Equal(t, map[string]any{"TARGET": "t-orders"}, got.Steps[0].Args["env"])
	assert.Equal(t, []any{"orders", "literal"}, got.Steps[0].Args["list"])
	assert.Equal(t, "DROP IF EXISTS orders", got.Steps[0].Rollback.Steps[0].Args["cmd"])
	assert.Equal(t, "check orders", got.Steps[0].Gate.Post[0].Command)
	assert.Equal(t, map[string]any{"label": "g-orders"}, got.Steps[0].Gate.Post[0].Params)
	assert.Equal(t, "pre orders", got.Gate.Pre[0].Command)
	assert.Equal(t, "batch orders", got.Batches.Gate.Batch[0].Command)
	assert.Equal(t, []string{"/etc/orders.conf"}, got.Rollback.SnapshotPaths)
}

// TestResolveInputsRefusesMalformedReference pins the post-condition: after
// substitution no brace expression mentioning `input.` may remain, so a
// malformed name cannot slip through as literal text.
func TestResolveInputsRefusesMalformedReference(t *testing.T) {
	wf := withInputs(
		[]InputParam{{Name: "table", Type: "string"}},
		map[string]any{"cmd": "echo {{input.}}"},
	)
	_, err := ResolveInputs(wf, map[string]string{"table": "orders"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInputUndeclared), "want ErrInputUndeclared, got %v", err)
}

// TestResolveInputsLeavesUnrelatedBraceTextAlone guards the opposite direction:
// a shell command that legitimately contains braces (or the template
// placeholder `{{.name}}`, a DIFFERENT layer) must not be disturbed.
func TestResolveInputsLeavesUnrelatedBraceTextAlone(t *testing.T) {
	wf := withInputs(
		[]InputParam{{Name: "n", Type: "int"}},
		map[string]any{"cmd": "awk '{print $1}' /tmp/f && echo {{input.n}}"},
	)
	got, err := ResolveInputs(wf, map[string]string{"n": "1"})
	require.NoError(t, err)
	assert.Equal(t, "awk '{print $1}' /tmp/f && echo 1", got.Steps[0].Args["cmd"])
}
