package conversation

// web_hub.go implements WebHub, the WebSocket integration layer that exposes
// the ConversationEngine to browser-based operators. Browsers connect over
// WebSocket, send WSRequest JSON frames and receive WSResponse JSON frames.
//
// Concurrency model:
//   - The clients map is only mutated inside Hub.Run, which serialises
//     register / unregister / broadcast events through channels. No goroutine
//     ever touches the map directly, so no mutex is required for the map.
//   - Each WSClient runs two goroutines (readPump / writePump) that own the
//     underlying WSConnection. The send channel is the only safe way for the
//     Hub to push data to a client.
//   - Stop closes the done channel; Run drains pending events and exits.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrNilEngine is returned when WebHubConfig.Engine is nil.
	ErrNilEngine = errors.New("conversation/web: nil conversation engine")
	// ErrHubClosed is returned when an operation is attempted on a stopped hub.
	ErrHubClosed = errors.New("conversation/web: hub closed")
	// ErrInvalidRequest is returned when a WSRequest cannot be processed.
	ErrInvalidRequest = errors.New("conversation/web: invalid request")
	// ErrConnClosed is returned when the underlying WebSocket connection is
	// closed while a pump is still running.
	ErrConnClosed = errors.New("conversation/web: connection closed")
)

// --- Defaults ---------------------------------------------------------------

const (
	// DefaultSendBufferSize is the capacity of each client's send channel.
	DefaultSendBufferSize = 64
	// WSMessageTypeText is the WebSocket text message opcode. We use a
	// constant here so the package does not depend on a specific WebSocket
	// library; the WSConnection interface is library-agnostic.
	WSMessageTypeText = 1
)

// --- Wire protocol ----------------------------------------------------------

// WSRequest is a client->server JSON frame.
type WSRequest struct {
	Type      string `json:"type"`       // "message" | "create_session" | "list_sessions" | "close_session"
	SessionID string `json:"session_id"` // required for "message" / "close_session"
	UserID    string `json:"user_id"`    // required for "create_session"; optional otherwise
	Text      string `json:"text"`       // required for "message"
}

// WSResponse is a server->client JSON frame.
type WSResponse struct {
	Type      string        `json:"type"`               // "reply" | "session_created" | "session_list" | "error" | "state_change"
	SessionID string        `json:"session_id"`         //
	Text      string        `json:"text,omitempty"`     // reply / error
	Card      *chatops.Card `json:"card,omitempty"`     // reply
	Action    *Action       `json:"action,omitempty"`   // reply
	Sessions  []SessionInfo `json:"sessions,omitempty"` // session_list
	Error     string        `json:"error,omitempty"`    // error
	State     string        `json:"state,omitempty"`    // state_change
}

