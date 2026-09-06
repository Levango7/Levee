// End-to-end tests for `levee system` (E-2): status/doctor tolerance against
// a temp config, the 12-key config get surface, and config set's 5-key
// allow-list with append-to-file persistence.
package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemE2E_StatusDoctorVersion(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	out := mustRun(t, append([]string{"system", "status"}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))
	res, _, err := freshJSON(t, append([]string{"system", "status", "--json"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)

	out = mustRun(t, append([]string{"system", "doctor"}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))

	out = mustRun(t, append([]string{"system", "version"}, cfg...)...)
	assert.Contains(t, out, "levee")
}

func TestSystemE2E_ConfigGetSet(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	// get: values resolve from the loaded config (quiet prints the value).
	out := mustRun(t, append([]string{"--quiet", "system", "config", "get",
		"database.path"}, cfg...)...)
	assert.Contains(t, trimOut(out), "levee.db")
	res, _, err := freshJSON(t, append([]string{"system", "config", "get",
		"server.data_dir", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, data["value"])

	// Every documented key resolves without error.
	for _, key := range []string{
		"server.data_dir", "server.log_level", "server.log_format",
		"database.driver", "database.path", "database.max_open_conns",
		"database.max_idle_conns", "log.level", "log.format", "log.output",
		"permission.default_team", "permission.default_env",
	} {
		mustRun(t, append([]string{"system", "config", "get", key}, cfg...)...)
	}

	// Unknown key rejected.
	err = runErr(t, append([]string{"system", "config", "get", "no.such.key"}, cfg...)...)
	assert.Error(t, err)

	// set on an allowed key appends nested YAML to the same config file and
	// the value reads back.
	mustRun(t, append([]string{"system", "config", "set", "log.level", "debug"}, cfg...)...)
	raw, err := os.ReadFile(e.cfgPath)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "level: debug")
	out = mustRun(t, append([]string{"--quiet", "system", "config", "get",
		"log.level"}, cfg...)...)
	assert.Contains(t, trimOut(out), "debug")

	// Dotted two-level key for a server.* entry (JSON variant).
	res, _, err = freshJSON(t, append([]string{"system", "config", "set",
		"server.log_format", "json", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok = res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, e.cfgPath, data["path"])

	// Non-settable key rejected with the allow-list message.
	err = runErr(t, append([]string{"system", "config", "set",
		"database.path", "/tmp/evil.db"}, cfg...)...)
	assert.Contains(t, err.Error(), "not settable")
}
