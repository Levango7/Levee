package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingHandler captures every alert dispatched by the gateway.
type recordingHandler struct {
	count  atomic.Int32
	alerts []*Alert
}

func (h *recordingHandler) HandleAlert(_ context.Context, a *Alert) error {
	h.count.Add(1)
	h.alerts = append(h.alerts, a)
	return nil
}

// newTestGateway builds a gateway with both adapters registered and a recording
// handler.
func newTestGateway(t *testing.T, dedup, aggregate time.Duration) (*AlertGateway, *recordingHandler) {
	t.Helper()
	h := &recordingHandler{}
	cfg := GatewayConfig{Addr: ":0", Dedup: dedup, Aggregate: aggregate}
	g := NewAlertGateway(cfg, h)
	g.RegisterAdapter(NewPrometheusAdapter())
	g.RegisterAdapter(NewCustomAdapter())
	return g, h
}

// freeAddr returns an unused 127.0.0.1:port address.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// startGateway starts g on a free port and returns the base URL and a stop
// function.
func startGateway(t *testing.T, g *AlertGateway) (string, func()) {
	t.Helper()
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx, addr) }()
	// Wait until the listener is ready.
	require.Eventually(t, func() bool {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)
	return "http://" + addr, func() {
		cancel()
		<-done
	}
}

// TestGatewayHealthz verifies the liveness endpoint.
func TestGatewayHealthz(t *testing.T) {
	g, _ := newTestGateway(t, 0, 0)
	base, stop := startGateway(t, g)
	defer stop()

	resp, err := http.Get(base + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, "ok", out["status"])
}

// TestGatewayWebhookPrometheus end-to-end: post an Alertmanager payload and
// verify the alert reaches the handler.
func TestGatewayWebhookPrometheus(t *testing.T) {
	g, h := newTestGateway(t, 0, 0)
	base, stop := startGateway(t, g)
	defer stop()

	payload := `[{"status":"firing","labels":{"alertname":"HighCpu","severity":"warning"},"startsAt":"2026-08-16T12:00:00Z"}]`
	resp, err := http.Post(base+"/webhook/prometheus", "application/json", strings.NewReader(payload))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	require.Eventually(t, func() bool { return h.count.Load() == 1 }, time.Second, 10*time.Millisecond)
	require.Len(t, h.alerts, 1)
	assert.Equal(t, "HighCpu", h.alerts[0].Title)
}

