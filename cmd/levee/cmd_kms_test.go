package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/credential"
)

// =========================================================================
// Command registration
// =========================================================================

func TestKMSCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("kms")
	require.NotNil(t, cmd, "kms subcommand should be registered")
	assert.Equal(t, "kms", cmd.Name())
}

func TestKMSSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("kms")
	require.NotNil(t, cmd)

	subNames := []string{"status", "config", "test"}
	for _, name := range subNames {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "kms should have %q subcommand", name)
	}
}

func TestKMSStatusCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("kms")
	require.NotNil(t, cmd)

	statusCmd := findSubCmd(cmd, "status")
	require.NotNil(t, statusCmd)

	err := statusCmd.Args(statusCmd, []string{"unexpected"})
	assert.Error(t, err, "kms status should not accept args")
}

func TestKMSConfigCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("kms")
	require.NotNil(t, cmd)

	configCmd := findSubCmd(cmd, "config")
	require.NotNil(t, configCmd)

	err := configCmd.Args(configCmd, []string{"unexpected"})
	assert.Error(t, err, "kms config should not accept args")
}

func TestKMSTestCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("kms")
	require.NotNil(t, cmd)

	testCmd := findSubCmd(cmd, "test")
	require.NotNil(t, testCmd)

	err := testCmd.Args(testCmd, []string{"unexpected"})
	assert.Error(t, err, "kms test should not accept args")
}

func TestKMSTestCmdHasNameFlag(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("kms")
	require.NotNil(t, cmd)

	testCmd := findSubCmd(cmd, "test")
	require.NotNil(t, testCmd)

	f := testCmd.Flags().Lookup("name")
	require.NotNil(t, f, "kms test should have --name flag")
}

// =========================================================================
// Human-readable output
// =========================================================================

func TestPrintKMSStatusHuman_NoProviders(t *testing.T) {
	defer resetRootFlags()
	var buf bytes.Buffer
	output := map[string]any{
		"providers":        []credential.ProviderStatus{},
		"fallback":         true,
		"default_provider": "",
	}
	printKMSStatusHuman(&buf, output)
	out := buf.String()
	assert.Contains(t, out, "KMS Provider Status")
	assert.Contains(t, out, "No KMS providers registered")
	assert.Contains(t, out, "Local fallback")
}

func TestPrintKMSStatusHuman_WithProviders(t *testing.T) {
	defer resetRootFlags()
	var buf bytes.Buffer
	output := map[string]any{
		"providers": []credential.ProviderStatus{
			{Name: "vault", Healthy: true},
			{Name: "aws-kms", Healthy: false, Error: "unreachable"},
		},
		"fallback":         false,
		"default_provider": "vault",
	}
	printKMSStatusHuman(&buf, output)
	out := buf.String()
	assert.Contains(t, out, "vault")
	assert.Contains(t, out, "aws-kms")
	assert.Contains(t, out, "ok")
	assert.Contains(t, out, "fail")
	assert.Contains(t, out, "unreachable")
}

func TestPrintKMSConfigHuman(t *testing.T) {
	defer resetRootFlags()
	var buf bytes.Buffer
	output := map[string]any{
		"providers":        []string{"vault", "aws-kms"},
		"default_provider": "vault",
		"fallback":         true,
	}
	printKMSConfigHuman(&buf, output)
	out := buf.String()
	assert.Contains(t, out, "KMS Configuration")
	assert.Contains(t, out, "vault")
	assert.Contains(t, out, "aws-kms")
	assert.Contains(t, out, "Default provider")
	assert.Contains(t, out, "Local fallback")
}

func TestPrintKMSConfigHuman_NoProviders(t *testing.T) {
	defer resetRootFlags()
	var buf bytes.Buffer
	output := map[string]any{
		"providers":        []string{},
		"default_provider": "",
		"fallback":         false,
	}
	printKMSConfigHuman(&buf, output)
	out := buf.String()
	assert.Contains(t, out, "(none)")
}

func TestPrintKMSTestHuman_NoProviders(t *testing.T) {
	defer resetRootFlags()
	var buf bytes.Buffer
	output := map[string]any{
		"results": []map[string]any{},
	}
	printKMSTestHuman(&buf, output)
	out := buf.String()
	assert.Contains(t, out, "No KMS providers registered")
}

func TestPrintKMSTestHuman_WithResults(t *testing.T) {
	defer resetRootFlags()
	var buf bytes.Buffer
	output := map[string]any{
		"results": []map[string]any{
			{"provider": "vault", "healthy": true, "get_secret": "ok", "error": ""},
			{"provider": "aws-kms", "healthy": false, "get_secret": "", "error": "down"},
		},
	}
	printKMSTestHuman(&buf, output)
	out := buf.String()
	assert.Contains(t, out, "vault")
	assert.Contains(t, out, "aws-kms")
	assert.Contains(t, out, "ok")
	assert.Contains(t, out, "fail")
	assert.Contains(t, out, "down")
}

// =========================================================================
// JSON output envelope
// =========================================================================

func TestKMSStatusOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"providers":        []credential.ProviderStatus{},
		"fallback":         true,
		"default_provider": "",
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

func TestKMSConfigOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"providers":        []string{"vault"},
		"default_provider": "vault",
		"fallback":         true,
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

func TestKMSTestOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"results": []map[string]any{
			{"provider": "vault", "healthy": true},
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

// =========================================================================
// strOrDefault helper
// =========================================================================

func TestStrOrDefault(t *testing.T) {
	assert.Equal(t, "hello", strOrDefault("hello", "default"))
	assert.Equal(t, "default", strOrDefault("", "default"))
}
