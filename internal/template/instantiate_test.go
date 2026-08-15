package template

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Helpers ----------------------------------------------------------------

// newTmpl builds a minimal Template with the given content and parameters.
func newTmpl(name, content string, params ...TemplateParam) *Template {
	return &Template{
		ID:         "tmpl-test",
		Name:       name,
		Content:    content,
		Parameters: params,
	}
}

// --- Instantiate: basic functionality ---------------------------------------

// TestInstantiate_FillsPlaceholders verifies that Instantiate replaces
// declared placeholders in the template content with supplied values.
func TestInstantiate_FillsPlaceholders(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"name: deploy-{{.target}}\nhost: {{.target}}",
		TemplateParam{Name: "target", Type: "string", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{"target": "prod"})
	require.NoError(t, err)
	assert.Equal(t, "deploy", res.TemplateName)
	assert.Equal(t, "name: deploy-prod\nhost: prod", res.Content)
	assert.Equal(t, map[string]string{"target": "prod"}, res.Params)
	assert.Empty(t, res.Missing)
}

// --- Instantiate: required parameter missing --------------------------------

// TestInstantiate_RequiredParamMissing verifies that a missing required
// parameter with no default yields ErrRequiredParamMissing and the
// missing names are reported in the result.
func TestInstantiate_RequiredParamMissing(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"target: {{.target}}",
		TemplateParam{Name: "target", Type: "string", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRequiredParamMissing),
		"want ErrRequiredParamMissing, got %v", err)
	// The partial result should carry the missing names.
	require.NotNil(t, res)
	assert.Equal(t, []string{"target"}, res.Missing)
}

// TestInstantiate_RequiredParamMissing_Multiple verifies that all missing
// required parameters are reported.
func TestInstantiate_RequiredParamMissing_Multiple(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"a: {{.a}}\nb: {{.b}}",
		TemplateParam{Name: "a", Required: true},
		TemplateParam{Name: "b", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRequiredParamMissing))
	require.NotNil(t, res)
	assert.ElementsMatch(t, []string{"a", "b"}, res.Missing)
}

// --- Instantiate: default value ---------------------------------------------

// TestInstantiate_DefaultValue verifies that an omitted parameter falls
// back to its declared Default.
func TestInstantiate_DefaultValue(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"env: {{.env}}",
		TemplateParam{Name: "env", Type: "string", Default: "dev"},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "env: dev", res.Content)
	assert.Equal(t, map[string]string{"env": "dev"}, res.Params)
}

// TestInstantiate_DefaultValue_Overridden verifies that a caller-supplied
// value takes precedence over the default.
func TestInstantiate_DefaultValue_Overridden(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"env: {{.env}}",
		TemplateParam{Name: "env", Type: "string", Default: "dev"},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{"env": "prod"})
	require.NoError(t, err)
	assert.Equal(t, "env: prod", res.Content)
	assert.Equal(t, map[string]string{"env": "prod"}, res.Params)
}

// --- Instantiate: type validation -------------------------------------------

// TestInstantiate_TypeInt_Valid verifies that valid integer values pass.
func TestInstantiate_TypeInt_Valid(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"count: {{.count}}",
		TemplateParam{Name: "count", Type: "int", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{"count": "42"})
	require.NoError(t, err)
	assert.Equal(t, "count: 42", res.Content)
}

// TestInstantiate_TypeInt_Invalid verifies that a non-integer value for
// an int-typed parameter returns ErrParamTypeMismatch.
func TestInstantiate_TypeInt_Invalid(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"count: {{.count}}",
		TemplateParam{Name: "count", Type: "int", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{"count": "not-a-number"})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.True(t, errors.Is(err, ErrParamTypeMismatch),
		"want ErrParamTypeMismatch, got %v", err)
}

// TestInstantiate_TypeBool_Valid verifies that accepted boolean spellings
// pass validation.
func TestInstantiate_TypeBool_Valid(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"flag: {{.flag}}",
		TemplateParam{Name: "flag", Type: "bool", Required: true},
	)

	for _, v := range []string{"true", "false", "yes", "no", "1", "0"} {
		res, err := inst.Instantiate(tmpl, map[string]string{"flag": v})
		require.NoError(t, err, "value %q should be accepted as bool", v)
		assert.Equal(t, "flag: "+v, res.Content)
	}
}

// TestInstantiate_TypeBool_Invalid verifies that a non-boolean value for
// a bool-typed parameter returns ErrParamTypeMismatch.
func TestInstantiate_TypeBool_Invalid(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"flag: {{.flag}}",
		TemplateParam{Name: "flag", Type: "bool", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{"flag": "maybe"})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.True(t, errors.Is(err, ErrParamTypeMismatch))
}

