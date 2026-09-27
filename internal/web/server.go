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
	"net"
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
	// Listener, when set, is served directly instead of binding Addr.
	// Addr is then only a display value.
	//
	// This exists because binding is not an atomic act: a caller that
	// reserves a port with net.Listen(":0"), closes it, and then asks the
	// server to bind that same address opens a window in which any other
	// process can take the port — the rebind then fails with EADDRINUSE.
	// Handing over the already-bound listener closes that window instead of
	// retrying around it. It is also the correct shape for socket-activated
	// launches (systemd fd passing), where the supervisor owns the socket.
	Listener net.Listener
}

// WebUIServer serves the LEVEE frontend and (optionally) proxies API calls
// to a gRPC-gateway backend. Construct with NewServer and start with Start.
type WebUIServer struct {
	cfg    ServerConfig
	server *http.Server
}

// NewServer constructs a WebUIServer. The server is not started; call Start.
func NewServer(cfg ServerConfig) (*WebUIServer, error) {
	if cfg.Addr == "" && cfg.Listener == nil {
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
		// Rewrite instead of the (Go 1.26-deprecated) Director: the
		// outbound path is preserved verbatim, so the gateway still sees
		// /api/v1/... even though we matched on /api/; SetXForwarded
		// mirrors the default director's X-Forwarded-* injection; and the
		// outbound Host is pinned to the upstream, as the old director
		// wrapper did.
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(upstream)
				pr.SetXForwarded()
				pr.Out.Host = upstream.Host
			},
		}
		mux.Handle("/api/", proxy)
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

// Start serves the frontend and blocks until the server stops. The context,
// if cancelled, triggers a graceful shutdown with a 5s drain deadline.
//
// With cfg.Listener set it serves that listener; otherwise it binds
// cfg.Addr. Callers that must know the listening address before accepting
// traffic should bind a listener themselves and pass it in — the alternative
// (reserve a port with :0, close it, then hand the address over) releases the
// port for exactly the window in which someone else can take it, so the
// rebind fails intermittently with "address already in use" under load.
func (s *WebUIServer) Start(ctx context.Context) error {
	handler, err := s.buildMux()
	if err != nil {
		return err
	}
	addr := s.cfg.Addr
	if s.cfg.Listener != nil && addr == "" {
		addr = s.cfg.Listener.Addr().String()
	}
	s.server = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		var serr error
		if s.cfg.Listener != nil {
			serr = s.server.Serve(s.cfg.Listener)
		} else {
			serr = s.server.ListenAndServe()
		}
		if serr != nil && serr != http.ErrServerClosed {
			errCh <- serr
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
//
//nolint:unused // symmetry helper, kept for future API path gating
func isAPIPath(path string) bool {
	return strings.HasPrefix(path, "/api/")
}
