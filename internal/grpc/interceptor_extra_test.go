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

	t.Run("control-character-only header yields generated id", func(t *testing.T) {
		ctx := incomingMDCtx(context.Background(), "x-request-id", "\r\n\x00")
		rid := ensureRequestID(ctx)
		require.Len(t, rid, 16, "unusable ids must be regenerated, not passed through")
		assert.Regexp(t, `^[0-9a-f]{16}$`, rid)
	})

	t.Run("client id with embedded CRLF is sanitized", func(t *testing.T) {
		ctx := incomingMDCtx(context.Background(), "x-request-id", "abc\r\nevil: header")
		assert.Equal(t, "abcevil: header", ensureRequestID(ctx))
	})
}

// --- sanitizeHeaderValue -----------------------------------------------------------------

func TestSanitizeHeaderValue(t *testing.T) {
	t.Run("strips control characters including CR/LF", func(t *testing.T) {
		assert.Equal(t, "cleanvalue", sanitizeHeaderValue("cle\r\nan\x00value"))
	})
	t.Run("keeps printable unicode", func(t *testing.T) {
		assert.Equal(t, "ops-team ✓", sanitizeHeaderValue("ops-team ✓"))
	})
	t.Run("caps length at 128", func(t *testing.T) {
		in := strings.Repeat("x", 300)
		require.Len(t, sanitizeHeaderValue(in), 128)
	})
	t.Run("empty stays empty", func(t *testing.T) {
		assert.Empty(t, sanitizeHeaderValue(""))
	})
}

// --- actor identity propagation ------------------------------------------------------------

func TestActorIdentityPropagation(t *testing.T) {
	t.Run("unary interceptor injects x-actor into context", func(t *testing.T) {
		ctx := incomingMDCtx(context.Background(), "x-request-id", "rid-actor", "x-actor", "alice")
		var seen string
		handler := func(hctx context.Context, _ interface{}) (interface{}, error) {
			seen = actorFromCtx(hctx)
			return nil, nil
		}
		info := &grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}
		_, err := loggingUnaryInterceptor(ctx, "req", info, handler)
		require.NoError(t, err)
		assert.Equal(t, "alice", seen, "handlers must observe the asserted actor")
	})

	t.Run("stream interceptor injects x-actor into stream context", func(t *testing.T) {
		ctx := incomingMDCtx(context.Background(), "x-actor", "bob")
		var seen string
		handler := func(_ interface{}, ss grpcpkg.ServerStream) error {
			seen = actorFromCtx(ss.Context())
			return nil
		}
		err := loggingStreamInterceptor(nil, &fakeStream{ctx: ctx},
			&grpcpkg.StreamServerInfo{FullMethod: "/test/Stream"}, handler)
		require.NoError(t, err)
		assert.Equal(t, "bob", seen)
	})

	t.Run("absent x-actor falls back to default", func(t *testing.T) {
		var seen string
		handler := func(hctx context.Context, _ interface{}) (interface{}, error) {
			seen = actorFromCtx(hctx)
			return nil, nil
		}
		_, err := loggingUnaryInterceptor(context.Background(), "req",
			&grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}, handler)
		require.NoError(t, err)
		assert.Equal(t, "grpc-user", seen)
	})

	t.Run("x-actor control characters are stripped", func(t *testing.T) {
		ctx := incomingMDCtx(context.Background(), "x-actor", "eve\nlog injection")
		var seen string
		handler := func(hctx context.Context, _ interface{}) (interface{}, error) {
			seen = actorFromCtx(hctx)
			return nil, nil
		}
		_, err := loggingUnaryInterceptor(ctx, "req",
			&grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}, handler)
		require.NoError(t, err)
		assert.Equal(t, "evelog injection", seen)
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

// --- auth stream interceptor -----------------------------------------------------------------

func TestAuthStreamInterceptor(t *testing.T) {
	handler := func(_ interface{}, _ grpcpkg.ServerStream) error { return nil }

	t.Run("health watch stream is exempt like unary check", func(t *testing.T) {
		err := AuthStreamInterceptor("s3cret")(nil, &fakeStream{ctx: context.Background()},
			&grpcpkg.StreamServerInfo{FullMethod: "/grpc.health.v1.Health/Watch"}, handler)
		assert.NoError(t, err, "health Watch must be reachable without credentials")
	})

	t.Run("business stream requires a token", func(t *testing.T) {
		ctx := incomingMDCtx(context.Background(), "authorization", "Bearer s3cret")
		err := AuthStreamInterceptor("s3cret")(nil, &fakeStream{ctx: ctx},
			&grpcpkg.StreamServerInfo{FullMethod: "/levee.ChangeService/WatchChange"}, handler)
		assert.NoError(t, err)

		err = AuthStreamInterceptor("s3cret")(nil, &fakeStream{ctx: context.Background()},
			&grpcpkg.StreamServerInfo{FullMethod: "/levee.ChangeService/WatchChange"}, handler)
		require.Error(t, err)
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
	})
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
