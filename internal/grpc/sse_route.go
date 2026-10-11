// sse_route.go — wiring the SSE endpoint into the change sub-path router.
//
// Two files, one route: sse.go holds the stream (framing, keep-alive,
// terminal rule), this holds the HTTP concerns — authorization, the 404 when
// no change service is registered, and the sub-path switch entry.

package grpc

import (
	"context"
	"net/http"

	"google.golang.org/grpc/metadata"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// handleChangeEvents serves GET /changes/<id>/events.
//
// Authorization is the change read gate (the same one /logs and /trace use),
// evaluated once: a stream is long-lived, and re-deciding per event would cost
// and could flip mid-stream.
func (gw *Gateway) handleChangeEvents(w http.ResponseWriter, r *http.Request, id string) {
	svc := gw.change
	if svc == nil {
		writeJSONError(w, http.StatusNotFound, "change service not configured")
		return
	}
	ctx := metadata.NewOutgoingContext(r.Context(), extractAuth(r))
	run, err := svc.RunForGate(ctx, id)
	// A missing change is not a policy question: answer 404 before the
	// gate, the way the trace route does. Judging the gate on an empty
	// environment would turn a typo into a permission refusal, which
	// sends the operator looking at the wrong problem.
	if run == nil {
		writeJSONError(w, http.StatusNotFound, "change not found: "+id)
		return
	}
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	if aerr := svc.authorizeRead(ctx, run, "events"); aerr != nil {
		writeGRPCError(w, aerr)
		return
	}
	ServeChangeEvents(w, r, svc, id)
}

// RunForGate loads the change the SSE stream is about. The stream needs the run
// only to run the same read gate the other change routes run before they
// answer.
func (s *ChangeService) RunForGate(ctx context.Context, changeID string) (*state.Run, error) {
	return s.store.GetRun(ctx, changeID)
}

// Subscribe / Unsubscribe implement ChangeEventSubscriber over the service's
// own event bus: the SSE client and WatchChange read the same fan-out
// (publishEvent), so the two transports cannot drift into two event sets.
func (s *ChangeService) Subscribe(changeID string) chan *pb.ChangeEvent {
	return s.getEventBus().subscribe(changeID)
}

func (s *ChangeService) Unsubscribe(changeID string, ch chan *pb.ChangeEvent) {
	s.getEventBus().unsubscribe(changeID, ch)
}
