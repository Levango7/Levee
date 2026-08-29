// auth_github_test.go covers the gateway-side GitHub SSO wiring: the
// /auth/github code-exchange endpoint (against a fake GitHub), session
// token acceptance through the normal auth path, the safety that a minted
// session token authenticates API calls and attributes the audit subject,
// and the /system/auth-info announcement.
package grpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexus/levee/internal/auth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ghTestSecret = "0123456789abcdef0123456789abcdef01234567"

// newGitHubGateway starts a test gateway with GitHub SSO pointed at fake
// GitHub endpoints and a shared session manager.
func newGitHubGateway(t *testing.T, mutate func(*auth.GitHubConfig)) (*Gateway, *httptest.Server, *auth.SessionManager) {
	t.Helper()

	fakeToken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != ghTestSecret {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gh-token-1"}`))
	}))
	t.Cleanup(fakeToken.Close)
	fakeAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"login":"octocat","id":1,"name":"The Octocat"}`))
		case "/user/teams":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"slug":"admins","organization":{"login":"acme"}},
				{"slug":"devs","organization":{"login":"acme"}}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fakeAPI.Close)

	cfg := auth.GitHubConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: ghTestSecret,
		Org:          "acme",
		TeamRoleMap:  map[string]string{"acme/admins": "admin"},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	gh, err := auth.NewGitHubAuthorizer(cfg, fakeToken.URL, fakeAPI.URL)
	require.NoError(t, err)

	sessions, err := auth.NewSessionManager(strings.Repeat("s", 32))
	require.NoError(t, err)

	gw, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{
		GitHub:   gh,
		Sessions: sessions,
	})
	return gw, srv, sessions
}

func TestGitHubLoginEndpoint_ExchangeAndSession(t *testing.T) {
	_, srv, sessions := newGitHubGateway(t, nil)

	// The login page only ever needs the fake's canonical callback URL
	// form; the handler reconstructs it from the request, so any value we
	// assert must match that reconstruction. Omit redirect_uri to let the
	// server derive it.
	body := `{"code":"code-1","state":"st-1"}`
	resp := doReq(t, http.MethodPost, srv.URL+"/auth/github", body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "github exchange must succeed against the fake")
	var login struct {
		Token   string   `json:"token"`
		Subject string   `json:"subject"`
		Roles   []string `json:"roles"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&login))
	readAndClose(resp)
	assert.Equal(t, "octocat", login.Subject)
	assert.Contains(t, login.Roles, "admin")

	// The minted token must verify under the same session manager and,
	// more importantly, authenticate a real API call with the GitHub
	// login as the audit subject.
	claims, err := sessions.VerifySession(login.Token)
	require.NoError(t, err)
	assert.Equal(t, "octocat", claims.Subject)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/changes", strings.NewReader(`{"label":"gh-change"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+login.Token)
	req.Header.Set("X-Acting-As", "mallory")
	apiResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer readAndClose(apiResp)
	require.Equal(t, http.StatusOK, apiResp.StatusCode)

	var created struct {
		ID        string `json:"id"`
		CreatedBy string `json:"createdBy"`
	}
	require.NoError(t, json.NewDecoder(apiResp.Body).Decode(&created))
	assert.Equal(t, "octocat", created.CreatedBy,
		"session-authenticated subject must win over the X-Acting-As assertion")
}

func TestGitHubLoginEndpoint_MissingFields(t *testing.T) {
	_, srv, _ := newGitHubGateway(t, nil)

	resp := doReq(t, http.MethodPost, srv.URL+"/auth/github", `{"code":"code-1"}`)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	readAndClose(resp)
	resp = doReq(t, http.MethodPost, srv.URL+"/auth/github", `{"code":"","state":""}`)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	readAndClose(resp)
}

func TestGitHubLoginEndpoint_Disabled(t *testing.T) {
	// No GitHub config: endpoint must 404.
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{AuthToken: "shared"})
	resp := doReq(t, http.MethodPost, srv.URL+"/auth/github", `{"code":"c","state":"s"}`)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	readAndClose(resp)
}

func TestAuthTokens_Resolve_SessionSource(t *testing.T) {
	sessions, err := auth.NewSessionManager(strings.Repeat("s", 32))
	require.NoError(t, err)
	tok, err := sessions.Issue("octocat", []string{"operator"}, "github")
	require.NoError(t, err)

	tokens := AuthTokens{Sessions: sessions}
	assert.True(t, tokens.Enabled(), "a session manager alone must enable authentication")

	id, ok := tokens.Resolve(context.Background(), tok)
	require.True(t, ok)
	assert.Equal(t, "octocat", id.Subject)
	assert.Equal(t, []string{"operator"}, id.Roles)

	// Garbage session tokens must not authenticate and must not be tried
	// against OIDC (none configured here anyway).
	_, ok = tokens.Resolve(context.Background(), "not-a-session-token")
	assert.False(t, ok)
}

func TestRESTSystemAuthInfo_GitHub(t *testing.T) {
	_, srv, _ := newGitHubGateway(t, nil)

	resp, err := http.Get(srv.URL + "/system/auth-info")
	require.NoError(t, err)
	defer readAndClose(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var info struct {
		GitHubEnabled  bool   `json:"githubEnabled"`
		GitHubClientID string `json:"githubClientId"`
		GitHubOrg      string `json:"githubOrg"`
		OIDCEnabled    bool   `json:"oidcEnabled"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&info))
	assert.True(t, info.GitHubEnabled)
	assert.Equal(t, "cid", info.GitHubClientID)
	assert.Equal(t, "acme", info.GitHubOrg)
	assert.False(t, info.OIDCEnabled)
}
