// auth_tokens_test.go covers the multi-token bearer authentication added on
// top of the original single shared token: AuthTokens.Resolve, the subject
// injection performed by the gRPC interceptors and the REST auth middleware,
// the precedence of the authenticated subject over the client-asserted
// X-Acting-As header, and the REST method enforcement for change actions.
package grpc

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpcpkg "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// --- AuthTokens.Resolve ------------------------------------------------------

func TestAuthTokens_Resolve(t *testing.T) {
	tests := []struct {
		name        string
		tokens      AuthTokens
		presented   string
		wantSubject string
		wantOK      bool
	}{
		{
			name:      "disabled set never matches",
			tokens:    AuthTokens{},
			presented: "anything",
			wantOK:    false,
		},
		{
			name:        "legacy token matches with empty subject",
			tokens:      AuthTokens{Legacy: "shared"},
			presented:   "shared",
			wantSubject: "",
			wantOK:      true,
		},
		{
			name:      "legacy token mismatch",
			tokens:    AuthTokens{Legacy: "shared"},
			presented: "wrong",
			wantOK:    false,
		},
		{
			name: "named token matches its subject",
			tokens: AuthTokens{Named: []TokenIdentity{
				{Token: "alice-secret", Subject: "alice"},
				{Token: "bob-secret", Subject: "bob"},
			}},
			presented:   "bob-secret",
			wantSubject: "bob",
			wantOK:      true,
		},
		{
			name: "named token mismatch",
			tokens: AuthTokens{Named: []TokenIdentity{
				{Token: "alice-secret", Subject: "alice"},
			}},
			presented: "mallory-secret",
			wantOK:    false,
		},
		{
			name: "legacy and named coexist",
			tokens: AuthTokens{
				Legacy: "shared",
				Named:  []TokenIdentity{{Token: "alice-secret", Subject: "alice"}},
			},
			presented:   "alice-secret",
			wantSubject: "alice",
			wantOK:      true,
		},
		{
			name: "empty named token entry is skipped",
			tokens: AuthTokens{Named: []TokenIdentity{
				{Token: "", Subject: "ghost"},
				{Token: "real", Subject: "real-user"},
			}},
			presented:   "real",
			wantSubject: "real-user",
			wantOK:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := tt.tokens.Resolve(context.Background(), tt.presented)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantSubject, id.Subject)
			assert.Empty(t, id.Roles, "static tokens never carry roles")
		})
	}
}

func TestAuthTokens_Enabled(t *testing.T) {
	assert.False(t, AuthTokens{}.Enabled())
	assert.True(t, AuthTokens{Legacy: "x"}.Enabled())
	assert.True(t, AuthTokens{Named: []TokenIdentity{{Token: "t", Subject: "s"}}}.Enabled())
}

// --- gRPC interceptor subject injection -------------------------------------

func authCtx(token string) context.Context {
	md := metadata.Pairs("authorization", "Bearer "+token)
	return metadata.NewIncomingContext(context.Background(), md)
}

func TestAuthInterceptorFor_InjectsNamedSubject(t *testing.T) {
	tokens := AuthTokens{Named: []TokenIdentity{{Token: "alice-secret", Subject: "alice"}}}
	interceptor := AuthInterceptorFor(tokens)

	var seenActor string
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		seenActor = actorFromCtx(ctx)
		return "ok", nil
	}
	info := &grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}
	resp, err := interceptor(authCtx("alice-secret"), "req", info, handler)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp)
	assert.Equal(t, "alice", seenActor, "authenticated subject must reach the handler")
}

func TestAuthInterceptorFor_LegacyTokenCarriesNoSubject(t *testing.T) {
	interceptor := AuthInterceptorFor(AuthTokens{Legacy: "shared"})

	var seenActor string
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		seenActor = actorFromCtx(ctx)
		return "ok", nil
	}
	info := &grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}
	_, err := interceptor(authCtx("shared"), "req", info, handler)
	require.NoError(t, err)
	// The legacy token injects no named subject, so the handler observes the
	// actorFromCtx default fallback rather than a fabricated identity.
	assert.Equal(t, "grpc-user", seenActor, "legacy token must not fabricate a subject")
}

func TestAuthInterceptorFor_RejectsInvalidToken(t *testing.T) {
	tokens := AuthTokens{Named: []TokenIdentity{{Token: "alice-secret", Subject: "alice"}}}
	interceptor := AuthInterceptorFor(tokens)

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		t.Fatal("handler should not be called")
		return nil, nil
	}
	info := &grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}
	_, err := interceptor(authCtx("wrong-secret"), "req", info, handler)
	require.Error(t, err)
	st, _ := status.FromError(err)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestAuthInterceptorFor_SkipsHealthCheck(t *testing.T) {
	tokens := AuthTokens{Named: []TokenIdentity{{Token: "alice-secret", Subject: "alice"}}}
	interceptor := AuthInterceptorFor(tokens)

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return "ok", nil
	}
	info := &grpcpkg.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}
	_, err := interceptor(context.Background(), "req", info, handler)
	require.NoError(t, err)
	assert.True(t, called, "health check must remain unauthenticated")
}

