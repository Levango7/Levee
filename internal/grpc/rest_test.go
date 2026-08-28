// rest_test.go exercises the REST-to-gRPC gateway routing layers: the
// RESTful dispatchers (/changes, /templates, ...), the legacy /api/v1/
// service-method paths, the bearer-token auth middleware and the CORS
// middleware. The gateway is driven through httptest with the same
// middleware chain that Gateway.serve installs.

package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/push"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTestGateway returns a *Gateway wired to real in-process services and
// an httptest.Server exposing the full middleware chain used by serve().
func startTestGateway(t *testing.T, cfg ServeGatewayConfig) (*Gateway, *httptest.Server, state.Store) {
	t.Helper()

	store := newTestStore(t)

	changeSvc := NewChangeService(store, nil, nil, nil)
	templateSvc := NewTemplateService(store, nil)
	targetSvc := NewTargetService(newTestStore(t), nil)
	auditSvc := NewAuditService(store)
	sysCfg := &config.Config{
		Server:   config.ServerConfig{DataDir: t.TempDir(), LogLevel: "info", LogFormat: "text"},
		Database: config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"},
	}
	systemSvc := NewSystemService(store, sysCfg, "", "test-v1.0", "abc123", "2024-01-01", "go1.21", time.Now())

	gw := NewGateway(cfg)
	gw.SetServices(changeSvc, templateSvc, targetSvc, auditSvc, systemSvc, nil, nil, nil)

	mux := http.NewServeMux()
	mux.Handle("/", corsMiddleware(cfg.CORSOrigins, gw.authMiddleware(gw.restRoute())))
	mux.Handle("/api/v1/", corsMiddleware(cfg.CORSOrigins, gw.authMiddleware(gw.route())))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return gw, srv, store
}

// --- RESTful change lifecycle -----------------------------------------------

