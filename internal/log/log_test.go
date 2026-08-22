package log

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetLogger restores the package singleton to the default text/info logger
// after each test so that tests do not leak state into each other.
func resetLogger() {
	logger = defaultLogger
	slog.SetDefault(defaultLogger)
}

// captureBuffer holds the bytes written by the logger under test.
type captureBuffer struct {
	bytes.Buffer
}

func (c *captureBuffer) Close() error { return nil }

func TestInitLogger_Defaults(t *testing.T) {
	t.Cleanup(resetLogger)

	// Unknown level and format should fall back to info/text, not panic.
	assert.NotPanics(t, func() {
		InitLogger("bogus-level", "bogus-format")
	})

	// Logger() must always return a non-nil logger.
	assert.NotNil(t, Logger())
}

func TestInitLogger_TextFormat(t *testing.T) {
	t.Cleanup(resetLogger)

	var buf captureBuffer
	InitLoggerWithWriter("debug", "text", &buf)

	Debug("hello", "key", "value")
	Info("world", "n", 42)

	out := buf.String()
	// Text handler emits key=value pairs and level=DEBUG/INFO markers.
	assert.Contains(t, out, "hello")
	assert.Contains(t, out, "world")
	assert.Contains(t, out, "key=value")
	assert.Contains(t, out, "n=42")
	assert.Contains(t, out, "level=DEBUG")
	assert.Contains(t, out, "level=INFO")
}

func TestInitLogger_JSONFormat(t *testing.T) {
	t.Cleanup(resetLogger)

	var buf captureBuffer
	InitLoggerWithWriter("info", "json", &buf)

	Info("user-action", "user", "alice", "action", "login")

	out := strings.TrimSpace(buf.String())
	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &record), "output must be valid JSON")

	assert.Equal(t, "INFO", record["level"])
	assert.Equal(t, "user-action", record["msg"])
	assert.Equal(t, "alice", record["user"])
	assert.Equal(t, "login", record["action"])
}

func TestInitLogger_LevelFiltering(t *testing.T) {
	t.Cleanup(resetLogger)

	var buf captureBuffer
	InitLoggerWithWriter("warn", "text", &buf)

	Debug("debug-msg")
	Info("info-msg")
	Warn("warn-msg")
	Error("error-msg")

	out := buf.String()
	// debug and info are below warn and must be suppressed.
	assert.NotContains(t, out, "debug-msg")
	assert.NotContains(t, out, "info-msg")
	assert.Contains(t, out, "warn-msg")
	assert.Contains(t, out, "error-msg")
}

func TestInitLogger_AllLevels(t *testing.T) {
	levels := []string{"debug", "info", "warn", "error"}
	for _, lvl := range levels {
		t.Run(lvl, func(t *testing.T) {
			t.Cleanup(resetLogger)
			assert.NotPanics(t, func() {
				InitLogger(lvl, "json")
			})
			assert.NotNil(t, Logger())
		})
	}
}

func TestInitLogger_CaseInsensitive(t *testing.T) {
	t.Cleanup(resetLogger)

	var buf captureBuffer
	InitLoggerWithWriter("DEBUG", "JSON", &buf)

	Debug("cased", "k", "v")
	out := strings.TrimSpace(buf.String())
	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &record))
	assert.Equal(t, "DEBUG", record["level"])
}

func TestWith_ContextFields(t *testing.T) {
	t.Cleanup(resetLogger)

	var buf captureBuffer
	InitLoggerWithWriter("info", "json", &buf)

	// With returns a child logger that always includes the given fields.
	child := With("component", "engine", "version", "1.0")
	child.Info("started")

	out := strings.TrimSpace(buf.String())
	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &record))
	assert.Equal(t, "engine", record["component"])
	assert.Equal(t, "1.0", record["version"])
	assert.Equal(t, "started", record["msg"])
}

func TestContextAwareVariants(t *testing.T) {
	t.Cleanup(resetLogger)

	type traceKey struct{}
	var buf captureBuffer
	InitLoggerWithWriter("debug", "json", &buf)

	ctx := context.WithValue(context.Background(), traceKey{}, "abc")
	DebugCtx(ctx, "ctx-debug")
	InfoCtx(ctx, "ctx-info")
	WarnCtx(ctx, "ctx-warn")
	ErrorCtx(ctx, "ctx-error")

	out := buf.String()
	assert.Contains(t, out, "ctx-debug")
	assert.Contains(t, out, "ctx-info")
	assert.Contains(t, out, "ctx-warn")
	assert.Contains(t, out, "ctx-error")
}

func TestEnabled(t *testing.T) {
	t.Cleanup(resetLogger)

	InitLogger("warn", "text")
	assert.False(t, Enabled(slog.LevelDebug))
	assert.False(t, Enabled(slog.LevelInfo))
	assert.True(t, Enabled(slog.LevelWarn))
	assert.True(t, Enabled(slog.LevelError))
}

func TestLogger_NonNilBeforeInit(t *testing.T) {
	t.Cleanup(resetLogger)
	// Even without InitLogger, Logger() must return a usable logger.
	assert.NotNil(t, Logger())
	assert.NotPanics(t, func() {
		Info("safe-before-init")
	})
}

func TestString_DoesNotPanic(t *testing.T) {
	t.Cleanup(resetLogger)
	InitLogger("info", "json")
	s := String()
	assert.Contains(t, s, "levee/log")
}
