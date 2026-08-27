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

// TokenIdentity binds a bearer token to the subject (identity) it
// represents. Multi-token deployments configure one entry per caller so that
// audit attribution reflects the authenticated identity rather than a
// client-supplied assertion.
type TokenIdentity struct {
	// Token is the secret presented in the Authorization header.
	Token string
	// Subject is the authenticated identity recorded for audit attribution.
	// It is sanitized before being placed in the request context.
	Subject string
}

// AuthTokens is the set of bearer tokens the server accepts. Either Legacy
// (a single shared token with no named identity) or Named (one identity per
// token) may be populated; when both are empty authentication is disabled
// (development mode).
type AuthTokens struct {
	// Legacy is the original single shared token. It carries no named
	// subject; the actor remains whatever the client asserts. Retained for
	// backward compatibility with single-token deployments.
	Legacy string
	// Named lists each accepted token mapped to the subject it
	// authenticates as.
	Named []TokenIdentity
}

// Enabled reports whether any token is configured. When false,
// authentication is disabled and all requests are admitted.
func (a AuthTokens) Enabled() bool { return a.Legacy != "" || len(a.Named) > 0 }

// Resolve validates a presented bearer token against the accepted set. On a
// match it returns the authenticated subject and true; the subject is "" for
// the legacy single token, which carries no named identity. On a mismatch it
// returns ("", false). Callers must check Enabled first — Resolve on a
// disabled set never matches.
//
// Comparisons use crypto/subtle.ConstantTimeCompare to avoid timing
// side-channels. The token set is small and operator-bounded, so the linear
// scan leaks at most the set size through timing, which is acceptable.
func (a AuthTokens) Resolve(token string) (subject string, ok bool) {
	if a.Legacy != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.Legacy)) == 1 {
		return "", true
	}
	for _, ti := range a.Named {
		if ti.Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(ti.Token)) == 1 {
			return ti.Subject, true
		}
	}
	return "", false
}

// AuthInterceptor returns a unary server interceptor that validates a single
// shared Bearer token. It is the backward-compatible entry point; multi-token
// deployments should use AuthInterceptorFor. When expected is empty the
// interceptor is a no-op (authentication disabled), the local-development
// default.
//
// SECURITY: All methods are authenticated by default. To skip authentication
// for a specific method, add it to the skipAuthMethods map below.
func AuthInterceptor(expected string) grpc.UnaryServerInterceptor {
	return AuthInterceptorFor(AuthTokens{Legacy: expected})
}

// AuthInterceptorFor is the multi-token unary auth interceptor. It validates
// the bearer token against the full accepted set and, when the matched token
// carries a named subject, injects it into the request context (actorKey) so
// audit attribution uses the authenticated identity rather than the
// client-asserted one. Tokens are compared in constant time; missing,
// malformed or mismatched tokens yield codes.Unauthenticated.
func AuthInterceptorFor(tokens AuthTokens) grpc.UnaryServerInterceptor {
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
		subject, err := checkAuthTokens(ctx, tokens)
		if err != nil {
			return nil, err
		}
		if subject != "" {
			ctx = context.WithValue(ctx, actorKey{}, subject)
		}
		return handler(ctx, req)
	}
}

// AuthStreamInterceptor is the streaming-RPC analogue of AuthInterceptor for
// a single shared token. It validates the bearer token once when the stream
// is established; subsequent messages on the same stream are not re-checked.
func AuthStreamInterceptor(expected string) grpc.StreamServerInterceptor {
	return AuthStreamInterceptorFor(AuthTokens{Legacy: expected})
}

// AuthStreamInterceptorFor is the multi-token streaming auth interceptor. It
// validates the bearer token once at stream establishment and, when the
// matched token carries a named subject, wraps the stream so handlers observe
// the authenticated subject in the context. Like the unary variant it honours
// skipAuthMethods, so the standard health Watch stream stays reachable by
// unauthenticated load balancers.
func AuthStreamInterceptorFor(tokens AuthTokens) grpc.StreamServerInterceptor {
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
		subject, err := checkAuthTokens(ss.Context(), tokens)
		if err != nil {
			return err
		}
		if subject != "" {
			ss = &authSubjectStream{ServerStream: ss, subject: subject}
		}
		return handler(srv, ss)
	}
}

// authSubjectStream overrides the stream context to carry the authenticated
// subject, mirroring the unary interceptor's context injection. It layers on
// top of any upstream context (e.g. the logging interceptor's asserted actor),
// so the authenticated subject wins on lookup.
type authSubjectStream struct {
	grpc.ServerStream
	subject string
}

func (s *authSubjectStream) Context() context.Context {
	return context.WithValue(s.ServerStream.Context(), actorKey{}, s.subject)
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

// checkAuthTokens performs the actual token validation against the accepted
// set. It returns the authenticated subject and nil when authentication is
// disabled (empty set) or when the request carries a matching Bearer token;
// otherwise it returns a gRPC status error with codes.Unauthenticated. The
// subject is "" for the legacy single token and for disabled auth.
func checkAuthTokens(ctx context.Context, tokens AuthTokens) (string, error) {
	// Empty token set ⇒ auth disabled (development mode).
	if !tokens.Enabled() {
		return "", nil
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing metadata")
	}

	values := md.Get(authorizationHeader)
	if len(values) == 0 {
		return "", status.Error(codes.Unauthenticated, "missing authorization header")
	}

	token, err := extractBearerToken(values[0])
	if err != nil {
		return "", err
	}

	subject, ok := tokens.Resolve(token)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "invalid token")
	}
	return subject, nil
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