func TestRESTChangeLifecycle(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	// 1. Create.
	body := `{"label":"rest-change","priority":"high","team":"platform","environment":"staging"}`
	resp, err := http.Post(srv.URL+"/changes", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.NotEmpty(t, created.ID)
	assert.Equal(t, "draft", created.Status)

	// 2. Get.
	getResp, err := http.Get(srv.URL + "/changes/" + created.ID)
	require.NoError(t, err)
	defer getResp.Body.Close()
	assert.Equal(t, http.StatusOK, getResp.StatusCode)

	// 3. List — the new change must appear.
	listResp, err := http.Get(srv.URL + "/changes")
	require.NoError(t, err)
	defer listResp.Body.Close()
	assert.Equal(t, http.StatusOK, listResp.StatusCode)
	var listed struct {
		TotalSize int32 `json:"totalSize"`
		Changes   []struct {
			ID string `json:"id"`
		} `json:"changes"`
	}
	require.NoError(t, json.NewDecoder(listResp.Body).Decode(&listed))
	assert.GreaterOrEqual(t, listed.TotalSize, int32(1))

	// 4. Approve.
	approveBody := `{"approver":"alice","comment":"lgtm"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/changes/"+created.ID+"/approve", strings.NewReader(approveBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	approveResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer approveResp.Body.Close()
	require.Equal(t, http.StatusOK, approveResp.StatusCode)
	var approved struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(approveResp.Body).Decode(&approved))
	assert.Equal(t, "approved", approved.Status)

	// 5. Cancel (approved → cancelled is legal).
	cancelReq, err := http.NewRequest(http.MethodPost, srv.URL+"/changes/"+created.ID+"/cancel",
		strings.NewReader(`{"reason":"no longer needed"}`))
	require.NoError(t, err)
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelResp, err := http.DefaultClient.Do(cancelReq)
	require.NoError(t, err)
	defer cancelResp.Body.Close()
	require.Equal(t, http.StatusOK, cancelResp.StatusCode)

	// 6. Archive.
	archReq, err := http.NewRequest(http.MethodPost, srv.URL+"/changes/"+created.ID+"/archive", nil)
	require.NoError(t, err)
	archResp, err := http.DefaultClient.Do(archReq)
	require.NoError(t, err)
	defer archResp.Body.Close()
	assert.Equal(t, http.StatusOK, archResp.StatusCode)
}

func TestRESTChangeNotFound(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	resp, err := http.Get(srv.URL + "/changes/change-does-not-exist")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestRESTUnknownSubPath(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/changes/some-id/bogus-action", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// --- Templates ---------------------------------------------------------------

func TestRESTTemplateLifecycle(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	// 1. Create.
	body := `{"name":"deploy-web","description":"web deploy","workflowContent":"steps: []"}`
	resp, err := http.Post(srv.URL+"/templates", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// 2. Get by name.
	getResp, err := http.Get(srv.URL + "/templates/deploy-web")
	require.NoError(t, err)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	var got struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&got))
	assert.Equal(t, "deploy-web", got.Name)

	// 3. List.
	listResp, err := http.Get(srv.URL + "/templates")
	require.NoError(t, err)
	defer listResp.Body.Close()
	assert.Equal(t, http.StatusOK, listResp.StatusCode)

	// 4. Delete.
	delReq, err := http.NewRequest(http.MethodDelete, srv.URL+"/templates/deploy-web", nil)
	require.NoError(t, err)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	defer delResp.Body.Close()
	assert.Equal(t, http.StatusOK, delResp.StatusCode)

	// 5. Gone after delete.
	getResp2, err := http.Get(srv.URL + "/templates/deploy-web")
	require.NoError(t, err)
	defer getResp2.Body.Close()
	assert.Equal(t, http.StatusNotFound, getResp2.StatusCode)
}

// --- Targets ------------------------------------------------------------------

func TestRESTTargetEndpoints(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	// AddTarget requires a live channel factory which the test target
	// service does not wire; expect either success or a clean error,
	// but never 404/400-routing failures.
	addBody := `{"hostname":"host-1","channelType":"ssh","port":22}`
	addResp, err := http.Post(srv.URL+"/targets", "application/json", strings.NewReader(addBody))
	require.NoError(t, err)
	defer addResp.Body.Close()
	assert.NotEqual(t, http.StatusNotFound, addResp.StatusCode, "route must exist")

	// GET list must succeed even with zero targets.
	listResp, err := http.Get(srv.URL + "/targets")
	require.NoError(t, err)
	defer listResp.Body.Close()
	assert.Equal(t, http.StatusOK, listResp.StatusCode)
	var listed struct {
		TotalSize int32 `json:"totalSize"`
	}
	require.NoError(t, json.NewDecoder(listResp.Body).Decode(&listed))
	assert.GreaterOrEqual(t, listed.TotalSize, int32(0))

	// GET single returns NotFound (no targets seeded).
	getResp, err := http.Get(srv.URL + "/targets/target-nope")
	require.NoError(t, err)
	defer getResp.Body.Close()
	assert.Equal(t, http.StatusNotFound, getResp.StatusCode)
}

// --- Audit & System ------------------------------------------------------------

func TestRESTAuditAndSystem(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	for _, path := range []string{"/audit/log", "/audit/traces"} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, path)

	}
	verifyResp, err := http.Get(srv.URL + "/audit/verify?runId=run-x")
	require.NoError(t, err)
	verifyResp.Body.Close()
	assert.Equal(t, http.StatusOK, verifyResp.StatusCode)

	versionResp, err := http.Get(srv.URL + "/system/version")
	require.NoError(t, err)
	defer versionResp.Body.Close()
	require.Equal(t, http.StatusOK, versionResp.StatusCode)
	var ver struct {
		Version string `json:"version"`
	}
	require.NoError(t, json.NewDecoder(versionResp.Body).Decode(&ver))
	assert.Equal(t, "test-v1.0", ver.Version)

	statusResp, err := http.Get(srv.URL + "/system/status")
	require.NoError(t, err)
	statusResp.Body.Close()
	assert.Equal(t, http.StatusOK, statusResp.StatusCode)
}

// --- Legacy /api/v1/ compatibility --------------------------------------------

func TestLegacyAPIv1RoutesStillWork(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	resp, err := http.Post(srv.URL+"/api/v1/SystemService/GetVersion", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	listResp, err := http.Get(srv.URL + "/api/v1/ChangeService/ListChanges")
	require.NoError(t, err)
	defer listResp.Body.Close()
	assert.Equal(t, http.StatusOK, listResp.StatusCode)

	badSvc, err := http.Get(srv.URL + "/api/v1/BogusService/Method")
	require.NoError(t, err)
	defer badSvc.Body.Close()
	assert.Equal(t, http.StatusBadRequest, badSvc.StatusCode)
}

// --- Routing fallbacks ----------------------------------------------------------

func TestRESTUnknownPathIs404(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	resp, err := http.Get(srv.URL + "/bogus/path")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// --- Auth middleware -------------------------------------------------------------

func TestGatewayAuthRequiredWhenTokenSet(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{AuthToken: "s3cret"})

	// No header → 401.
	noAuth, err := http.Get(srv.URL + "/system/version")
	require.NoError(t, err)
	noAuth.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, noAuth.StatusCode)

	// Wrong token → 401.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/system/version", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer wrong-token")
	wrong, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	wrong.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, wrong.StatusCode)

	// Correct token → 200.
	reqOK, err := http.NewRequest(http.MethodGet, srv.URL+"/system/version", nil)
	require.NoError(t, err)
	reqOK.Header.Set("Authorization", "Bearer s3cret")
	okResp, err := http.DefaultClient.Do(reqOK)
	require.NoError(t, err)
	okResp.Body.Close()
	assert.Equal(t, http.StatusOK, okResp.StatusCode)

	// Malformed scheme → 401.
	reqBad, err := http.NewRequest(http.MethodGet, srv.URL+"/system/version", nil)
	require.NoError(t, err)
	reqBad.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	badScheme, err := http.DefaultClient.Do(reqBad)
	require.NoError(t, err)
	badScheme.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, badScheme.StatusCode)
}

func TestGatewayAuthDisabledWhenTokenEmpty(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	resp, err := http.Get(srv.URL + "/system/version")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// --- CORS middleware ---------------------------------------------------------------

func TestCORSDeniedByDefault(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/system/version", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"),
		"empty CORSOrigins must not emit wildcard or reflected allow-origin headers")
}

func TestCORSAllowedExplicitOrigin(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{CORSOrigins: []string{"https://app.example.com"}})

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/system/version", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://app.example.com")
	okResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	okResp.Body.Close()
	assert.Equal(t, "https://app.example.com", okResp.Header.Get("Access-Control-Allow-Origin"))

	reqEvil, err := http.NewRequest(http.MethodGet, srv.URL+"/system/version", nil)
	require.NoError(t, err)
	reqEvil.Header.Set("Origin", "https://evil.example.com")
	evitResp, err := http.DefaultClient.Do(reqEvil)
	require.NoError(t, err)
	evitResp.Body.Close()
	assert.Empty(t, evitResp.Header.Get("Access-Control-Allow-Origin"))
}

func TestCORSWildcardOnlyViaExplicitStar(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{CORSOrigins: []string{"*"}})

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/system/version", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://anywhere.example.com")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, "https://anywhere.example.com", resp.Header.Get("Access-Control-Allow-Origin"))
}

func TestCORSPreflightOptions(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{CORSOrigins: []string{"https://app.example.com"}})

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/changes", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://app.example.com")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "https://app.example.com", resp.Header.Get("Access-Control-Allow-Origin"))
}

// --- Deeplink endpoints ---------------------------------------------------------------

type stubMobileApproval struct {
	lastAction string
	lastToken  string
	err        error
}

func (s *stubMobileApproval) ApproveViaDeepLink(_ context.Context, token string) error {
	s.lastAction = "approve"
	s.lastToken = token
	return s.err
}

func (s *stubMobileApproval) RejectViaDeepLink(_ context.Context, token string) error {
	s.lastAction = "reject"
	s.lastToken = token
	return s.err
}

func TestDeeplinkApproveWithoutServiceIs503(t *testing.T) {
	_, srv, _ := startTestGateway(t, ServeGatewayConfig{})

	resp, err := http.Post(srv.URL+"/changes/deeplink/approve", "application/json",
		strings.NewReader(`{"token":"tok"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestDeeplinkApproveWithService(t *testing.T) {
	gw, srv, _ := startTestGateway(t, ServeGatewayConfig{})
	stub := &stubMobileApproval{}
	gw.SetMobileApproval(stub)

	resp, err := http.Post(srv.URL+"/changes/deeplink/approve", "application/json",
		strings.NewReader(`{"token":"tok-123"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "approve", stub.lastAction)
	assert.Equal(t, "tok-123", stub.lastToken)

	rejectResp, err := http.Post(srv.URL+"/changes/deeplink/reject", "application/json",
		strings.NewReader(`{"token":"tok-456"}`))
	require.NoError(t, err)
	defer rejectResp.Body.Close()
	require.Equal(t, http.StatusOK, rejectResp.StatusCode)
	assert.Equal(t, "reject", stub.lastAction)
	assert.Equal(t, "tok-456", stub.lastToken)
}

func TestDeeplinkMissingTokenIs400(t *testing.T) {
	gw, srv, _ := startTestGateway(t, ServeGatewayConfig{})
	gw.SetMobileApproval(&stubMobileApproval{})

	resp, err := http.Post(srv.URL+"/changes/deeplink/approve", "application/json",
		strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestDeeplinkBypassesBearerAuthWhenTokenConfigured pins the mobile approval
// flow: the deeplink endpoints authenticate via the one-time token in the
// request body, so they must stay reachable from devices that cannot send a
// Bearer header, while every other endpoint keeps requiring the token.
func TestDeeplinkBypassesBearerAuthWhenTokenConfigured(t *testing.T) {
	gw, srv, _ := startTestGateway(t, ServeGatewayConfig{AuthToken: "s3cret"})
	stub := &stubMobileApproval{}
	gw.SetMobileApproval(stub)

	// No Authorization header: the one-time token in the body is the
	// credential, so the request must reach the handler.
	resp, err := http.Post(srv.URL+"/changes/deeplink/approve", "application/json",
		strings.NewReader(`{"token":"tok-123"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "approve", stub.lastAction)
	assert.Equal(t, "tok-123", stub.lastToken)

	rejectResp, err := http.Post(srv.URL+"/changes/deeplink/reject", "application/json",
		strings.NewReader(`{"token":"tok-456"}`))
	require.NoError(t, err)
	defer rejectResp.Body.Close()
	require.Equal(t, http.StatusOK, rejectResp.StatusCode)
	assert.Equal(t, "reject", stub.lastAction)
	assert.Equal(t, "tok-456", stub.lastToken)

	// Regression guard: ordinary endpoints still require the Bearer token.
	noAuth, err := http.Get(srv.URL + "/system/version")
	require.NoError(t, err)
	noAuth.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, noAuth.StatusCode)
}

// TestDeeplinkInvalidTokenIs401 verifies that token-validation failures are
// reported as 401 (not an opaque 500) so mobile clients can tell an expired
// or forged link apart from a server fault.
func TestDeeplinkInvalidTokenIs401(t *testing.T) {
	for name, sentinel := range map[string]error{
		"invalid": push.ErrInvalidToken,
		"expired": push.ErrTokenExpired,
	} {
		t.Run(name, func(t *testing.T) {
			gw, srv, _ := startTestGateway(t, ServeGatewayConfig{AuthToken: "s3cret"})
			stub := &stubMobileApproval{
				err: fmt.Errorf("approval: mobile: validate token: %w", sentinel),
			}
			gw.SetMobileApproval(stub)

			resp, err := http.Post(srv.URL+"/changes/deeplink/approve", "application/json",
				strings.NewReader(`{"token":"forged"}`))
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}
}
