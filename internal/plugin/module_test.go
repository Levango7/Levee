// module_test.go pins the module plugin adapter: the meta handshake turns a
// registry row into a declared surface (actions + idempotency), a module that
// declares nothing is refused, one invocation maps onto executor.ModuleOutput,
// and every refusal path (validate, execute, missing binary) fails closed with
// the plugin named. The child binary is the test itself re-executed, the same
// harness protocol_test.go uses.
package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moduleRecord builds the registry row the adapter consumes. BinaryPath points
// at the test binary; the child personality is selected by childEnvKey.
func moduleRecord(name string) *RegistryRecord {
	return &RegistryRecord{
		Name:       name,
		Type:       TypeModule,
		State:      StateEnabled,
		BinaryPath: os.Args[0],
	}
}

// testTarget is the addressing the adapter forwards to the plugin.
type testTarget struct {
	host string
	port int
	typ  string
}

func (t testTarget) Host() string                       { return t.host }
func (t testTarget) Port() int                          { return t.port }
func (t testTarget) Type() string                       { return t.typ }
func (t testTarget) Credentials() channel.CredentialRef { return channel.CredentialRef{} }

// Only an enabled module-type record can become a module: the other two states
// and the other three types are refusals, not silent skips.
func TestNewPluginModuleRefusesBadRecords(t *testing.T) {
	t.Setenv(childEnvKey, "module")

	_, err := NewPluginModule(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil registry record")

	rec := moduleRecord("wrong-type")
	rec.Type = TypeGate
	_, err = NewPluginModule(context.Background(), rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPluginType)

	rec = moduleRecord("not-enabled")
	rec.State = StateDisabled
	_, err = NewPluginModule(context.Background(), rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPluginNotEnabled)
}

// The meta handshake is what makes early action validation possible: the
// declared verbs reach Actions() and the idempotency flag reaches Idempotent().
func TestNewPluginModuleReadsDeclaredSurface(t *testing.T) {
	t.Setenv(childEnvKey, "module")

	m, err := NewPluginModule(context.Background(), moduleRecord("cloud-apply"))
	require.NoError(t, err)

	assert.Equal(t, "cloud-apply", m.Name())
	assert.Equal(t, []string{"apply", "destroy"}, m.Actions())
	assert.True(t, m.Idempotent())
	assert.False(t, m.NeedsChannel(), "a module plugin owns its transport")
}

// A module that declares no action list would register a capability that
// accepts nothing, so the record is refused rather than registered.
func TestNewPluginModuleRefusesModulesWithoutActions(t *testing.T) {
	t.Setenv(childEnvKey, "module-noactions")

	_, err := NewPluginModule(context.Background(), moduleRecord("silent"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declares no actions")
}

// A binary that disappeared after install is refused at construction, naming
// the path — the boot-time usability question, not a first-step surprise.
func TestNewPluginModuleRefusesMissingBinary(t *testing.T) {
	rec := moduleRecord("gone")
	rec.BinaryPath = filepath.Join(t.TempDir(), "not-there")

	_, err := NewPluginModule(context.Background(), rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary not found")
}

// One invocation maps the plugin's ModuleResult onto executor.ModuleOutput,
// including the changed flag and duration the audit trail records.
func TestPluginModuleExecuteMapsResult(t *testing.T) {
	t.Setenv(childEnvKey, "module")

	rec := moduleRecord("cloud-apply")
	rec.ConfigYAML = "region: cn-north-1"
	m, err := NewPluginModule(context.Background(), rec)
	require.NoError(t, err)

	out, err := m.Execute(context.Background(), "apply", executor.ModuleInput{
		Args:   map[string]any{"name": "bucket-a"},
		Target: testTarget{host: "inventory-row", port: 22, typ: "ssh"},
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, 0, out.ExitCode)
	assert.Equal(t, "applied apply", out.Stdout)
	assert.True(t, out.Changed)
	assert.Equal(t, int64(42)*1e6, int64(out.Duration))
}

// A plugin refusal surfaces as the step error with the plugin's own words —
// the run must not read an upstream 500 as a success.
func TestPluginModuleExecuteRefusesPluginError(t *testing.T) {
	t.Setenv(childEnvKey, "module")

	m, err := NewPluginModule(context.Background(), moduleRecord("cloud-apply"))
	require.NoError(t, err)

	_, err = m.Execute(context.Background(), "explode", executor.ModuleInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `execute refused action "explode"`)
	assert.Contains(t, err.Error(), "upstream API returned 500")
}

// Validate runs before Execute: a malformed step is stopped before it mutates
// anything, and the reason is the plugin's.
func TestPluginModuleValidateRefusalStopsBeforeExecute(t *testing.T) {
	t.Setenv(childEnvKey, "module")

	m, err := NewPluginModule(context.Background(), moduleRecord("cloud-apply"))
	require.NoError(t, err)

	_, err = m.Execute(context.Background(), "bad_input", executor.ModuleInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `validate refused action "bad_input"`)
	assert.Contains(t, err.Error(), "args missing region")
}

// The wire shape forwarded to the plugin: verb, args, and the target as
// addressing (the plugin has no channel to the inventory row).
func TestModuleParamsCarriesActionArgsAndTarget(t *testing.T) {
	params := moduleParams("apply", executor.ModuleInput{
		Args:   map[string]any{"name": "bucket-a"},
		Target: testTarget{host: "h1", port: 2222, typ: "winrm"},
	})

	assert.Equal(t, "apply", params["action"])
	assert.Equal(t, map[string]any{"name": "bucket-a"}, params["args"])
	assert.Equal(t, map[string]any{"host": "h1", "port": 2222, "type": "winrm"}, params["target"])
}

// Nil args are sent as an empty map (not null) and a nil target omits the
// key: the plugin always sees a well-formed params object.
func TestModuleParamsHandlesEmptyInput(t *testing.T) {
	params := moduleParams("apply", executor.ModuleInput{})

	assert.Equal(t, "apply", params["action"])
	assert.Equal(t, map[string]any{}, params["args"])
	_, hasTarget := params["target"]
	assert.False(t, hasTarget)
}
