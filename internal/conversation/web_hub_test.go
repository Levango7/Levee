package conversation

// web_hub_test.go exercises the WebHub WebSocket integration layer. It uses
// a channel-based mockConn to simulate WebSocket reads / writes without
// pulling in a real WebSocket library.

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mockConn ---------------------------------------------------------------
//
// mockConn implements WSConnection using unbuffered channels. Tests push
// inbound frames into readCh and consume outbound frames from writeCh.

type mockConn struct {
	readCh  chan []byte // frames the test wants the client to receive
	writeCh chan []byte // frames the client has written out
	closeCh chan struct{}

	closeOnce sync.Once
	closed    bool
	mu        sync.Mutex
}

func newMockConn() *mockConn {
	return &mockConn{
		readCh:  make(chan []byte, 8),
		writeCh: make(chan []byte, 8),
		closeCh: make(chan struct{}),
	}
}

func (m *mockConn) ReadMessage() (int, []byte, error) {
	select {
	case data := <-m.readCh:
		return WSMessageTypeText, data, nil
	case <-m.closeCh:
		return 0, nil, io.EOF
	}
}

func (m *mockConn) WriteMessage(_ int, data []byte) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return io.ErrClosedPipe
	}
	m.mu.Unlock()
	select {
	case m.writeCh <- data:
		return nil
	case <-m.closeCh:
		return io.ErrClosedPipe
	}
}

func (m *mockConn) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		close(m.closeCh)
	})
	return nil
}

// push queues an inbound frame for the client to read.
func (m *mockConn) push(t *testing.T, frame WSRequest) {
	t.Helper()
	data, err := json.Marshal(frame)
	require.NoError(t, err)
	m.readCh <- data
}

// pushRaw queues a raw byte frame (possibly invalid JSON).
func (m *mockConn) pushRaw(data []byte) { m.readCh <- data }

// recv reads the next outbound frame, failing the test on timeout.
func (m *mockConn) recv(t *testing.T) WSResponse {
	t.Helper()
	select {
	case data := <-m.writeCh:
		var resp WSResponse
		require.NoError(t, json.Unmarshal(data, &resp))
		return resp
	case <-time.After(2 * time.Second):
		t.Fatal("mockConn: timed out waiting for write")
		return WSResponse{}
	}
}

// recvRaw returns the next outbound frame as raw bytes.
func (m *mockConn) recvRaw(t *testing.T) []byte {
	t.Helper()
	select {
	case data := <-m.writeCh:
		return data
	case <-time.After(2 * time.Second):
		t.Fatal("mockConn: timed out waiting for write")
		return nil
	}
}

// --- test helpers -----------------------------------------------------------

// newTestHub builds a WebHub backed by a real ConversationEngine with nil
// recommend / diagnose engines. The engine accepts session management calls
// and returns ErrNilRecommend from /recommend, which is fine for the hub
// tests.
func newTestHub(t *testing.T) *WebHub {
	t.Helper()
	engine := NewConversationEngine(ConversationEngineConfig{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	hub, err := NewWebHub(WebHubConfig{
		Engine: engine,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	return hub
}

// startHub runs the hub in a goroutine and returns a stop function.
func startHub(t *testing.T, hub *WebHub) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		hub.Run()
		close(done)
	}()
	return func() {
		hub.Stop()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("hub did not stop in time")
		}
	}
}

// dialClient starts HandleClient in a goroutine and returns the mockConn
// plus a wait function that blocks until the client pump exits.
func dialClient(t *testing.T, hub *WebHub, userID string) (*mockConn, func()) {
	t.Helper()
	before := hub.ClientCount()
	conn := newMockConn()
	done := make(chan struct{})
	go func() {
		hub.HandleClient(conn, userID)
		close(done)
	}()
	// Wait until the client has actually been registered (count increases).
	require.Eventually(t, func() bool {
		return hub.ClientCount() >= before+1
	}, 2*time.Second, 10*time.Millisecond, "client did not register")
	return conn, func() {
		conn.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("client did not exit in time")
		}
	}
}

// --- NewWebHub --------------------------------------------------------------

func TestNewWebHub_NilEngine(t *testing.T) {
	hub, err := NewWebHub(WebHubConfig{Engine: nil})
	require.ErrorIs(t, err, ErrNilEngine)
	require.Nil(t, hub)
}

