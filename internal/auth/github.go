// github.go implements the server-side half of the GitHub OAuth login flow.
//
// GitHub is not an OIDC provider: its token endpoint does not serve browser
// CORS and its access token is opaque. Therefore the flow diverges from the
// OIDC path in two ways:
//
//   - the frontend sends the authorization code to the LEVEE gateway
//     (POST /auth/github), and the GATEWAY exchanges it for the access token
//     (client_secret never reaches the browser),
//   - the resulting GitHub token is used exactly once to fetch /user and
//     /user/teams, then discarded; the gateway mints a LEVEE session token
//     for the browser (see session.go) and every later request verifies
//     that local token without touching api.github.com.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHubConfig configures the GitHub OAuth login flow.
type GitHubConfig struct {
	// Enabled turns the flow on. When false the gateway exposes no GitHub
	// endpoints and /system/auth-info reports no GitHub login.
	Enabled bool
	// ClientID / ClientSecret come from a GitHub OAuth App registration.
	ClientID     string
	ClientSecret string
	// Org, when set, restricts login to members of that GitHub org. Empty
	// admits any GitHub user that completed the OAuth grant — usually not
	// what an internal deployment wants.
	Org string
	// TeamRoleMap maps GitHub team slugs ("org-slug/team-slug") to LEVEE
	// roles. Only teams present in the map contribute roles; membership
	// alone grants nothing.
	TeamRoleMap map[string]string
}

// GitHubLoginURL is the authorization endpoint browsers are redirected to.
const GitHubLoginURL = "https://github.com/login/oauth/authorize"

const (
	githubTokenURL = "https://github.com/login/oauth/access_token"
	githubAPIBase  = "https://api.github.com"
)

// GitHubAuthorizer runs the code-exchange half of the flow. The zero value
// is unusable; construct with NewGitHubAuthorizer. A nil *GitHubAuthorizer
// is a valid "disabled" value: Enabled() is nil-safe.
type GitHubAuthorizer struct {
	cfg GitHubConfig
	cli *http.Client

	// githubTokenEndpoint / githubAPIBase default to the production GitHub
	// URLs; they are unexported fields so tests can point them at fakes.
	githubTokenEndpoint string
	githubAPIBase       string
}

// NewGitHubAuthorizer validates the configuration and returns the
// authorizer. Missing client_id/client_secret (or a too-short secret, which
// GitHub rejects anyway) fail here rather than at first login.
//
// testEndpoints is production-nil: tests point the authorizer at fake
// GitHub servers by supplying both URLs. Exactly one of the two must be
// set, or both empty (production endpoints).
func NewGitHubAuthorizer(cfg GitHubConfig, testEndpoints ...string) (*GitHubAuthorizer, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("github: client_id is required when enabled")
	}
	if strings.TrimSpace(cfg.ClientSecret) == "" {
		return nil, errors.New("github: client_secret is required when enabled")
	}
	if len(cfg.ClientSecret) < 32 {
		return nil, errors.New("github: client_secret looks too short (GitHub secrets are 40 chars)")
	}
	g := &GitHubAuthorizer{
		cfg:                 cfg,
		cli:                 &http.Client{Timeout: 15 * time.Second},
		githubTokenEndpoint: githubTokenURL,
		githubAPIBase:       githubAPIBase,
	}
	switch len(testEndpoints) {
	case 0:
	case 2:
		g.githubTokenEndpoint = testEndpoints[0]
		g.githubAPIBase = testEndpoints[1]
	default:
		return nil, errors.New("github: test endpoints must be supplied as a (tokenURL, apiBase) pair")
	}
	return g, nil
}

// Enabled reports whether the authorizer is configured. Nil-safe.
func (g *GitHubAuthorizer) Enabled() bool { return g != nil }

// ClientID returns the configured OAuth client id. Nil-safe.
func (g *GitHubAuthorizer) ClientID() string {
	if g == nil {
		return ""
	}
	return g.cfg.ClientID
}