// --- REST middleware subject precedence -------------------------------------

// TestRESTAuthMiddleware_SubjectOverridesAssertion verifies that a request
// authenticated with a named token is attributed to the token's subject even
// when the client also asserts a different identity via X-Acting-As.
func TestRESTAuthMiddleware_SubjectOverridesAssertion(t *testing.T) {
	cfg := ServeGatewayConfig{
		AuthTokens: []TokenIdentity{{Token: "alice-secret", Subject: "alice"}},
	}
	_, srv, _ := startTestGatewayFull(t, cfg)

	body := `{"label":"attributed-change"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/changes", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice-secret")
	req.Header.Set("X-Acting-As", "mallory")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer readAndClose(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The gateway responds in protojson (lowerCamelCase), so decode into a
	// plain struct with matching wire names rather than the protobuf struct.
	var created struct {
		ID        string `json:"id"`
		CreatedBy string `json:"createdBy"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, "alice", created.CreatedBy,
		"authenticated subject must win over the X-Acting-As assertion")
}

// TestRESTAuthMiddleware_AssertionUsedWithoutNamedToken verifies the legacy
// behaviour is preserved: with only a shared token, the X-Acting-As assertion
// still provides the actor.
func TestRESTAuthMiddleware_AssertionUsedWithoutNamedToken(t *testing.T) {
	cfg := ServeGatewayConfig{AuthToken: "shared"}
	_, srv, _ := startTestGatewayFull(t, cfg)

	body := `{"label":"asserted-change"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/changes", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer shared")
	req.Header.Set("X-Acting-As", "operator-bob")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer readAndClose(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var created struct {
		ID        string `json:"id"`
		CreatedBy string `json:"createdBy"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, "operator-bob", created.CreatedBy,
		"shared token keeps the client assertion as the actor")
}

// TestRESTAuthMiddleware_RejectsInvalidToken verifies the gateway rejects a
// bearer token that matches neither the legacy nor any named token.
func TestRESTAuthMiddleware_RejectsInvalidToken(t *testing.T) {
	cfg := ServeGatewayConfig{
		AuthToken:  "shared",
		AuthTokens: []TokenIdentity{{Token: "alice-secret", Subject: "alice"}},
	}
	_, srv, _ := startTestGatewayFull(t, cfg)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/changes", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer nope")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer readAndClose(resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// --- REST method enforcement -------------------------------------------------

// TestRESTChangeMethodEnforcement verifies that state-changing change actions
// reject non-POST verbs and read-only sub-paths reject non-GET verbs. This is
// the regression guard for the GET-can-trigger-state-change bug.
func TestRESTChangeMethodEnforcement(t *testing.T) {
	gw, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})
	ctx := context.Background()

	created, err := gw.change.CreateChange(ctx, &pb.CreateChangeRequest{Label: "method-enforcement"})
	require.NoError(t, err)
	id := created.GetId()

	mutating := []string{"plan", "apply", "approve", "reject", "pause", "resume", "cancel", "retry", "rollback", "archive"}
	for _, sub := range mutating {
		resp, err := http.Get(srv.URL + "/changes/" + id + "/" + sub)
		require.NoError(t, err, "sub-path %s", sub)
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode,
			"GET /changes/{id}/%s must be rejected", sub)
		readAndClose(resp)
	}

	for _, sub := range []string{"logs", "trace"} {
		resp, err := http.Post(srv.URL+"/changes/"+id+"/"+sub, "application/json", strings.NewReader("{}"))
		require.NoError(t, err, "sub-path %s", sub)
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode,
			"POST /changes/{id}/%s must be rejected", sub)
		readAndClose(resp)
	}
}

// TestRESTChangeOptionalBodyMalformed verifies that pause/resume/archive treat
// an empty body as valid but reject a malformed JSON body with 400 instead of
// silently swallowing the decode error.
func TestRESTChangeOptionalBodyMalformed(t *testing.T) {
	gw, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})
	ctx := context.Background()

	created, err := gw.change.CreateChange(ctx, &pb.CreateChangeRequest{Label: "body-validation"})
	require.NoError(t, err)
	id := created.GetId()

	for _, sub := range []string{"pause", "resume", "archive"} {
		resp := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/"+sub, "{not-json")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "sub-path %s", sub)
		readAndClose(resp)
	}
}