// SessionInfo is a compact, JSON-friendly projection of a Session used in
// session_list responses.
type SessionInfo struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	State     string `json:"state"`
	AlertID   string `json:"alert_id,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// --- Request type constants -------------------------------------------------

const (
	ReqMessage       = "message"
	ReqCreateSession = "create_session"
	ReqListSessions  = "list_sessions"
	ReqCloseSession  = "close_session"
)

const (
	RespReply          = "reply"
	RespSessionCreated = "session_created"
	RespSessionList    = "session_list"
	RespError          = "error"
	RespStateChange    = "state_change"
)

// --- WSConnection -----------------------------------------------------------

// WSConnection is the minimal WebSocket connection interface. Real
// implementations wrap gorilla/websocket or nhooyr/websocket; tests use a
// channel-based mock.
type WSConnection interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	Close() error
}

// --- WSBroadcast ------------------------------------------------------------

// WSBroadcast is an internal message queued onto the Hub's broadcast channel.
// When UserID is non-empty the payload is delivered only to clients whose
// userID matches; otherwise it is delivered to every connected client.
type WSBroadcast struct {
	message []byte
	userID  string
}

// --- WSClient ---------------------------------------------------------------

// WSClient represents a single active WebSocket connection. It is owned by
// the Hub; callers obtain one via Hub.HandleClient and should not construct
// it directly.
type WSClient struct {
	hub    *WebHub
	conn   WSConnection
	userID string
	send   chan []byte

	closeOnce sync.Once
}

// closeSend closes the client's send channel exactly once. It is safe to
// call from both the readPump (when the connection drops) and the Run loop
// (when the client is unregistered).
func (c *WSClient) closeSend() {
	c.closeOnce.Do(func() {
		close(c.send)
	})
}

// readPump reads messages from the underlying connection, dispatches each
// WSRequest through Hub.handleRequest and writes the resulting WSResponse
// back to the client. When the connection returns an error (clean close or
// network failure) the pump unregisters the client and exits.
func (c *WSClient) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.closeSend()
		_ = c.conn.Close()
	}()

	for {
		_, payload, err := c.conn.ReadMessage()
		if err != nil {
			c.hub.log.Debug("conversation/web: read pump exit",
				"user_id", c.userID, "err", err)
			return
		}

		var req WSRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			c.sendError(fmt.Sprintf("invalid json: %v", err))
			continue
		}

		resp := c.hub.handleRequest(c, req)
		data, err := json.Marshal(resp)
		if err != nil {
			c.hub.log.Error("conversation/web: marshal response",
				"err", err, "type", resp.Type)
			continue
		}
		select {
		case c.send <- data:
		default:
			// Send buffer full; drop the client to avoid blocking the pump.
			c.hub.log.Warn("conversation/web: send buffer full, dropping client",
				"user_id", c.userID)
			return
		}
	}
}

// writePump drains the client's send channel and writes each frame to the
// underlying connection. It exits when the send channel is closed, which
// happens when the Hub unregisters the client.
func (c *WSClient) writePump() {
	for data := range c.send {
		if err := c.conn.WriteMessage(WSMessageTypeText, data); err != nil {
			c.hub.log.Debug("conversation/web: write pump exit",
				"user_id", c.userID, "err", err)
			return
		}
	}
}

// sendError encodes an error response and pushes it onto the send channel.
// It is non-blocking: when the send buffer is full the error is dropped.
func (c *WSClient) sendError(msg string) {
	resp := WSResponse{Type: RespError, Error: msg}
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	select {
	case c.send <- data:
	default:
	}
}

// --- WebHubConfig -----------------------------------------------------------

// WebHubConfig configures a WebHub. Engine is required; Logger falls back to
// the package singleton from internal/log when nil.
type WebHubConfig struct {
	Engine *ConversationEngine
	Logger *slog.Logger
}

// --- WebHub -----------------------------------------------------------------

// WebHub is the WebSocket integration layer for the ConversationEngine. It
// owns the set of active clients and routes JSON frames between browsers and
// the engine.
//
// The Hub is safe for concurrent use: clients are registered / unregistered
// through channels and the clients map is only mutated inside Run.
type WebHub struct {
	engine *ConversationEngine

	clients    map[*WSClient]bool
	register   chan *WSClient
	unregister chan *WSClient
	broadcast  chan WSBroadcast
	countReq   chan chan int

	log  *slog.Logger
	done chan struct{}

	stopOnce sync.Once
	runMu    sync.Mutex
	running  bool
}

// NewWebHub constructs a WebHub from the given config. It returns
// ErrNilEngine when cfg.Engine is nil.
func NewWebHub(cfg WebHubConfig) (*WebHub, error) {
	if cfg.Engine == nil {
		return nil, ErrNilEngine
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.With("component", "conversation_web_hub")
	}
	return &WebHub{
		engine:     cfg.Engine,
		clients:    make(map[*WSClient]bool),
		register:   make(chan *WSClient, 16),
		unregister: make(chan *WSClient, 16),
		broadcast:  make(chan WSBroadcast, 64),
		countReq:   make(chan chan int, 4),
		log:        lg,
		done:       make(chan struct{}),
	}, nil
}

// Run starts the Hub event loop. It blocks until Stop is called and drains
// any pending register / unregister / broadcast events before returning.
//
// Run is intended to run in its own goroutine. Calling Run twice on the same
// Hub is a no-op: the second call returns immediately.
func (h *WebHub) Run() {
	h.runMu.Lock()
	if h.running {
		h.runMu.Unlock()
		return
	}
	h.running = true
	h.runMu.Unlock()

	defer func() {
		h.runMu.Lock()
		h.running = false
		h.runMu.Unlock()
	}()

	for {
		select {
		case <-h.done:
			h.drain()
			return
		case client := <-h.register:
			h.clients[client] = true
			h.log.Debug("conversation/web: client registered",
				"user_id", client.userID, "total", len(h.clients))
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				client.closeSend()
				h.log.Debug("conversation/web: client unregistered",
					"user_id", client.userID, "total", len(h.clients))
			}
		case msg := <-h.broadcast:
			h.deliverBroadcast(msg)
		case replyCh := <-h.countReq:
			replyCh <- len(h.clients)
		}
	}
}

// deliverBroadcast fans a single broadcast out to the relevant clients.
func (h *WebHub) deliverBroadcast(msg WSBroadcast) {
	for client := range h.clients {
		if msg.userID != "" && client.userID != msg.userID {
			continue
		}
		select {
		case client.send <- msg.message:
		default:
		}
	}
}

// drain processes any events that were queued before Stop was called. It is
// best-effort: once done is closed we no longer accept new events.
func (h *WebHub) drain() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				client.closeSend()
			}
		case <-h.broadcast:
			// drop pending broadcasts
		case replyCh := <-h.countReq:
			replyCh <- len(h.clients)
		default:
			return
		}
	}
}

// Stop signals the Hub to shut down. It is idempotent. After Stop returns
// the Hub unregisters all clients on the next Run iteration; callers that
// want a synchronous shutdown should run Run in a goroutine and wait for it
// to return.
func (h *WebHub) Stop() {
	h.stopOnce.Do(func() {
		close(h.done)
	})
}

// HandleClient takes ownership of a freshly accepted WebSocket connection.
// It creates a WSClient, registers it with the Hub and starts the read /
// write pumps. The call blocks until the connection closes (either because
// the client disconnected or because the Hub is shutting down).
//
// HandleClient is intended to be called from an HTTP handler, typically in
// its own goroutine per connection.
func (h *WebHub) HandleClient(conn WSConnection, userID string) {
	client := &WSClient{
		hub:    h,
		conn:   conn,
		userID: userID,
		send:   make(chan []byte, DefaultSendBufferSize),
	}

	h.register <- client

	done := make(chan struct{})
	go func() {
		client.writePump()
		close(done)
	}()
	client.readPump()
	<-done
}

// ClientCount returns the number of currently active clients. The value is
// a point-in-time snapshot and may change immediately after the call
// returns. When the Hub is stopped the count is 0.
func (h *WebHub) ClientCount() int {
	select {
	case <-h.done:
		return 0
	default:
	}
	// If Run is not currently active there is nobody to answer the count
	// query, so we short-circuit and report 0.
	h.runMu.Lock()
	running := h.running
	h.runMu.Unlock()
	if !running {
		return 0
	}
	replyCh := make(chan int, 1)
	select {
	case h.countReq <- replyCh:
		select {
		case n := <-replyCh:
			return n
		case <-h.done:
			return 0
		}
	default:
		return 0
	}
}

// --- Request dispatch -------------------------------------------------------

// handleRequest routes a parsed WSRequest to the appropriate engine method
// and returns a WSResponse ready to be JSON-encoded and sent to the client.
// Unknown request types yield an error response rather than a sentinel
// error so the front-end can display a helpful message.
func (h *WebHub) handleRequest(client *WSClient, req WSRequest) WSResponse {
	switch req.Type {
	case ReqCreateSession:
		return h.handleCreateSession(client, req)
	case ReqMessage:
		return h.handleMessage(client, req)
	case ReqListSessions:
		return h.handleListSessions(client, req)
	case ReqCloseSession:
		return h.handleCloseSession(client, req)
	default:
		return WSResponse{
			Type:  RespError,
			Error: fmt.Sprintf("unknown request type: %q", req.Type),
		}
	}
}

// handleCreateSession creates a new conversation session for the requesting
// user. When req.UserID is empty the client's bound userID is used.
func (h *WebHub) handleCreateSession(client *WSClient, req WSRequest) WSResponse {
	uid := req.UserID
	if uid == "" {
		uid = client.userID
	}
	if uid == "" {
		return WSResponse{Type: RespError, Error: "user_id is required"}
	}
	sess, err := h.engine.NewSession(uid)
	if err != nil {
		return WSResponse{Type: RespError, Error: err.Error()}
	}
	return WSResponse{
		Type:      RespSessionCreated,
		SessionID: sess.ID,
		State:     sess.GetState().String(),
	}
}

// handleMessage dispatches a chat message to the engine and returns the
// resulting reply. Both SessionID and Text are required.
func (h *WebHub) handleMessage(client *WSClient, req WSRequest) WSResponse {
	if req.SessionID == "" {
		return WSResponse{Type: RespError, Error: "session_id is required"}
	}
	if req.Text == "" {
		return WSResponse{Type: RespError, Error: "text is required"}
	}
	uid := req.UserID
	if uid == "" {
		uid = client.userID
	}
	reply, err := h.engine.HandleMessage(context.Background(), req.SessionID, uid, req.Text)
	if err != nil {
		return WSResponse{Type: RespError, SessionID: req.SessionID, Error: err.Error()}
	}
	resp := WSResponse{
		Type:      RespReply,
		SessionID: req.SessionID,
		Text:      reply.Text,
		Card:      reply.Card,
		Action:    reply.Action,
	}
	// Attach the current session state so the front-end can update its UI.
	if sess, err := h.engine.GetSession(req.SessionID); err == nil {
		resp.State = sess.GetState().String()
	}
	return resp
}

// handleListSessions returns all live sessions for the requesting user.
func (h *WebHub) handleListSessions(client *WSClient, req WSRequest) WSResponse {
	uid := req.UserID
	if uid == "" {
		uid = client.userID
	}
	sessions := h.engine.ListSessions(uid)
	infos := make([]SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		infos = append(infos, SessionInfo{
			ID:        s.ID,
			UserID:    s.UserID,
			State:     s.GetState().String(),
			AlertID:   s.AlertID,
			UpdatedAt: s.UpdatedAt.Format(time.RFC3339),
		})
	}
	return WSResponse{
		Type:     RespSessionList,
		Sessions: infos,
	}
}

// handleCloseSession closes the session with the given id. It is
// idempotent from the client's perspective: a missing session yields an
// error response but does not disconnect the client.
func (h *WebHub) handleCloseSession(_ *WSClient, req WSRequest) WSResponse {
	if req.SessionID == "" {
		return WSResponse{Type: RespError, Error: "session_id is required"}
	}
	if err := h.engine.CloseSession(req.SessionID); err != nil {
		return WSResponse{Type: RespError, SessionID: req.SessionID, Error: err.Error()}
	}
	return WSResponse{
		Type:      RespReply,
		SessionID: req.SessionID,
		Text:      "session closed",
		State:     StateFailed.String(),
	}
}

// --- Broadcast helpers ------------------------------------------------------

// broadcastToUser sends a pre-encoded JSON frame to every active client
// bound to the given user. It is asynchronous: the frame is queued onto the
// Hub's broadcast channel and delivered by Run.
func (h *WebHub) broadcastToUser(userID string, message []byte) {
	select {
	case <-h.done:
		return
	default:
	}
	select {
	case h.broadcast <- WSBroadcast{message: message, userID: userID}:
	default:
		h.log.Warn("conversation/web: broadcast channel full, dropping message",
			"user_id", userID)
	}
}

// broadcastAll sends a pre-encoded JSON frame to every active client. It is
// asynchronous.
func (h *WebHub) broadcastAll(message []byte) {
	select {
	case <-h.done:
		return
	default:
	}
	select {
	case h.broadcast <- WSBroadcast{message: message}:
	default:
		h.log.Warn("conversation/web: broadcast channel full, dropping message")
	}
}

// --- Helpers ----------------------------------------------------------------

// encodeResponse marshals a WSResponse to JSON. It is a convenience helper
// for callers that want to broadcast structured frames.
func encodeResponse(resp WSResponse) []byte {
	data, err := json.Marshal(resp)
	if err != nil {
		return nil
	}
	return data
}