// TestInstantiate_TypeString_AnyValue verifies that a string-typed
// parameter accepts any value, including values that would be invalid
// for other types.
func TestInstantiate_TypeString_AnyValue(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"note: {{.note}}",
		TemplateParam{Name: "note", Type: "string", Required: true},
	)

	for _, v := range []string{"hello", "42", "true", "a,b,c", "", "with spaces"} {
		res, err := inst.Instantiate(tmpl, map[string]string{"note": v})
		require.NoError(t, err, "value %q should be accepted as string", v)
		assert.Equal(t, "note: "+v, res.Content)
	}
}

// TestInstantiate_TypeList_Valid verifies that a list-typed parameter
// accepts comma-separated values.
func TestInstantiate_TypeList_Valid(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"hosts: {{.hosts}}",
		TemplateParam{Name: "hosts", Type: "list", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{"hosts": "h1,h2,h3"})
	require.NoError(t, err)
	assert.Equal(t, "hosts: h1,h2,h3", res.Content)
}

// TestInstantiate_TypeUnknown_TreatedAsString verifies that an unknown
// type is treated as "string" and accepts any value.
func TestInstantiate_TypeUnknown_TreatedAsString(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"x: {{.x}}",
		TemplateParam{Name: "x", Type: "weird-type", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{"x": "anything"})
	require.NoError(t, err)
	assert.Equal(t, "x: anything", res.Content)
}

// TestInstantiate_TypeEmpty_TreatedAsString verifies that an empty type
// is treated as "string".
func TestInstantiate_TypeEmpty_TreatedAsString(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"x: {{.x}}",
		TemplateParam{Name: "x", Type: "", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{"x": "anything"})
	require.NoError(t, err)
	assert.Equal(t, "x: anything", res.Content)
}

// --- Instantiate: unknown parameter -----------------------------------------

// TestInstantiate_UnknownParam verifies that supplying a parameter not
// declared by the template returns ErrUnknownParam.
func TestInstantiate_UnknownParam(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"target: {{.target}}",
		TemplateParam{Name: "target", Type: "string", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{
		"target": "prod",
		"bogus":  "value",
	})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.True(t, errors.Is(err, ErrUnknownParam),
		"want ErrUnknownParam, got %v", err)
}

// --- Instantiate: multiple parameters ---------------------------------------

// TestInstantiate_MultipleParams verifies that several placeholders are
// substituted in a single pass.
func TestInstantiate_MultipleParams(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"name: {{.name}}\nenv: {{.env}}\nreplicas: {{.replicas}}",
		TemplateParam{Name: "name", Type: "string", Required: true},
		TemplateParam{Name: "env", Type: "string", Default: "dev"},
		TemplateParam{Name: "replicas", Type: "int", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{
		"name":     "api",
		"replicas": "3",
	})
	require.NoError(t, err)
	assert.Equal(t, "name: api\nenv: dev\nreplicas: 3", res.Content)
	assert.Equal(t, map[string]string{
		"name":     "api",
		"env":      "dev",
		"replicas": "3",
	}, res.Params)
}

// --- Instantiate: no-parameter template -------------------------------------

// TestInstantiate_NoParams verifies that a template without placeholders
// is returned verbatim.
func TestInstantiate_NoParams(t *testing.T) {
	inst := NewInstantiator()
	content := "name: static-workflow\nsteps: []\n"
	tmpl := newTmpl("static", content)

	res, err := inst.Instantiate(tmpl, map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, content, res.Content)
	assert.Empty(t, res.Params)
}

// --- Instantiate: nil template ----------------------------------------------

// TestInstantiate_NilTemplate verifies that a nil template produces an
// error without panicking.
func TestInstantiate_NilTemplate(t *testing.T) {
	inst := NewInstantiator()
	res, err := inst.Instantiate(nil, map[string]string{})
	require.Error(t, err)
	assert.Nil(t, res)
}

// --- Instantiate: empty content ---------------------------------------------

// TestInstantiate_EmptyContent verifies that an empty content field
// returns ErrEmptyContent.
func TestInstantiate_EmptyContent(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("empty", "")

	res, err := inst.Instantiate(tmpl, map[string]string{})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.True(t, errors.Is(err, ErrEmptyContent),
		"want ErrEmptyContent, got %v", err)
}

// --- Instantiate: optional, no default --------------------------------------

// TestInstantiate_OptionalNoDefault verifies that an optional parameter
// with no default leaves its placeholder in the content unchanged and is
// absent from the Params map.
func TestInstantiate_OptionalNoDefault(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"target: {{.target}}\nnote: {{.note}}",
		TemplateParam{Name: "target", Type: "string", Required: true},
		TemplateParam{Name: "note", Type: "string"}, // optional, no default
	)

	res, err := inst.Instantiate(tmpl, map[string]string{"target": "prod"})
	require.NoError(t, err)
	// The optional placeholder remains in the content.
	assert.Equal(t, "target: prod\nnote: {{.note}}", res.Content)
	// Only the resolved parameter appears in Params.
	assert.Equal(t, map[string]string{"target": "prod"}, res.Params)
}

