package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/alert"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewAlertCmd verifies the command tree.
func TestNewAlertCmd(t *testing.T) {
	cmd := newAlertCmd()
	require.NotNil(t, cmd)
	assert.Equal(t, "alert", cmd.Use)

	subs := cmd.Commands()
	names := make([]string, 0, len(subs))
	for _, s := range subs {
		names = append(names, s.Name())
	}
	assert.Contains(t, names, "serve")
	assert.Contains(t, names, "list")
	assert.Contains(t, names, "show")
	assert.Contains(t, names, "silence")
	assert.Contains(t, names, "history")
}

// TestAlertServeCmdFlags verifies flag wiring.
func TestAlertServeCmdFlags(t *testing.T) {
	cmd := newAlertServeCmd()
	for _, f := range []string{"addr", "dedup", "aggregate"} {
		assert.NotNil(t, cmd.Flag(f), "missing --%s", f)
	}
}

// TestAlertListRequiresServer errors without --server.
func TestAlertListRequiresServer(t *testing.T) {
	alertOptServer = ""
	err := runAlertList(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--server")
}

// TestAlertListAgainstServer exercises the happy path against a test gateway.
func TestAlertListAgainstServer(t *testing.T) {
	g := alert.NewAlertGateway(alert.GatewayConfig{Addr: ":0"}, nil)
	g.RegisterAdapter(alert.NewPrometheusAdapter())
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	// Post one alert so /alerts has something.
	payload := `[{"status":"firing","labels":{"alertname":"X"},"startsAt":"2026-08-16T12:00:00Z"}]`
	resp, err := http.Post(srv.URL+"/webhook/prometheus", "application/json", strings.NewReader(payload))
	require.NoError(t, err)
	resp.Body.Close()

	alertOptServer = srv.URL
	out, err := captureStdout(func() error { return runAlertList(nil, nil) })
	require.NoError(t, err)
	assert.Contains(t, out, "X")
}

// TestAlertShowMissingID errors when the ID is not found.
func TestAlertShowMissingID(t *testing.T) {
	g := alert.NewAlertGateway(alert.GatewayConfig{Addr: ":0"}, nil)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	alertOptServer = srv.URL
	err := runAlertShow(nil, []string{"no-such-id"})
	require.Error(t, err)
}

// TestAlertShowRequiresServer errors without --server.
func TestAlertShowRequiresServer(t *testing.T) {
	alertOptServer = ""
	err := runAlertShow(nil, []string{"x"})
	require.Error(t, err)
}

// TestAlertSilenceAddInProcess adds a rule without a server.
func TestAlertSilenceAddInProcess(t *testing.T) {
	alertOptServer = ""
	alertOptMatch = []string{"host=n1", "env=prod"}
	alertOptDuration = time.Hour
	alertOptReason = "maintenance"
	alertOptSource = "prometheus"

	out, err := captureStdout(func() error { return runAlertSilenceAdd(nil, nil) })
	require.NoError(t, err)
	assert.Contains(t, out, "silence-")
	assert.Contains(t, out, "maintenance")
}

// TestAlertSilenceListAgainstServer lists rules via HTTP.
func TestAlertSilenceListAgainstServer(t *testing.T) {
	g := alert.NewAlertGateway(alert.GatewayConfig{Addr: ":0"}, nil)
	g.Silencer().AddRule(alert.SilenceRule{Match: map[string]string{"host": "n1"}})
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	alertOptServer = srv.URL
	out, err := captureStdout(func() error { return runAlertSilenceList(nil, nil) })
	require.NoError(t, err)
	assert.Contains(t, out, "silence-")
}

// TestAlertSilenceRemoveAgainstServer removes a rule.
func TestAlertSilenceRemoveAgainstServer(t *testing.T) {
	g := alert.NewAlertGateway(alert.GatewayConfig{Addr: ":0"}, nil)
	id := g.Silencer().AddRule(alert.SilenceRule{Match: map[string]string{"host": "n1"}})
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	alertOptServer = srv.URL
	out, err := captureStdout(func() error { return runAlertSilenceRemove(nil, []string{id}) })
	require.NoError(t, err)
	assert.Contains(t, out, id)
}

// TestAlertSilenceRemoveNotFound returns an error.
func TestAlertSilenceRemoveNotFound(t *testing.T) {
	g := alert.NewAlertGateway(alert.GatewayConfig{Addr: ":0"}, nil)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	alertOptServer = srv.URL
	err := runAlertSilenceRemove(nil, []string{"no-such"})
	require.Error(t, err)
}

// TestAlertHistoryAgainstServer exercises the history command.
func TestAlertHistoryAgainstServer(t *testing.T) {
	g := alert.NewAlertGateway(alert.GatewayConfig{Addr: ":0"}, nil)
	g.RegisterAdapter(alert.NewCustomAdapter())
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	payload := `{"title":"DiskFull","severity":"critical","starts_at":"2026-08-16T12:00:00Z"}`
	resp, err := http.Post(srv.URL+"/webhook/custom", "application/json", strings.NewReader(payload))
	require.NoError(t, err)
	resp.Body.Close()

	alertOptServer = srv.URL
	alertOptLimit = 10
	out, err := captureStdout(func() error { return runAlertHistory(nil, nil) })
	require.NoError(t, err)
	assert.Contains(t, out, "DiskFull")
}

// TestAlertSilenceAddViaHTTP adds a rule via the gateway REST API.
func TestAlertSilenceAddViaHTTP(t *testing.T) {
	g := alert.NewAlertGateway(alert.GatewayConfig{Addr: ":0"}, nil)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	alertOptServer = srv.URL
	alertOptMatch = []string{"env=prod"}
	alertOptDuration = 30 * time.Minute
	alertOptReason = "deploy"
	alertOptSource = ""

	out, err := captureStdout(func() error { return runAlertSilenceAdd(nil, nil) })
	require.NoError(t, err)
	assert.Contains(t, out, "id")

	// Verify the rule landed on the server.
	rules := g.Silencer().ListRules()
	require.Len(t, rules, 1)
	assert.Equal(t, "deploy", rules[0].Reason)
}

// TestAlertGetAndPrint covers the helper directly.
func TestAlertGetAndPrint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	out, err := captureStdout(func() error { return alertGetAndPrint(srv.URL) })
	require.NoError(t, err)
	assert.Contains(t, out, "ok")
}