func TestNewWebHub_Valid(t *testing.T) {
	engine := NewConversationEngine(ConversationEngineConfig{})
	hub, err := NewWebHub(WebHubConfig{Engine: engine})
	require.NoError(t, err)
	require.NotNil(t, hub)
	require.NotNil(t, hub.clients)
	require.NotNil(t, hub.register)
	require.NotNil(t, hub.unregister)
	require.NotNil(t, hub.broadcast)
	require.NotNil(t, hub.done)
}

func TestNewWebHub_DefaultLogger(t *testing.T) {
	engine := NewConversationEngine(ConversationEngineConfig{})
	hub, err := NewWebHub(WebHubConfig{Engine: engine})
	require.NoError(t, err)
	require.NotNil(t, hub.log)
}

// --- handleRequest dispatch -------------------------------------------------

func TestWebHandleRequest_UnknownType(t *testing.T) {
	hub := newTestHub(t)
	defer hub.Stop()
	client := &WSClient{hub: hub, userID: "u1", send: make(chan []byte, 1)}
	resp := hub.handleRequest(client, WSRequest{Type: "bogus"})
	require.Equal(t, RespError, resp.Type)
	require.Contains(t, resp.Error, "unknown request type")
	require.Contains(t, resp.Error, "bogus")
}

func TestWebHandleRequest_TableDriven(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	// Create a session up-front for message / close tests.
	sess, err := hub.engine.NewSession("alice")
	require.NoError(t, err)

	client := &WSClient{hub: hub, userID: "alice", send: make(chan []byte, 4)}

	cases := []struct {
		name string
		req  WSRequest
		want func(t *testing.T, resp WSResponse)
	}{
		{
			name: "create_session with explicit user_id",
			req:  WSRequest{Type: ReqCreateSession, UserID: "alice"},
			want: func(t *testing.T, resp WSResponse) {
				assert.Equal(t, RespSessionCreated, resp.Type)
				assert.NotEmpty(t, resp.SessionID)
				assert.Equal(t, "idle", resp.State)
			},
		},
		{
			name: "create_session falls back to client user_id",
			req:  WSRequest{Type: ReqCreateSession},
			want: func(t *testing.T, resp WSResponse) {
				assert.Equal(t, RespSessionCreated, resp.Type)
				assert.NotEmpty(t, resp.SessionID)
			},
		},
		{
			name: "message normal",
			req:  WSRequest{Type: ReqMessage, SessionID: sess.ID, Text: "hello"},
			want: func(t *testing.T, resp WSResponse) {
				assert.Equal(t, RespReply, resp.Type)
				assert.Equal(t, sess.ID, resp.SessionID)
				assert.NotEmpty(t, resp.Text)
			},
		},
		{
			name: "message missing session_id",
			req:  WSRequest{Type: ReqMessage, Text: "hi"},
			want: func(t *testing.T, resp WSResponse) {
				assert.Equal(t, RespError, resp.Type)
				assert.Contains(t, resp.Error, "session_id")
			},
		},
		{
			name: "message empty text",
			req:  WSRequest{Type: ReqMessage, SessionID: sess.ID, Text: ""},
			want: func(t *testing.T, resp WSResponse) {
				assert.Equal(t, RespError, resp.Type)
				assert.Contains(t, resp.Error, "text")
			},
		},
		{
			name: "message session not found",
			req:  WSRequest{Type: ReqMessage, SessionID: "no-such-session", Text: "hi"},
			want: func(t *testing.T, resp WSResponse) {
				assert.Equal(t, RespError, resp.Type)
				assert.Contains(t, resp.Error, "not found")
			},
		},
		{
			name: "list_sessions returns alice sessions",
			req:  WSRequest{Type: ReqListSessions},
			want: func(t *testing.T, resp WSResponse) {
				assert.Equal(t, RespSessionList, resp.Type)
				assert.NotEmpty(t, resp.Sessions)
				for _, s := range resp.Sessions {
					assert.Equal(t, "alice", s.UserID)
				}
			},
		},
		{
			name: "close_session normal",
			req:  WSRequest{Type: ReqCloseSession, SessionID: sess.ID},
			want: func(t *testing.T, resp WSResponse) {
				assert.Equal(t, RespReply, resp.Type)
				assert.Equal(t, sess.ID, resp.SessionID)
				assert.Equal(t, "failed", resp.State)
			},
		},
		{
			name: "close_session missing id",
			req:  WSRequest{Type: ReqCloseSession},
			want: func(t *testing.T, resp WSResponse) {
				assert.Equal(t, RespError, resp.Type)
				assert.Contains(t, resp.Error, "session_id")
			},
		},
		{
			name: "close_session not found",
			req:  WSRequest{Type: ReqCloseSession, SessionID: "ghost"},
			want: func(t *testing.T, resp WSResponse) {
				assert.Equal(t, RespError, resp.Type)
				assert.Contains(t, resp.Error, "not found")
			},
		},
		{
			name: "create_session empty user_id with empty client user_id",
			req:  WSRequest{Type: ReqCreateSession, UserID: ""},
			want: func(t *testing.T, resp WSResponse) {
				// client.userID is "alice" so this still succeeds.
				assert.Equal(t, RespSessionCreated, resp.Type)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := hub.handleRequest(client, tc.req)
			tc.want(t, resp)
		})
	}
}

