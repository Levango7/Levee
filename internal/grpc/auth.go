// auth.go implements Bearer-token authentication interceptors for the
// gRPC server. Two flavours are provided: a unary interceptor for
// regular request/response RPCs and a stream interceptor for streaming
// RPCs (WatchChange, StreamLogs). Both share the same token-validation
// logic.
//
// When the configured token is empty, authentication is disabled
// entirely (development mode). This is the default; production
// deployments must supply a non-empty token via WithAuthToken.
//
// The token is extracted from the "authorization" gRPC metadata header,
// which mirrors the HTTP header of the same name. The value must use
// the "Bearer <token>" scheme; any other scheme (e.g. "Basic") is
// rejected with codes.Unauthenticated.
package grpc

import (
	"context"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// authorizationHeader is the gRPC/HTTP metadata key carrying the
// bearer token. We honour the canonical lower-case form required by
// gRPC metadata.
const authorizationHeader = "authorization"

// AuthInterceptor returns a unary server interceptor that validates
// Bearer tokens against the supplied expected token. When expected is
// empty, the interceptor is a no-op (authentication disabled), which
// is the appropriate default for local development.
//
// Tokens are compared in constant time to avoid timing side-channels.
// Missing, malformed or mismatched tokens yield codes.Unauthenticated.
//
// SECURITY: All methods are authenticated by default. To skip authentication
// for a specific method, add it to the skipAuthMethods map below.
func AuthInterceptor(expected string) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		// Skip auth for methods that are explicitly exempted.
		if info != nil && skipAuthMethods[info.FullMethod] {
			return handler(ctx, req)
		}
		if err := checkAuth(ctx, expected); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// AuthStreamInterceptor is the streaming-RPC analogue of
// AuthInterceptor. It validates the bearer token once when the stream
// is established; subsequent messages on the same stream are not
// re-checked (the client is already authenticated).
//
// Like the unary interceptor it honours skipAuthMethods, so the standard
// health Watch stream is reachable by unauthenticated load balancers.
func AuthStreamInterceptor(expected string) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		// Skip auth for methods that are explicitly exempted.
		if info != nil && skipAuthMethods[info.FullMethod] {
			return handler(srv, ss)
		}
		if err := checkAuth(ss.Context(), expected); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// skipAuthMethods lists gRPC methods that are exempt from authentication.
// This whitelist must stay minimal. Every business RPC requires a valid
// Bearer token; only the standard gRPC health probes are public, so that
// load balancers and orchestrators can probe without credentials.
var skipAuthMethods = map[string]bool{
	// gRPC health check service, registered in server.go. Both the unary
	// Check and the streaming Watch must be exempt — a probe setup using
	// long-lived Watch streams would otherwise fail auth while Check
	// succeeded.
	"/grpc.health.v1.Health/Check": true,
	"/grpc.health.v1.Health/Watch": true,
}

// checkAuth performs the actual token validation. It returns nil when
// authentication is disabled (empty expected token) or when the
// request carries a matching Bearer token; otherwise it returns a
// gRPC status error with codes.Unauthenticated.
func checkAuth(ctx context.Context, expected string) error {
	// Empty expected token ⇒ auth disabled (development mode).
	if expected == "" {
		return nil
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}

	values := md.Get(authorizationHeader)
	if len(values) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization header")
	}

	token, err := extractBearerToken(values[0])
	if err != nil {
		return err
	}

	// Constant-time comparison via the standard library. subtle returns 0
	// only on an exact match; note it reports length mismatches immediately
	// (returning 0), which leaks token length but not content — the same
	// trade-off the previous hand-rolled comparison made.
	if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid token")
	}
	return nil
}

// extractBearerToken parses a "Bearer <token>" header value and returns
// the token. It is case-insensitive on the scheme name and tolerant of
// surrounding whitespace, matching the loose parsing recommended by
// RFC 6750.
func extractBearerToken(header string) (string, error) {
	h := strings.TrimSpace(header)
	if h == "" {
		return "", status.Error(codes.Unauthenticated, "empty authorization header")
	}
	// Find the scheme separator.
	idx := strings.IndexByte(h, ' ')
	if idx < 0 {
		return "", status.Error(codes.Unauthenticated, "malformed authorization header")
	}
	scheme := h[:idx]
	if !strings.EqualFold(scheme, "bearer") {
		return "", status.Error(codes.Unauthenticated, "unsupported authorization scheme; expected Bearer")
	}
	token := strings.TrimSpace(h[idx+1:])
	if token == "" {
		return "", status.Error(codes.Unauthenticated, "empty bearer token")
	}
	return token, nil
}
