package tracing

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

func TestNoopTracerGeneratesAndPropagatesTraceID(t *testing.T) {
	tr := NoopTracer{}

	ctx, finish := tr.Start(context.Background(), "change.apply", map[string]string{"change_id": "c1"})
	require.NotNil(t, finish)

	id := TraceIDFromContext(ctx)
	assert.Len(t, id, traceIDHexLen, "trace id must be 128-bit hex")
	assert.True(t, isHex(id))
	assert.NotPanics(t, func() { finish("succeeded", nil) })

	// A child span inherits the same trace id.
	childCtx, childFinish := tr.Start(ctx, "engine.run", nil)
	assert.Equal(t, id, TraceIDFromContext(childCtx))
	require.NotNil(t, childFinish)
	childFinish("failed", errors.New("boom"))
}

func TestTraceIDFromContext(t *testing.T) {
	assert.Empty(t, TraceIDFromContext(nil), "nil context carries no id")
	assert.Empty(t, TraceIDFromContext(context.Background()))

	ctx := WithTraceID(context.Background(), "abc123")
	assert.Equal(t, "abc123", TraceIDFromContext(ctx))
}

func TestWithTraceIDEmptyIsNoop(t *testing.T) {
	base := context.Background()
	assert.Equal(t, base, WithTraceID(base, ""), "empty id must not wrap the context")
}

func TestWithTraceIDAndFallbackKey(t *testing.T) {
	ctx := WithTraceID(context.Background(), "deadbeef")
	assert.Equal(t, "deadbeef", TraceIDFromContext(ctx))
}

// TestTraceIDFromContextPrefersOTelSpan verifies an active OpenTelemetry
// span context wins over the internal fallback key.
func TestTraceIDFromContextPrefersOTelSpan(t *testing.T) {
	tid, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	sid, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)

	ctx := WithTraceID(context.Background(), "fallback-id")
	ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid,
		SpanID:  sid,
	}))
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", TraceIDFromContext(ctx))
}

