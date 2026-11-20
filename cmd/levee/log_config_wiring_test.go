package main

// log_config_wiring_test.go — 日志配置面必须真的接到 logger 上，而且没有旁路。
//
// Why this guard exists: the five log fields (`server.log_level`,
// `server.log_format`, `log.level`, `log.format`, `log.output`) were
// validated at load time and then dropped on the floor — the process always
// ran on internal/log's built-in default (info / text / stderr), so
// `log.level: debug` changed nothing at all. A knob that validates but does
// nothing is the worst shape a setting can have: the operator gets a
// boot-time error when they spell it wrong, and silence when they spell it
// right and expect it to work.
//
// Two things have to hold for that not to come back:
//
//  1. every command that loads the configuration goes through loadConfig(),
//     which applies the log settings. A command that reaches for
//     config.Load directly would silently run on the default logger again —
//     so the guard counts the config.Load calls in cmd/levee and requires
//     exactly one, in log_config.go;
//  2. the wiring really reinitialises the process logger, and the three
//     `log.*` fields really are read from the loaded configuration.
//
// The legacy `server.log_*` alias is resolved inside internal/config
// (postProcess mirrors it into log.* when log.* is unset). That resolution is
// pinned behaviourally in internal/config (TestLoad_LogMirrorsServerWhenAbsent)
// rather than textually here — the mirror lives in the config package by
// design, and this guard scans cmd/levee only.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cmdLeveeProductionSources returns every non-test Go file in cmd/levee.
func cmdLeveeProductionSources(t *testing.T) map[string]string {
	t.Helper()
	dir := "."
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoErrorf(t, err, "read %s", name)
		out[name] = string(b)
	}
	require.NotEmpty(t, out, "the scan found no production Go source — the probe would prove nothing")
	return out
}

// functionSource returns the source of the named top-level function, so a
// failed assertion prints the function instead of the whole file.
func functionSource(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "func "+name+"(")
	require.True(t, start >= 0, "function %s not found", name)
	rest := src[start:]
	end := strings.Index(rest, "\n}\n")
	require.True(t, end > 0, "function %s has no closing brace", name)
	return rest[:end]
}

// TestEveryConfigLoadAppliesTheLogSettings is the no-bypass guard.
func TestEveryConfigLoadAppliesTheLogSettings(t *testing.T) {
	sources := cmdLeveeProductionSources(t)

	loads := 0
	for name, src := range sources {
		if !strings.Contains(src, "config.Load(") {
			continue
		}
		loads++
		assert.Equal(t, "log_config.go", name,
			"cmd/levee must load the configuration through loadConfig(), which applies the log settings; "+
				"%s calls config.Load directly and would run on the default logger", name)
	}
	assert.Equal(t, 1, loads,
		"cmd/levee must have exactly one config.Load call — the one inside loadConfig()")
}

// TestLogConfigFieldsAreReadAndApplied pins the other half: the three log.*
// fields are read from the loaded configuration, and the wiring hands them to
// the logger.
//
// Other readers are allowed — `system config get` reports the configured
// values to the operator, which is a read of the same fields — but the
// wiring seam itself must read all three, or one of them would be a knob
// that only the reporting surface knows about.
func TestLogConfigFieldsAreReadAndApplied(t *testing.T) {
	sources := cmdLeveeProductionSources(t)

	wiring, ok := sources["log_config.go"]
	require.True(t, ok, "cmd/levee/log_config.go is the log wiring seam")

	for _, selector := range []string{"Log.Level", "Log.Format", "Log.Output"} {
		assert.Contains(t, wiring, selector,
			"the wiring seam must read %s — otherwise it is a knob only the reporting surface knows", selector)

		found := false
		for _, src := range sources {
			if strings.Contains(src, selector) {
				found = true
				break
			}
		}
		assert.True(t, found, "the log settings are never read: %s", selector)
	}

	assert.Contains(t, functionSource(t, wiring, "loadConfig"), "applyLogConfig(cfg)",
		"loadConfig() must apply the log settings it loaded")
	assert.Contains(t, functionSource(t, wiring, "applyLogConfig"), "log.InitLoggerWithWriter(",
		"the wiring must reinitialise the process logger")
}
