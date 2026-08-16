package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewServer_Validation checks that misconfiguration is rejected.
func TestNewServer_Validation(t *testing.T) {
	if _, err := NewServer(ServerConfig{}); err == nil {
		t.Error("expected error for missing addr")
	}
	if _, err := NewServer(ServerConfig{Addr: ":8080", DevMode: true}); err == nil {
		t.Error("expected error for dev mode without dev server url")
	}
}

// TestServer_ServesSpa verifies that the production server serves the
// embedded SPA shell on a non-API path.
func TestServer_ServesSpa(t *testing.T) {
	srv, err := NewServer(ServerConfig{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	handler, err := srv.buildMux()
	if err != nil {
		t.Fatalf("build mux: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "LEVEE") {
		t.Errorf("expected placeholder shell to mention LEVEE, got %q", rec.Body.String())
	}
}

// TestServer_ApiProxy verifies that /api/* is forwarded to the upstream
// gateway when APIBackendURL is configured.
func TestServer_ApiProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	srv, err := NewServer(ServerConfig{Addr: "127.0.0.1:0", APIBackendURL: upstream.URL})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	handler, err := srv.buildMux()
	if err != nil {
		t.Fatalf("build mux: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/changes", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Error("expected upstream header, proxy not wired")
	}
}

// TestServer_DevMode verifies that dev mode proxies to the Vite dev server.
func TestServer_DevMode(t *testing.T) {
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("vite dev server"))
	}))
	defer dev.Close()

	srv, err := NewServer(ServerConfig{
		Addr:         "127.0.0.1:0",
		DevMode:      true,
		DevServerURL: dev.URL,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	handler, err := srv.buildMux()
	if err != nil {
		t.Fatalf("build mux: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "vite") {
		t.Errorf("expected dev server response, got %q", rec.Body.String())
	}
}

// TestServer_StartShutdown exercises the start/stop lifecycle.
func TestServer_StartShutdown(t *testing.T) {
	srv, err := NewServer(ServerConfig{Addr: "127.0.0.1:0", ReadHeaderTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	// Give the server a moment to bind, then shut it down.
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("start returned error: %v", err)
	}
}
