package grpc

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startProductionGateway(t *testing.T, gw *Gateway) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.serveOn(ln, ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("production gateway did not stop after context cancellation")
		}
	})
	return "http://" + ln.Addr().String()
}

func TestGatewaySecurityHeadersCoverUnreadyHealthz(t *testing.T) {
	baseURL := startProductionGateway(t, NewGateway(ServeGatewayConfig{}))

	resp, err := http.Get(baseURL + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	assert.Equal(t, "default-src 'none'; frame-ancestors 'none'", resp.Header.Get("Content-Security-Policy"))
}

func TestGatewaySecurityHeadersPreservePrintableSheetPolicy(t *testing.T) {
	gw, _, store := startTestGatewayFull(t, ServeGatewayConfig{})
	seedTicketRun(t, store, "run-security-headers")
	baseURL := startProductionGateway(t, gw)

	resp, err := http.Get(baseURL + "/changes/run-security-headers/ticket?format=html")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	assert.Equal(t,
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; frame-ancestors 'none'",
		resp.Header.Get("Content-Security-Policy"))
}

func TestGatewaySecurityHeadersCoverProductionRoutes(t *testing.T) {
	const baselineCSP = "default-src 'none'; frame-ancestors 'none'"
	gw, _, _ := startTestGatewayFull(t, ServeGatewayConfig{
		AuthToken:     "test-token",
		CORSOrigins:   []string{"https://app.example"},
		MetricsPublic: true,
	})
	gw.SetExtraRoute("/metrics", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	baseURL := startProductionGateway(t, gw)
	authHeaders := http.Header{"Authorization": []string{"Bearer test-token"}}

	tests := []struct {
		name    string
		method  string
		path    string
		headers http.Header
		status  int
	}{
		{name: "ready healthz", method: http.MethodGet, path: "/healthz", status: http.StatusOK},
		{name: "API success", method: http.MethodGet, path: "/api/v1/system/version", headers: authHeaders, status: http.StatusOK},
		{name: "API not found", method: http.MethodGet, path: "/api/v1/changes/missing", headers: authHeaders, status: http.StatusNotFound},
		{name: "API unauthenticated", method: http.MethodGet, path: "/system/version", status: http.StatusUnauthorized},
		{name: "CORS preflight", method: http.MethodOptions, path: "/api/v1/changes", headers: http.Header{"Origin": []string{"https://app.example"}}, status: http.StatusNoContent},
		{name: "operational route", method: http.MethodGet, path: "/metrics", status: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, baseURL+tt.path, nil)
			require.NoError(t, err)
			for key, values := range tt.headers {
				for _, value := range values {
					req.Header.Add(key, value)
				}
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, tt.status, resp.StatusCode)
			assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
			assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
			assert.Equal(t, baselineCSP, resp.Header.Get("Content-Security-Policy"))
			if tt.method == http.MethodOptions {
				assert.Equal(t, "https://app.example", resp.Header.Get("Access-Control-Allow-Origin"))
			}
		})
	}
}
