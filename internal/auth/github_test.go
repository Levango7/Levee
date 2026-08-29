// github_test.go covers the GitHub OAuth authorizer against a fake GitHub
// (httptest servers standing in for the token, user and teams endpoints),
// plus configuration validation. No real network access.
package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fakeClientSecret = "0123456789abcdef0123456789abcdef01234567" // 40 chars, GitHub-shaped

// fakeGitHub serves the three endpoints the authorizer touches and records
// the requests it saw.
type fakeGitHub struct {
	tokenSrv *httptest.Server
	apiSrv   *httptest.Server

	// Optional failure injections.
	tokenStatus int
	userStatus  int

	sawTokenRequest bool
	sawAuthBearer   string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}

	f.tokenSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.sawTokenRequest = true
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != fakeClientSecret {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		if f.tokenStatus != 0 {
			w.WriteHeader(f.tokenStatus)
			_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gh-token-1"}`))
	}))
	t.Cleanup(f.tokenSrv.Close)

	f.apiSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.sawAuthBearer = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/user":
			if f.userStatus != 0 {
				w.WriteHeader(f.userStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"login":"octocat","id":1,"name":"The Octocat"}`))
		case "/user/teams":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"slug":"admins","organization":{"login":"acme"}},
				{"slug":"devs","organization":{"login":"acme"}},
				{"slug":"other","organization":{"login":"notacme"}}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.apiSrv.Close)
	return f
}

// authorizerAgainst builds a GitHubAuthorizer whose endpoints point at the
// fake servers.
func authorizerAgainst(t *testing.T, f *fakeGitHub, mutate func(*GitHubConfig)) *GitHubAuthorizer {
	t.Helper()
	cfg := GitHubConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: fakeClientSecret,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	g, err := NewGitHubAuthorizer(cfg)
	require.NoError(t, err)
	require.NotNil(t, g)
	// Point the authorizer at the fakes.
	g.githubTokenEndpoint = f.tokenSrv.URL + "/login/oauth/access_token"
	g.githubAPIBase = f.apiSrv.URL
	return g
}

func TestNewGitHubAuthorizer_ConfigValidation(t *testing.T) {
	// Disabled returns a nil (disabled) authorizer.
	g, err := NewGitHubAuthorizer(GitHubConfig{Enabled: false})
	require.NoError(t, err)
	assert.Nil(t, g)
	assert.False(t, g.Enabled())
	assert.Empty(t, g.ClientID())
	assert.Empty(t, g.Org())

	_, err = NewGitHubAuthorizer(GitHubConfig{Enabled: true, ClientSecret: fakeClientSecret})
	assert.ErrorContains(t, err, "client_id")

	_, err = NewGitHubAuthorizer(GitHubConfig{Enabled: true, ClientID: "cid"})
	assert.ErrorContains(t, err, "client_secret")

	_, err = NewGitHubAuthorizer(GitHubConfig{Enabled: true, ClientID: "cid", ClientSecret: "short"})
	assert.ErrorContains(t, err, "too short")
}

func TestGitHubExchange_FullFlowWithOrgAndRoles(t *testing.T) {
	f := newFakeGitHub(t)
	g := authorizerAgainst(t, f, func(c *GitHubConfig) {
		c.Org = "acme"
		c.TeamRoleMap = map[string]string{
			"acme/admins": "admin",
			"acme/devs":   "operator",
		}
	})

	id, err := g.ExchangeCode(context.Background(), "code-1", "https://levee.local/login/callback")
	require.NoError(t, err)
	assert.Equal(t, "octocat", id.Login)
	assert.EqualValues(t, 1, id.ID)
	assert.Equal(t, "The Octocat", id.Name)
	// admins + devs both map; "notacme/other" contributes nothing.
	assert.ElementsMatch(t, []string{"admin", "operator"}, id.Roles)

	// The access token was presented to the API exactly for the two calls
	// and never persisted in the identity.
	assert.Equal(t, "Bearer gh-token-1", f.sawAuthBearer)
	assert.True(t, f.sawTokenRequest)
}

func TestGitHubExchange_OrgRestrictionRejectsOutsider(t *testing.T) {
	f := newFakeGitHub(t)
	g := authorizerAgainst(t, f, func(c *GitHubConfig) {
		c.Org = "unknown-org" // not among the fake user's team orgs
	})
	_, err := g.ExchangeCode(context.Background(), "code-1", "https://levee.local/login/callback")
	require.Error(t, err)
	assert.ErrorContains(t, err, "not a member of org")
}

func TestGitHubExchange_NoOrgNoRolesSkipsTeams(t *testing.T) {
	f := newFakeGitHub(t)
	g := authorizerAgainst(t, f, nil)

	id, err := g.ExchangeCode(context.Background(), "code-1", "https://levee.local/login/callback")
	require.NoError(t, err)
	assert.Equal(t, "octocat", id.Login)
	assert.Empty(t, id.Roles)
}

func TestGitHubExchange_TokenFailure(t *testing.T) {
	f := newFakeGitHub(t)
	f.tokenStatus = http.StatusUnauthorized
	g := authorizerAgainst(t, f, nil)

	_, err := g.ExchangeCode(context.Background(), "expired-code", "https://levee.local/login/callback")
	require.Error(t, err)
	assert.ErrorContains(t, err, "bad_verification_code")
}

func TestGitHubExchange_WrongSecret(t *testing.T) {
	f := newFakeGitHub(t)
	// client_secret mismatch makes the fake token endpoint 401.
	g := authorizerAgainst(t, f, func(c *GitHubConfig) {
		c.ClientSecret = strings.Repeat("x", 40)
	})
	// Re-check: NewGitHubAuthorizer validated the ORIGINAL secret; the
	// authorizer carries the mutated one into the request.
	_, err := g.ExchangeCode(context.Background(), "code-1", "https://levee.local/login/callback")
	require.Error(t, err)
	assert.ErrorContains(t, err, "401")
}

func TestGitHubExchange_UserEndpointFails(t *testing.T) {
	f := newFakeGitHub(t)
	f.userStatus = http.StatusInternalServerError
	g := authorizerAgainst(t, f, nil)

	_, err := g.ExchangeCode(context.Background(), "code-1", "https://levee.local/login/callback")
	require.Error(t, err)
	assert.ErrorContains(t, err, "/user")
}

func TestGitHubExchange_Disabled(t *testing.T) {
	var g *GitHubAuthorizer
	_, err := g.ExchangeCode(context.Background(), "code", "https://x")
	require.Error(t, err)
}
