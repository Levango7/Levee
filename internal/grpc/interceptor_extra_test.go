// interceptor_extra_test.go exercises the logging/recovery interceptor
// helpers that the earlier interceptor coverage missed: request-id
// propagation (including client-supplied x-request-id metadata), the
// requestIDStream context wrapper, panic recovery for both unary and
// streaming handlers and the panic-value formatting helpers.
package grpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpcpkg "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// incomingMDCtx returns ctx with the supplied key/value pairs attached as
// incoming gRPC metadata, mirroring what the gRPC server does for client
// headers.
func incomingMDCtx(ctx context.Context, kv ...string) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs(kv...))
}

// --- ensureRequestID ---------------------------------------------------------

func TestEnsureRequestID(t *testing.T) {
	t.Run("generates random id when no metadata", func(t *testing.T) {
		rid := ensureRequestID(context.Background())
		require.Len(t, rid, 16, "expected a 16-hex-char request id")
		assert.Regexp(t, `^[0-9a-f]{16}$`, rid)
	})

	t.Run("generates distinct ids per call", func(t *testing.T) {
		assert.NotEqual(t, ensureRequestID(context.Background()), ensureRequestID(context.Background()))
	})

	t.Run("passes through client supplied x-request-id", func(t *testing.T) {
		ctx := incomingMDCtx(context.Background(), "x-request-id", "client-abc-123")
		assert.Equal(t, "client-abc-123", ensureRequestID(ctx))
	})

	t.Run("empty header value yields generated id", func(t *testing.T) {
		ctx := incomingMDCtx(context.Background(), "x-request-id", "")
		rid := ensureRequestID(ctx)
		require.NotEmpty(t, rid)
		assert.NotEqual(t, "", rid)
		assert.Len(t, rid, 16)
	})
}

// --- RequestIDFromContext / unary interceptor ---------------------------------

func TestLoggingUnaryInterceptorInjectsRequestID(t *testing.T) {
	ctx := incomingMDCtx(context.Background(), "x-request-id", "rid-unary-1")
	var seen string
	handler := func(hctx context.Context, _ interface{}) (interface{}, error) {
		seen = RequestIDFromContext(hctx)
		return "ok", nil
	}
	info := &grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}

	resp, err := loggingUnaryInterceptor(ctx, "req", info, handler)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp)
	assert.Equal(t, "rid-unary-1", seen,
		"handler must observe the client-supplied request id via RequestIDFromContext")
}

func TestLoggingUnaryInterceptorGeneratesRequestIDWhenAbsent(t *testing.T) {
	var seen string
	handler := func(hctx context.Context, _ interface{}) (interface{}, error) {
		seen = RequestIDFromContext(hctx)
		return nil, nil
	}
	info := &grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}

	_, err := loggingUnaryInterceptor(context.Background(), "req", info, handler)
	require.NoError(t, err)
	require.Len(t, seen, 16)
}

func TestRequestIDFromContextOutsideServerCallIsEmpty(t *testing.T) {
	assert.Empty(t, RequestIDFromContext(context.Background()))
}

// --- requestIDStream / stream interceptor --------------------------------------

func TestRequestIDStreamContextCarriesRequestID(t *testing.T) {
	inner := &fakeStream{ctx: context.Background()}
	wrapped := &requestIDStream{ServerStream: inner, requestID: "rid-stream-9"}

	ctx := wrapped.Context()
	assert.Equal(t, "rid-stream-9", RequestIDFromContext(ctx))
	// The wrapper must not have mutated the inner stream's context.
	assert.Empty(t, RequestIDFromContext(inner.ctx))
}

func TestLoggingStreamInterceptorInjectsRequestID(t *testing.T) {
	ctx := incomingMDCtx(context.Background(), "x-request-id", "rid-stream-2")
	var seen string
	var gotErr error
	handler := func(_ interface{}, ss grpcpkg.ServerStream) error {
		seen = RequestIDFromContext(ss.Context())
		gotErr = status.Error(codes.NotFound, "done")
		return gotErr
	}
	info := &grpcpkg.StreamServerInfo{FullMethod: "/test/Stream"}

	err := loggingStreamInterceptor(nil, &fakeStream{ctx: ctx}, info, handler)
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
	assert.Equal(t, "rid-stream-2", seen)
	assert.Equal(t, err, gotErr)
}

