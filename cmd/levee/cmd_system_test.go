package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("system")
	require.NotNil(t, cmd, "system subcommand should be registered")
	assert.Equal(t, "system", cmd.Name())
}

func TestSystemSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("system")
	require.NotNil(t, cmd)

	subNames := []string{"version", "status", "config", "doctor"}
	for _, name := range subNames {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "system should have %q subcommand", name)
	}
}

func TestSystemConfigSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("system")
	require.NotNil(t, cmd)

	configCmd := findSubCmd(cmd, "config")
	require.NotNil(t, configCmd, "system should have config subcommand")

	subNames := []string{"get", "set"}
	for _, name := range subNames {
		found := false
		for _, sub := range configCmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "config should have %q subcommand", name)
	}
}

func TestSystemVersionCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("system")
	require.NotNil(t, cmd)

	versionCmd := findSubCmd(cmd, "version")
	require.NotNil(t, versionCmd)

	err := versionCmd.Args(versionCmd, []string{"unexpected"})
	assert.Error(t, err, "system version should not accept args")
}

func TestSystemStatusCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("system")
	require.NotNil(t, cmd)

	statusCmd := findSubCmd(cmd, "status")
	require.NotNil(t, statusCmd)

	err := statusCmd.Args(statusCmd, []string{"unexpected"})
	assert.Error(t, err, "system status should not accept args")
}

func TestSystemStatusCmdFormatFlag(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("system")
	require.NotNil(t, cmd)

	statusCmd := findSubCmd(cmd, "status")
	require.NotNil(t, statusCmd)

	f := statusCmd.Flags().Lookup("format")
	require.NotNil(t, f, "system status should have --format flag")
}

func TestSystemConfigGetCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("system")
	require.NotNil(t, cmd)

	configCmd := findSubCmd(cmd, "config")
	require.NotNil(t, configCmd)

	getCmd := findSubCmd(configCmd, "get")
	require.NotNil(t, getCmd)

	err := getCmd.Args(getCmd, []string{})
	assert.Error(t, err, "config get requires a key argument")
}

func TestSystemConfigSetCmdRequiresArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("system")
	require.NotNil(t, cmd)

	configCmd := findSubCmd(cmd, "config")
	require.NotNil(t, configCmd)

	setCmd := findSubCmd(configCmd, "set")
	require.NotNil(t, setCmd)

	err := setCmd.Args(setCmd, []string{"key_only"})
	assert.Error(t, err, "config set requires key and value arguments")
}

func TestSystemDoctorCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("system")
	require.NotNil(t, cmd)

	doctorCmd := findSubCmd(cmd, "doctor")
	require.NotNil(t, doctorCmd)

	err := doctorCmd.Args(doctorCmd, []string{"unexpected"})
	assert.Error(t, err, "system doctor should not accept args")
}

func TestGetConfigValueKnownKeys(t *testing.T) {
	defer resetRootFlags()

	// We can't easily construct a full Config without a file, so we test
	// the error case for unknown keys.
	_, err := getConfigValue(nil, "unknown.key")
	assert.Error(t, err, "unknown key should return error")
	assert.Contains(t, err.Error(), "unknown config key")
}

func TestValidateConfigKey(t *testing.T) {
	defer resetRootFlags()

	// Settable keys.
	for _, key := range []string{"server.log_level", "server.log_format", "log.level", "log.format", "log.output"} {
		err := validateConfigKey(key)
		assert.NoError(t, err, "key %q should be valid", key)
	}

	// Non-settable keys.
	err := validateConfigKey("database.path")
	assert.Error(t, err, "database.path should not be settable")
}

func TestSortedKeys(t *testing.T) {
	m := map[string]bool{"c": true, "a": true, "b": true}
	keys := sortedKeys(m)
	assert.Equal(t, []string{"a", "b", "c"}, keys)
}

func TestPrintSystemStatusHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"version":     "1.0.0",
		"config_path": "/etc/levee/config.yaml",
		"db_status":   "ok",
		"db_path":     "/var/lib/levee/levee.db",
	}

	var buf bytes.Buffer
	printSystemStatusHuman(&buf, output)
	out := buf.String()
	assert.Contains(t, out, "1.0.0")
	assert.Contains(t, out, "ok")
	assert.Contains(t, out, "/etc/levee/config.yaml")
}

func TestPrintConfigGetHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"key":   "server.log_level",
		"value": "info",
	}

	var buf bytes.Buffer
	printConfigGetHuman(&buf, output)
	assert.Contains(t, buf.String(), "server.log_level")
	assert.Contains(t, buf.String(), "info")
}

func TestPrintConfigSetHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"key":   "server.log_level",
		"value": "debug",
		"path":  "/etc/levee/config.yaml",
	}

	var buf bytes.Buffer
	printConfigSetHuman(&buf, output)
	assert.Contains(t, buf.String(), "server.log_level")
	assert.Contains(t, buf.String(), "debug")
}

func TestPrintDoctorHuman(t *testing.T) {
	defer resetRootFlags()

	checks := []map[string]any{
		{"check": "config", "status": "OK"},
		{"check": "database", "status": "OK"},
		{"check": "permission_matrix", "status": "OK", "teams": 3, "envs": 2},
	}

	var buf bytes.Buffer
	printDoctorHuman(&buf, "healthy", checks)
	out := buf.String()
	assert.Contains(t, out, "healthy")
	assert.Contains(t, out, "config")
	assert.Contains(t, out, "database")
	assert.Contains(t, out, "permission_matrix")
}

func TestPrintDoctorHumanWithErrors(t *testing.T) {
	defer resetRootFlags()

	checks := []map[string]any{
		{"check": "config", "status": "FAIL", "error": "file not found"},
		{"check": "database", "status": "SKIP", "error": "config not loaded"},
	}

	var buf bytes.Buffer
	printDoctorHuman(&buf, "unhealthy", checks)
	out := buf.String()
	assert.Contains(t, out, "unhealthy")
	assert.Contains(t, out, "FAIL")
	assert.Contains(t, out, "file not found")
}

func TestSystemStatusOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"version":     "1.0.0",
		"config_path": "/etc/levee/config.yaml",
		"db_status":   "ok",
		"db_path":     "/var/lib/levee/levee.db",
	}

	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, map[string]any{
		"data":  data,
		"meta":  nil,
		"error": nil,
	}))

	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	assert.NotNil(t, env.Data)
}

func TestDoctorOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"overall": "healthy",
		"checks": []map[string]any{
			{"check": "config", "status": "OK"},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, map[string]any{
		"data":  data,
		"meta":  nil,
		"error": nil,
	}))

	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	assert.NotNil(t, env.Data)
}
