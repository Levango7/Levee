// Package log provides a thin wrapper around the standard library log/slog
// package. It exposes a process-wide singleton logger that can be initialised
// once at startup and then used through the package-level Debug/Info/Warn/Error
// helpers without callers having to thread a *slog.Logger through every call
// site.
//
// Two output formats are supported: "json" (machine-parseable, recommended for
// production) and "text" (human-readable, recommended for development). Four
// severity levels are supported: debug, info, warn, error. Unknown levels fall
// back to info; unknown formats fall back to text.
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// defaultLogger is the zero-value logger used before InitLogger is called. It
// writes to stderr at info level in text format so that early-boot messages are
// not lost even when the caller forgets to initialise the logger.
var defaultLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// logger is the active singleton.
//
// It is stored atomically on purpose. The logger used to be written exactly
// once, before any goroutine existed, so a plain variable was safe. Now that
// the CLI actually configures the logger at start-up (and tests reconfigure it
// while background goroutines are still logging), a plain variable would race
// with every concurrent reader. atomic.Pointer makes the swap safe for
// readers that only ever load the pointer.
var logger atomic.Pointer[slog.Logger]

func init() {
	logger.Store(defaultLogger)
}

// currentLogger returns the active singleton. It is never nil: before the
// first Store (and if a caller ever stored a nil pointer) it falls back to
// the default logger.
func currentLogger() *slog.Logger {
	if l := logger.Load(); l != nil {
		return l
	}
	return defaultLogger
}

// InitLogger configures the package-level singleton logger. It must be called
// once during process start-up, before any logging calls. Calling it again
// replaces the previous logger (useful in tests).
//
// level  — one of "debug", "info", "warn", "error" (case-insensitive).
// format — one of "json", "text" (case-insensitive).
//
// Unknown levels default to info; unknown formats default to text. Output is
// written to os.Stderr. This function never panics.
func InitLogger(level string, format string) {
	l := newLogger(level, format, os.Stderr)
	logger.Store(l)
	// Make the standard library's slog.Default() point at the same handler so
	// that third-party libraries using slog.Default() also honour our config.
	slog.SetDefault(l)
}

// InitLoggerWithWriter is like InitLogger but writes to the given writer
// instead of os.Stderr. It is primarily intended for tests that want to
// capture log output.
func InitLoggerWithWriter(level string, format string, w io.Writer) {
	l := newLogger(level, format, w)
	logger.Store(l)
	slog.SetDefault(l)
}

// levelSpec pairs one accepted level spelling with the slog level it maps
// to. The table IS the implementation's vocabulary: Levels() reports its
// names and parseLevel reads it, so a spelling cannot be listed without being
// dispatched (or dispatched without being listed).
type levelSpec struct {
	name  string
	level slog.Level
}

var levelSpecs = []levelSpec{
	{"debug", slog.LevelDebug},
	{"info", slog.LevelInfo},
	{"warn", slog.LevelWarn},
	{"warning", slog.LevelWarn},
	{"error", slog.LevelError},
}

// formatSpec pairs one accepted format spelling with the handler it selects.
type formatSpec struct {
	name string
	json bool
}

var formatSpecs = []formatSpec{
	{"text", false},
	{"json", true},
}

// Levels lists the level spellings the logger dispatches on, in table order.
// It is the implementation side of the vocabulary: the config side has its
// own list (config.LogLevels), and the guard that keeps the two together
// lives in internal/config — it cannot live here, because internal/config
// imports internal/state, which imports this package.
func Levels() []string {
	out := make([]string, 0, len(levelSpecs))
	for _, s := range levelSpecs {
		out = append(out, s.name)
	}
	return out
}

// Formats lists the format spellings the logger dispatches on, in table
// order. See Levels for why the guard lives on the other side.
func Formats() []string {
	out := make([]string, 0, len(formatSpecs))
	for _, s := range formatSpecs {
		out = append(out, s.name)
	}
	return out
}

// newLogger builds a *slog.Logger from the human-friendly level/format names.
func newLogger(level string, format string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if formatIsJSON(format) {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}

// formatIsJSON reports whether the format spelling selects the JSON handler.
// Unknown spellings fall back to text.
func formatIsJSON(format string) bool {
	t := strings.ToLower(strings.TrimSpace(format))
	for _, s := range formatSpecs {
		if t == s.name {
			return s.json
		}
	}
	return false
}

// parseLevel converts a string level name to an slog.Level. The comparison is
// case-insensitive and whitespace-tolerant, matching the config side
// (config.LogLevelAccepted). Unknown values default to LevelInfo.
func parseLevel(level string) slog.Level {
	t := strings.ToLower(strings.TrimSpace(level))
	for _, s := range levelSpecs {
		if t == s.name {
			return s.level
		}
	}
	return slog.LevelInfo // unknown → info
}

// Logger returns the current singleton *slog.Logger. It is guaranteed non-nil
// even before InitLogger is called.
func Logger() *slog.Logger {
	return currentLogger()
}

// With returns a new *slog.Logger that always includes the given key-value
// pairs in every emitted record. It is the package-level equivalent of
// slog.Logger.With.
func With(args ...any) *slog.Logger {
	return currentLogger().With(args...)
}

// --- Convenience wrappers around the singleton logger -----------------------

// Debug logs at Debug level with the given message and structured key-value
// pairs. The pairs must be a sequence of alternating keys (string) and values.
func Debug(msg string, args ...any) {
	currentLogger().Debug(msg, args...)
}

// Info logs at Info level.
func Info(msg string, args ...any) {
	currentLogger().Info(msg, args...)
}

// Warn logs at Warn level.
func Warn(msg string, args ...any) {
	currentLogger().Warn(msg, args...)
}

// Error logs at Error level.
func Error(msg string, args ...any) {
	currentLogger().Error(msg, args...)
}

// --- Context-aware variants -------------------------------------------------

// DebugCtx is the context-aware variant of Debug.
func DebugCtx(ctx context.Context, msg string, args ...any) {
	currentLogger().DebugContext(ctx, msg, args...)
}

// InfoCtx is the context-aware variant of Info.
func InfoCtx(ctx context.Context, msg string, args ...any) {
	currentLogger().InfoContext(ctx, msg, args...)
}

// WarnCtx is the context-aware variant of Warn.
func WarnCtx(ctx context.Context, msg string, args ...any) {
	currentLogger().WarnContext(ctx, msg, args...)
}

// ErrorCtx is the context-aware variant of Error.
func ErrorCtx(ctx context.Context, msg string, args ...any) {
	currentLogger().ErrorContext(ctx, msg, args...)
}

// --- Level helpers ----------------------------------------------------------

// Enabled reports whether the given level is enabled on the current logger.
// Callers can use it to avoid expensive argument construction when the level
// is filtered out.
func Enabled(level slog.Level) bool {
	return currentLogger().Enabled(context.Background(), level)
}

// String returns a human-readable description of the current logger
// configuration, primarily for diagnostics and startup banners.
func String() string {
	return fmt.Sprintf("levee/log: singleton logger ready (handler=%T)", currentLogger().Handler())
}
