// sse_test.go pins the SSE wire contract end to end.
//
// The point of the endpoint is that events arrive as they happen rather than on
// the console's 2-second poll, so the test drives the bus from another
// goroutine and asserts what a browser would actually read off the wire.

package grpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// fakeSSEBus is the ChangeEventSubscriber a test hands to ServeChangeEvents.
type fakeSSEBus struct {
	ch chan *pb.ChangeEvent
}

func (b *fakeSSEBus) Subscribe(string) chan *pb.ChangeEvent { return b.ch }

func (b *fakeSSEBus) Unsubscribe(_ string, ch chan *pb.ChangeEvent) {
	// Wake the stream loop so it exits without waiting for a real event.
	select {
	case ch <- nil:
	default:
	}
}

func TestServeChangeEvents_FramesAndStopsOnTerminal(t *testing.T) {
	bus := &fakeSSEBus{ch: make(chan *pb.ChangeEvent, 4)}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/changes/c1/events", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		ServeChangeEvents(rec, req, bus, "c1")
	}()
	time.Sleep(50 * time.Millisecond)

	bus.ch <- &pb.ChangeEvent{ChangeId: "c1", EventType: "status_changed", NewStatus: "running", Timestamp: 1}
	time.Sleep(50 * time.Millisecond)
	bus.ch <- &pb.ChangeEvent{ChangeId: "c1", EventType: "status_changed", NewStatus: "completed", Timestamp: 2}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not stop on a terminal status")
	}

	out := rec.Body.String()
	assert.Contains(t, out, "event: status_changed")
	assert.Contains(t, out, `"newStatus":"running"`, "the first frame carries the running event")
	assert.Contains(t, out, `"newStatus":"completed"`, "the terminal frame is delivered before the stop")
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no", rec.Header().Get("X-Accel-Buffering"))

	// Two events, two frames: the blank-terminated shape is the contract.
	assert.Equal(t, 2, strings.Count(out, "\n\n"), "each event is one frame: %s", out)
}

// TestServeChangeEvents_StopsWhenTheClientGoes: a stream must not outlive its
// request context — otherwise a browser that navigated away holds a
// subscription open forever.
func TestServeChangeEvents_StopsWhenTheClientGoes(t *testing.T) {
	bus := &fakeSSEBus{ch: make(chan *pb.ChangeEvent, 1)}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/changes/c2/events", nil)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		ServeChangeEvents(rec, req, bus, "c2")
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not stop when the client went away")
	}
	assert.Empty(t, rec.Body.String(), "no event was published, so nothing should have been written")
}

func TestHandleChangeEvents_RefusesSubjectWhoCannotSeeTheEnvironment(t *testing.T) {
	gw, _, store := startTestGatewayFull(t, ServeGatewayConfig{})
	ctx := context.Background()
	id := "run-sse-gate"
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: id, WorkflowName: "name: sse\n", Status: "running",
		Creator: "dave", IncidentID: "prod",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}))
	// An authorizer whose default is the read-gate posture for a caller
	// with no provable identity: the refusal we can actually observe is
	// a REGISTERED subject whose environment is not visible.
	gw.change.WithAuthorizer(newDevViewerAuthorizer(t))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/changes/"+id+"/events", nil)
	ctxWithSubject := ContextWithSubject(req.Context(), "dave")
	req = req.WithContext(ctxWithSubject)
	gw.handleChangeEvents(rec, req, id)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a subject who cannot see prod may not stream prod events, whatever the shared-token posture is")

	// An unknown change is a 404, not an open stream on a typoid id.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/changes/run-nope/events", nil)
	req = req.WithContext(ContextWithSubject(req.Context(), "dave"))
	gw.handleChangeEvents(rec, req, "run-nope")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestHandleChangeEvents_MethodIsGETOnly: the route sits in the read-only
// sub-path family; a POST must not be able to open a stream.
func TestHandleChangeEvents_MethodIsGETOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/changes/c1/events", nil)
	gw := NewGateway(ServeGatewayConfig{})
	gw.dispatchChange(rec, req, "POST", "/changes/c1/events")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
