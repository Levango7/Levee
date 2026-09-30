package dbschema

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClusterNodesSchemaIsSingleSourced keeps the cluster membership table at
// exactly one definition.
//
// It had two: internal/state/pgschema.sql and internal/cluster/pg_registry.go
// each ran CREATE TABLE IF NOT EXISTS cluster_nodes, so whichever package
// touched the database first decided the shape of every later one — state's
// copy carried a `capabilities` column and UNIQUE (address) that cluster's
// lacked, while cluster defaulted role / status / last_heartbeat that state
// declared NOT NULL without a default. Nothing failed, because the columns
// actually written happen to be the ones both copies agreed on.
func TestClusterNodesSchemaIsSingleSourced(t *testing.T) {
	const marker = "CREATE TABLE IF NOT EXISTS cluster_nodes"

	var sites []string
	// DDL lives in production Go constants and embedded .sql files under
	// internal/ and cmd/. Walking the repository root instead would also scan
	// .git and web/node_modules (~90s) and cannot hold schema definitions;
	// test sources are excluded because they necessarily contain the very
	// string this file is searching for.
	for _, root := range []string{"../../internal", "../../cmd"} {
		require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			ext := filepath.Ext(path)
			if (ext != ".go" && ext != ".sql") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for n := strings.Count(string(src), marker); n > 0; n-- {
				sites = append(sites, filepath.ToSlash(path))
			}
			return nil
		}))
	}

	assert.Equal(t, []string{"internal/dbschema/cluster_nodes.go"}, unique(sites),
		"cluster_nodes must be created by exactly one definition; a second IF-NOT-EXISTS copy "+
			"makes whichever package initialises the database first the owner of its shape")
}

// TestClusterNodesDDLCoversEveryReaderAndWriter pins the other half: the one
// definition must declare every column a reader selects or the registry
// writes. Text-scanned, because a live database is not available to ordinary
// unit tests.
func TestClusterNodesDDLCoversEveryReaderAndWriter(t *testing.T) {
	ddl := columnNames(ClusterNodesDDL, "CREATE TABLE IF NOT EXISTS cluster_nodes (")
	require.NotEmpty(t, ddl, "the shared definition must declare columns")

	selected := columnsOfLine(t, readGo(t, "../state/pgstore.go"), "FROM cluster_nodes", "SELECT ", " FROM cluster_nodes")
	require.NotEmpty(t, selected, "expected ListClusterNodes to name its columns explicitly")

	inserted := columnsOfLine(t, readGo(t, "../cluster/pg_registry.go"), "INSERT INTO cluster_nodes (",
		"INSERT INTO cluster_nodes (", ")")
	require.NotEmpty(t, inserted, "expected the registry to name its columns explicitly")

	for _, col := range append(append([]string{}, selected...), inserted...) {
		assert.Contains(t, ddl, col,
			"column %q is read/written by production code but missing from the single definition", col)
	}

	// The column set is pinned exactly, not just "covers the users": the whole
	// point of merging the two copies is that neither side loses a column it
	// used to declare. `capabilities` is in there on purpose even though no
	// production code reads or writes it yet — it existed only in the state
	// copy, so a merge that dropped it would silently shrink the shape of
	// every cluster-first database. Adding, renaming or removing a column means
	// updating this list deliberately.
	assert.Equal(t,
		[]string{"id", "address", "status", "role", "last_heartbeat", "capabilities", "joined_at"},
		ddl, "cluster_nodes column set changed")

	// No UNIQUE (address). Only the state copy declared it, and it conflicts
	// with how the table is written: identity is `id`, and the same listener
	// address legitimately reappears under a new node id. Measured on a live
	// PostgreSQL — keeping it made ten internal/takeover cases fail with
	// SQLSTATE 23505 on cluster_nodes_address_key. Re-adding it here would
	// silently put failover re-registration behind a uniqueness error.
	assert.NotContains(t, ClusterNodesDDL, "UNIQUE (address)",
		"address is not unique: takeover/failover re-registers the same address under a new id")

	// Every NOT NULL column is defaulted (or is the key). This is the actual
	// origin of the drift: state declared `last_heartbeat TIMESTAMPTZ NOT NULL`
	// with no default while cluster declared it with DEFAULT NOW(), so whether
	// a writer could omit the column depended on which package created the
	// table — a fixture in internal/state had to start spelling the column out
	// just to become order-independent.
	for _, line := range strings.Split(blockOf(ClusterNodesDDL, "CREATE TABLE IF NOT EXISTS cluster_nodes ("), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "NOT NULL") || !strings.Contains(trimmed, "TIMESTAMPTZ") {
			continue
		}
		assert.Contains(t, trimmed, "DEFAULT",
			"timestamp column without a default makes writes depend on creation order: %q", trimmed)
	}
}

// blockOf returns the text after `signature` up to the line that closes it.
func blockOf(src, signature string) string {
	i := strings.Index(src, signature)
	if i < 0 {
		return ""
	}
	rest := src[i+len(signature):]
	if j := strings.Index(rest, "\n)"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// columnsOfLine finds the line containing `mustContain` and returns the
// identifiers between `before` and `after` on that line.
func columnsOfLine(t *testing.T, src, mustContain, before, after string) []string {
	t.Helper()
	for _, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, mustContain) {
			continue
		}
		return splitIdentifiersBetween(line, before, after)
	}
	return nil
}

func readGo(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	require.NoError(t, err, "the schema contract depends on this file existing at %s", path)
	return string(src)
}

// columnNames extracts the identifier list of the `signature (` block, reading
// line by line until the block closes. A paren-depth or first-")" scan would
// stop inside `DEFAULT NOW()` and silently report a shorter column list than
// the table actually has — which is how this test first failed.
func columnNames(src, signature string) []string {
	i := strings.Index(src, signature)
	if i < 0 {
		return nil
	}
	var out []string
	for _, line := range strings.Split(src[i+len(signature):], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if strings.HasPrefix(trimmed, ")") {
			break
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "UNIQUE", "CHECK", "CONSTRAINT", "PRIMARY", "FOREIGN":
			continue // table constraints, not columns
		}
		out = append(out, strings.TrimSuffix(fields[0], ","))
	}
	return out
}

// splitIdentifiersBetween returns the comma-separated identifiers found in
// src between the two markers on the same line.
func splitIdentifiersBetween(src, before, after string) []string {
	i := strings.Index(src, before)
	if i < 0 {
		return nil
	}
	rest := src[i+len(before):]
	j := strings.Index(rest, after)
	if j < 0 {
		return nil
	}
	var out []string
	for _, part := range strings.Split(rest[:j], ",") {
		if id := strings.TrimSpace(part); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimPrefix(s, "../../")
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
