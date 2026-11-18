package main

// channel_config_wiring_test.go — 通道配置面的每个字段，要么真的接进通道，
// 要么被显式登记为「已知未接线」并写明理由。
//
// Why this guard exists: the `channel.ssh.*` / `channel.winrm.*` config
// sections are validated at load time — a bad value fails the boot with a
// named message — which reads exactly like a setting that does something.
// It does not. Measured on 2026-11-18: of the thirteen fields, only four
// ever reach a transport (strict_host_check, known_hosts, become_method,
// become_user, forwarded by cmd_serve.go through
// sshchannel.SetDefaultConfig). The other nine are validated and then
// dropped on the floor:
//
//   - channel.ssh.auth_method — the SSH channel's Config.AuthMethod is
//     empty in production, so buildAuthMethods infers the method from the
//     credential instead of from the operator's setting;
//   - channel.ssh.key_path — the key path comes from the resolved
//     credential (CredentialRef.KeyPath), not from the config file;
//   - channel.ssh.port — the port comes from Target.Port();
//   - channel.ssh.connect_timeout — the channel uses its own
//     DefaultConnectTimeout (30s), while the config default says 10s;
//   - channel.ssh.pool_size — the SSH pool carries its own defaults;
//   - channel.winrm.* — the WinRM factory is registered with Config{}
//     (internal/channel/winrm's init), so the whole section is inert.
//
// A knob that validates but does nothing is the worst shape a setting can
// have: the operator gets a boot-time error when they spell it wrong, and
// silence when they spell it right and expect it to work.
//
// This guard does not decide what the right semantics are (that is a product
// decision, registered as D-9 in docs/blocked-items.md). It makes the
// inventory machine-checkable so the gap cannot grow silently: a new field
// added to either config struct turns this test red until it is either
// wired or registered here with a reason.
//
// The wiring check is textual on purpose: it looks for the selector
// `Channel.<section>.<Field>` in production Go source outside
// internal/config. A field reached through a copied struct
// (`sshCfg := cfg.Channel.SSH; … sshCfg.AuthMethod`) does not match — that
// is intentional. The inventory must name the configuration surface
// explicitly, because "somebody wired it somewhere" is not something this
// test can honestly assert.

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/config"
)

// knownUnwiredChannelConfig lists the channel-config fields that do NOT
// reach a transport today, each with the reason and what actually governs
// the behaviour instead. The reasons are load-bearing: a bare "not wired"
// would rot into "we forgot", and this file is the only place that says so.
//
// Keys are the selector the wiring check looks for, i.e.
// `Channel.<section>.<Field>`.
var knownUnwiredChannelConfig = map[string]string{
	"Channel.SSH.AuthMethod": "the SSH channel's own Config.AuthMethod is empty in " +
		"production (ssh.NewConfig never sets it), so buildAuthMethods infers the " +
		"method from the resolved credential — the operator's auth_method does not " +
		"decide anything. Governing behaviour: the credential's key/password shape.",
	"Channel.SSH.KeyPath": "the private key path comes from the resolved credential " +
		"(channel.CredentialRef.KeyPath), not from the config file; the config value " +
		"is only a default for the credential store to consult.",
	"Channel.SSH.Port": "the port comes from the target (Target.Port()), and the " +
		"channel falls back to its own DefaultPort when the target has none; " +
		"the config value never overrides a per-target port.",
	"Channel.SSH.ConnectTimeout": "the channel uses its own DefaultConnectTimeout " +
		"(30s); the config default (10s) is not forwarded.",
	"Channel.SSH.PoolSize": "the SSH connection pool carries its own defaults " +
		"(internal/channel/ssh/pool.go); the config value is not forwarded.",
	"Channel.WinRM.Port": "the WinRM factory is registered with Config{} " +
		"(internal/channel/winrm's init), so the whole winrm section is inert; " +
		"the port comes from the target / the channel's DefaultPort.",
	"Channel.WinRM.Transport": "same: the factory is registered with Config{}, so " +
		"the transport is always the channel default (negotiate). The vocabulary is " +
		"also mismatched — the config accepts negotiate|kerberos while the channel " +
		"implements negotiate|ntlm — which stays invisible until the section is wired.",
	"Channel.WinRM.ConnectTimeout": "the WinRM channel uses its own DefaultTimeout " +
		"(30s); the config value is not forwarded.",
	"Channel.WinRM.PoolSize": "the WinRM pool carries its own defaults; the config " +
		"value is not forwarded.",
}

