package cluster

// capabilities_schema_test.go ties the declaration a node makes to the column it
// lands in.
//
// Two owners had drifted: the shared DDL defaulted cluster_nodes.capabilities to
// '{}' — an empty OBJECT — while upsertNode wrote '[]' for a node that declared
// nothing. Neither value is wrong alone; two spellings of "nothing" in one column
// means the row a reader finds depends on which code path created the table. The
// general check (what a name may look like, what "no match" means) lives in
// internal/capability, next to the gateway that filters with it; what is pinned
// here is the registry's own boundary: the value a caller hands a Node, and the
// DDL that receives it.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/capability"
	"github.com/nexus/levee/internal/dbschema"
)

func TestStoredCapabilities(t *testing.T) {
	got, err := storedCapabilities("")
	require.NoError(t, err)
	assert.Equal(t, capability.Empty, got,
		"an undeclared node stores the column's canonical empty value")

	got, err = storedCapabilities(`["shell", "pkg", "shell"]`)
	require.NoError(t, err)
	assert.Equal(t, `["pkg","shell"]`, got, "the registry canonicalises on the way in")

	// A caller who hand-built a Node with a comma list (the shape the serve flag
	// takes before normalisation) must not get it stored: every reader of this
	// column parses JSON, and a row that parses as nothing is a node that
	// silently never matches a filter.
	for _, bad := range []string{"shell,pkg", "{}", "shell", `[1]`} {
		_, err := storedCapabilities(bad)
		assert.Error(t, err, "storedCapabilities(%q) accepted a value no reader can parse", bad)
	}
}

// TestCapabilitiesColumnDefaultMatchesWhatTheWriterSends is the seam between the
// DDL and the writer. It reads the DDL text instead of a live database because
// ordinary unit tests have no PostgreSQL, and the claim being checked — that both
// definitions name the same empty value — is a source-level fact.
func TestCapabilitiesColumnDefaultMatchesWhatTheWriterSends(t *testing.T) {
	empty, err := capability.Normalize(nil)
	require.NoError(t, err)
	require.Equal(t, capability.Empty, empty)

	written, err := storedCapabilities("")
	require.NoError(t, err)
	assert.Equal(t, empty, written, "the registry's own empty value must be the canonical one")

	line := capabilitiesColumnLine(t, dbschema.ClusterNodesDDL)
	assert.Contains(t, line, "DEFAULT '"+empty+"'",
		"cluster_nodes.capabilities must DEFAULT what a writer sends, got: "+line)
}

// capabilitiesColumnLine returns the DDL line declaring the capabilities column.
func capabilitiesColumnLine(t *testing.T, ddl string) string {
	t.Helper()
	for _, line := range strings.Split(ddl, "\n") {
		if strings.Contains(line, "capabilities") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatal("the shared cluster_nodes DDL no longer declares a capabilities column")
	return ""
}
