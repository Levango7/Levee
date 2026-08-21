package main

// grpc_client.go implements the gRPC client side used by the CLI when running
// in --remote mode. A single grpcClient bundles a *grpc.ClientConn together
// with the five generated service clients (Change, Template, Target, Audit,
// System), so commands can talk to a remote LEVEE server through the same
// adapter interfaces they use for the local in-process implementation.
//
// Connection security:
//   - Insecure (plaintext) by default — appropriate for local development and
//     for deployments where TLS is terminated by a sidecar (e.g. Envoy).
//   - TLS can be enabled by supplying a *tls.Config via the withTLS option.
//
// Authentication:
//   - Optional Bearer token. When non-empty, every RPC carries an
//     "authorization: Bearer <token>" metadata header, satisfying the
//     AuthInterceptor installed on the server side. The token is attached
//     via grpc.PerRPCCredentials so it applies uniformly to unary and
//     streaming RPCs.
//
// Connection establishment is non-blocking (grpc.NewClient semantics). Callers
// that want to fail fast when the server is unreachable can use waitForReady
// with a context deadline.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/grpc/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// defaultConnectTimeout is the deadline waitForReady uses when no explicit
// timeout is supplied. It is long enough to cover a slow TLS handshake but
// short enough that a misconfigured --server flag fails noticeably fast.
const defaultConnectTimeout = 10 * time.Second

// grpcClient bundles a gRPC ClientConn and the five generated service clients.
// All fields are read-only after construction; the type is safe for concurrent
// use because the underlying *grpc.ClientConn is.
type grpcClient struct {
	conn     *grpc.ClientConn
	change   pb.ChangeServiceClient
	template pb.TemplateServiceClient
	target   pb.TargetServiceClient
	audit    pb.AuditServiceClient
	system   pb.SystemServiceClient

	// token is retained for diagnostic output (never logged in full).
	token string
}

// grpcClientOption configures a grpcClient at construction time.
type grpcClientOption func(*grpcClientConfig)

// grpcClientConfig is the internal configuration bag assembled from options.
type grpcClientConfig struct {
	tlsConfig      *tls.Config
	connectTimeout time.Duration
}

// newGRPCClient dials addr and returns a grpcClient holding all five service
// clients. The connection is established lazily; pass a context with a
// deadline to waitForReady to block until the server is reachable.
//
// token may be empty to disable authentication (development mode).
func newGRPCClient(addr string, token string, _opts ...grpcClientOption) (*grpcClient, error) {
	if addr == "" {
		return nil, errors.New("grpc client: empty server address")
	}

	cfg := &grpcClientConfig{connectTimeout: defaultConnectTimeout}
	for _, opt := range _opts {
		opt(cfg)
	}

	// Build dial options: transport credentials + optional per-RPC auth.
	dialOpts := []grpc.DialOption{}
	if cfg.tlsConfig != nil {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(cfg.tlsConfig)))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	if token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(bearerTokenCredentials{token: token}))
	}

	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpc client: dial %s: %w", addr, err)
	}

	return &grpcClient{
		conn:     conn,
		change:   pb.NewChangeServiceClient(conn),
		template: pb.NewTemplateServiceClient(conn),
		target:   pb.NewTargetServiceClient(conn),
		audit:    pb.NewAuditServiceClient(conn),
		system:   pb.NewSystemServiceClient(conn),
		token:    token,
	}, nil
}

// close releases the underlying gRPC connection. It is safe to call multiple
// times; subsequent calls are no-ops.
func (c *grpcClient) close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	if err != nil {
		return fmt.Errorf("grpc client: close: %w", err)
	}
	return nil
}

// waitForReady blocks until the underlying connection enters Ready state or
// the context expires. When ctx already carries a deadline it is honoured
// as-is; otherwise the configured connect timeout is applied. This is a
// convenience for callers that want fail-fast behaviour on startup.
func (c *grpcClient) waitForReady(ctx context.Context) error {
	if c == nil || c.conn == nil {
		return errors.New("grpc client: not connected")
	}

	// Apply a default deadline when the caller did not supply one.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultConnectTimeout)
		defer cancel()
	}

	// Trigger connection establishment in the background.
	c.conn.Connect()

	for {
		state := c.conn.GetState()
		if state == connectivity.Ready {
			return nil
		}
		// WaitForStateChange returns false when ctx expires before the
		// state changes, which is our timeout signal.
		if !c.conn.WaitForStateChange(ctx, state) {
			return fmt.Errorf("grpc client: connect timeout: connection stuck in %s state", state)
		}
	}
}

// bearerTokenCredentials implements grpc.PerRPCCredentials to attach a
// "Bearer <token>" authorization header to every outgoing RPC. Using the
// PerRPCCredentials interface (rather than a unary interceptor) ensures the
// header is also present on streaming RPCs such as WatchChange and
// StreamLogs.
//
// RequireTransportSecurity returns false so the token can be used over
// plaintext connections during local development. Production deployments
// should pair the token with TLS via withTLS.
type bearerTokenCredentials struct {
	token string
}

// GetRequestMetadata returns the authorization header that gRPC attaches to
// the outgoing call.
func (b bearerTokenCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

// RequireTransportSecurity returns false so the credential is usable in both
// plaintext (dev) and TLS (prod) modes.
func (b bearerTokenCredentials) RequireTransportSecurity() bool { return false }
