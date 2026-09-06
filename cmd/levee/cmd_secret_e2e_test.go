// End-to-end tests for `levee secret` (E-2): encrypted credential CRUD via
// the master password from LEVEE_MASTER_PASSWORD. `rotate` prompts on stdin,
// which the tests drive by swapping os.Stdin (restored via cleanup).
package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stdinFrom points os.Stdin at a reader for the duration of the test.
func stdinFrom(t *testing.T, text string) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString(text)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})
}

func TestSecretE2E_RequiresMasterPassword(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	t.Setenv("LEVEE_MASTER_PASSWORD", "")

	err := runErr(t, append([]string{"secret", "list"}, cfg...)...)
	assert.Contains(t, err.Error(), "LEVEE_MASTER_PASSWORD")
	err = runErr(t, append([]string{"secret", "add", "--name", "x",
		"--value", "y"}, cfg...)...)
	assert.Contains(t, err.Error(), "LEVEE_MASTER_PASSWORD")
}

func TestSecretE2E_CRUDLifecycle(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	t.Setenv("LEVEE_MASTER_PASSWORD", "master-pass-1")

	// add: human + JSON variants.
	out := mustRun(t, append([]string{"secret", "add", "--name", "db-pass",
		"--value", "s3cret"}, cfg...)...)
	assert.Contains(t, out, "db-pass")
	res, _, err := freshJSON(t, append([]string{"secret", "add", "--name", "api-token",
		"--type", "api_token", "--value", "tok-abc", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "api-token", data["name"])
	assert.Equal(t, "api_token", data["type"])

	// list: both credentials present, plaintext never echoed.
	out = mustRun(t, append([]string{"secret", "list"}, cfg...)...)
	assert.Contains(t, out, "db-pass")
	assert.NotContains(t, out, "s3cret")
	res, _, err = freshJSON(t, append([]string{"secret", "list", "--json"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)

	// show exposes metadata only (no plaintext field).
	res, _, err = freshJSON(t, append([]string{"secret", "show", "--name", "db-pass",
		"--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok = res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "db-pass", data["name"])
	assert.NotContains(t, data, "plaintext")
	human := mustRun(t, append([]string{"secret", "show", "--name", "db-pass"}, cfg...)...)
	assert.Contains(t, human, "db-pass")

	// rotate reads the new value from stdin.
	stdinFrom(t, "new-pass-2\n")
	mustRun(t, append([]string{"secret", "rotate", "--name", "db-pass"}, cfg...)...)

	// rotate on a missing credential fails.
	stdinFrom(t, "whatever\n")
	err = runErr(t, append([]string{"secret", "rotate", "--name", "ghost"}, cfg...)...)
	assert.Error(t, err)

	// revoke: JSON variant, then show/list no longer see it.
	res, _, err = freshJSON(t, append([]string{"secret", "revoke", "--name", "api-token",
		"--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok = res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, data["revoked"])
	err = runErr(t, append([]string{"secret", "show", "--name", "api-token"}, cfg...)...)
	assert.Contains(t, err.Error(), "not found")

	// show on a missing credential fails with the not-found sentinel.
	err = runErr(t, append([]string{"secret", "show", "--name", "never"}, cfg...)...)
	assert.Contains(t, err.Error(), "not found")

	// Missing required flags rejected.
	err = runErr(t, append([]string{"secret", "add", "--name", "onlyname"}, cfg...)...)
	assert.Error(t, err)
}

func TestSecretE2E_ListEmpty(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	t.Setenv("LEVEE_MASTER_PASSWORD", strings.Repeat("p", 20))

	out := mustRun(t, append([]string{"secret", "list"}, cfg...)...)
	assert.Contains(t, out, "No credentials found")
}
