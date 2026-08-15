package main

// cmd_compile_test.go — unit tests for the `levee compile` command.
//
// Coverage:
//   - the compile sub-command is registered with the right name/flags
//   - --strict / --lenient / --ir / --check-only flags exist
//   - compile on a valid workflow succeeds
//   - compile --ir emits a JSON IR with ir_version "1.0"
//   - compile --check-only does not emit IR
//   - compile on a missing file returns an error
//   - compile --lenient reports warnings but succeeds on type-mismatched wf

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validCompileYAML is a complete workflow that passes both basic validation
// and type checking.
const validCompileYAML = `
name: compile-test
version: "1.0"
description: "compile command test"
input:
  - name: pkg
    type: string
    required: true
    default: nginx
  - name: wait
    type: duration
    default: 5m
target:
  type: host
  query: "env=prod"
batches:
  strategy: percent
  steps: [1, 50, 100]
  max_concurrency: 10
approval:
  level: high
  min_approvers: 1
steps:
  - name: exec
    action: shell.exec
    args:
      cmd: "uname -r"
      timeout: 30s
`

// typeMismatchYAML has a step arg type error (timeout expects duration, got
// bool). This passes the parser and basic validator but fails the type
// checker, making it suitable for strict/lenient mode tests.
const typeMismatchYAML = `
name: type-mismatch
target:
  type: host
  query: "env=prod"
steps:
  - name: exec
    action: shell.exec
    args:
      cmd: "uname -r"
      timeout: true
`

// writeTempYAML writes content to a temp file and returns its path.
func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// resetCompileFlags restores the compile command flags to their defaults.
func resetCompileFlags() {
	compileOptStrict = true
	compileOptLenient = false
	compileOptIR = false
	compileOptCheckOnly = false
}

// --- Command registration --------------------------------------------------

func TestCompileCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("compile")
	require.NotNil(t, cmd, "compile subcommand should be registered")
	assert.Equal(t, "compile", cmd.Name())
}

func TestCompileCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("compile")
	require.NotNil(t, cmd)

	for _, name := range []string{"strict", "lenient", "ir", "check-only"} {
		f := cmd.Flags().Lookup(name)
		require.NotNil(t, f, "compile command should have --%s flag", name)
	}
}

func TestCompileCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("compile")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err)
}

func TestCompileCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("compile")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"a", "b"})
	assert.Error(t, err)
}

// --- Compile on valid workflow ---------------------------------------------

func TestCompileValidWorkflow(t *testing.T) {
	defer resetRootFlags()
	defer resetCompileFlags()

	path := writeTempYAML(t, validCompileYAML)
	cmd := findSub("compile")
	require.NotNil(t, cmd)

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.RunE(cmd, []string{path})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "ok")
}

func TestCompileCheckOnly(t *testing.T) {
	defer resetRootFlags()
	defer resetCompileFlags()
	compileOptCheckOnly = true

	path := writeTempYAML(t, validCompileYAML)
	cmd := findSub("compile")
	require.NotNil(t, cmd)

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.RunE(cmd, []string{path})
	require.NoError(t, err)
	// --check-only should not emit IR JSON.
	assert.NotContains(t, buf.String(), `"ir_version"`)
}

// --- Compile --ir ----------------------------------------------------------

func TestCompileEmitIR(t *testing.T) {
	defer resetRootFlags()
	defer resetCompileFlags()
	compileOptIR = true

	path := writeTempYAML(t, validCompileYAML)
	cmd := findSub("compile")
	require.NotNil(t, cmd)

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.RunE(cmd, []string{path})
	require.NoError(t, err)

	// The output should be a JSON IR with ir_version "1.0".
	var ir map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &ir), "output should be valid JSON: %s", buf.String())
	assert.Equal(t, "1.0", ir["ir_version"])
	wf, ok := ir["workflow"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "compile-test", wf["name"])
}

func TestCompileEmitIRJSONEnvelope(t *testing.T) {
	defer resetRootFlags()
	defer resetCompileFlags()
	compileOptIR = true
	optJSON = true

	path := writeTempYAML(t, validCompileYAML)
	cmd := findSub("compile")
	require.NotNil(t, cmd)

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.RunE(cmd, []string{path})
	require.NoError(t, err)

	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	require.NotNil(t, env.Data)
}

// --- Compile on missing file ----------------------------------------------

func TestCompileMissingFile(t *testing.T) {
	defer resetRootFlags()
	defer resetCompileFlags()

	cmd := findSub("compile")
	require.NotNil(t, cmd)

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.RunE(cmd, []string{"/nonexistent/path/wf.yaml"})
	require.Error(t, err)
}

// --- Strict vs lenient mode ------------------------------------------------

func TestCompileStrictModeFailsOnTypeError(t *testing.T) {
	defer resetRootFlags()
	defer resetCompileFlags()
	compileOptStrict = true
	compileOptLenient = false

	path := writeTempYAML(t, typeMismatchYAML)
	cmd := findSub("compile")
	require.NotNil(t, cmd)

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.RunE(cmd, []string{path})
	require.Error(t, err)
	// The error should mention type error(s).
	assert.Contains(t, err.Error(), "type error")
}

func TestCompileLenientModeSucceedsWithWarnings(t *testing.T) {
	defer resetRootFlags()
	defer resetCompileFlags()
	compileOptStrict = false
	compileOptLenient = true

	path := writeTempYAML(t, typeMismatchYAML)
	cmd := findSub("compile")
	require.NotNil(t, cmd)

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.RunE(cmd, []string{path})
	// Lenient mode should not return a fatal error.
	require.NoError(t, err, "lenient mode should not fail on type errors")
	// Warnings should be printed to stderr (captured in buf).
	output := buf.String()
	assert.True(t, strings.Contains(output, "warning") || strings.Contains(output, "duration"),
		"lenient mode should report warnings, got: %s", output)
}

// --- Flag defaults ---------------------------------------------------------

func TestCompileFlagDefaults(t *testing.T) {
	defer resetRootFlags()
	defer resetCompileFlags()
	resetCompileFlags()

	assert.True(t, compileOptStrict, "strict should default to true")
	assert.False(t, compileOptLenient)
	assert.False(t, compileOptIR)
	assert.False(t, compileOptCheckOnly)
}

// --- newCompileCmd returns a usable command --------------------------------

func TestNewCompileCmd(t *testing.T) {
	cmd := newCompileCmd()
	require.NotNil(t, cmd)
	assert.Equal(t, "compile", cmd.Name())
	assert.True(t, cmd.HasAvailableFlags())
	// Verify Args validator is set.
	assert.NotNil(t, cmd.Args)
}

// --- Ensure cobra command is the one registered ----------------------------

func TestCompileCmdIsRegisteredInstance(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("compile")
	require.NotNil(t, cmd)
	// The registered command should have the same Use line as the one built
	// by newCompileCmd.
	built := newCompileCmd()
	assert.Equal(t, built.Use, cmd.Use)
}

// Silence the unused-import warning for cobra by referencing it in a no-op
// var so the test file compiles even when the assertions above don't use it
// directly.
var _ = cobra.Command{}