// channelConfigSelectors enumerates every field of the channel config
// sections by reflection, so a field added to the config structs shows up
// here without anyone remembering to update a list.
func channelConfigSelectors(t *testing.T) []string {
	t.Helper()
	cfgType := reflect.TypeOf(config.Config{})
	channelField, ok := cfgType.FieldByName("Channel")
	require.True(t, ok, "config.Config has no Channel field")

	var selectors []string
	for _, section := range []string{"SSH", "WinRM"} {
		sectionField, ok := channelField.Type.FieldByName(section)
		require.Truef(t, ok, "config.ChannelConfig has no %s field", section)
		for i := 0; i < sectionField.Type.NumField(); i++ {
			f := sectionField.Type.Field(i)
			selectors = append(selectors, "Channel."+section+"."+f.Name)
		}
	}
	sort.Strings(selectors)
	return selectors
}

// productionGoSources returns every non-test Go file under cmd/ and internal/,
// except internal/config (which defines and validates the fields — reading it
// would make every field look "wired").
func productionGoSources(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	var files []string
	for _, dir := range []string{"cmd", "internal"} {
		require.NoError(t, filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "config" && filepath.Base(filepath.Dir(path)) == "internal" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files = append(files, path)
			return nil
		}))
	}
	require.NotEmpty(t, files, "the scan found no production Go source — the probe would prove nothing")
	return files
}

func TestChannelConfigFieldsAreWiredOrRegistered(t *testing.T) {
	files := productionGoSources(t)
	contents := make([]string, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoErrorf(t, err, "read %s", f)
		contents = append(contents, string(b))
	}
	joined := strings.Join(contents, "\n")

	for _, selector := range channelConfigSelectors(t) {
		reason, registered := knownUnwiredChannelConfig[selector]
		wired := strings.Contains(joined, selector)
		switch {
		case wired && registered:
			t.Errorf("%s is wired in production code AND listed as unwired — "+
				"the inventory is stale; drop it from knownUnwiredChannelConfig", selector)
		case wired:
			// The good case: the operator's setting reaches a transport.
		case registered:
			assert.NotEmptyf(t, reason,
				"%s is registered as unwired but carries no reason — a bare "+
					"'not wired' rots into 'we forgot'", selector)
		default:
			t.Errorf("%s is neither wired into a transport nor registered in "+
				"knownUnwiredChannelConfig: a config field that validates but does "+
				"nothing is the worst shape a setting can have. Wire it (the SSH "+
				"seam is sshchannel.SetDefaultConfig in cmd/levee/cmd_serve.go), or "+
				"register it with the reason and what actually governs the behaviour "+
				"today — see docs/blocked-items.md D-9", selector)
		}
	}
}

func TestKnownUnwiredChannelConfigEntriesStillExist(t *testing.T) {
	existing := map[string]bool{}
	for _, s := range channelConfigSelectors(t) {
		existing[s] = true
	}
	for selector := range knownUnwiredChannelConfig {
		assert.Truef(t, existing[selector],
			"knownUnwiredChannelConfig names %q, which is no longer a field of the "+
				"channel config sections — a stale entry hides the fact that the "+
				"field was removed or renamed", selector)
	}
}

// TestChannelConfigInventoryIsNotEmpty keeps the probe from passing
// vacuously: an empty selector list would make every assertion above
// trivially true.
func TestChannelConfigInventoryIsNotEmpty(t *testing.T) {
	selectors := channelConfigSelectors(t)
	assert.GreaterOrEqual(t, len(selectors), 13,
		"the channel config inventory shrank — the probe no longer covers the "+
			"fields it is supposed to police")
}
