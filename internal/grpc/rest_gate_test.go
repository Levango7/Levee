// rest_gate_test.go pins the GateService semantics: ad-hoc checks reuse
// the engine-grade gate constructors (cmd/probe/slo), "human" is
// rejected as an approval-chain concern, missing runtime dependencies
// fail closed, results are audited, and the REST endpoint answers
// 404 without the service / 400 on bad requests.

package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/state"
)

// seedGateTarget writes an inventory row with the given status.
func seedGateTarget(t *testing.T, store state.Store, host, status string) {
	t.Helper()
	require.NoError(t, store.UpsertTarget(context.Background(), &state.Target{
		ID:          "tgt-" + host,
		Hostname:    host,
		Port:        22,
		ChannelType: "ssh",
		Status:      status,
		CreatedAt:   time.Now().UTC(),
	}))
}

// loopbackDialer returns a dialer handing out the existing probeChannel
// stub (Exec → exit 0, stdout "ok\n"), reusing the CheckTarget test
// infrastructure.
func loopbackDialer(t *testing.T) ChannelDialer {
	t.Helper()
	return func(ctx context.Context, host string) (channel.Channel, error) {
		return &probeChannel{}, nil
	}
}

// failingLoopbackDialer hands out a channel whose Exec reports a
// non-zero exit so failure-path semantics can be pinned.
func failingLoopbackDialer() ChannelDialer {
	return func(ctx context.Context, host string) (channel.Channel, error) {
		return &failingProbeChannel{}, nil
	}
}

// failingProbeChannel is a probeChannel variant with a failing Exec.
type failingProbeChannel struct{}

func (c *failingProbeChannel) Connect(ctx context.Context) error { return nil }
func (c *failingProbeChannel) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	return &channel.ExecResult{ExitCode: 3, Stderr: "scripted failure"}, nil
}
func (c *failingProbeChannel) Upload(ctx context.Context, remotePath string, content io.Reader) error {
	return nil
}
func (c *failingProbeChannel) Download(ctx context.Context, remotePath string) (io.Reader, error) {
	return nil, nil
}
func (c *failingProbeChannel) Close() error      { return nil }
func (c *failingProbeChannel) IsConnected() bool { return true }

// --- GateService.Verify ------------------------------------------------------

func TestGateVerify_CmdCheckRequiresTarget(t *testing.T) {
	// The verify CommandGate runs remotely over a channel by design;
	// a targetless cmd has no channel. Refuse up front.
	store := newTestStore(t)
	svc := NewGateService(store, nil, "")
	_, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "cmd",
		Params: map[string]any{"cmd": "echo levee"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a target")
}

func TestGateVerify_CmdCheckFailsClosedOnExitMismatch(t *testing.T) {
	// Through a loopback channel whose Exec reports a scripted failure:
	// exit mismatch is an honest failure result, not an error.
	store := newTestStore(t)
	seedGateTarget(t, store, "lb-1", "active")
	svc := NewGateService(store, failingLoopbackDialer(), "")
	resp, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "cmd",
		Target: "lb-1",
		Params: map[string]any{"cmd": "exit 3", "expect_exit": 0},
	})
	require.NoError(t, err)
	assert.False(t, resp.Passed)
}

func TestGateVerify_CmdMissingCmdParamIsBadRequest(t *testing.T) {
	store := newTestStore(t)
	svc := NewGateService(store, nil, "")
	_, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "cmd",
		Params: map[string]any{"expect_exit": 0},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cmd")
}