func TestWebHandleCreateSession_EmptyUserAndClient(t *testing.T) {
	hub := newTestHub(t)
	defer hub.Stop()
	client := &WSClient{hub: hub, userID: "", send: make(chan []byte, 1)}
	resp := hub.handleCreateSession(client, WSRequest{UserID: ""})
	require.Equal(t, RespError, resp.Type)
	require.Contains(t, resp.Error, "user_id")
}

func TestWebHandleCreateSession_EngineError(t *testing.T) {
	hub := newTestHub(t)
	defer hub.Stop()
	client := &WSClient{hub: hub, userID: "", send: make(chan []byte, 1)}
	// Empty user id triggers ErrEmptyMessage from the engine.
	resp := hub.handleCreateSession(client, WSRequest{UserID: "   "})
	// The engine trims and rejects empty user ids.
	require.Equal(t, RespError, resp.Type)
}

// --- ClientCount ------------------------------------------------------------

func TestWebClientCount_Empty(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()
	require.Zero(t, hub.ClientCount())
}

func TestWebClientCount_AfterConnect(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	conn, wait := dialClient(t, hub, "alice")
	defer wait()
	defer conn.Close()

	require.Eventually(t, func() bool {
		return hub.ClientCount() == 1
	}, 2*time.Second, 10*time.Millisecond)
}

func TestWebClientCount_Multiple(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	var conns []*mockConn
	var waits []func()
	for i := 0; i < 3; i++ {
		c, w := dialClient(t, hub, "alice")
		conns = append(conns, c)
		waits = append(waits, w)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
		for _, w := range waits {
			w()
		}
	}()

	require.Eventually(t, func() bool {
		return hub.ClientCount() == 3
	}, 2*time.Second, 10*time.Millisecond)
}

func TestWebClientCount_StoppedHub(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	stop()
	require.Zero(t, hub.ClientCount())
}

// --- End-to-end pump tests --------------------------------------------------

func TestWebHandleClient_CreateSessionRoundTrip(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	conn, wait := dialClient(t, hub, "alice")
	defer wait()

	conn.push(t, WSRequest{Type: ReqCreateSession, UserID: "alice"})
	resp := conn.recv(t)
	require.Equal(t, RespSessionCreated, resp.Type)
	require.NotEmpty(t, resp.SessionID)

	// Use the new session for a message round-trip.
	conn.push(t, WSRequest{Type: ReqMessage, SessionID: resp.SessionID, Text: "hello"})
	resp2 := conn.recv(t)
	require.Equal(t, RespReply, resp2.Type)
	require.Equal(t, resp.SessionID, resp2.SessionID)
	require.NotEmpty(t, resp2.Text)
}

func TestWebHandleClient_ListSessions(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	// Pre-create a session for the user.
	_, err := hub.engine.NewSession("alice")
	require.NoError(t, err)

	conn, wait := dialClient(t, hub, "alice")
	defer wait()

	conn.push(t, WSRequest{Type: ReqListSessions})
	resp := conn.recv(t)
	require.Equal(t, RespSessionList, resp.Type)
	require.NotEmpty(t, resp.Sessions)
	require.Equal(t, "alice", resp.Sessions[0].UserID)
}

