package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetRootFlags restores the global option variables to their defaults. Tests
// that flip --json / --quiet etc. call it in a defer to avoid leaking state
// into sibling tests.
func resetRootFlags() {
	optJSON = false
	optQuiet = false
	optVerbose = false
	optNoColor = false
	optConfigPath = ""
	optProfile = "default"
	optTimeout = "30m"
	optAPIURL = ""
	optAPIToken = ""
	optLocal = true
	optRemote = false
	optServer = "localhost:9090"
}

// findSub returns the cobra sub-command with the given name registered on the
// root, or nil when it is not present.
func findSub(name string) *cobra.Command {
	for _, sub := range rootCmd.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	return nil
}

// --- root command wiring ----------------------------------------------------

func TestRootCmdHasBasicMetadata(t *testing.T) {
	defer resetRootFlags()
	assert.Equal(t, "levee", rootCmd.Name())
	assert.NotEmpty(t, rootCmd.Short)
	assert.NotEmpty(t, rootCmd.Long)
}

func TestRootCmdPersistentFlagsDefined(t *testing.T) {
	defer resetRootFlags()
	cases := []string{"config", "json", "quiet", "verbose", "no-color", "profile", "timeout", "api", "token", "local", "remote", "server"}
	for _, name := range cases {
		f := rootCmd.PersistentFlags().Lookup(name)
		require.NotNil(t, f, "missing persistent flag %q", name)
	}
}

func TestRegisterCommandAddsSubcommand(t *testing.T) {
	defer resetRootFlags()
	sub := &cobra.Command{Use: "test-register-cmd", Run: func(*cobra.Command, []string) {}}
	RegisterCommand(sub)
	require.Contains(t, rootCmd.Commands(), sub)
	// Clean up so the dummy command does not leak into other tests.
	rootCmd.RemoveCommand(sub)
}

// --- version sub-command ----------------------------------------------------

func TestVersionSubcommandRegistered(t *testing.T) {
	defer resetRootFlags()
	require.NotNil(t, findSub("version"), "version subcommand should be registered")
}

func TestVersionHumanOutput(t *testing.T) {
	defer resetRootFlags()
	version = "1.2.3"
	buildTime = "2026-08-15T10:00:00Z"
	commitHash = "abc123"
	goVersion = "go1.25.0"

	var buf bytes.Buffer
	printVersionHuman(&buf, versionInfo{
		Version:    version,
		BuildTime:  buildTime,
		GoVersion:  goVersion,
		CommitHash: commitHash,
	})
	out := buf.String()
	assert.Contains(t, out, "levee 1.2.3")
	assert.Contains(t, out, "2026-08-15T10:00:00Z")
	assert.Contains(t, out, "abc123")
	assert.Contains(t, out, "go1.25.0")
}

func TestVersionJSONOutput(t *testing.T) {
	defer resetRootFlags()
	version = "1.2.3"
	buildTime = "2026-08-15T10:00:00Z"
	commitHash = "abc123"
	goVersion = "go1.25.0"

	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, map[string]any{
		"data": versionInfo{
			Version:    version,
			BuildTime:  buildTime,
			GoVersion:  goVersion,
			CommitHash: commitHash,
		},
		"meta":  nil,
		"error": nil,
	}))

	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	require.NotNil(t, env.Data)
	// Re-marshal Data and decode into versionInfo to inspect fields.
	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	var info versionInfo
	require.NoError(t, json.Unmarshal(raw, &info))
	assert.Equal(t, "1.2.3", info.Version)
	assert.Equal(t, "2026-08-15T10:00:00Z", info.BuildTime)
	assert.Equal(t, "go1.25.0", info.GoVersion)
	assert.Equal(t, "abc123", info.CommitHash)
}

// --- exitCodeFor -----------------------------------------------------------

func TestExitCodeForNil(t *testing.T) {
	assert.Equal(t, 0, exitCodeFor(nil))
}

func TestExitCodeForGeneric(t *testing.T) {
	assert.Equal(t, 1, exitCodeFor(assert.AnError))
}

func TestExitCodeForMarker(t *testing.T) {
	err := strings.NewReader("target unreachable [exit=7]")
	_ = err
	// Simulate an error carrying the [exit=N] marker.
	e := &markerErr{msg: "target unreachable [exit=7]"}
	assert.Equal(t, 7, exitCodeFor(e))
}

// markerErr is a minimal error type used to exercise exitCodeFor's marker
// parsing without depending on the errors package.
type markerErr struct{ msg string }

func (m *markerErr) Error() string { return m.msg }

// --- PrintJSON / PrintHuman -------------------------------------------------

func TestPrintJSONWrapsScalar(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, "hello"))
	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	require.NotNil(t, env.Data)
	assert.Equal(t, "hello", env.Data)
}

func TestPrintJSONPassesEnvelopeThrough(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, outputEnvelope{Data: "x", Meta: "m", Error: "e"}))
	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	assert.Equal(t, "x", env.Data)
	assert.Equal(t, "m", env.Meta)
	assert.Equal(t, "e", env.Error)
}

func TestPrintJSONPassesMapEnvelopeThrough(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, map[string]any{
		"data":  42,
		"meta":  nil,
		"error": nil,
	}))
	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	assert.Equal(t, float64(42), env.Data)
}

func TestPrintHumanString(t *testing.T) {
	var buf bytes.Buffer
	PrintHuman(&buf, "hello world")
	assert.Equal(t, "hello world\n", buf.String())
}

func TestPrintHumanMap(t *testing.T) {
	var buf bytes.Buffer
	PrintHuman(&buf, map[string]any{"a": 1, "b": "two"})
	out := buf.String()
	assert.Contains(t, out, "a:")
	assert.Contains(t, out, "1")
	assert.Contains(t, out, "b:")
	assert.Contains(t, out, "two")
}

func TestPrintHumanStruct(t *testing.T) {
	var buf bytes.Buffer
	PrintHuman(&buf, versionInfo{Version: "9.9.9"})
	out := buf.String()
	assert.Contains(t, out, "version:")
	assert.Contains(t, out, "9.9.9")
}

func TestPrintHumanSliceOfMapsAsTable(t *testing.T) {
	var buf bytes.Buffer
	PrintHuman(&buf, []map[string]any{
		{"change_id": "run-1", "status": "running"},
		{"change_id": "run-2", "status": "completed"},
	})
	out := buf.String()
	assert.Contains(t, out, "CHANGE_ID")
	assert.Contains(t, out, "STATUS")
	assert.Contains(t, out, "run-1")
	assert.Contains(t, out, "run-2")
}

func TestPrintHumanNilIsNoop(t *testing.T) {
	var buf bytes.Buffer
	PrintHuman(&buf, nil)
	assert.Empty(t, buf.String())
}
