// rest_conversation_owner_test.go covers the conversation REST session
// endpoints' ownership rules (P2-2): an authenticated subject always wins
// over the client-asserted user_id, cross-owner access to a session is
// rejected with 403, and development mode (no configured tokens) keeps the
// client-asserted fallback usable.
package grpc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexus/levee/internal/conversation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startConversationGateway wires a gateway whose only capability is the
// conversation engine, behind the same middleware chain serve() uses.
func startConversationGateway(t *testing.T, cfg ServeGatewayConfig) (*Gateway, *httptest.Server) {
	t.Helper()
	engine := conversation.NewConversationEngine(conversation.ConversationEngineConfig{})
	gw := NewGateway(cfg)
	gw.SetConversationEngine(engine)

	mux := http.NewServeMux()
	mux.Handle("/", requestIDMiddlewareForTest(gw, corsMiddleware(cfg.CORSOrigins, gw.authMiddleware(gw.rateLimitMiddleware(gw.restRoute())))))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return gw, srv
}

// doConvReq issues one HTTP request with an optional bearer token and JSON
// body; the caller must close the response body.
func doConvReq(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// convSessionDTO is the subset of the session DTO the tests assert on.
type convSessionDTO struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
}

// createConvSession POSTs a new session and returns the decoded DTO.
func createConvSession(t *testing.T, srv *httptest.Server, token, body string) convSessionDTO {
	t.Helper()
	resp := doConvReq(t, http.MethodPost, srv.URL+"/conversation/sessions", token, body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var created struct {
		Session convSessionDTO `json:"session"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.NoError(t, resp.Body.Close())
	require.NotEmpty(t, created.Session.ID)
	return created.Session
}

func TestConversationREST_SubjectOverridesAssertedUser(t *testing.T) {
	cfg := ServeGatewayConfig{
		Addr: "127.0.0.1:0",
		AuthTokens: []TokenIdentity{
			{Token: "alice-secret", Subject: "alice"},
			{Token: "bob-secret", Subject: "bob"},
		},
	}
	_, srv := startConversationGateway(t, cfg)

	// Create: the asserted user_id is ignored; the session belongs to the
	// authenticated subject.
	sess := createConvSession(t, srv, "alice-secret", `{"user_id":"mallory"}`)
	assert.Equal(t, "alice", sess.UserID)

	// List: the asserted query user_id is ignored as well; alice sees her
	// own session even while asking for someone else's.
	resp := doConvReq(t, http.MethodGet, srv.URL+"/conversation/sessions?user_id=mallory", "alice-secret", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var listed struct {
		Sessions []convSessionDTO `json:"sessions"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listed))
	require.NoError(t, resp.Body.Close())
	require.Len(t, listed.Sessions, 1)
	assert.Equal(t, sess.ID, listed.Sessions[0].ID)
}

func TestConversationREST_CrossUserForbidden(t *testing.T) {
	cfg := ServeGatewayConfig{
		Addr: "127.0.0.1:0",
		AuthTokens: []TokenIdentity{
			{Token: "alice-secret", Subject: "alice"},
			{Token: "bob-secret", Subject: "bob"},
		},
	}
	_, srv := startConversationGateway(t, cfg)
	sess := createConvSession(t, srv, "alice-secret", `{"user_id":"alice"}`)

	// bob cannot read, message, or close alice's session.
	resp := doConvReq(t, http.MethodGet, srv.URL+"/conversation/sessions/"+sess.ID, "bob-secret", "")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	resp = doConvReq(t, http.MethodPost, srv.URL+"/conversation/sessions/"+sess.ID+"/messages", "bob-secret", `{"user_id":"alice","text":"/help"}`)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	resp = doConvReq(t, http.MethodDelete, srv.URL+"/conversation/sessions/"+sess.ID, "bob-secret", "")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// alice can read, message, and close her own session. The message
	// reply arrives in the {reply} envelope (P2-1 contract).
	resp = doConvReq(t, http.MethodGet, srv.URL+"/conversation/sessions/"+sess.ID, "alice-secret", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	resp = doConvReq(t, http.MethodPost, srv.URL+"/conversation/sessions/"+sess.ID+"/messages", "alice-secret", `{"user_id":"alice","text":"/help"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var replied struct {
		Reply struct {
			Text string `json:"text"`
		} `json:"reply"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&replied))
	require.NoError(t, resp.Body.Close())
	assert.NotEmpty(t, replied.Reply.Text)

	resp = doConvReq(t, http.MethodDelete, srv.URL+"/conversation/sessions/"+sess.ID, "alice-secret", "")
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
}

func TestConversationREST_InsecureFallback(t *testing.T) {
	// No tokens configured: authentication is disabled (development mode)
	// and the client-asserted user_id is the fallback identity.
	cfg := ServeGatewayConfig{Addr: "127.0.0.1:0"}
	_, srv := startConversationGateway(t, cfg)

	sess := createConvSession(t, srv, "", `{"user_id":"dev-user"}`)
	assert.Equal(t, "dev-user", sess.UserID)

	// A missing user_id is still rejected on create.
	resp := doConvReq(t, http.MethodPost, srv.URL+"/conversation/sessions", "", `{}`)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// A different asserted user cannot read the session.
	resp = doConvReq(t, http.MethodGet, srv.URL+"/conversation/sessions/"+sess.ID+"?user_id=other", "", "")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	// No assertion at all passes through (development-mode convenience).
	resp = doConvReq(t, http.MethodGet, srv.URL+"/conversation/sessions/"+sess.ID, "", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
}
