// End-to-end coverage for `levee target label`: the verb exists, it is wired to
// the command tree, it writes through the same store the rest of `target`
// uses, and the two refusals (env, region) hold through the real CLI rather
// than only at the library seam.
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// labelRowOf returns the listed row for host, or fails.
func labelRowOf(t *testing.T, rows []any, host string) map[string]any {
	t.Helper()
	for _, r := range rows {
		row, ok := r.(map[string]any)
		require.True(t, ok)
		if row["hostname"] == host {
			return row
		}
	}
	require.FailNow(t, "target not listed", "no row for %s in %v", host, rows)
	return nil
}

func labelsOf(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	if row["labels"] == nil {
		return map[string]any{}
	}
	labels, ok := row["labels"].(map[string]any)
	require.True(t, ok, "labels must be an object: %v", row["labels"])
	return labels
}

func TestTargetLabelE2E_SetKeepsOtherLabelsAndPersists(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	inv := writeInventoryYAML(t)
	mustRun(t, append([]string{"target", "import", "--file", inv}, cfg...)...)

	out := mustRun(t, append([]string{"target", "label", "set", "web-01.example.com", "region=dc-a"}, cfg...)...)
	assert.Contains(t, out, "region=dc-a")
	assert.Contains(t, out, "role=front",
		"an edit keeps the labels it was not asked about; that is the difference from `target import`")

	res, _, err := freshJSON(t, append([]string{"target", "list", "--json"}, cfg...)...)
	require.NoError(t, err)
	rows, ok := res["data"].([]any)
	require.True(t, ok)
	labels := labelsOf(t, labelRowOf(t, rows, "web-01.example.com"))
	assert.Equal(t, "dc-a", labels["region"])
	assert.Equal(t, "front", labels["role"])

	// The same edit again reports no change — the CLI must not claim a write it
	// did not make.
	res, _, err = freshJSON(t, append([]string{"target", "label", "set", "web-01.example.com", "region=dc-a", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, data["changed"])

	// unset removes exactly the named key.
	out = mustRun(t, append([]string{"target", "label", "unset", "web-01.example.com", "role"}, cfg...)...)
	assert.NotContains(t, out, "role=")
	assert.Contains(t, out, "region=dc-a")
}

func TestTargetLabelE2E_Refusals(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	inv := writeInventoryYAML(t)
	mustRun(t, append([]string{"target", "import", "--file", inv}, cfg...)...)

	// env: governed by target-ownership.yaml, refused with the file named.
	err := runErr(t, append([]string{"target", "label", "set", "web-01.example.com", "env=dev"}, cfg...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target-ownership.yaml")
	assert.Contains(t, err.Error(), "target import", "the refusal must say where env is edited")

	err = runErr(t, append([]string{"target", "label", "unset", "web-01.example.com", "env"}, cfg...)...)
	require.Error(t, err, "removing env is the same bypass as overwriting it")

	// region: not a name any node declaration could match.
	err = runErr(t, append([]string{"target", "label", "set", "web-01.example.com", "region=cn north"}, cfg...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "region")

	// Unknown host.
	err = runErr(t, append([]string{"target", "label", "set", "ghost.example.com", "role=x"}, cfg...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no target")

	// `unset` takes keys, not assignments.
	err = runErr(t, append([]string{"target", "label", "unset", "web-01.example.com", "role=front"}, cfg...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "label set")

	// None of the refusals wrote anything.
	res, _, err := freshJSON(t, append([]string{"target", "list", "--json"}, cfg...)...)
	require.NoError(t, err)
	rows, ok := res["data"].([]any)
	require.True(t, ok)
	labels := labelsOf(t, labelRowOf(t, rows, "web-01.example.com"))
	assert.Equal(t, map[string]any{"role": "front"}, labels,
		"a refused edit must leave the row exactly as it was")
}

// TestTargetLabelE2E_RemoteIsRefusedBeforeTheStoreOpens: the mode guard runs in
// PersistentPreRunE, so an operator who asked for the server cannot be silently
// answered by the local database. There is no RPC that edits labels, and this
// test is what says so out loud.
func TestTargetLabelE2E_RemoteIsRefusedBeforeTheStoreOpens(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	err := runErr(t, "--remote", "target", "label", "set", "web-01.example.com", "role=x", "--config", e.cfgPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "levee target label set")
	assert.Contains(t, err.Error(), "--remote is not wired")
}

func TestParseLabelAssignments(t *testing.T) {
	got, err := parseLabelAssignments([]string{"region=dc-a", "role=web", "empty="})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"region": "dc-a", "role": "web", "empty": ""}, got,
		"an empty value is the vocabulary's business, not the parser's")

	_, err = parseLabelAssignments([]string{"region"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key=value")

	_, err = parseLabelAssignments([]string{"=dc-a"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be empty")
}
