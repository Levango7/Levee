// Package grpc implements LEVEE's gRPC server infrastructure. It exposes
// the five protobuf-defined services (ChangeService, TemplateService,
// TargetService, AuditService, SystemService) over a single gRPC server
// with pluggable interceptors for auth, logging and recovery.
//
// The Server type is the top-level entry point: construct it with
// NewServer, configure via Option values, then call Start to listen and
// serve. Stop performs a graceful shutdown, releasing the listener and
// draining in-flight RPCs.
//
// TLS is optional: when no TLS config is supplied the server listens in
// plaintext, which is appropriate for local development or when TLS is
// terminated by a sidecar (e.g. Envoy). Supply a *tls.Config via the
// WithTLS option to enable TLS directly.
package grpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/pause"
	"github.com/nexus/levee/internal/state"

	ggrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// DefaultListenAddr is the default address the gRPC server binds to when
// no explicit address is supplied to Start. It uses port 9090, the
// conventional LEVEE gRPC port.
const DefaultListenAddr = ":9090"

// Server is the LEVEE gRPC server. It owns the underlying *grpc.Server,
// the listener and the registered service implementations. A Server is
// safe to construct but must not be copied after Start.
type Server struct {
	grpcServer *ggrpc.Server
	listener   net.Listener
	store      state.Store

	// Service implementations. They are fields rather than local
	// variables so tests can swap them via Option functions.
	changeService   *ChangeService
	templateService pb.TemplateServiceServer
	targetService   pb.TargetServiceServer
	auditService    pb.AuditServiceServer
	systemService   pb.SystemServiceServer

	// healthServer implements the standard grpc.health.v1 service so
	// load balancers and orchestrators can probe the server.
	healthServer *health.Server

	// Configuration captured at NewServer time.
	tlsConfig  *tls.Config
	authToken  string
	listenAddr string

	// started guards against double Start; closed when Stop completes.
	mu      sync.Mutex
	started bool
}

// Option configures a Server at construction time. Options are applied
// in order; later options win.
type Option func(*Server)

// WithTLS enables TLS using the supplied *tls.Config. Pass nil to
// disable TLS (plaintext mode, suitable for local development).
func WithTLS(cfg *tls.Config) Option {
	return func(s *Server) {
		s.tlsConfig = cfg
	}
}

// WithAuthToken configures Bearer-token authentication. When token is
// empty, authentication is disabled (development mode). When non-empty,
// every RPC must carry an "authorization: Bearer <token>" metadata
// header; mismatched or missing tokens are rejected with
// codes.Unauthenticated.
func WithAuthToken(token string) Option {
	return func(s *Server) {
		s.authToken = token
	}
}

// WithListenAddr overrides the default listen address (:9090).
func WithListenAddr(addr string) Option {
	return func(s *Server) {
		s.listenAddr = addr
	}
}

// WithChangeService replaces the default ChangeService implementation.
// This is primarily intended for tests that want to inject a stub.
func WithChangeService(svc *ChangeService) Option {
	return func(s *Server) {
		s.changeService = svc
	}
}

// WithTemplateService replaces the default TemplateService implementation.
// When not supplied, NewServer uses the generated Unimplemented stub.
func WithTemplateService(svc pb.TemplateServiceServer) Option {
	return func(s *Server) {
		s.templateService = svc
	}
}

// WithTargetService replaces the default TargetService implementation.
// When not supplied, NewServer uses the generated Unimplemented stub.
func WithTargetService(svc pb.TargetServiceServer) Option {
	return func(s *Server) {
		s.targetService = svc
	}
}

// WithAuditService replaces the default AuditService implementation.
// When not supplied, NewServer uses the generated Unimplemented stub.
func WithAuditService(svc pb.AuditServiceServer) Option {
	return func(s *Server) {
		s.auditService = svc
	}
}

// WithSystemService replaces the default SystemService implementation.
// When not supplied, NewServer uses the generated Unimplemented stub.
func WithSystemService(svc pb.SystemServiceServer) Option {
	return func(s *Server) {
		s.systemService = svc
	}
}

