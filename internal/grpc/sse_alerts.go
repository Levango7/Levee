// sse_alerts.go — the SSE endpoint for the alert stream.
//
// Why this exists: SubscribeAlerts is a gRPC server stream the gateway's
// proto-JSON table answers with 501 ("streaming RPCs not supported"), so the
// console could never receive alerts live. The change-events route
// (sse_route.go + sse.go) established the shape: one auth decision up front,
// then text/event-stream frames with keep-alives.
//
// Two deltas from the change-events route, both rooted in how alerts differ
// from change events:
//
//   - The payload is the alert's own JSON (id/source/severity/environment…)
//     rather than a protojson ChangeEvent — the frontend renders severity and
//     target directly, and the internal alert.Alert already carries the
//     environment the source registration gave it.
//   - The stream REPLAYS the ring snapshot before going live. Change events
//     start empty because a change's history is readable through /trace; the
//     alert ring is the only record of what already fired, so a subscriber
//     that received nothing until the next fire would miss everything the
//     operator is currently looking at.
//
// Visibility follows GetAlertStatus/SubscribeAlerts: judged on the alert's
// OWNED environment (the source registration's answer, never caller-supplied),
// applied to both the replay and the live tail.
package grpc

import (
	"encoding/json"
	"net/http"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/nexus/levee/internal/alert"
)

// handleAlertsStream serves GET /alerts/stream.
//
// Authorization is evaluated ONCE, per alert, the way SubscribeAlerts does it:
// a stream is long-lived and the ring is replayed, so this route judges every
// alert through the same resourceVisibility predicate the gRPC stream uses —
// a caller entitled to one environment still receives it, and an empty stream
// is the honest answer for a caller entitled to none.
func (gw *Gateway) handleAlertsStream(w http.ResponseWriter, r *http.Request) {
	svc := gw.alert
	if svc == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "AlertService not configured")
		return
	}
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	visible := resourceVisibility(ctx, svc.authz)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	writeAlert := func(a *alert.Alert) {
		if a == nil {
			return
		}
		if visible != nil && !visible(a.Environment) {
			return
		}
		data, err := json.Marshal(a)
		if err != nil {
			return
		}
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		flusher.Flush()
	}

	// Replay: the ring's current contents, oldest first. Duplicates with the
	// live tail are acceptable (an alert arriving between Snapshot and
	// Subscribe would otherwise be lost — at-least-once beats possibly-never
	// for an operations feed), and the client keys on alert.id.
	for _, a := range svc.Snapshot() {
		writeAlert(a)
	}

	ch, remove := svc.SubscribeAlertsChan()
	defer remove()

	keepAlive := time.NewTicker(SSEKeepAliveInterval)
	defer keepAlive.Stop()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case <-keepAlive.C:
			_, _ = w.Write([]byte(": keep-alive\n\n"))
			flusher.Flush()
		case a := <-ch:
			writeAlert(a)
		}
	}
}

// dispatchAlerts routes the "alerts" RESTful family. GET /alerts/stream is the
// SSE endpoint; every other shape answers a pointer to the two legitimate
// forms (proto-JSON table for GetAlertStatus/ReceiveAlert, this route for the
// stream), because a silent 400 on a guessed path is how integrations end up
// scraping instead of subscribing.
func (gw *Gateway) dispatchAlerts(w http.ResponseWriter, r *http.Request, method, path string) {
	switch {
	case path == "/alerts/stream" && method == http.MethodGet:
		gw.handleAlertsStream(w, r)
	default:
		writeJSONError(w, http.StatusBadRequest,
			"unknown alerts path: "+path+" (GetAlertStatus/ReceiveAlert live under /api/v1/AlertService/, the stream is /alerts/stream)")
	}
}
