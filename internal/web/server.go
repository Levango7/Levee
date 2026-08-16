// server.go provides the WebUIServer type that ties the embedded SPA handler
// together with an optional reverse proxy to the gRPC-gateway backend. In
// production the proxy is usually not needed (the gateway runs in the same
// process), but in development it lets the operator run `levee web --dev`
// and have the binary serve the SPA while forwarding /api to a separate
// grpc-gateway process.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// ServerConfig configures a WebUIServer.
type ServerConfig struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// APIBackendURL is the upstream gRPC-gateway URL that receives /api/*
	// requests. Empty means no proxying: /api/* returns 404. This is the
	// production mode where the gateway is mounted on the same mux by the
	// caller.
	APIBackendURL string
	// DevMode, when true, proxies all non-API requests to the Vite dev
	// server at DevServerURL. This lets `levee web --dev` serve hot-reloaded
	// assets without rebuilding the Go binary.
	DevMode bool
	// DevServerURL is the Vite dev server URL. Required when DevMode is true.
	DevServerURL string
	// ReadHeaderTimeout is forwarded to the http.Server. Defaults to 10s.
	ReadHeaderTimeout time.Duration
}

// WebUIServer serves the LEVEE frontend and (optionally) proxies API calls
// to a gRPC-gateway backend. Construct with NewServer and start with Start.
type WebUIServer struct {
	cfg    ServerConfig
	server *http.Server
}

// NewServer constructs a WebUIServer. The server is not started; call Start.
func NewServer(cfg ServerConfig) (*WebUIServer, error) {
	if cfg.Addr == "" {
		return nil, errors.New("web: addr is required")
	}
	if cfg.DevMode && cfg.DevServerURL == "" {
		return nil, errors.New("web: dev-server-url is required when dev mode is enabled")
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = 10 * time.Second
	}
	return &WebUIServer{cfg: cfg}, nil
}

// buildMux wires the request multiplexer. It is split out of Start so tests
// can inspect the handler without binding a socket.
func (s *WebUIServer) buildMux() (http.Handler, error) {
	mux := http.NewServeMux()

	// API proxy. When APIBackendURL is set, /api/* is forwarded to the
	// grpc-gateway. Otherwise /api/* 404s (the SPA handler also returns 404
	// for /api/*, so this is just an explicit placeholder).
	if s.cfg.APIBackendURL != "" {
		upstream, err := url.Parse(s.cfg.APIBackendURL)
		if err != nil {
			return nil, fmt.Errorf("web: parse api backend url: %w", err)
		}
		proxy := httputil.NewSingleHostReverseProxy(upstream)
		// Restore the original path so the gateway sees /api/v1/... even
		// though we matched on /api/.
		originalDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			originalDirector(req)
			req.Host = upstream.Host
		}
		mux.Handle("/api/", proxy)
		mux.Handle("/events/", proxy)
	}

	// Static assets / SPA shell.
	if s.cfg.DevMode {
		devUpstream, err := url.Parse(s.cfg.DevServerURL)
		if err != nil {
			return nil, fmt.Errorf("web: parse dev server url: %w", err)
		}
		devProxy := httputil.NewSingleHostReverseProxy(devUpstream)
		// In dev mode every non-API request goes to Vite, which serves
		// the SPA with HMR. We deliberately do not fall back to the
		// embedded placeholder here.
		mux.Handle("/", devProxy)
	} else {
		mux.Handle("/", Handler())
	}

	return mux, nil
}

// Start binds the listener and blocks until the server stops. The context,
// if cancelled, triggers a graceful shutdown with a 5s drain deadline.
func (s *WebUIServer) Start(ctx context.Context) error {
	handler, err := s.buildMux()
	if err != nil {
		return err
	}
	s.server = &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// DevProxyEnabled reports whether dev-mode proxying is active. Exposed for
// tests and the CLI to surface in startup logs.
func (s *WebUIServer) DevProxyEnabled() bool {
	return s.cfg.DevMode
}

// isAPIPath is a small helper kept for symmetry with the SPA handler's path
// classification. It returns true for paths that should never be served by
// the embedded SPA.
func isAPIPath(path string) bool {
	return strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/events/")
}