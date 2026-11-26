package main

// channel_config_wiring_test.go — 通道配置面的每个字段，要么真的接进通道，
// 要么被显式登记为「已知未接线」并写明理由。
//
// Why this guard exists: the `channel.ssh.*` / `channel.winrm.*` config
// sections are validated at load time — a bad value fails the boot with a
// named message — which reads exactly like a setting that does something.
// It does not. Measured on 2026-11-18: of the thirteen fields, only four
// originally reached a transport (strict_host_check, known_hosts,
// become_method, become_user). The D-9 wiring then connected SSH auth_method,
// port, connect_timeout (total seven) and — in a later pass — all four
// channel.winrm.* fields via winrm.SetDefaultConfig, bringing the wired total
// to eleven. The registered WinRM factory now reads those defaults at Create
// time instead of a stale Config{} snapshot. The remaining two are validated
// and then dropped on the floor:
//
//   - channel.ssh.key_path — the key path comes from the resolved
//     credential (CredentialRef.KeyPath), not from the config file;
//   - channel.ssh.pool_size — the SSH pool carries its own defaults.
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
	"Channel.SSH.KeyPath": "the private key path comes from the resolved credential " +
		"(channel.CredentialRef.KeyPath), not from the config file; the config value " +
		"is expanded and validated but not forwarded to the transport.",
	"Channel.SSH.PoolSize": "the SSH connection pool carries its own defaults " +
		"(internal/channel/ssh/pool.go); the config value is not forwarded.",
	// WinRM port/transport/connect_timeout/pool_size are all forwarded via
	// winrm.SetDefaultConfig (D-9); the registered factory reads process
	// defaults at Create time. NOTE: pool_size only reaches DIRECT winrm.NewPool
	// callers via DefaultPoolSize — the production serve path builds no WinRM
	// pool, so its runtime effect there is nil (documented in cmd_serve.go and
	// CHANGELOG). The wiring itself is real; the reach is honestly scoped.
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
