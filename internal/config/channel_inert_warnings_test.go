package config

// D-9 channel config: an operator-facing warning for fields that validate but
// do not reach a transport.
//
// The code-side gap is already closed and machine-checked
// (cmd/levee/channel_config_wiring_test.go polices that every field is either
// wired or registered with a reason). This attack is on the OPERATOR side:
// a knob that validates but does nothing is the worst shape a setting can
// have. Today, writing `channel.winrm.pool_size: 20` in a config file boots
// cleanly and changes nothing, and writing `channel.winrm.pool_size: 0` fails
// the boot over a field that has no effect. Both teach the operator that the
// section is authoritative, which is exactly what D-9 registers as the harm.
//
// These warnings close that gap without changing any behaviour: the shipped
// defaults are untouched, so a deployment that never mentioned these keys
// stays silent, and one that explicitly set one is told the truth at the
// moment it would otherwise be misled.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// untouchedChannelConfig mirrors what setDefaults + postProcess produce for a
// config file that never mentioned the channel keys.
//
// It goes through the same expansion as production (expectedKeyPath) because
// key_path ships as "~/.ssh/id_rsa" and postProcess expands it before any
// caller sees it. Using the literal here is how this test file's first version
// asserted the wrong thing.
func untouchedChannelConfig(t *testing.T) *Config {
	t.Helper()
	expected, ok := expectedKeyPath()
	require.True(t, ok, "home dir must resolve in a test environment")
	cfg := &Config{}
	cfg.Channel.SSH.KeyPath = expected
	cfg.Channel.SSH.PoolSize = inertSSHPoolSizeDefault
	cfg.Channel.WinRM.Port = inertWinRMPortDefault
	cfg.Channel.WinRM.Transport = inertWinRMTransportDeflt
	cfg.Channel.WinRM.ConnectTimeout = inertWinRMTimeoutDeflt
	cfg.Channel.WinRM.PoolSize = inertWinRMPoolSizeDefault
	return cfg
}

// TestInertChannelWarnings_SilentOnShippedDefaults is the contract that
// matters most: a deployment that never touched these keys must stay quiet.
// A warning on an untouched deployment is the same "validates but means
// nothing" noise this change exists to remove.
func TestInertChannelWarnings_SilentOnShippedDefaults(t *testing.T) {
	warnings := InertChannelWarnings(untouchedChannelConfig(t))
	assert.Empty(t, warnings,
		"a deployment that never set these keys must not be nagged about them")
}

// TestInertChannelWarnings_OnePerExplicitlySetField is the core contract: each
// inert field the operator actually changed produces exactly one note.
func TestInertChannelWarnings_OnePerExplicitlySetField(t *testing.T) {
	cfg := untouchedChannelConfig(t)

	// Two explicit changes, the rest untouched.
	cfg.Channel.WinRM.PoolSize = 20
	cfg.Channel.SSH.KeyPath = "/etc/levee/id_ed25519"

	warnings := InertChannelWarnings(cfg)
	require.Len(t, warnings, 2, "exactly the two changed fields must be reported")

	joined := strings.Join(warnings, "\n")
	assert.Contains(t, joined, "channel.winrm.pool_size",
		"the note must name the field the operator typed")
	assert.Contains(t, joined, "channel.ssh.key_path")
}

// TestInertChannelWarnings_SayWhatGovernsInstead requires every note to name
// what actually governs the behaviour today. "not wired" without that sentence
// rots into "we forgot".
func TestInertChannelWarnings_SayWhatGovernsInstead(t *testing.T) {
	cfg := untouchedChannelConfig(t)
	cfg.Channel.SSH.KeyPath = "/some/other/key"

	warnings := InertChannelWarnings(cfg)
	require.Len(t, warnings, 1)

	note := warnings[0]
	assert.Contains(t, note, "credential",
		"the note must say the resolved credential governs the key path")
	assert.Contains(t, note, "channel.ssh.key_path",
		"the note must name the field the operator typed")
	// A governing explanation, not a bare "has no effect".
	assert.Greater(t, len(note), 80,
		"a one-line 'no effect' is not enough; the operator needs to know what to configure instead")
}

// TestInertChannelWarnings_InventoryStaysAligned keeps the notes tied to the
// machine-checked inventory: a field the wirer connects later must stop being
// reported here, and a field newly added must not be silently ignored.
func TestInertChannelWarnings_InventoryStaysAligned(t *testing.T) {
	inert := InertChannelFields()

	// The inventory is the guard test's registry, restated for operators.
	assert.ElementsMatch(t, []string{
		"channel.ssh.key_path",
		"channel.ssh.pool_size",
		"channel.winrm.port",
		"channel.winrm.transport",
		"channel.winrm.connect_timeout",
		"channel.winrm.pool_size",
	}, inert, "the inert inventory drifted from the wiring guard in cmd/levee")
}

// TestInertChannelWarnings_DefaultKeyPathAfterExpansion is the regression test for
// a false positive found by running the binary, not by the unit tests.
//
// key_path ships as "~/.ssh/id_rsa" and postProcess runs expandHome over it,
// so a config that never mentioned the key arrives here as
// "<home>/.ssh/id_rsa". The first version of this file compared against the
// literal "~/.ssh/id_rsa" and warned on every such deployment. A warning on
// an untouched deployment is a contract violation, not a cosmetic bug: it is
// the same "validates but means nothing" noise D-9 is being closed for.
func TestInertChannelWarnings_DefaultKeyPathAfterExpansion(t *testing.T) {
	expected, ok := expectedKeyPath()
	require.True(t, ok, "home dir must resolve in a test environment")
	require.NotEqual(t, inertSSHKeyPathDefault, expected,
		"premise: postProcess expands ~, so the two forms must differ")

	cfg := &Config{}
	cfg.Channel.SSH.KeyPath = expected // what setDefaults + postProcess produce
	cfg.Channel.SSH.PoolSize = inertSSHPoolSizeDefault
	cfg.Channel.WinRM.Port = inertWinRMPortDefault
	cfg.Channel.WinRM.Transport = inertWinRMTransportDeflt
	cfg.Channel.WinRM.ConnectTimeout = inertWinRMTimeoutDeflt
	cfg.Channel.WinRM.PoolSize = inertWinRMPoolSizeDefault

	assert.Empty(t, InertChannelWarnings(cfg),
		"the expanded default must be recognised as untouched")

	// And an explicitly different path still warns.
	cfg.Channel.SSH.KeyPath = filepath.Join(filepath.Dir(expected), "other_key")
	require.Len(t, InertChannelWarnings(cfg), 1)
}

// TestInertChannelWarnings_ZeroValueIsAChange makes the boundary explicit: an
// operator typing `pool_size: 0` currently gets a boot failure over a no-op
// field. That is at least as confusing as silence, so it must be reported too
// (the boot failure itself stays — it is a real typo signal for the fields
// that ARE wired, and removing validation is a separate decision).
func TestInertChannelWarnings_ZeroValueIsAChange(t *testing.T) {
	cfg := untouchedChannelConfig(t)
	cfg.Channel.WinRM.PoolSize = 0 // != shipped default 5

	warnings := InertChannelWarnings(cfg)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "channel.winrm.pool_size")
}
