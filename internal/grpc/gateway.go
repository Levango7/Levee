// gateway.go provides a REST/HTTP gateway in front of the gRPC server
// using grpc-gateway. The gateway translates HTTP/JSON requests into
// gRPC calls, allowing non-gRPC clients (curl, web UIs, monitoring
// systems) to consume the LEVEE API.
//
// Status: placeholder. The full grpc-gateway integration requires
// generating a *.gw.pb.go file from the proto definition and linking
// against github.com/grpc-ecosystem/grpc-gateway/v2. To avoid pulling
// in that dependency at this stage of the MVP, the gateway is not yet
// wired in. The ServeGateway function below returns codes.Unimplemented
// so callers fail fast rather than silently getting no gateway.
//
// TODO(M2): generate levee.pb.gw.go via protoc-gen-grpc-gateway and
// implement ServeGateway. The generated Register*HandlerFromEndpoint
// functions will be called here.
package grpc

import (
	"context"
	"net"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GatewayConfig configures the REST gateway.
type GatewayConfig struct {
	// Addr is the address the HTTP server listens on, e.g. ":8080".
	Addr string
	// GRPCAddr is the address of the upstream gRPC server.
	GRPCAddr string
	// CORSOrigins is the list of allowed CORS origins. "*" disables
	// CORS restrictions (suitable for development).
	CORSOrigins []string
}

// ServeGateway starts the REST gateway. It blocks until the context is
// cancelled or the HTTP server encounters a fatal error.
//
// This is a placeholder implementation: it starts an HTTP server that
// returns 501 Not Implemented for every request. The real
// implementation will use grpc-gateway to proxy requests to the gRPC
// server.
func ServeGateway(ctx context.Context, cfg GatewayConfig) error {
	if cfg.Addr == "" {
		return status.Error(codes.InvalidArgument, "gateway: addr is required")
	}

	// Placeholder handler: 501 for everything.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"grpc-gateway not yet implemented; use gRPC directly on :9090"}`))
	})

	// Apply basic CORS middleware so browser-based tools can at least
	// see the 501 response during development.
	handler := corsMiddleware(mux, cfg.CORSOrigins)

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return status.Errorf(codes.Internal, "gateway: listen: %v", err)
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return status.Errorf(codes.Internal, "gateway: serve: %v", err)
		}
		return nil
	}
}

// corsMiddleware wraps an http.Handler with permissive CORS headers.
// When origins is empty or contains "*", the middleware allows all
// origins. Otherwise it checks the Origin header against the allowlist.
func corsMiddleware(h http.Handler, origins []string) http.Handler {
	allowAll := len(origins) == 0
	for _, o := range origins {
		if o == "*" {
			allowAll = true
			break
		}
	}
	originSet := make(map[string]bool, len(origins))
	for _, o := range origins {
		originSet[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			h.ServeHTTP(w, r)
			return
		}
		if allowAll || originSet[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
