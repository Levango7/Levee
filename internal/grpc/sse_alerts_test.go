package grpc

// sse_alerts_test.go pins the alert SSE route's three behaviours: the ring
// REPLAY (a subscriber must see what already fired — the ring is the only
// record of it), the live tail arriving after the subscribe, and the
// visibility narrowing that follows GetAlertStatus/SubscribeAlerts (judged on
// the alert's OWNED environment). The route's reason to exist at all is that
// SubscribeAlerts is a gRPC stream the gateway answers 501.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/grpc/pb"
)

func newAlertTestService(t *testing.T) *AlertService {
	t.Helper()
	return NewAlertService(nil, nil)
}

func ingestAlert(t *testing.T, svc *AlertService, id, severity string) {
	t.Helper()
	// No alert-sources.yaml configured: the bare-context ingress is the
	// in-process gateway path (the same shape TestReceiveAlert_Success uses).
	_, err := svc.ReceiveAlert(context.Background(), &pb.AlertMessage{
		Id: id, Source: "prometheus", Severity: severity, Title: "t" + id,
	})
	require.NoError(t, err)
}

func alertStreamServer(svc *AlertService) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gw := &Gateway{alert: svc, authz: svc.authz}
		gw.handleAlertsStream(w, r)
	}))
}

// readAlertStream drains the stream for d and returns everything it wrote.
func readAlertStream(t *testing.T, url string, d time.Duration) string {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	deadline := time.Now().Add(d)
	var out strings.Builder
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
			if strings.Count(out.String(), "\n\n") >= 1 {
				// At least one full frame; give a second read to coalesce a
				// possible tail, then stop — the assertions read what landed.
				time.Sleep(80 * time.Millisecond)
				n2, _ := resp.Body.Read(buf)
				out.Write(buf[:n2])
				break
			}
		}
		if err != nil {
			break
		}
	}
	return out.String()
}

// TestAlertsStreamRouteIsWired pins the route-table entry that makes
// /alerts/stream reachable. The other cases in this file drive
// handleAlertsStream directly, so removing the `case path == "/alerts/stream"`
// branch in dispatchAlerts — or the "alerts" family entry in the gateway's
// resource table — would drop the console's live alert feed and leave every
// existing test green. The ledger records the SSE stream as landed
// (docs/product-roadmap.md, rest_exposure_matrix_test.go); this is the
// code-side half of that claim, and it fails the moment the route is unwired.
func TestAlertsStreamRouteIsWired(t *testing.T) {
	svc := newAlertTestService(t)
	ingestAlert(t, svc, "a-wired", "critical")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gw := &Gateway{alert: svc, authz: svc.authz}
		gw.dispatchAlerts(w, r, r.Method, r.URL.Path)
	}))
	defer srv.Close()

	out := readAlertStream(t, srv.URL+"/alerts/stream", 700*time.Millisecond)
	assert.Contains(t, out, `"id":"a-wired"`,
		"GET /alerts/stream must reach the stream handler through dispatchAlerts (the ring replay proves it)")

	// An unknown path under the family must get the pointer error, not a bare
	// 400 — the message is what keeps an integration from scraping a guessed
	// route instead of subscribing to the real one.
	resp, err := http.Get(srv.URL + "/alerts/nope")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "/alerts/stream",
		"the unknown-path error must point at the real stream route")
}

func TestAlertsStreamReplaysRing(t *testing.T) {
	svc := newAlertTestService(t)
	ingestAlert(t, svc, "a-1", "critical")

	srv := alertStreamServer(svc)
	defer srv.Close()

	out := readAlertStream(t, srv.URL+"/alerts/stream", 700*time.Millisecond)
	assert.Contains(t, out, `"id":"a-1"`, "the ring replay must reach the subscriber")
	assert.Contains(t, out, "data: ", "frames are SSE-shaped")
}

func TestAlertsStreamLiveTail(t *testing.T) {
	svc := newAlertTestService(t)

	srv := alertStreamServer(svc)
	defer srv.Close()

	// Fire one alert AFTER the stream is open; it must arrive live.
	go func() {
		time.Sleep(150 * time.Millisecond)
		ingestAlert(t, svc, "a-live", "warning")
	}()

	out := readAlertStream(t, srv.URL+"/alerts/stream", 2*time.Second)
	assert.Contains(t, out, `"id":"a-live"`, "an alert fired after subscribe must arrive live")
}

// Visibility: the stream narrows by the alert's OWNED environment, the same
// predicate SubscribeAlerts applies. Proving it needs an authenticated
// identity with a narrow view, which the bare-context ingress cannot carry —
// that narrowing is already pinned at the service layer by
// TestServePolicyRefusesEveryUngrantedCall's alert probe, so this file does
// not duplicate it with a weaker fixture.
func TestAlertsStreamUnattributedAlertsReachEveryone(t *testing.T) {
	svc := newAlertTestService(t)
	ingestAlert(t, svc, "a-any", "critical")

	srv := alertStreamServer(svc)
	defer srv.Close()

	out := readAlertStream(t, srv.URL+"/alerts/stream", 700*time.Millisecond)
	assert.Contains(t, out, `"id":"a-any"`, "an unattributed alert (no sources configured) reaches the default view")
}