func TestWebHandleClient_CloseSession(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	sess, err := hub.engine.NewSession("alice")
	require.NoError(t, err)

	conn, wait := dialClient(t, hub, "alice")
	defer wait()

	conn.push(t, WSRequest{Type: ReqCloseSession, SessionID: sess.ID})
	resp := conn.recv(t)
	require.Equal(t, RespReply, resp.Type)
	require.Equal(t, sess.ID, resp.SessionID)

	// Subsequent list should not include the closed session.
	conn.push(t, WSRequest{Type: ReqListSessions})
	resp2 := conn.recv(t)
	require.Equal(t, RespSessionList, resp2.Type)
	for _, s := range resp2.Sessions {
		require.NotEqual(t, sess.ID, s.ID)
	}
}

func TestWebHandleClient_UnknownType(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	conn, wait := dialClient(t, hub, "alice")
	defer wait()

	conn.push(t, WSRequest{Type: "frobnicate"})
	resp := conn.recv(t)
	require.Equal(t, RespError, resp.Type)
	require.Contains(t, resp.Error, "unknown request type")
}

func TestWebHandleClient_InvalidJSON(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	conn, wait := dialClient(t, hub, "alice")
	defer wait()

	conn.pushRaw([]byte("{not json"))
	resp := conn.recv(t)
	require.Equal(t, RespError, resp.Type)
	require.Contains(t, resp.Error, "invalid json")
}

func TestWebHandleClient_ConnectionClose(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	conn := newMockConn()
	done := make(chan struct{})
	go func() {
		hub.HandleClient(conn, "alice")
		close(done)
	}()

	require.Eventually(t, func() bool {
		return hub.ClientCount() == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Close the connection; the read pump should exit and unregister.
	conn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleClient did not return after close")
	}

	require.Eventually(t, func() bool {
		return hub.ClientCount() == 0
	}, 2*time.Second, 10*time.Millisecond)
}

// --- Stop -------------------------------------------------------------------

func TestWebStop_Idempotent(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	// Calling Stop twice must not panic.
	hub.Stop()
	hub.Stop()
	// Wait for Run to exit.
	time.Sleep(50 * time.Millisecond)
	_ = stop
}

func TestWebStop_DrainsClients(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)

	conn, wait := dialClient(t, hub, "alice")
	// Stop the hub first, then close the client connection so the pumps
	// exit. Stop does not actively close client connections; clients are
	// expected to detect the shutdown via their own keep-alive / reconnect
	// logic.
	stop()
	conn.Close()
	wait()
}

// --- Broadcast --------------------------------------------------------------

func TestWebBroadcastToUser_Targeted(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	connAlice, waitAlice := dialClient(t, hub, "alice")
	defer waitAlice()
	connBob, waitBob := dialClient(t, hub, "bob")
	defer waitBob()

	msg := encodeResponse(WSResponse{Type: RespStateChange, Text: "ping"})
	hub.broadcastToUser("alice", msg)

	// Alice should receive it.
	resp := connAlice.recv(t)
	require.Equal(t, RespStateChange, resp.Type)
	require.Equal(t, "ping", resp.Text)

	// Bob should not.
	select {
	case data := <-connBob.writeCh:
		t.Fatalf("bob received targeted message: %s", data)
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestWebBroadcastToUser_NoClients(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()
	// Should not block or panic.
	hub.broadcastToUser("nobody", []byte("hi"))
}

func TestWebBroadcastAll_Everyone(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	connAlice, waitAlice := dialClient(t, hub, "alice")
	defer waitAlice()
	connBob, waitBob := dialClient(t, hub, "bob")
	defer waitBob()

	msg := encodeResponse(WSResponse{Type: RespStateChange, Text: "broadcast"})
	hub.broadcastAll(msg)

	respA := connAlice.recv(t)
	require.Equal(t, "broadcast", respA.Text)
	respB := connBob.recv(t)
	require.Equal(t, "broadcast", respB.Text)
}

func TestWebBroadcast_AfterStop(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	stop()
	// Should not block or panic after stop.
	hub.broadcastAll([]byte("late"))
	hub.broadcastToUser("x", []byte("late"))
}

// --- JSON round-trip --------------------------------------------------------

func TestWebWSRequest_JSONUnmarshal(t *testing.T) {
	raw := `{"type":"message","session_id":"s1","user_id":"u1","text":"hi"}`
	var req WSRequest
	require.NoError(t, json.Unmarshal([]byte(raw), &req))
	require.Equal(t, ReqMessage, req.Type)
	require.Equal(t, "s1", req.SessionID)
	require.Equal(t, "u1", req.UserID)
	require.Equal(t, "hi", req.Text)
}

func TestWebWSResponse_JSONMarshal(t *testing.T) {
	resp := WSResponse{
		Type:      RespReply,
		SessionID: "s1",
		Text:      "hello",
		State:     "idle",
	}
	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var back WSResponse
	require.NoError(t, json.Unmarshal(data, &back))
	require.Equal(t, resp, back)
}

func TestWebWSResponse_OmitEmpty(t *testing.T) {
	resp := WSResponse{Type: RespError, Error: "boom"}
	data, err := json.Marshal(resp)
	require.NoError(t, err)
	// Text should be omitted.
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))
	_, hasText := m["text"]
	require.False(t, hasText)
	_, hasSessions := m["sessions"]
	require.False(t, hasSessions)
}