func TestParseTraceparentValid(t *testing.T) {
	id := parseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", id)
	assert.Equal(t, id, ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"))
}

func TestParseTraceparentVariants(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"uppercase hex accepted", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00F067AA0BA902B7-01", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"surrounding whitespace trimmed", "  00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01\t", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"future version with extra fields", "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"not sampled flag still valid", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", "4bf92f3577b34da6a3ce929d0e0e4736"},

		{"empty header", "", ""},
		{"too few fields", "00-4bf92f3577b34da6a3ce929d0e0e4736", ""},
		{"version ff rejected", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", ""},
		{"bad version hex", "zz-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", ""},
		{"short trace id", "00-4bf92f35-00f067aa0ba902b7-01", ""},
		{"non-hex trace id", "00-4bf92f3577b34da6a3ce929d0e0e47zz-00f067aa0ba902b7-01", ""},
		{"all-zero trace id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", ""},
		{"all-zero span id", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", ""},
		{"short span id", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f0-01", ""},
		{"bad flags", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-xx", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseTraceparent(tt.header))
		})
	}
}

func TestFormatTraceparent(t *testing.T) {
	got := FormatTraceparent("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NotEmpty(t, got)

	parts := strings.Split(got, "-")
	require.Len(t, parts, 4)
	assert.Equal(t, traceparentVersion, parts[0])
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", parts[1])
	assert.Len(t, parts[2], spanIDHexLen)
	assert.True(t, isHex(parts[2]))
	assert.NotEqual(t, strings.Repeat("0", spanIDHexLen), parts[2])
	assert.Equal(t, flagsSampled, parts[3])

	// Round-trips through the parser.
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", parseTraceparent(got))

	// Uppercase input is normalised.
	upper := FormatTraceparent("4BF92F3577B34DA6A3CE929D0E0E4736")
	require.NotEmpty(t, upper)
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", parseTraceparent(upper))

	// Invalid inputs yield an empty header.
	assert.Empty(t, FormatTraceparent(""))
	assert.Empty(t, FormatTraceparent("short"))
	assert.Empty(t, FormatTraceparent("4bf92f3577b34da6a3ce929d0e0e47zz"))
	assert.Empty(t, FormatTraceparent("00000000000000000000000000000000"))
}

func TestNewDisabledReturnsNoop(t *testing.T) {
	tr, shutdown, err := New(Config{Enabled: false})
	require.NoError(t, err)
	require.NotNil(t, tr)
	_, ok := tr.(NoopTracer)
	assert.True(t, ok, "disabled config must yield the NoopTracer")
	require.NoError(t, shutdown(context.Background()))
}

func TestNewEnabledWithNoneExporter(t *testing.T) {
	for _, exporter := range []string{ExporterNone, ""} {
		tr, shutdown, err := New(Config{Enabled: true, Exporter: exporter})
		require.NoError(t, err, "exporter %q", exporter)
		_, ok := tr.(NoopTracer)
		assert.True(t, ok, "exporter %q must yield the NoopTracer", exporter)
		require.NoError(t, shutdown(context.Background()))
	}
}

func TestNewRejectsOTLPAndUnknownExporters(t *testing.T) {
	_, _, err := New(Config{Enabled: true, Exporter: ExporterOTLP})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")

	_, _, err = New(Config{Enabled: true, Exporter: "kafka"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown exporter")
}

func TestNewStdoutExporter(t *testing.T) {
	restoreStdout := captureStdout(t)
	defer restoreStdout()

	tr, shutdown, err := New(Config{Enabled: true, Exporter: ExporterStdout})
	require.NoError(t, err)
	require.NotNil(t, tr)
	_, isOtel := tr.(*otelTracer)
	assert.True(t, isOtel, "stdout exporter must yield the OTel tracer")

	ctx, finish := tr.Start(context.Background(), "change.apply", map[string]string{"change_id": "c1"})
	id := TraceIDFromContext(ctx)
	assert.Len(t, id, traceIDHexLen)
	assert.True(t, isHex(id))
	finish("succeeded", nil)

	// Shutdown flushes the batcher and releases the exporter; it must be
	// callable without error.
	require.NoError(t, shutdown(context.Background()))
}

// captureStdout swaps os.Stdout for a pipe and returns a restore
// function that also drains the pipe, keeping exporter JSON out of the
// test log.
func captureStdout(t *testing.T) func() {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	return func() {
		_ = w.Close()
		_, _ = io.ReadAll(r)
		_ = r.Close()
		os.Stdout = old
	}
}

// TestStdoutExporterEmitsSpans redirects os.Stdout and checks that the
// exporter actually writes span JSON for completed spans.
func TestStdoutExporterEmitsSpans(t *testing.T) {
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = oldStdout }()

	tr, shutdown, err := New(Config{Enabled: true, Exporter: ExporterStdout, ServiceName: "levee-test"})
	require.NoError(t, err)

	_, finish := tr.Start(context.Background(), "span-emission-check", nil)
	finish("ok", nil)
	require.NoError(t, shutdown(context.Background()))

	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)

	text := string(out)
	assert.Contains(t, text, "span-emission-check", "exported span JSON should carry the span name")
	assert.Contains(t, text, "levee-test", "exported span JSON should carry the service name")
}

func TestOtelTracerRecordsErrorStatus(t *testing.T) {
	restoreStdout := captureStdout(t)
	defer restoreStdout()

	tr, shutdown, err := New(Config{Enabled: true, Exporter: ExporterStdout})
	require.NoError(t, err)

	_, finish := tr.Start(context.Background(), "failing-op", nil)
	require.NotNil(t, finish)
	finish("failed", errors.New("something broke"))

	// Empty status must also be tolerated.
	_, finish2 := tr.Start(context.Background(), "no-status-op", nil)
	finish2("", nil)

	require.NoError(t, shutdown(context.Background()))
}

func TestSetDefaultAndDefault(t *testing.T) {
	original := Default()
	t.Cleanup(func() { SetDefault(original) })

	require.NotNil(t, Default(), "Default must never be nil")
	_, isNoop := Default().(NoopTracer)
	assert.True(t, isNoop, "before any New call the default is the NoopTracer")

	custom := NoopTracer{}
	SetDefault(custom)
	assert.Equal(t, Tracer(custom), Default())

	SetDefault(nil)
	_, isNoop = Default().(NoopTracer)
	assert.True(t, isNoop, "SetDefault(nil) must restore the NoopTracer")
}

func TestSortedAttributesDeterministicOrder(t *testing.T) {
	assert.Nil(t, sortedAttributes(nil))
	assert.Nil(t, sortedAttributes(map[string]string{}))

	got := sortedAttributes(map[string]string{"zeta": "z", "alpha": "a", "mid": "m"})
	require.Len(t, got, 3)
	assert.Equal(t, "alpha", string(got[0].Key))
	assert.Equal(t, "mid", string(got[1].Key))
	assert.Equal(t, "zeta", string(got[2].Key))
}

func TestHexHelpers(t *testing.T) {
	assert.True(t, isHex("0123456789abcdef"))
	assert.True(t, isHex("ABCDEF"))
	assert.False(t, isHex(""))
	assert.False(t, isHex("xyz"))
	assert.False(t, isHex("abc-def"))

	assert.True(t, isAllZero("0000"))
	assert.True(t, isAllZero(""))
	assert.False(t, isAllZero("0001"))

	id := newTraceID()
	assert.Len(t, id, traceIDHexLen)
	assert.True(t, isHex(id))
	assert.False(t, isAllZero(id))

	h, err := randomHex(8)
	require.NoError(t, err)
	assert.Len(t, h, 16)
	assert.True(t, isHex(h))
}

// TestEndToEndTraceparentHandoff simulates an inbound request with a
// traceparent header flowing through the noop tracer into an outbound
// header.
func TestEndToEndTraceparentHandoff(t *testing.T) {
	inbound := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	id := ParseTraceparent(inbound)
	require.NotEmpty(t, id)

	ctx := WithTraceID(context.Background(), id)
	tr := Default()
	childCtx, finish := tr.Start(ctx, "grpc.ApplyChange", nil)
	defer finish("succeeded", nil)

	assert.Equal(t, id, TraceIDFromContext(childCtx))

	outbound := FormatTraceparent(TraceIDFromContext(childCtx))
	assert.Equal(t, id, parseTraceparent(outbound))
}