// TestAlertGetAndPrintError propagates non-200.
func TestAlertGetAndPrintError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()
	err := alertGetAndPrint(srv.URL)
	require.Error(t, err)
}

// TestRunAlertServeStartsAndStops exercises the serve command lifecycle.
// Signal-based shutdown is platform-dependent, so we test the underlying
// gateway Start/Stop directly and only smoke-test the cobra wiring.
func TestRunAlertServeStartsAndStops(t *testing.T) {
	// Smoke-test: verify the command can be built and flags are wired.
	cmd := newAlertServeCmd()
	require.NotNil(t, cmd.RunE)

	// Direct gateway lifecycle test.
	g := alert.NewAlertGateway(alert.GatewayConfig{Addr: ":0"}, nil)
	g.RegisterAdapter(alert.NewPrometheusAdapter())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.Listener.Addr().String()
	srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx, addr) }()

	// Wait for the listener to be ready.
	require.Eventually(t, func() bool {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not shut down in time")
	}
}

// TestAlertSilenceCmdTree verifies the silence sub-commands.
func TestAlertSilenceCmdTree(t *testing.T) {
	cmd := newAlertSilenceCmd()
	names := []string{}
	for _, c := range cmd.Commands() {
		names = append(names, c.Name())
	}
	assert.Contains(t, names, "add")
	assert.Contains(t, names, "list")
	assert.Contains(t, names, "remove")
}

// TestAlertHistoryRequiresServer errors without --server.
func TestAlertHistoryRequiresServer(t *testing.T) {
	alertOptServer = ""
	err := runAlertHistory(nil, nil)
	require.Error(t, err)
}

// TestAlertSilenceListRequiresServer errors without --server.
func TestAlertSilenceListRequiresServer(t *testing.T) {
	alertOptServer = ""
	err := runAlertSilenceList(nil, nil)
	require.Error(t, err)
}

// TestAlertSilenceRemoveRequiresServer errors without --server.
func TestAlertSilenceRemoveRequiresServer(t *testing.T) {
	alertOptServer = ""
	err := runAlertSilenceRemove(nil, []string{"x"})
	require.Error(t, err)
}

// TestAlertSilenceAddMarshalSmoke is a defensive smoke test.
func TestAlertSilenceAddMarshalSmoke(t *testing.T) {
	rule := alert.SilenceRule{Match: map[string]string{"k": "v"}}
	_, err := json.Marshal(rule)
	require.NoError(t, err)
}

// Ensure unused imports are referenced (context used by TestRunAlertServe).
var _ = context.Background
