// sse.go — the SSE endpoint that gives the browser console a real event stream.
//
// What was missing: the console polled GET /changes/<id>/logs every 2 seconds,
// so progress arrived up to 2s late and every poll re-sent the whole tail. The
// engine has always had live events — publishEvent fans out to the bus that
// WatchChange (gRPC) subscribes to — but nothing exposed them over HTTP, which
// is the only transport a browser can hold open.
//
// This file is the HTTP side of that bus: subscribe through the same pair
// WatchChange uses, encode each event as a text/event-stream frame, and stop
// where WatchChange stops (on a terminal status), so the two transports cannot
// disagree about when a change is finished.
//
// Authorization is the change's read gate, decided once up front by the caller:
// a stream is long-lived, and re-deciding per event would cost and could flip
// mid-stream.

package grpc

import (
	"fmt"
	"net/http"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/runstatus"
)

// SSEKeepAliveInterval is how often an idle stream emits a comment frame.
// Between that and the terminal status the connection carries nothing, and
// intermediaries treat silence as a dead peer.
const SSEKeepAliveInterval = 15 * time.Second

// ChangeEventSubscriber is the half of the event bus this endpoint uses:
// subscribe for one change, and drop the subscription again. Declared here
// rather than taking a *eventBus so the HTTP layer names only what it needs,
// and a test can hand in its own.
type ChangeEventSubscriber interface {
	Subscribe(changeID string) chan *pb.ChangeEvent
	Unsubscribe(changeID string, ch chan *pb.ChangeEvent)
}

// ServeChangeEvents streams one change's events to w as text/event-stream.
//
// bus must be non-nil; a gateway with no event bus (services never registered)
// must answer 404 before this is called. The run's existence is validated by
// the caller too, so this function can assume changeID names a real change.
func ServeChangeEvents(w http.ResponseWriter, r *http.Request, bus ChangeEventSubscriber, changeID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Proxies buffer by default; without this a frame can sit in a proxy and
	// defeat the point of the endpoint.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	source := bus.Subscribe(changeID)
	defer bus.Unsubscribe(changeID, source)

	heartbeat := time.NewTicker(SSEKeepAliveInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// A comment line carries no data field by definition, which is
			// what makes it a safe keep-alive.
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-source:
			if !ok {
				return
			}
			payload, err := protojson.Marshal(ev)
			if err != nil {
				// One unencodable event must not kill the stream.
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.GetEventType(), payload); err != nil {
				return
			}
			flusher.Flush()
			// The same rule WatchChange uses. Two pumps stopping on two
			// lists is how the two ends disagree about a finished change.
			if runstatus.IsTerminal(ev.GetNewStatus()) {
				return
			}
		}
	}
}