func TestGateVerify_CmdWithTargetRequiresDialer(t *testing.T) {
	// Fail closed: no dialer (engine not wired) ⇒ explicit error, never
	// a silent pass or a guess.
	store := newTestStore(t)
	svc := NewGateService(store, nil, "")
	_, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "cmd",
		Target: "web-1",
		Params: map[string]any{"cmd": "true"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dialer")
}

func TestGateVerify_CmdWithUnknownTargetRefused(t *testing.T) {
	store := newTestStore(t)
	called := false
	svc := NewGateService(store, func(ctx context.Context, host string) (channel.Channel, error) {
		called = true
		return nil, nil
	}, "")
	_, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "cmd",
		Target: "ghost-1",
		Params: map[string]any{"cmd": "true"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in inventory")
	assert.False(t, called, "dialer must never run for an unknown host")
}

func TestGateVerify_CmdRetiredTargetRefused(t *testing.T) {
	store := newTestStore(t)
	seedGateTarget(t, store, "old-1", "retired")
	svc := NewGateService(store, func(ctx context.Context, host string) (channel.Channel, error) {
		t.Fatal("dialer must never run for a retired host")
		return nil, nil
	}, "")
	_, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "cmd",
		Target: "old-1",
		Params: map[string]any{"cmd": "true"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retired")
}

func TestGateVerify_ProbeCheckSelfDescribing(t *testing.T) {
	store := newTestStore(t)
	svc := NewGateService(store, nil, "")
	// A tcp probe against a closed port fails honestly (the gate
	// validates params itself; no runtime dependency needed).
	resp, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type: "probe",
		Params: map[string]any{
			"kind": "tcp", "host": "127.0.0.1", "port": 1, "mode": "connect",
		},
	})
	require.NoError(t, err)
	assert.False(t, resp.Passed, "port 1 connect must fail: %s", resp.Message)
}

func TestGateVerify_SLORequiresPrometheusURL(t *testing.T) {
	store := newTestStore(t)
	svc := NewGateService(store, nil, "") // no Prometheus URL
	_, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "slo",
		Params: map[string]any{"query": "up", "threshold": 1},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Prometheus")
}

func TestGateVerify_SLORequiresQueryAndThreshold(t *testing.T) {
	store := newTestStore(t)
	svc := NewGateService(store, nil, "http://prom:9090")
	_, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "slo",
		Params: map[string]any{"threshold": 1},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query")

	_, err = svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "slo",
		Params: map[string]any{"query": "up"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "threshold")
}

func TestGateVerify_HumanRejected(t *testing.T) {
	// The approval chain owns human checkpoints; ad-hoc verification
	// must not fabricate a blocking approval.
	store := newTestStore(t)
	svc := NewGateService(store, nil, "")
	_, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "human",
		Params: map[string]any{"prompt": "ok?"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "approval chain")
}

func TestGateVerify_UnknownTypeRejected(t *testing.T) {
	store := newTestStore(t)
	svc := NewGateService(store, nil, "")
	_, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "telepathy",
		Params: map[string]any{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cmd|probe|slo")
}

func TestGateVerify_Audited(t *testing.T) {
	store := newTestStore(t)
	seedGateTarget(t, store, "lb-audit", "active")
	svc := NewGateService(store, loopbackDialer(t), "")
	_, err := svc.Verify(context.Background(), &GateVerifyRequest{
		Type:   "cmd",
		Target: "lb-audit",
		RunID:  "run-audit",
		Params: map[string]any{"cmd": "true"},
	})
	require.NoError(t, err)

	rows, err := store.ListAudits(context.Background(), state.AuditFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	var saw bool
	for _, a := range rows {
		if a.Action == "gate_verify" {
			saw = true
			assert.Equal(t, "run-audit", a.RunID)
			assert.Contains(t, a.Result, "passed")
		}
	}
	assert.True(t, saw, "ad-hoc verification must leave an audit row")
}

// --- REST endpoint -----------------------------------------------------------

func newGateTestGateway(t *testing.T, gateSvc gateServiceHandler) *Gateway {
	t.Helper()
	gw := NewGateway(ServeGatewayConfig{Addr: "127.0.0.1:0"})
	gw.SetGateService(gateSvc)
	return gw
}

func TestGateREST_VerifyEndpoint(t *testing.T) {
	store := newTestStore(t)
	seedGateTarget(t, store, "lb-rest", "active")
	gw := newGateTestGateway(t, NewGateService(store, loopbackDialer(t), ""))

	body, _ := json.Marshal(GateVerifyRequest{
		Type:   "cmd",
		Name:   "rest-check",
		Target: "lb-rest",
		Params: map[string]any{"cmd": "echo ok", "expect_stdout": "ok"},
	})
	req := httptest.NewRequest(http.MethodPost, "/gates/verify", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	gw.handleGateVerify(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp GateVerifyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Passed)
	assert.Equal(t, "rest-check", resp.Gate)
}

func TestGateREST_BadJSONIs400(t *testing.T) {
	store := newTestStore(t)
	gw := newGateTestGateway(t, NewGateService(store, nil, ""))
	req := httptest.NewRequest(http.MethodPost, "/gates/verify", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	gw.handleGateVerify(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGateREST_UnconfiguredServiceIs404(t *testing.T) {
	gw := newGateTestGateway(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/gates/verify", strings.NewReader(`{"type":"cmd"}`))
	rec := httptest.NewRecorder()
	gw.handleGateVerify(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGateREST_UnknownTypeIs400(t *testing.T) {
	store := newTestStore(t)
	gw := newGateTestGateway(t, NewGateService(store, nil, ""))
	body, _ := json.Marshal(GateVerifyRequest{Type: "vibes"})
	req := httptest.NewRequest(http.MethodPost, "/gates/verify", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	gw.handleGateVerify(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "cmd|probe|slo")
}

func TestGateREST_DispatchRoutesGatesBranch(t *testing.T) {
	// The restRoute dispatcher must send /gates/verify to the gate
	// handler and 404 unknown /gates/* paths.
	store := newTestStore(t)
	seedGateTarget(t, store, "lb-dispatch", "active")
	gw := newGateTestGateway(t, NewGateService(store, loopbackDialer(t), ""))

	req := httptest.NewRequest(http.MethodPost, "/gates/verify",
		strings.NewReader(`{"type":"cmd","target":"lb-dispatch","params":{"cmd":"true"}}`))
	rec := httptest.NewRecorder()
	gw.restRoute().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	req = httptest.NewRequest(http.MethodGet, "/gates/verify", nil)
	rec = httptest.NewRecorder()
	gw.restRoute().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/gates/nonsense", nil)
	rec = httptest.NewRecorder()
	gw.restRoute().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
