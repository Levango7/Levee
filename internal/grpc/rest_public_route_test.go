// rest_public_route_test.go pins the per-route bearer exemption that the
// ChatOps callback ingress depends on. The failure this prevents is not
// hypothetical in the other direction: MetricsPublic is a gateway-wide
// switch, so the obvious shortcut for a callback route (flip it on) would
// also publish /metrics to whoever can reach the port.
package grpc

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func okHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

func getRoute(t *testing.T, h http.Handler, url string, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPublicRoute_ExemptWhileMetricsStaysPrivate(t *testing.T) {
	gw := NewGateway(ServeGatewayConfig{AuthToken: "s3cret"})
	gw.SetExtraRoute("/metrics", okHandler("metrics"))
	gw.SetPublicRoute("/chatops/callback/bot", okHandler("callback"))

	mux := http.NewServeMux()
	gw.mountExtraRoutes(mux)

	// Without any credential: the public route answers, /metrics does not.
	rec := getRoute(t, mux, "/chatops/callback/bot", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "callback", rec.Body.String())

	rec = getRoute(t, mux, "/metrics", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code,
		"a per-route exemption must not leak into the other extra routes")

	// With a credential both answer, exactly as before.
	require.Equal(t, http.StatusOK, getRoute(t, mux, "/metrics", "s3cret").Code)
	require.Equal(t, http.StatusOK, getRoute(t, mux, "/chatops/callback/bot", "s3cret").Code)
}

func TestPublicRoute_NoAuthConfiguredEverythingAnswers(t *testing.T) {
	// With no token configured the deployment has no identity model at all,
	// so the exemption changes nothing — documented so it is not misread as
	// the public route bypassing a check.
	gw := NewGateway(ServeGatewayConfig{})
	gw.SetExtraRoute("/metrics", okHandler("metrics"))
	gw.SetPublicRoute("/chatops/callback/bot", okHandler("callback"))

	mux := http.NewServeMux()
	gw.mountExtraRoutes(mux)

	assert.Equal(t, http.StatusOK, getRoute(t, mux, "/chatops/callback/bot", "").Code)
	assert.Equal(t, http.StatusOK, getRoute(t, mux, "/metrics", "").Code)
}