// NewServer constructs a Server backed by the given Store. The Store
// must be non-nil; passing nil returns an error from Start. Options are
// applied in order; later options override earlier ones.
//
// NewServer does not open any sockets or background goroutines — call
// Start to begin serving.
func NewServer(store state.Store, opts ...Option) *Server {
	s := &Server{
		store:      store,
		listenAddr: DefaultListenAddr,
	}
	for _, opt := range opts {
		opt(s)
	}

	// Build the grpc.Server with interceptors and optional TLS.
	serverOpts := []ggrpc.ServerOption{
		ggrpc.ChainUnaryInterceptor(
			recoveryUnaryInterceptor,
			loggingUnaryInterceptor,
			AuthInterceptor(s.authToken),
		),
		ggrpc.ChainStreamInterceptor(
			recoveryStreamInterceptor,
			loggingStreamInterceptor,
			AuthStreamInterceptor(s.authToken),
		),
	}
	if s.tlsConfig != nil {
		serverOpts = append(serverOpts, ggrpc.Creds(credentials.NewTLS(s.tlsConfig)))
	}
	s.grpcServer = ggrpc.NewServer(serverOpts...)

	// Construct the default ChangeService if the caller did not supply
	// one. We pass nil for the engine/approval dependencies; the
	// ChangeService methods degrade gracefully (returning
	// codes.Unimplemented) when their engine dependency is nil, which
	// keeps the server usable in reduced modes (e.g. CLI-only or
	// query-only deployments).
	if s.changeService == nil {
		s.changeService = NewChangeService(store, nil, nil, nil)
	}

	// Register all five services. The four non-Change services use the
	// generated Unimplemented*Server stubs for now; they will be
	// replaced with real implementations in subsequent tasks.
	pb.RegisterChangeServiceServer(s.grpcServer, s.changeService)
	if s.templateService == nil {
		s.templateService = &pb.UnimplementedTemplateServiceServer{}
	}
	if s.targetService == nil {
		s.targetService = &pb.UnimplementedTargetServiceServer{}
	}
	if s.auditService == nil {
		s.auditService = &pb.UnimplementedAuditServiceServer{}
	}
	if s.systemService == nil {
		s.systemService = &pb.UnimplementedSystemServiceServer{}
	}
	pb.RegisterTemplateServiceServer(s.grpcServer, s.templateService)
	pb.RegisterTargetServiceServer(s.grpcServer, s.targetService)
	pb.RegisterAuditServiceServer(s.grpcServer, s.auditService)
	pb.RegisterSystemServiceServer(s.grpcServer, s.systemService)

	// Register the standard health check service. The overall server
	// status is flipped to SERVING in Start and NOT_SERVING in Stop.
	// This entry is exempt from auth (see skipAuthMethods in auth.go)
	// because load balancers and orchestrators probe it without
	// credentials.
	s.healthServer = health.NewServer()
	healthpb.RegisterHealthServer(s.grpcServer, s.healthServer)

	return s
}

// Start binds the listener and begins serving RPCs. It returns an error
// if the server is already started, if the Store is nil, or if the
// listener cannot be bound. Start blocks until Stop or GracefulStop is
// called; callers that want non-blocking behaviour should invoke it in
// a goroutine.
//
// If addr is empty, the Server's configured listen address (set via
// WithListenAddr or defaulting to :9090) is used.
func (s *Server) Start(addr string) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("grpc: server already started")
	}
	s.mu.Unlock()

	if s.store == nil {
		return errors.New("grpc: store is nil")
	}

	if addr == "" {
		addr = s.listenAddr
	}

	// Normalize the address: a bare port ":N" is fine for net.Listen,
	// but a missing leading colon (e.g. "9090") is a common mistake we
	// fix up rather than fail on.
	if addr != "" && !strings.HasPrefix(addr, ":") && !strings.Contains(addr, ":") {
		addr = ":" + addr
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("grpc: listen on %s: %w", addr, err)
	}
	s.listener = ln

	s.mu.Lock()
	s.started = true
	s.mu.Unlock()

	if s.healthServer != nil {
		s.healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	}

	log.Info("grpc server listening", "addr", ln.Addr().String(), "tls", s.tlsConfig != nil)
	if err := s.grpcServer.Serve(ln); err != nil {
		// Serve returns the listener error when the listener is closed
		// under it (Stop). Distinguish that benign case from a real
		// serve error by checking whether the server was stopped.
		s.mu.Lock()
		stopped := !s.started
		s.mu.Unlock()
		if stopped {
			return nil
		}
		return fmt.Errorf("grpc: serve: %w", err)
	}
	return nil
}

