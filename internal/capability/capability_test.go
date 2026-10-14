package capability

// capability_test.go pins the three things this package exists to keep
// unanimous: what may be declared, what the column ends up holding, and what a
// matcher answers.
//
// None of it had an owner before. The declaration column was written from a
// field no production caller set, defaulted to a different empty value than the
// writer sent, and read by a card that said "未声明能力" for every node — so
// there was no answer to "which nodes can run pkg" anywhere in the product.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"nothing declared", nil, Empty},
		{"empty slice", []string{}, Empty},
		{"single", []string{"shell"}, `["shell"]`},
		{"sorted so the set is stored, not the order it was typed",
			[]string{"pkg", "file", "shell"}, `["file","pkg","shell"]`},
		{"duplicates collapse", []string{"shell", "shell", " pkg "}, `["pkg","shell"]`},
		{"two orders of one set are byte-identical", []string{"shell", "pkg"}, `["pkg","shell"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNormalizeRejectsNamesThatWouldMatchNothing(t *testing.T) {
	// Cobra's CSV splitting happily produces "shell, ,pkg"; storing the blank
	// would put a capability on a node's card that matches no module and no
	// filter, which is the decorative-field shape this repo keeps having to
	// remove elsewhere.
	for _, in := range [][]string{{""}, {"shell", "  "}, {" "}} {
		_, err := Normalize(in)
		require.Error(t, err, "input %q must not become a stored capability", in)
		assert.Contains(t, err.Error(), "must not be empty")
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	// serve normalises the flag, and the registry normalises again on its way
	// into SQL. A second pass that changed the text would rewrite the row on
	// every restart and make "did the declaration change?" unanswerable.
	first, err := Normalize([]string{"shell", "pkg"})
	require.NoError(t, err)
	second, err := Normalize(mustNames(t, first))
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestNames(t *testing.T) {
	cases := []struct {
		stored string
		want   []string
		ok     bool
	}{
		{``, nil, true},
		{Empty, []string{}, true},
		{`["pkg"]`, []string{"pkg"}, true},
		{`["pkg", "shell"]`, []string{"pkg", "shell"}, true},
		// A blank name cannot match anything, so echoing it back would let a
		// caller filter by capability and receive rows that declared nothing.
		{`["pkg", ""]`, []string{"pkg"}, true},
		// '{}' is the value the column used to DEFAULT to. It is not a capability
		// set, and laundering it into "declared nothing" is precisely how a wrong
		// shape becomes invisible.
		{`{}`, nil, false},
		{`["a": "b"]`, nil, false},
		{`shell,pkg`, nil, false},
		{`[1,2]`, nil, false},
	}
	for _, tc := range cases {
		got, err := Names(tc.stored)
		if tc.ok {
			require.NoError(t, err, "stored %q", tc.stored)
			assert.Equal(t, tc.want, got, "stored %q", tc.stored)
			continue
		}
		require.Error(t, err, "stored %q must be reported, not read as empty", tc.stored)
	}
}

func TestHas(t *testing.T) {
	cases := []struct {
		name   string
		stored string
		want   string
		expect bool
	}{
		{"declared name matches", `["pkg","shell"]`, "pkg", true},
		{"undeclared name does not", `["pkg","shell"]`, "file", false},
		{"matching is exact, not prefix", `["pkg"]`, "pk", false},
		{"nothing declared matches nothing asked", Empty, "pkg", false},
		{"an empty question declares no interest", `["pkg"]`, "", false},
		{"whitespace question likewise", `["pkg"]`, "   ", false},
		// Safe direction for a filter: a row this package cannot read does not
		// claim to be what was asked for.
		{"an unreadable row never matches", `{}`, "pkg", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expect, Has(tc.stored, tc.want))
		})
	}
}

func mustNames(t *testing.T, stored string) []string {
	t.Helper()
	names, err := Names(stored)
	require.NoError(t, err)
	return names
}