func TestLoggingStreamInterceptorSuccessPath(t *testing.T) {
	handler := func(_ interface{}, _ grpcpkg.ServerStream) error { return nil }
	err := loggingStreamInterceptor(nil, &fakeStream{ctx: context.Background()},
		&grpcpkg.StreamServerInfo{FullMethod: "/test/Stream"}, handler)
	require.NoError(t, err)
}

// --- recovery interceptors -------------------------------------------------------

func TestRecoveryUnaryInterceptorRecoversPanic(t *testing.T) {
	handler := func(_ context.Context, _ interface{}) (interface{}, error) {
		panic(errors.New("boom"))
	}
	info := &grpcpkg.UnaryServerInfo{FullMethod: "/test/Panic"}

	resp, err := recoveryUnaryInterceptor(context.Background(), "req", info, handler)
	require.Error(t, err)
	assert.Nil(t, resp)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Internal, st.Code())
	assert.Contains(t, st.Message(), "panic: boom")
}

func TestRecoveryUnaryInterceptorPassthroughOnError(t *testing.T) {
	wantErr := status.Error(codes.NotFound, "nope")
	handler := func(_ context.Context, _ interface{}) (interface{}, error) {
		return nil, wantErr
	}
	resp, err := recoveryUnaryInterceptor(context.Background(), "req",
		&grpcpkg.UnaryServerInfo{FullMethod: "/test/Ok"}, handler)
	assert.Equal(t, wantErr, err)
	assert.Nil(t, resp)
}

func TestRecoveryStreamInterceptorRecoversPanic(t *testing.T) {
	handler := func(_ interface{}, _ grpcpkg.ServerStream) error {
		panic("stream blew up")
	}
	err := recoveryStreamInterceptor(nil, &fakeStream{ctx: context.Background()},
		&grpcpkg.StreamServerInfo{FullMethod: "/test/PanicStream"}, handler)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Internal, st.Code())
	assert.Contains(t, st.Message(), "panic: stream blew up")
}

func TestRecoveryStreamInterceptorPassthrough(t *testing.T) {
	handler := func(_ interface{}, _ grpcpkg.ServerStream) error { return nil }
	err := recoveryStreamInterceptor(nil, &fakeStream{ctx: context.Background()},
		&grpcpkg.StreamServerInfo{FullMethod: "/test/OkStream"}, handler)
	require.NoError(t, err)
}

// --- panic value formatting -------------------------------------------------------

func TestFmtPanicValue(t *testing.T) {
	tests := []struct {
		name   string
		panicV interface{}
		want   string
	}{
		{name: "error value uses Error()", panicV: errors.New("kaput"), want: "kaput"},
		{name: "string value used verbatim", panicV: "plain", want: "plain"},
		{name: "nil becomes placeholder", panicV: nil, want: "<nil>"},
		{name: "bool true", panicV: true, want: "true"},
		{name: "bool false", panicV: false, want: "false"},
		{name: "int rendered decimally", panicV: 42, want: "42"},
		{name: "negative int", panicV: -7, want: "-7"},
		{name: "int64 rendered", panicV: int64(1234), want: "1234"},
		{name: "unprintable type falls back", panicV: struct{}{}, want: "<unprintable>"},
		{name: "long string truncated at 256", panicV: strings.Repeat("x", 300),
			want: strings.Repeat("x", 256) + "...(truncated)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, fmtPanicValue(tc.panicV))
		})
	}
}

func TestIntToString(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{9, "9"},
		{-1, "-1"},
		{123456789, "123456789"},
		{-987654321, "-987654321"},
		{9223372036854775807, "9223372036854775807"},
		{-9223372036854775807, "-9223372036854775807"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, intToString(tc.in), "intToString(%d)", tc.in)
	}
}

func TestLogPanicDoesNotCrash(t *testing.T) {
	// logPanic must never panic itself, even with exotic values.
	assert.NotPanics(t, func() { logPanic("/test/M", errors.New("e")) })
	assert.NotPanics(t, func() { logPanic("/test/M", 3.14) })
}