func TestWebSessionInfo_JSON(t *testing.T) {
	si := SessionInfo{
		ID:        "s1",
		UserID:    "u1",
		State:     "idle",
		UpdatedAt: "2024-01-01T00:00:00Z",
	}
	data, err := json.Marshal(si)
	require.NoError(t, err)
	var back SessionInfo
	require.NoError(t, json.Unmarshal(data, &back))
	require.Equal(t, si, back)
}

// --- readPump / writePump direct -------------------------------------------

func TestWebReadPump_ExitOnReadError(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	conn := newMockConn()
	client := &WSClient{
		hub:    hub,
		conn:   conn,
		userID: "alice",
		send:   make(chan []byte, DefaultSendBufferSize),
	}
	hub.register <- client
	require.Eventually(t, func() bool {
		return hub.ClientCount() == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Start writePump to drain send.
	go client.writePump()
	// Start readPump; it will exit when ReadMessage returns an error.
	go client.readPump()

	// Close the connection to make ReadMessage return io.EOF.
	conn.Close()

	require.Eventually(t, func() bool {
		return hub.ClientCount() == 0
	}, 2*time.Second, 10*time.Millisecond, "client not unregistered after read error")
}

func TestWebWritePump_ExitOnSendClose(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	conn := newMockConn()
	client := &WSClient{
		hub:    hub,
		conn:   conn,
		userID: "alice",
		send:   make(chan []byte, DefaultSendBufferSize),
	}

	done := make(chan struct{})
	go func() {
		client.writePump()
		close(done)
	}()

	// Closing the send channel should terminate writePump.
	close(client.send)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writePump did not exit after send close")
	}
}

func TestWebWritePump_WriteError(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	conn := newMockConn()
	client := &WSClient{
		hub:    hub,
		conn:   conn,
		userID: "alice",
		send:   make(chan []byte, DefaultSendBufferSize),
	}

	done := make(chan struct{})
	go func() {
		client.writePump()
		close(done)
	}()

	// Close the connection so the next write fails.
	conn.Close()
	client.send <- []byte("will fail")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writePump did not exit after write error")
	}
}

// --- Run twice --------------------------------------------------------------

func TestWebRun_TwiceIsNoop(t *testing.T) {
	hub := newTestHub(t)
	done1 := make(chan struct{})
	go func() {
		hub.Run()
		close(done1)
	}()

	// Wait until the first Run has actually started.
	require.Eventually(t, func() bool {
		hub.runMu.Lock()
		defer hub.runMu.Unlock()
		return hub.running
	}, 2*time.Second, 5*time.Millisecond, "first Run did not start")

	// Second Run should return immediately.
	done2 := make(chan struct{})
	go func() {
		hub.Run()
		close(done2)
	}()
	select {
	case <-done2:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second Run did not return immediately")
	}

	hub.Stop()
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("first Run did not exit after Stop")
	}
}

// --- encodeResponse ---------------------------------------------------------