// Org returns the configured org restriction ("" = unrestricted). Nil-safe.
func (g *GitHubAuthorizer) Org() string {
	if g == nil {
		return ""
	}
	return g.cfg.Org
}

// Identity is the GitHub-verified identity used for the session token.
type GitHubIdentity struct {
	Login string
	ID    int64
	Name  string
	Roles []string
}

// ExchangeCode trades the one-time authorization code for the GitHub-verified
// identity: access token (POST, server-side only) -> /user -> optional
// org/teams checks. redirectURI must match the value used in the
// authorization request. state is the CSRF nonce the frontend generated;
// the caller (the gateway handler) must compare it against the value stashed
// at redirect time BEFORE calling this — GitHub does not echo it for us.
func (g *GitHubAuthorizer) ExchangeCode(ctx context.Context, code, redirectURI string) (*GitHubIdentity, error) {
	if !g.Enabled() {
		return nil, errors.New("github: authorizer not configured")
	}

	form := url.Values{
		"client_id":     {g.cfg.ClientID},
		"client_secret": {g.cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.githubTokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("github: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := g.doJSON(req, &tok); err != nil {
		return nil, err
	}
	if tok.Error != "" || tok.AccessToken == "" {
		return nil, fmt.Errorf("github: token exchange failed (%s)", orDash(tok.Error))
	}

	// Fetch the user the token belongs to; the token is discarded afterwards.
	var user struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Name  string `json:"name"`
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, g.githubAPIBase+"/user", nil)
	if err != nil {
		return nil, fmt.Errorf("github: build user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	if err := g.doJSON(req, &user); err != nil {
		return nil, err
	}
	if user.Login == "" {
		return nil, errors.New("github: user response had no login")
	}

	roles, err := g.rolesFor(ctx, tok.AccessToken, user.Login)
	if err != nil {
		return nil, err
	}
	return &GitHubIdentity{Login: user.Login, ID: user.ID, Name: user.Name, Roles: roles}, nil
}

// rolesFor enforces the org restriction (when configured) and maps team
// membership to LEVEE roles. Teams are only fetched when either knob needs
// them.
func (g *GitHubAuthorizer) rolesFor(ctx context.Context, accessToken, login string) ([]string, error) {
	needOrg := g.cfg.Org != ""
	needTeams := len(g.cfg.TeamRoleMap) > 0
	if !needOrg && !needTeams {
		return nil, nil
	}

	var teams []struct {
		Slug string `json:"slug"`
		Org  struct {
			Login string `json:"login"`
		} `json:"organization"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.githubAPIBase+"/user/teams?per_page=100", nil)
	if err != nil {
		return nil, fmt.Errorf("github: build teams request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	if err := g.doJSON(req, &teams); err != nil {
		return nil, err
	}

	if needOrg {
		inOrg := false
		for _, t := range teams {
			if strings.EqualFold(t.Org.Login, g.cfg.Org) {
				inOrg = true
				break
			}
		}
		if !inOrg {
			return nil, fmt.Errorf("github: user %s is not a member of org %q", login, g.cfg.Org)
		}
	}

	var roles []string
	for _, t := range teams {
		full := t.Org.Login + "/" + t.Slug
		if role, ok := g.cfg.TeamRoleMap[full]; ok {
			roles = append(roles, role)
		}
	}
	return roles, nil
}

// doJSON executes req and decodes a JSON body into out. Non-2xx responses
// are surfaced with their status; GitHub error payloads are small, so up to
// 4KB of body is read for context.
func (g *GitHubAuthorizer) doJSON(req *http.Request, out any) error {
	resp, err := g.cli.Do(req)
	if err != nil {
		return fmt.Errorf("github: request to %s: %w", req.URL.Host, err)
	}
	defer func() { _ = resp.Body.Close }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("github: %s -> HTTP %d: %s", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("github: decode response from %s: %w", req.URL.Path, err)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "no error detail"
	}
	return s
}
