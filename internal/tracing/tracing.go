// Package tracing provides distributed tracing plumbing for LEVEE's
// change lifecycle. When enabled it builds an OpenTelemetry
// TracerProvider with a stdout exporter; when disabled it falls back to
// a dependency-free path that still generates 128-bit hex trace IDs,
// propagates them through context.Context and understands W3C
// traceparent headers, so the CLI/gRPC -> engine -> audit chain carries
// one trace_id either way.
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Exporter names accepted in Config.Exporter.
const (
	// ExporterStdout writes spans as JSON lines to the process stdout.
	ExporterStdout = "stdout"
	// ExporterOTLP is reserved for a future OTLP exporter; this build
	// does not ship one and New reports an error for it.
	ExporterOTLP = "otlp"
	// ExporterNone keeps tracing enabled in-process (trace IDs flow
	// through contexts) but exports nothing.
	ExporterNone = "none"
)

// DefaultServiceName is used as the resource service.name when
// Config.ServiceName is empty.
const DefaultServiceName = "levee"

// W3C traceparent sizing: 128-bit trace id, 64-bit span id, one byte
// of flags.
const (
	traceIDHexLen   = 32
	spanIDHexLen    = 16
	flagsSampled    = "01"
	flagsNotSampled = "00"
	// traceparentVersion is the only version this implementation emits.
	traceparentVersion = "00"
)

// traceIDKey is the context key holding the fallback (non-OTel) trace
// id. A private type guards against collisions with keys defined in
// other packages.
type traceIDKey struct{}

// Tracer starts named spans and returns a finish closure that records
// the terminal status. Implementations must be safe for concurrent
// use.
type Tracer interface {
	// Start opens a span named name as a child of whatever span the
	// context already carries. The returned context contains the new
	// span; the returned closure ends it, recording status (free-form,
	// e.g. "succeeded"/"failed") and err (may be nil).
	Start(ctx context.Context, name string, attrs map[string]string) (context.Context, func(status string, err error))
}

// Config selects the tracing behaviour for one LEVEE process.
type Config struct {
	// Enabled toggles tracing. When false New returns a NoopTracer and
	// no exporter is constructed.
	Enabled bool
	// Exporter is one of ExporterStdout, ExporterOTLP or ExporterNone.
	// Ignored when Enabled is false. An empty value behaves like
	// ExporterNone.
	Exporter string
	// Endpoint is reserved for the future OTLP exporter and ignored by
	// the stdout exporter.
	Endpoint string
	// ServiceName labels exported spans (resource service.name).
	// Defaults to DefaultServiceName when empty.
	ServiceName string
}

// defaultTracer is the process-wide tracer consulted by subsystems that
// cannot have a Tracer injected (engine, audit). It starts as a
// NoopTracer so tracing is safe to use before New has run.
var (
	defaultMu     sync.RWMutex
	defaultTracer Tracer = NoopTracer{}
)

// SetDefault installs t as the process-wide tracer. Passing nil
// restores the NoopTracer so callers can reset during shutdown.
func SetDefault(t Tracer) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if t == nil {
		defaultTracer = NoopTracer{}
		return
	}
	defaultTracer = t
}

// Default returns the process-wide tracer (NoopTracer until SetDefault
// replaces it). It never returns nil.
func Default() Tracer {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultTracer
}

// NoopTracer exports nothing but still generates and propagates trace
// IDs through the context, keeping the trace_id chain intact when
// tracing is disabled.
type NoopTracer struct{}

// Start implements Tracer. It inherits the trace id already present in
// ctx, or generates a fresh one, and stores it under the internal key
// so TraceIDFromContext can read it back.
func (NoopTracer) Start(ctx context.Context, _ string, _ map[string]string) (context.Context, func(string, error)) {
	id := TraceIDFromContext(ctx)
	if id == "" {
		id = newTraceID()
	}
	return WithTraceID(ctx, id), func(string, error) {}
}

// otelTracer wraps an OpenTelemetry tracer backed by the SDK tracer
// provider constructed in New.
type otelTracer struct {
	tracer trace.Tracer
}

