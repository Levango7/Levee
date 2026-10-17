// vocabulary_ownership_test.go — cluster's own order-bearing lists must cover every
// NodeStatus / NodeRole constant declared in node.go.
//
// Why the parse lives here and not in the cross-language guard: the guard's job is
// to stop a *second copy* from drifting, and the way to do that is to generate the
// copy from data (internal/docgen). But data can still be incomplete — a fourth
// status declared in node.go and never added to nodeStatusOrder would reach the
// wire with no name in the console's table. That is a claim about THIS package's
// declarations, so it is checked here, against this package's source, once.
//
// The failure it replaces: internal/cluster/node_vocabulary_test.go parsed node.go
// for the constants and web/src/utils/node.ts for the mirror, so the set it checked
// was derived correctly but the list the generator will use did not exist yet. Move
// the parse to the owner and everything downstream becomes data-to-data.
package cluster

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clusterVocabRepoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("cannot locate %s from the test working directory", rel)
	return ""
}

// goEnumValues parses `Name Type = "value"` declarations for one type name out of
// node.go. Line-anchored and end-anchored so a comment mentioning the same shape
// cannot be collected as a declaration.
func goEnumValues(t *testing.T, typeName string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(clusterVocabRepoFile(t, "internal/cluster/node.go"))
	require.NoError(t, err)
	out := map[string]string{}
	re := regexp.MustCompile(`(?m)^[ \t]*(\w+)[ \t]+` + regexp.QuoteMeta(typeName) + `[ \t]*=[ \t]*"([^"]+)"$`)
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	require.NotEmpty(t, out,
		"no %s constant parsed from internal/cluster/node.go — the declaration shape changed and this guard went blind", typeName)
	return out
}

func TestNodeStatusListCoversEveryDeclaredStatus(t *testing.T) {
	declared := goEnumValues(t, "NodeStatus")
	listed := NodeStatusNames()

	require.Len(t, listed, len(declared),
		"nodeStatusOrder lists %d statuses while node.go declares %d — a new one would reach the wire unnamed", len(listed), len(declared))
	set := map[string]bool{}
	for _, value := range listed {
		assert.False(t, set[value], "nodeStatusOrder names %q twice", value)
		set[value] = true
	}
	for name, value := range declared {
		assert.Truef(t, set[value], "%s (%q) is declared in node.go but missing from nodeStatusOrder", name, value)
	}
}

func TestNodeRoleListCoversEveryDeclaredRole(t *testing.T) {
	declared := goEnumValues(t, "NodeRole")
	listed := NodeRoleNames()

	require.Len(t, listed, len(declared),
		"nodeRoleOrder lists %d roles while node.go declares %d", len(listed), len(declared))
	set := map[string]bool{}
	for _, value := range listed {
		assert.False(t, set[value], "nodeRoleOrder names %q twice", value)
		set[value] = true
	}
	for name, value := range declared {
		assert.Truef(t, set[value], "%s (%q) is declared in node.go but missing from nodeRoleOrder", name, value)
	}
}

// TestNamesAreCopies keeps the documented "may be modified by the caller" honest.
// A shared slice would let one caller reorder the vocabulary the generator reads.
func TestNamesAreCopies(t *testing.T) {
	statuses := NodeStatusNames()
	require.NotEmpty(t, statuses)
	statuses[0] = "clobbered"
	assert.NotEqual(t, "clobbered", NodeStatusNames()[0],
		"NodeStatusNames handed out the owner's own slice; a caller could reorder the vocabulary docgen reads")

	roles := NodeRoleNames()
	require.NotEmpty(t, roles)
	roles[0] = "clobbered"
	assert.NotEqual(t, "clobbered", NodeRoleNames()[0], "NodeRoleNames handed out the owner's own slice")
}
