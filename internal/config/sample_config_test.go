// The two shipped configuration examples are things operators copy, so they are
// judgement surfaces: both must load and validate, and the driver vocabulary
// they teach in comments must be the vocabulary the program actually accepts.
// Before this file neither was checked — `configs/config.yaml` had no `database`
// section at all, and `config.example.yaml` still said PostgreSQL was a future
// plan while `serve --cluster --pg-dsn` had been running on it for versions.
package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/nexus/levee/internal/config"
)

// shippedConfigs are the configuration artifacts the repository hands out.
var shippedConfigs = []string{
	filepath.Join("..", "..", "configs", "config.yaml"),
	filepath.Join("..", "..", "config.example.yaml"),
}

func TestShippedSampleConfigLoads(t *testing.T) {
	for _, p := range shippedConfigs {
		t.Run(filepath.Base(p), func(t *testing.T) {
			cfg, err := config.Load(p)
			require.NoError(t, err, "the example must load and validate: operators copy it")
			assert.Equal(t, "sqlite", cfg.Database.Driver,
				"both examples ship the default driver; a reader must see the same value the binary defaults to")
			assert.NotEmpty(t, cfg.Database.Path, "database.path must survive postProcess")
		})
	}
}

// TestShippedSampleChannelSectionUsesTheRealSchema guards the nesting of the
// shipped examples. The loader reads `channel.ssh` / `channel.winrm`; a
// top-level `ssh:` / `winrm:` is silently ignored by the YAML decoder, so the
// sample would show values that never reach the transport — the exact class of
// fake knob this section is supposed to eliminate.
func TestShippedSampleChannelSectionUsesTheRealSchema(t *testing.T) {
	for _, p := range shippedConfigs {
		t.Run(filepath.Base(p), func(t *testing.T) {
			raw, err := os.ReadFile(p)
			require.NoError(t, err)

			var doc map[string]any
			require.NoError(t, yaml.Unmarshal(raw, &doc))
			require.Contains(t, doc, "channel",
				"%s must nest the channel section where the loader reads it", p)
			assert.NotContains(t, doc, "ssh",
				"%s has a top-level ssh section; the loader only reads channel.ssh", p)
			assert.NotContains(t, doc, "winrm",
				"%s has a top-level winrm section; the loader only reads channel.winrm", p)
		})
	}
}

// TestShippedSampleSSHDefaultsAreUnset pins the "not configured" semantics on
// both shipped examples. A sample that writes port 22 / auth_method key /
// connect_timeout 10s would, once copied, silently override the target port,
// force key auth and shorten the channel's 30s timeout.
func TestShippedSampleSSHDefaultsAreUnset(t *testing.T) {
	for _, p := range shippedConfigs {
		t.Run(filepath.Base(p), func(t *testing.T) {
			cfg, err := config.Load(p)
			require.NoError(t, err)
			assert.Zero(t, cfg.Channel.SSH.Port,
				"%s must leave the port unset so the target's own port wins", p)
			assert.Empty(t, cfg.Channel.SSH.AuthMethod,
				"%s must leave auth_method unset so credentials decide", p)
			assert.Zero(t, cfg.Channel.SSH.ConnectTimeout,
				"%s must leave connect_timeout unset so the channel default (30s) applies", p)
		})
	}
}

// TestShippedSampleLogSettingsMirrorTheServerAlias keeps the shipped samples
// honest about the log precedence. The samples leave log.level / log.format
// unset, so the legacy server.log_* alias is what actually decides the
// logger. A sample that hard-coded log.level would silently win over the
// alias and turn the server.* keys in the very same file into dead settings.
func TestShippedSampleLogSettingsMirrorTheServerAlias(t *testing.T) {
	for _, p := range shippedConfigs {
		t.Run(filepath.Base(p), func(t *testing.T) {
			cfg, err := config.Load(p)
			require.NoError(t, err)
			assert.Equal(t, cfg.Server.LogLevel, cfg.Log.Level,
				"%s must leave log.level unset so the server.log_level alias decides", p)
			assert.Equal(t, cfg.Server.LogFormat, cfg.Log.Format,
				"%s must leave log.format unset so the server.log_format alias decides", p)
			assert.NotEmpty(t, cfg.Log.Output, "%s must name a log sink", p)
		})
	}
}

// TestSampleDriverCommentMatchesAcceptedDrivers reads the driver list out of each
// example's own comment and asks the validator about every entry. A comment that
// advertises a driver the program refuses — or omits one it accepts — is the
// documentation drifting away from the gate, which is what an operator then
// copies into a deployment that fails at startup.
func TestSampleDriverCommentMatchesAcceptedDrivers(t *testing.T) {
	for _, p := range shippedConfigs {
		t.Run(filepath.Base(p), func(t *testing.T) {
			comment := driverCommentOf(t, p)
			require.Contains(t, comment, "sqlite",
				"%s must advertise the accepted drivers on its database.driver line", p)

			for _, name := range strings.Split(comment, "|") {
				name = strings.TrimSpace(name)
				require.NotEmpty(t, name, "empty driver name in %q", comment)

				cfg, err := config.Load(p)
				require.NoError(t, err)
				cfg.Database.Driver = name
				if name != "sqlite" {
					cfg.Database.DSN = "postgres://levee@127.0.0.1:5432/levee"
				}
				assert.NoError(t, config.Validate(cfg),
					"driver %q is advertised by %s but rejected by Validate", name, p)
			}

			// A driver the program does not implement must still be refused, or
			// the loop above proves nothing: an always-accepting validator would
			// pass every advertised name too.
			cfg, err := config.Load(p)
			require.NoError(t, err)
			cfg.Database.Driver = "mysql"
			assert.Error(t, config.Validate(cfg), "an unimplemented driver must not validate")
		})
	}
}

// TestExampleCoversTheClusterShapeOnTheRightDatabase guards the specific mistake
// this change exists to prevent: a cluster example that points the CLI at a
// local SQLite file while the server runs on PostgreSQL. The two would both
// "succeed" and freeze periods would be written where nothing reads them.
func TestExampleCoversTheClusterShapeOnTheRightDatabase(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	require.NoError(t, err)
	text := string(raw)

	require.Contains(t, text, "场景二：多节点集群", "the cluster scenario block moved; update this gate")
	block := text[strings.Index(text, "场景二：多节点集群"):]
	if cut := strings.Index(block, "场景三"); cut > 0 {
		block = block[:cut]
	}
	assert.Contains(t, block, "driver: postgres",
		"a cluster example that says sqlite is an invitation to split the deployment across two databases")
	assert.Contains(t, block, "dsn:", "the cluster example must show database.dsn")
	assert.NotContains(t, block, "仅支持 sqlite", "the example still claims SQLite is the only supported driver")
}

// driverCommentOf returns the inline comment of the first active (uncommented)
// `driver:` line in the file.
func driverCommentOf(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "shipped config %s moved", path)
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "driver:") && strings.Contains(trimmed, "#") {
			return strings.TrimSpace(strings.SplitN(trimmed, "#", 2)[1])
		}
	}
	require.FailNow(t, "no `driver:` line with an inline comment in %s", path)
	return ""
}
