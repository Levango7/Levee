package config_test

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/log"
)

// The config side owns the accepted vocabulary (config.LogLevels /
// config.LogFormats); internal/log owns what it actually dispatches. The
// guards below pin the two together in both directions, so a level the
// logger honours cannot be refused by the config (and a spelling the config
// accepts cannot be swallowed by the logger's fallback).
//
// Before this change the config accepted only debug|info|warn|error in exact
// lowercase, while the logger also honoured "warning" and any casing or
// surrounding whitespace: one field, two vocabularies, with the config side
// the narrower one. The same drift-guard shape as the chatops platform and
// the WinRM transport vocabularies.
//
// The guard lives here rather than in internal/log because internal/config
// imports internal/state, which imports internal/log — a test inside
// internal/log could not import this package without creating a cycle. The
// direction of the dependency is therefore the same as for the other
// vocabularies: the implementation exports its table, the config side
// compares.

// TestLogVocabularyTablesAreTheSameTable compares the two lists including
// their order, so the config's error message and the logger's dispatch table
// cannot drift apart unnoticed.
func TestLogVocabularyTablesAreTheSameTable(t *testing.T) {
	assert.Equal(t, log.Levels(), config.LogLevels(),
		"the config's accepted log levels and the logger's dispatch table must be the same list")
	assert.Equal(t, log.Formats(), config.LogFormats(),
		"the config's accepted log formats and the logger's dispatch table must be the same list")
}

// TestLoggerHonoursEveryLevelTheConfigAccepts is the behaviour-level half:
// every spelling the config accepts must actually change what the logger
// emits. A vocabulary that only matched as a list of strings would still pass
// if the logger fell back to info for a spelling the config promised.
func TestLoggerHonoursEveryLevelTheConfigAccepts(t *testing.T) {
	t.Cleanup(func() { log.InitLoggerWithWriter("info", "text", os.Stderr) })

	want := map[string][]string{
		"debug":   {"DEBUG", "INFO", "WARN", "ERROR"},
		"info":    {"INFO", "WARN", "ERROR"},
		"warn":    {"WARN", "ERROR"},
		"warning": {"WARN", "ERROR"},
		"error":   {"ERROR"},
	}
	for _, spelling := range config.LogLevels() {
		t.Run(spelling, func(t *testing.T) {
			expected, ok := want[spelling]
			require.True(t, ok,
				"the config accepts log level %q but this guard does not know what it means", spelling)

			var buf bytes.Buffer
			log.InitLoggerWithWriter(spelling, "text", &buf)
			log.Debug("m-debug")
			log.Info("m-info")
			log.Warn("m-warn")
			log.Error("m-error")
			out := buf.String()

			for _, level := range expected {
				assert.Contains(t, out, "level="+level,
					"log level %q must let %s through", spelling, level)
			}
			for _, level := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
				if !slices.Contains(expected, level) {
					assert.NotContains(t, out, "level="+level,
						"log level %q must filter %s out", spelling, level)
				}
			}
		})
	}
}

// TestLoggerHonoursEveryFormatTheConfigAccepts is the same idea for the
// format: "json" must produce a JSON record and "text" a text record — not
// both quietly landing on the text fallback.
func TestLoggerHonoursEveryFormatTheConfigAccepts(t *testing.T) {
	t.Cleanup(func() { log.InitLoggerWithWriter("info", "text", os.Stderr) })

	for _, spelling := range config.LogFormats() {
		t.Run(spelling, func(t *testing.T) {
			var buf bytes.Buffer
			log.InitLoggerWithWriter("info", spelling, &buf)
			log.Info("m-format")
			out := strings.TrimSpace(buf.String())
			require.NotEmpty(t, out)

			switch spelling {
			case "json":
				var rec map[string]any
				require.NoError(t, json.Unmarshal([]byte(out), &rec),
					"log format %q must produce a JSON record, got %q", spelling, out)
				assert.Equal(t, "m-format", rec["msg"])
			case "text":
				assert.Contains(t, out, "msg=m-format",
					"log format %q must produce a text record, got %q", spelling, out)
			default:
				t.Fatalf("config.LogFormats() grew a spelling this guard does not know: %q", spelling)
			}
		})
	}
}

// TestLogVocabularyTolerance pins the comparison both sides use:
// case-insensitive and whitespace-tolerant, so the same config file is
// accepted and honoured regardless of how it is written.
func TestLogVocabularyTolerance(t *testing.T) {
	for _, spelling := range []string{"DEBUG", " debug ", "Debug"} {
		assert.True(t, config.LogLevelAccepted(spelling),
			"the config must accept %q", spelling)
	}
	for _, spelling := range []string{"WARNING", " warning ", "Warning"} {
		assert.True(t, config.LogLevelAccepted(spelling),
			"the config must accept %q", spelling)
	}
	for _, spelling := range []string{"JSON", " json "} {
		assert.True(t, config.LogFormatAccepted(spelling),
			"the config must accept %q", spelling)
	}
}

// TestLogVocabularyRefusesUndispatchedSpellings keeps the config side a gate
// rather than a rubber stamp: a value the logger would silently fold into
// its fallback must be refused at load time, so the operator learns about
// the typo instead of wondering why the level did not change.
func TestLogVocabularyRefusesUndispatchedSpellings(t *testing.T) {
	for _, spelling := range []string{"trace", "verbose", "fatal", "notice"} {
		assert.False(t, config.LogLevelAccepted(spelling),
			"the config must refuse log level %q", spelling)
	}
	for _, spelling := range []string{"xml", "logfmt", "console"} {
		assert.False(t, config.LogFormatAccepted(spelling),
			"the config must refuse log format %q", spelling)
	}
}
