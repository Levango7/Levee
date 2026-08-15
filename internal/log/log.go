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
)

// defaultLogger is the zero-value logger used before InitLogger is called. It
// writes to stderr at info level in text format so that early-boot messages are
// not lost even when the caller forgets to initialise the logger.
var defaultLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// logger is the active singleton. It always points to a non-nil *slog.Logger.
var logger = defaultLogger

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
	logger = newLogger(level, format, os.Stderr)
	// Make the standard library's slog.Default() point at the same handler so
	// that third-party libraries using slog.Default() also honour our config.
	slog.SetDefault(logger)
}

// InitLoggerWithWriter is like InitLogger but writes to the given writer
// instead of os.Stderr. It is primarily intended for tests that want to
// capture log output.
func InitLoggerWithWriter(level string, format string, w io.Writer) {
	logger = newLogger(level, format, w)
	slog.SetDefault(logger)
}

// newLogger builds a *slog.Logger from the human-friendly level/format names.
func newLogger(level string, format string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		handler = slog.NewJSONHandler(w, opts)
	default: // "text" or anything else
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}

// parseLevel converts a string level name to an slog.Level. Unknown values
// default to LevelInfo.
func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default: // "info" or anything else
		return slog.LevelInfo
	}
}

// Logger returns the current singleton *slog.Logger. It is guaranteed non-nil
// even before InitLogger is called.
func Logger() *slog.Logger {
	return logger
}

// With returns a new *slog.Logger that always includes the given key-value
// pairs in every emitted record. It is the package-level equivalent of
// slog.Logger.With.
func With(args ...any) *slog.Logger {
	return logger.With(args...)
}

// --- Convenience wrappers around the singleton logger -----------------------

// Debug logs at Debug level with the given message and structured key-value
// pairs. The pairs must be a sequence of alternating keys (string) and values.
func Debug(msg string, args ...any) {
	logger.Debug(msg, args...)
}

// Info logs at Info level.
func Info(msg string, args ...any) {
	logger.Info(msg, args...)
}

// Warn logs at Warn level.
func Warn(msg string, args ...any) {
	logger.Warn(msg, args...)
}

// Error logs at Error level.
func Error(msg string, args ...any) {
	logger.Error(msg, args...)
}

// --- Context-aware variants -------------------------------------------------

// DebugCtx is the context-aware variant of Debug.
func DebugCtx(ctx context.Context, msg string, args ...any) {
	logger.DebugContext(ctx, msg, args...)
}

// InfoCtx is the context-aware variant of Info.
func InfoCtx(ctx context.Context, msg string, args ...any) {
	logger.InfoContext(ctx, msg, args...)
}

// WarnCtx is the context-aware variant of Warn.
func WarnCtx(ctx context.Context, msg string, args ...any) {
	logger.WarnContext(ctx, msg, args...)
}

// ErrorCtx is the context-aware variant of Error.
func ErrorCtx(ctx context.Context, msg string, args ...any) {
	logger.ErrorContext(ctx, msg, args...)
}

// --- Level helpers ----------------------------------------------------------

// Enabled reports whether the given level is enabled on the current logger.
// Callers can use it to avoid expensive argument construction when the level
// is filtered out.
func Enabled(level slog.Level) bool {
	return logger.Enabled(context.Background(), level)
}

// String returns a human-readable description of the current logger
// configuration, primarily for diagnostics and startup banners.
func String() string {
	return fmt.Sprintf("levee/log: singleton logger ready (handler=%T)", logger.Handler())
}