// Stop immediately closes the listener and stops the gRPC server. All
// in-flight RPCs are cancelled. Stop is idempotent and safe to call
// multiple times.
func (s *Server) Stop() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = false
	s.mu.Unlock()

	// Flip the health status first so probes stop routing traffic
	// before the listener goes away.
	if s.healthServer != nil {
		s.healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	}

	if s.grpcServer != nil {
		s.grpcServer.Stop()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	log.Info("grpc server stopped")
	return nil
}

// GracefulStop waits for all in-flight RPCs to complete, then stops the
// server. It is the preferred shutdown path for production deployments
// because it avoids cancelling in-progress changes. The optional ctx
// imposes a deadline: when the context expires before all RPCs drain,
// GracefulStop falls back to a hard Stop.
func (s *Server) GracefulStop(ctx context.Context) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		if s.grpcServer != nil {
			s.grpcServer.GracefulStop()
		}
		close(done)
	}()

	select {
	case <-done:
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		log.Info("grpc server graceful stop completed")
		return nil
	case <-ctx.Done():
		log.Warn("grpc server graceful stop timed out, forcing stop")
		return s.Stop()
	}
}

// Addr returns the address the server is listening on, or an empty
// string if the server has not been started.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// GrpcServer returns the underlying *grpc.Server. This is intended for
// advanced use cases (e.g. registering services that live outside this
// package) and for tests that need to inspect the registered services.
func (s *Server) GrpcServer() *ggrpc.Server {
	return s.grpcServer
}

// ServiceDeps bundles the optional internal dependencies that the
// ChangeService can use to drive complex operations. When a field is
// nil, the corresponding RPC falls back to a store-only code path or
// returns codes.Unimplemented. This keeps the server usable in
// reduced-functionality deployments (e.g. query-only).
type ServiceDeps struct {
	Engine   *EngineAdapter
	Approval *approval.Service
	Pause    *pause.PauseManager
}

// EngineAdapter is a seam for the engine.ClosureRunner. The real
// engine has a concrete *ClosureRunner type; we expose only the methods
// the gRPC layer needs so the gRPC package does not import the engine
// package directly (which would create an import cycle in some test
// configurations). Production code constructs an EngineAdapter that
// delegates to a real ClosureRunner; tests use a stub.
type EngineAdapter struct {
	// Run executes a plan and returns a run ID and success flag. The
	// implementation must be safe for concurrent use.
	Run func(ctx context.Context, changeID string, autoApprove bool, maxConcurrency int32) (runID string, success bool, err error)

	// Plan generates a plan for a change without applying it. Returns
	// the plan as a *pb.Plan ready to be returned to the gRPC client.
	Plan func(ctx context.Context, changeID string, targetHosts []string) (*pb.Plan, error)

	// Rollback rolls back a change. Returns the rollback run ID and the
	// list of hosts that were rolled back.
	Rollback func(ctx context.Context, changeID, runID string, autoApprove bool) (rollbackRunID string, rolledBackHosts []string, err error)

	// Retry re-runs failed hosts for a change.
	Retry func(ctx context.Context, changeID string, replan bool, targetHosts []string) error
}