// Start implements Tracer on top of the OpenTelemetry API.
func (t *otelTracer) Start(ctx context.Context, name string, attrs map[string]string) (context.Context, func(string, error)) {
	ctx, span := t.tracer.Start(ctx, name, trace.WithAttributes(sortedAttributes(attrs)...))
	return ctx, func(status string, err error) {
		if status != "" {
			span.SetAttributes(attribute.String("levee.status", status))
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, status)
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}

// New constructs a Tracer from cfg together with a shutdown function
// that flushes and releases exporter resources. When tracing is
// disabled (or Exporter is none/empty) it returns a NoopTracer and a
// nil shutdown that never fails, so callers can wire the result
// unconditionally.
func New(cfg Config) (Tracer, func(ctx context.Context) error, error) {
	exporter := strings.ToLower(strings.TrimSpace(cfg.Exporter))
	if !cfg.Enabled || exporter == ExporterNone || exporter == "" {
		return NoopTracer{}, func(context.Context) error { return nil }, nil
	}

	switch exporter {
	case ExporterStdout:
		return newOtelStdoutTracer(cfg)
	case ExporterOTLP:
		return nil, nil, fmt.Errorf("tracing: exporter %q is not supported in this build; use %q or %q",
			ExporterOTLP, ExporterStdout, ExporterNone)
	default:
		return nil, nil, fmt.Errorf("tracing: unknown exporter %q (want %q, %q or %q)",
			cfg.Exporter, ExporterStdout, ExporterOTLP, ExporterNone)
	}
}

// newOtelStdoutTracer builds an SDK tracer provider that writes spans
// to stdout as JSON lines.
func newOtelStdoutTracer(cfg Config) (Tracer, func(ctx context.Context) error, error) {
	// Pass os.Stdout explicitly: stdouttrace's package-level default
	// captures the stdout file at init time, which would defeat tests
	// and embedders that replace os.Stdout before calling New.
	exporter, err := stdouttrace.New(
		stdouttrace.WithWriter(os.Stdout),
		stdouttrace.WithoutTimestamps(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: create stdout exporter: %w", err)
	}

	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = DefaultServiceName
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", serviceName),
		)),
	)

	return &otelTracer{tracer: provider.Tracer(serviceName)},
		func(ctx context.Context) error {
			if err := provider.Shutdown(ctx); err != nil {
				return fmt.Errorf("tracing: shutdown tracer provider: %w", err)
			}
			return nil
		}, nil
}

// TraceIDFromContext extracts the trace id carried by ctx. An active
// OpenTelemetry span wins; otherwise the internal key written by
// WithTraceID (or NoopTracer) is consulted. It returns "" when ctx
// holds no trace id.
func TraceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		if id := sc.TraceID(); id.IsValid() {
			return id.String()
		}
	}
	if id, ok := ctx.Value(traceIDKey{}).(string); ok {
		return id
	}
	return ""
}

// WithTraceID stores id in ctx under the internal key so downstream
// code (engine, audit) can correlate work with the originating request.
// An empty id leaves ctx unchanged.
func WithTraceID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, traceIDKey{}, id)
}

// ParseTraceparent extracts the trace id from a W3C traceparent header
// value of the form "00-<trace-id>-<span-id>-<flags>". It returns ""
// for malformed or all-zero headers. Exported for use by transport
// layers (gRPC interceptors, REST middleware).
func ParseTraceparent(header string) string {
	return parseTraceparent(header)
}

// parseTraceparent validates a W3C traceparent header and returns its
// trace id. Validation follows the specification: four dash-separated
// fields, version != "ff", lowercase hex fields of the right length,
// and trace/span ids that are not all zero. Forward-compatible headers
// with extra trailing fields are accepted.
func parseTraceparent(header string) string {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) < 4 {
		return ""
	}
	version := strings.ToLower(parts[0])
	if len(version) != 2 || !isHex(version) || version == "ff" {
		return ""
	}
	traceID := strings.ToLower(parts[1])
	if len(traceID) != traceIDHexLen || !isHex(traceID) || isAllZero(traceID) {
		return ""
	}
	spanID := strings.ToLower(parts[2])
	if len(spanID) != spanIDHexLen || !isHex(spanID) || isAllZero(spanID) {
		return ""
	}
	flags := strings.ToLower(parts[3])
	if len(flags) != 2 || !isHex(flags) {
		return ""
	}
	return traceID
}

// FormatTraceparent renders a W3C traceparent header value for the
// given 32-hex-char trace id, sampling flag set. It returns "" when
// traceID is not a valid 128-bit hex id, so callers can skip the
// header instead of sending a malformed one.
func FormatTraceparent(traceID string) string {
	id := strings.ToLower(strings.TrimSpace(traceID))
	if len(id) != traceIDHexLen || !isHex(id) || isAllZero(id) {
		return ""
	}
	spanID, err := randomHex(spanIDHexLen / 2)
	if err != nil {
		// crypto/rand is effectively infallible on supported
		// platforms; fall back to a fixed non-zero span id rather
		// than dropping correlation.
		spanID = "00f067aa0ba902b7"
	}
	return traceparentVersion + "-" + id + "-" + spanID + "-" + flagsSampled
}

// newTraceID generates a fresh random 128-bit trace id as 32 lowercase
// hex characters. It never fails hard: on entropy errors it falls back
// to a fixed non-zero id so the chain still carries an identifier.
func newTraceID() string {
	id, err := randomHex(traceIDHexLen / 2)
	if err != nil || isAllZero(id) {
		return "4bf92f3577b34da6a3ce929d0e0e4736"
	}
	return id
}

// randomHex returns n random bytes hex-encoded (2n characters).
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("tracing: read entropy: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// isHex reports whether s consists solely of hexadecimal characters.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// isAllZero reports whether s consists solely of '0' characters.
func isAllZero(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// sortedAttributes converts the attrs map into OTel attributes sorted
// by key, keeping span output deterministic for tests and diffable
// logs.
func sortedAttributes(attrs map[string]string) []attribute.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]attribute.KeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, attribute.String(k, attrs[k]))
	}
	return out
}
