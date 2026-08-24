// interceptor.go implements the logging and recovery interceptors
// chained into every gRPC RPC by NewServer. The logging interceptor
// emits a structured log line per RPC with the method name, duration
// and outcome (OK / error code). The recovery interceptor catches
// panics from handler code, converts them into codes.Internal errors
// and logs the panic value with a stack trace, preventing a single
// buggy handler from crashing the whole process.
//
// Both interceptors have unary and stream variants. They are
// deliberately small and dependency-free so they can be reused in
// tests without dragging in the full Server.
package grpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"runtime/debug"
	"strings"
	"time"

	"github.com/nexus/levee/internal/log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// requestIDKey is the context key under which the per-RPC request id is
// stored. Handlers can retrieve it via RequestIDFromContext to include
// it in their own log lines.
type requestIDKey struct{}

// requestIDHeader is the metadata key clients may set to propagate
// their own correlation id. Absent, empty or invalid values yield a
// fresh id.
const requestIDHeader = "x-request-id"

// actorHeader is the metadata key clients may set to assert the actor
// they are acting on behalf of ("X-Acting-As" over REST). The value is
// an assertion only — see the SECURITY note on actorFromCtx.
const actorHeader = "x-actor"

// maxHeaderValueLen caps client-supplied header-derived values (request
// ids, actor names) before they are stored in contexts, echoed in
// response headers or written to logs.
const maxHeaderValueLen = 128

// sanitizeHeaderValue strips CR/LF and all other control characters and
// caps the result at maxHeaderValueLen. Client-supplied values are
// echoed into response headers and structured logs; passing them through
// unfiltered would allow log injection and HTTP header splitting. An
// empty result means the input carried no usable content.
func sanitizeHeaderValue(s string) string {
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1 // drop control characters (CR, LF, NUL, ...)
		}
		return r
	}, s)
	if len(clean) > maxHeaderValueLen {
		clean = clean[:maxHeaderValueLen]
	}
	return clean
}

// ensureRequestID returns the client-supplied request id from the
// incoming metadata, sanitized via sanitizeHeaderValue. Absent, control-
// character-only or over-long-to-nothing values fall back to a freshly
// generated random 16-hex-char id.
func ensureRequestID(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(requestIDHeader); len(vals) > 0 {
			if rid := sanitizeHeaderValue(vals[0]); rid != "" {
				return rid
			}
		}
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}

// ensureActor returns the asserted actor identity from the incoming
// "x-actor" metadata, sanitized; "" when unset or unusable.
func ensureActor(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(actorHeader)
	if len(vals) == 0 {
		return ""
	}
	return sanitizeHeaderValue(vals[0])
}

// RequestIDFromContext returns the per-RPC request id injected by the
// logging interceptors, or "" outside of a server call.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

// loggingUnaryInterceptor emits a structured log line for every unary
// RPC and records its duration. It does not inspect the request or
// response payloads (which may contain sensitive data); only the
// method name, status code and duration are logged.
func loggingUnaryInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	start := time.Now()
	rid := ensureRequestID(ctx)
	ctx = context.WithValue(ctx, requestIDKey{}, rid)
	// Propagate the (asserted) actor identity alongside the request id so
	// handlers can attribute audits via actorFromCtx.
	if actor := ensureActor(ctx); actor != "" {
		ctx = context.WithValue(ctx, actorKey{}, actor)
	}
	resp, err := handler(ctx, req)
	duration := time.Since(start)

	if err != nil {
		log.Warn("grpc unary rpc failed",
			"request_id", rid,
			"method", info.FullMethod,
			"duration", duration.String(),
			"code", status.Code(err).String(),
			"error", err.Error(),
		)
	} else {
		log.Debug("grpc unary rpc completed",
			"request_id", rid,
			"method", info.FullMethod,
			"duration", duration.String(),
		)
	}
	return resp, err
}

// loggingStreamInterceptor is the streaming-RPC analogue of
// loggingUnaryInterceptor. It logs stream establishment and
// completion; per-message logging is left to the handler.
func loggingStreamInterceptor(
	srv interface{},
	ss grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	start := time.Now()
	rid := ensureRequestID(ss.Context())
	err := handler(srv, &requestIDStream{ServerStream: ss, requestID: rid, actor: ensureActor(ss.Context())})
	duration := time.Since(start)

	if err != nil {
		log.Warn("grpc stream rpc failed",
			"request_id", rid,
			"method", info.FullMethod,
			"duration", duration.String(),
			"code", status.Code(err).String(),
			"error", err.Error(),
		)
	} else {
		log.Debug("grpc stream rpc completed",
			"request_id", rid,
			"method", info.FullMethod,
			"duration", duration.String(),
		)
	}
	return err
}

// requestIDStream wraps a ServerStream so handlers observe the request
// id and asserted actor identity via the stream context.
type requestIDStream struct {
	grpc.ServerStream
	requestID string
	actor     string
}

func (s *requestIDStream) Context() context.Context {
	ctx := context.WithValue(s.ServerStream.Context(), requestIDKey{}, s.requestID)
	if s.actor != "" {
		ctx = context.WithValue(ctx, actorKey{}, s.actor)
	}
	return ctx
}

// recoveryUnaryInterceptor recovers from panics in unary handlers,
// converting them into codes.Internal gRPC errors. The panic value and
// a stack trace are logged at ERROR level so operators can diagnose
// the root cause. Without this interceptor, a panic in any handler
// would crash the entire gRPC server process.
func recoveryUnaryInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			logPanic(info.FullMethod, r)
			err = status.Errorf(codes.Internal, "panic: %v", r)
			resp = nil
		}
	}()
	return handler(ctx, req)
}

// recoveryStreamInterceptor is the streaming-RPC analogue of
// recoveryUnaryInterceptor.
func recoveryStreamInterceptor(
	srv interface{},
	ss grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logPanic(info.FullMethod, r)
			err = status.Errorf(codes.Internal, "panic: %v", r)
		}
	}()
	return handler(srv, ss)
}

// logPanic emits a structured log line for a recovered panic. The
// stack trace is trimmed to the first 4 KB to avoid flooding logs
// with runtime frames.
func logPanic(method string, r interface{}) {
	stack := debug.Stack()
	const maxStack = 4096
	if len(stack) > maxStack {
		stack = stack[:maxStack]
	}
	log.Error("grpc handler panic recovered",
		"method", method,
		"panic", fmtPanicValue(r),
		"stack", string(stack),
	)
}

// fmtPanicValue renders a panic value as a short string suitable for
// logging. It special-cases error values to use their Error() method
// and truncates anything longer than 256 characters.
func fmtPanicValue(r interface{}) string {
	var s string
	switch v := r.(type) {
	case error:
		s = v.Error()
	case string:
		s = v
	default:
		s = strings.TrimSpace(stringify(v))
	}
	const max = 256
	if len(s) > max {
		s = s[:max] + "...(truncated)"
	}
	return s
}

// stringify is a minimal fmt.Sprint replacement that avoids importing
// fmt just for one call site. It handles the common types we expect
// to see in a panic; anything else falls back to the type name.
func stringify(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return intToString(int64(x))
	case int64:
		return intToString(x)
	case string:
		return x
	default:
		return "<unprintable>"
	}
}

// intToString converts an int64 to its decimal string representation
// without importing strconv (which is already imported elsewhere in
// the package via other files; this keeps the dependency graph tidy).
func intToString(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
