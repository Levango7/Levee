// Placeholder detection tests. Consumers that persist instantiated content need
// this because neither the LEVEELang parser nor the structural validator treats
// a surviving "{{.x}}" as an error — it is a legal string value — so an
// unsubstituted parameter only shows up as template syntax in a command sent to
// a target.
package template

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnsubstitutedPlaceholders(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "clean document",
			content: "steps:\n  - cmd: \"yum update -y nginx\"\n",
			want:    nil,
		},
		{
			name:    "one left",
			content: "cmd: \"yum update -y {{.package}}\"\n",
			want:    []string{"package"},
		},
		{
			name:    "both ways in",
			content: "query: \"group={{ .target_group }}\"\ncmd: \"{{.package}}\"\n",
			want:    []string{"target_group", "package"},
		},
		{
			name:    "repeats collapse",
			content: "a: {{.pkg}}\nb: {{.pkg}}\nc: {{.pkg}}\n",
			want:    []string{"pkg"},
		},
		{
			name: "shell braces are not parameters",
			// The grammar requires the leading dot, so awk/sed programs in a
			// step's command line cannot be mistaken for template parameters.
			content: "cmd: \"awk '{{print $1}}' /var/log/x\"\n",
			want:    nil,
		},
		{
			name:    "bare braces and jinja filters",
			content: "cmd: \"echo {{}} {{ other }} {{ a | b }}\"\n",
			want:    nil,
		},
		{
			name: "header prose is not a placeholder",
			// Templates document their own placeholders in comments. Flagging
			// those would make the check cry wolf, and a gate with false
			// positives teaches people to route around it.
			content: "# then instantiate with {{.package}} set:\nname: x\n",
			want:    nil,
		},
		{
			name: "a hash inside a quoted command is data",
			// Only whole-line comments are stripped: treating this `#` as a
			// comment start would hide the placeholder that follows it.
			content: "cmd: \"echo '# tag' && systemctl restart {{.service}}\"\n",
			want:    []string{"service"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, UnsubstitutedPlaceholders(tc.content))
		})
	}
}

// TestUnsubstitutedPlaceholdersFindsUndeclaredParam is the case the
// instantiator itself cannot see: substitution walks the declared parameters,
// so a placeholder in the content that no parameter names is never reported as
// missing — it just stays in the rendered text.
func TestUnsubstitutedPlaceholdersFindsUndeclaredParam(t *testing.T) {
	tmpl := &Template{
		Name:    "typo",
		Content: "cmd: \"systemctl restart {{.svcie}}\"\n",
		Parameters: []TemplateParam{
			{Name: "service", Type: "string", Required: true},
		},
	}

	result, err := NewInstantiator().Instantiate(tmpl, map[string]string{"service": "nginx"})
	require.NoError(t, err, "substitution succeeds: the declared parameter was supplied")
	assert.Equal(t, []string{"svcie"}, UnsubstitutedPlaceholders(result.Content),
		"the misspelled placeholder survives rendering and must be reported")
}