// --- Instantiate: nil params ------------------------------------------------

// TestInstantiate_NilParams verifies that a nil params map is acceptable
// when all parameters have defaults or are optional.
func TestInstantiate_NilParams(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"env: {{.env}}",
		TemplateParam{Name: "env", Type: "string", Default: "dev"},
	)

	res, err := inst.Instantiate(tmpl, nil)
	require.NoError(t, err)
	assert.Equal(t, "env: dev", res.Content)
}

// --- Instantiate: repeated placeholder --------------------------------------

// TestInstantiate_RepeatedPlaceholder verifies that a placeholder that
// occurs multiple times in the content is replaced in all positions.
func TestInstantiate_RepeatedPlaceholder(t *testing.T) {
	inst := NewInstantiator()
	tmpl := newTmpl("deploy",
		"{{.x}}-{{.x}}-{{.x}}",
		TemplateParam{Name: "x", Type: "string", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{"x": "A"})
	require.NoError(t, err)
	assert.Equal(t, "A-A-A", res.Content)
}

// --- Instantiate: zero-value Instantiator -----------------------------------

// TestInstantiate_ZeroValueInstantiator verifies that the zero-value
// Instantiator works without calling NewInstantiator.
func TestInstantiate_ZeroValueInstantiator(t *testing.T) {
	var inst Instantiator
	tmpl := newTmpl("deploy",
		"x: {{.x}}",
		TemplateParam{Name: "x", Type: "string", Required: true},
	)

	res, err := inst.Instantiate(tmpl, map[string]string{"x": "1"})
	require.NoError(t, err)
	assert.Equal(t, "x: 1", res.Content)
}

// --- ParseParams ------------------------------------------------------------

// TestParseParams_Basic verifies parsing of a simple "k=v,k2=v2" string.
func TestParseParams_Basic(t *testing.T) {
	m, err := ParseParams("k=v,k2=v2")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"k": "v", "k2": "v2"}, m)
}

// TestParseParams_Empty verifies that an empty input yields an empty
// (non-nil) map.
func TestParseParams_Empty(t *testing.T) {
	m, err := ParseParams("")
	require.NoError(t, err)
	assert.NotNil(t, m)
	assert.Empty(t, m)
}

// TestParseParams_NoEquals verifies that a pair without '=' returns an
// error.
func TestParseParams_NoEquals(t *testing.T) {
	_, err := ParseParams("just-a-key")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "missing '='"),
		"error should mention missing '=', got %v", err)
}

// TestParseParams_TrimsWhitespace verifies that keys and values are
// trimmed of surrounding whitespace.
func TestParseParams_TrimsWhitespace(t *testing.T) {
	m, err := ParseParams("  key  =  value  , k2 = v2 ")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"key": "value", "k2": "v2"}, m)
}

// TestParseParams_EscapedComma verifies that "\," inside a value is
// preserved as a literal comma rather than treated as a pair separator.
func TestParseParams_EscapedComma(t *testing.T) {
	m, err := ParseParams(`tags=a\,b,c=d`)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"tags": "a,b", "c": "d"}, m)
}

// TestParseParams_EscapedComma_Multiple verifies that multiple escaped
// commas in a single value are all preserved.
func TestParseParams_EscapedComma_Multiple(t *testing.T) {
	m, err := ParseParams(`hosts=h1\,h2\,h3`)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"hosts": "h1,h2,h3"}, m)
}

// TestParseParams_SinglePair verifies parsing of a single pair.
func TestParseParams_SinglePair(t *testing.T) {
	m, err := ParseParams("key=value")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"key": "value"}, m)
}

// TestParseParams_EmptyValue verifies that a pair with an empty value
// (e.g. "key=") is accepted.
func TestParseParams_EmptyValue(t *testing.T) {
	m, err := ParseParams("key=")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"key": ""}, m)
}

// TestParseParams_EmptyKey verifies that a pair with an empty key (e.g.
// "=value") returns an error.
func TestParseParams_EmptyKey(t *testing.T) {
	_, err := ParseParams("=value")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "empty key"),
		"error should mention empty key, got %v", err)
}

// TestParseParams_DuplicateKey verifies that the last occurrence of a
// duplicate key wins.
func TestParseParams_DuplicateKey(t *testing.T) {
	m, err := ParseParams("k=first,k=second")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"k": "second"}, m)
}

// TestParseParams_ValueWithEquals verifies that '=' inside a value is
// preserved (only the first '=' splits key from value).
func TestParseParams_ValueWithEquals(t *testing.T) {
	m, err := ParseParams("expr=a=b")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"expr": "a=b"}, m)
}

// TestParseParams_TrailingBackslash verifies that a trailing backslash
// (not followed by a comma) is preserved as a literal character.
func TestParseParams_TrailingBackslash(t *testing.T) {
	m, err := ParseParams(`path=C:\foo`)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"path": `C:\foo`}, m)
}
