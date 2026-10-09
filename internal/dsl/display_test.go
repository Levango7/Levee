package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// validDoc is a document the parser accepts (target is required), which is the
// only shape in which a declared name can be recovered. A document that does
// NOT parse is a case of its own below, and deliberately so.
const validDoc = "name: rolling-patch\nversion: \"1.0\"\ntarget:\n  type: host\n  query: \"env=prod\"\nsteps:\n  - name: deploy\n    action: shell.exec\n    args:\n      cmd: \"true\"\n"

// noNameDoc parses but declares no name; corruptDoc does not parse at all.
// Both are returned verbatim — the point of the fallback is that the operator
// can still recognise what they are looking at.
const (
	noNameDoc  = "target:\n  type: host\n  query: \"env=test\"\nsteps: []\n"
	corruptDoc = "name: broken\nsteps:\n  - name: x\n\t bad indentation\n"
)

// TestDisplayName pins the one implementation both readers of a run's
// WorkflowName use: the CLI's list/show/audit output and the gRPC change board
// (Change.Label). The cases mirror the CLI's own test, which established them
// before the code moved here.
func TestDisplayName(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"empty stays empty", "", ""},
		{"whitespace only stays empty", "   \n  ", ""},
		{"a path is already a name", "workflows/patch.yaml", "workflows/patch.yaml"},
		{"a plain name is trimmed", "  plain-name  ", "plain-name"},
		{"an inline document yields its declared name", validDoc, "rolling-patch"},
		{"a document without a name is shown as itself", noNameDoc, strings.TrimSpace(noNameDoc)},
		{"a corrupt document is shown as itself", corruptDoc, strings.TrimSpace(corruptDoc)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DisplayName(tc.src))
		})
	}
}

// The two directions matter separately. A recovered name must never carry the
// document's line breaks (it is printed into a table cell and a board row),
// while the fallback must keep them: collapsing a broken document into a
// guessed one-line name would hide exactly what the operator needs to see.
func TestDisplayName_CollapsesNamesAndKeepsTheRawFallback(t *testing.T) {
	assert.Equal(t, "rolling-patch", DisplayName(validDoc))

	raw := DisplayName(corruptDoc)
	assert.Equal(t, strings.TrimSpace(corruptDoc), raw)
	assert.Contains(t, raw, "\n", "the fallback is the source itself, not a guess")
}