// TestGatewayWebhookCustom end-to-end for the custom adapter.
func TestGatewayWebhookCustom(t *testing.T) {
	g, h := newTestGateway(t, 0, 0)
	base, stop := startGateway(t, g)
	defer stop()

	payload := `{"source":"custom","severity":"critical","title":"DiskFull","starts_at":"2026-08-16T12:00:00Z"}`
	resp, err := http.Post(base+"/webhook/custom", "application/json", strings.NewReader(payload))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.Eventually(t, func() bool { return h.count.Load() == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, "DiskFull", h.alerts[0].Title)
}

// TestGatewayUnknownAdapter returns 404.
func TestGatewayUnknownAdapter(t *testing.T) {
	g, _ := newTestGateway(t, 0, 0)
	base, stop := startGateway(t, g)
	defer stop()

	resp, err := http.Post(base+"/webhook/bogus", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestGatewayBadPayload returns 400.
func TestGatewayBadPayload(t *testing.T) {
	g, _ := newTestGateway(t, 0, 0)
	base, stop := startGateway(t, g)
	defer stop()

	resp, err := http.Post(base+"/webhook/prometheus", "application/json", strings.NewReader(`not json`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestGatewayDedup rejects the second identical alert.
func TestGatewayDedup(t *testing.T) {
	g, h := newTestGateway(t, time.Hour, 0)
	base, stop := startGateway(t, g)
	defer stop()

	payload := `[{"status":"firing","labels":{"alertname":"X"},"startsAt":"2026-08-16T12:00:00Z"}]`
	for i := 0; i < 2; i++ {
		resp, err := http.Post(base+"/webhook/prometheus", "application/json", strings.NewReader(payload))
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", body)
	}
	require.Eventually(t, func() bool { return h.count.Load() == 1 }, time.Second, 10*time.Millisecond)
}

// TestGatewaySilencing rejects alerts matching a silence rule.
func TestGatewaySilencing(t *testing.T) {
	g, h := newTestGateway(t, 0, 0)
	g.Silencer().AddRule(SilenceRule{Match: map[string]string{"alertname": "Silenced"}})
	base, stop := startGateway(t, g)
	defer stop()

	payload := `[{"status":"firing","labels":{"alertname":"Silenced"},"startsAt":"2026-08-16T12:00:00Z"}]`
	resp, err := http.Post(base+"/webhook/prometheus", "application/json", strings.NewReader(payload))
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	results := out["results"].([]any)
	first := results[0].(map[string]any)
	assert.Equal(t, "silenced", first["status"])
	require.Eventually(t, func() bool { return h.count.Load() == 0 }, time.Second, 10*time.Millisecond)
}

// TestGatewayListAlerts returns recently received alerts.
func TestGatewayListAlerts(t *testing.T) {
	g, _ := newTestGateway(t, 0, 0)
	base, stop := startGateway(t, g)
	defer stop()

	payload := `[{"status":"firing","labels":{"alertname":"X"},"startsAt":"2026-08-16T12:00:00Z"}]`
	resp, err := http.Post(base+"/webhook/prometheus", "application/json", strings.NewReader(payload))
	require.NoError(t, err)
	resp.Body.Close()

	resp2, err := http.Get(base + "/alerts")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&out))
	assert.Equal(t, float64(1), out["count"])
}

// TestGatewaySilencesCRUD exercises the silence REST endpoints.
func TestGatewaySilencesCRUD(t *testing.T) {
	g, _ := newTestGateway(t, 0, 0)
	base, stop := startGateway(t, g)
	defer stop()

	// Add.
	rule := SilenceRule{Match: map[string]string{"host": "n1"}, Reason: "test"}
	body, _ := json.Marshal(rule)
	resp, err := http.Post(base+"/silences", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var created map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	id := created["id"].(string)
	resp.Body.Close()

	// List.
	resp2, err := http.Get(base + "/silences")
	require.NoError(t, err)
	var list map[string]any
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&list))
	assert.Equal(t, float64(1), list["count"])
	resp2.Body.Close()

	// Get by ID.
	resp3, err := http.Get(base + "/silences/" + id)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
	resp3.Body.Close()

	// Delete.
	req, _ := http.NewRequest(http.MethodDelete, base+"/silences/"+id, nil)
	resp4, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, resp4.StatusCode)
	resp4.Body.Close()

	// Delete again -> 404.
	req2, _ := http.NewRequest(http.MethodDelete, base+"/silences/"+id, nil)
	resp5, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp5.StatusCode)
	resp5.Body.Close()
}

// TestGatewayMethodNotAllowed verifies 405 responses.
func TestGatewayMethodNotAllowed(t *testing.T) {
	g, _ := newTestGateway(t, 0, 0)
	base, stop := startGateway(t, g)
	defer stop()

	resp, err := http.Get(base + "/webhook/prometheus")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// TestGatewayProcessAlertDirect exercises processAlert without HTTP.
func TestGatewayProcessAlertDirect(t *testing.T) {
	g, h := newTestGateway(t, 0, 0)

	a := NewAlert("prometheus", "X", SeverityWarning, nil, time.Now())
	status, _ := g.processAlert(context.Background(), a)
	assert.Equal(t, "accepted", status)
	assert.Equal(t, int32(1), h.count.Load())

	// Invalid alert.
	bad := &Alert{Source: "", Title: "", StartsAt: time.Time{}}
	status, _ = g.processAlert(context.Background(), bad)
	assert.Equal(t, "invalid", status)
}

// TestGatewayAdapterNames returns sorted names.
func TestGatewayAdapterNames(t *testing.T) {
	g, _ := newTestGateway(t, 0, 0)
	names := g.AdapterNames()
	assert.Equal(t, []string{"custom", "prometheus"}, names)
}

// TestGatewayStartNoAddr errors.
func TestGatewayStartNoAddr(t *testing.T) {
	g := NewAlertGateway(GatewayConfig{}, nil)
	err := g.Start(context.Background(), "")
	require.Error(t, err)
}

// TestGatewayHandlerError surfaces handler errors.
func TestGatewayHandlerError(t *testing.T) {
	g := NewAlertGateway(GatewayConfig{Addr: ":0"}, AlertHandlerFunc(func(_ context.Context, _ *Alert) error {
		return assert.AnError
	}))
	g.RegisterAdapter(NewPrometheusAdapter())
	base, stop := startGateway(t, g)
	defer stop()

	payload := `[{"status":"firing","labels":{"alertname":"X"},"startsAt":"2026-08-16T12:00:00Z"}]`
	resp, err := http.Post(base+"/webhook/prometheus", "application/json", strings.NewReader(payload))
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	results := out["results"].([]any)
	first := results[0].(map[string]any)
	assert.Equal(t, "error", first["status"])
}

// TestGatewayMuxSmoke uses httptest.NewServer to exercise the mux without
// binding a real port.
func TestGatewayMuxSmoke(t *testing.T) {
	g, _ := newTestGateway(t, 0, 0)
	srv := httptest.NewServer(g.buildMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestReadBodyEmpty errors on empty input.
func TestReadBodyEmpty(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	_, err := readBody(req, 1024)
	require.Error(t, err)
}

// TestWriteError emits a JSON envelope.
func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusBadRequest, "boom")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "boom")
}