func TestWebEncodeResponse(t *testing.T) {
	resp := WSResponse{Type: RespReply, Text: "hi"}
	data := encodeResponse(resp)
	require.NotNil(t, data)
	var back WSResponse
	require.NoError(t, json.Unmarshal(data, &back))
	require.Equal(t, "hi", back.Text)
}

// --- Sentinel errors --------------------------------------------------------

func TestWebSentinelErrors(t *testing.T) {
	require.NotSame(t, ErrNilEngine, ErrHubClosed)
	require.NotSame(t, ErrHubClosed, ErrInvalidRequest)
	require.NotSame(t, ErrInvalidRequest, ErrConnClosed)

	require.Equal(t, "conversation/web: nil conversation engine", ErrNilEngine.Error())
	require.Equal(t, "conversation/web: hub closed", ErrHubClosed.Error())
	require.Equal(t, "conversation/web: invalid request", ErrInvalidRequest.Error())
	require.Equal(t, "conversation/web: connection closed", ErrConnClosed.Error())
}

// Ensure sentinel errors are usable with errors.Is.
func TestWebSentinelErrors_Is(t *testing.T) {
	wrapped := errors.New("conversation/web: wrap: " + ErrNilEngine.Error())
	require.NotErrorIs(t, wrapped, ErrNilEngine)

	proper := &wrapErr{inner: ErrHubClosed}
	require.ErrorIs(t, proper, ErrHubClosed)
}

type wrapErr struct{ inner error }

func (w *wrapErr) Error() string { return w.inner.Error() }
func (w *wrapErr) Unwrap() error { return w.inner }

// --- Extra coverage tests ---------------------------------------------------

// TestWebClientCount_RunNotStarted exercises the branch where ClientCount is
// called before Run has started.
func TestWebClientCount_RunNotStarted(t *testing.T) {
	hub := newTestHub(t)
	defer hub.Stop()
	require.Zero(t, hub.ClientCount())
}

// TestWebBroadcastToUser_AfterStop ensures broadcastToUser does not block
// when the hub is stopped.
func TestWebBroadcastToUser_AfterStop(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	stop()
	hub.broadcastToUser("alice", []byte("late"))
}

// TestWebBroadcastAll_AfterStop2 ensures broadcastAll does not block when
// the hub is stopped.
func TestWebBroadcastAll_AfterStop2(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	stop()
	hub.broadcastAll([]byte("late"))
}

// TestWebHandleMessage_WithUserID tests that an explicit user_id in the
// request is used for authorization.
func TestWebHandleMessage_WithUserID(t *testing.T) {
	hub := newTestHub(t)
	stop := startHub(t, hub)
	defer stop()

	sess, err := hub.engine.NewSession("alice")
	require.NoError(t, err)

	client := &WSClient{hub: hub, userID: "bob", send: make(chan []byte, 1)}
	resp := hub.handleMessage(client, WSRequest{
		Type:      ReqMessage,
		SessionID: sess.ID,
		UserID:    "alice",
		Text:      "hi",
	})
	require.Equal(t, RespReply, resp.Type)

	// Wrong user should get an error.
	resp2 := hub.handleMessage(client, WSRequest{
		Type:      ReqMessage,
		SessionID: sess.ID,
		UserID:    "mallory",
		Text:      "hi",
	})
	require.Equal(t, RespError, resp2.Type)
}

// TestWebHandleListSessions_EmptyUser tests list_sessions with no user id
// and no client user id.
func TestWebHandleListSessions_EmptyUser(t *testing.T) {
	hub := newTestHub(t)
	defer hub.Stop()
	client := &WSClient{hub: hub, userID: "", send: make(chan []byte, 1)}
	resp := hub.handleListSessions(client, WSRequest{UserID: ""})
	require.Equal(t, RespSessionList, resp.Type)
	require.Empty(t, resp.Sessions)
}

// TestWebHandleCreateSession_WithClientUserID tests that create_session
// falls back to the client's user_id.
func TestWebHandleCreateSession_WithClientUserID(t *testing.T) {
	hub := newTestHub(t)
	defer hub.Stop()
	client := &WSClient{hub: hub, userID: "alice", send: make(chan []byte, 1)}
	resp := hub.handleCreateSession(client, WSRequest{UserID: ""})
	require.Equal(t, RespSessionCreated, resp.Type)
}
