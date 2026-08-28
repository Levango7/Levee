// Package auth provides identity verification for LEVEE's API surfaces.
//
// The OIDC verifier validates JWTs issued by any OpenID Connect provider
// against the provider's published JWKS (discovered via the standard
// .well-known endpoint), extracting an authenticated subject and optional
// roles. It is IdP-agnostic: Keycloak, Zitadel, Entra ID, Okta and any
// conformant provider work without code changes. Static bearer tokens
// (machine identities / break-glass) are handled separately by the grpc
// package and take precedence over JWT verification.
package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDCConfig configures the OIDC token verifier.
type OIDCConfig struct {
	// Enabled activates OIDC JWT verification. When false, NewVerifier
	// returns a nil verifier and static tokens are the only accepted
	// credentials.
	Enabled bool
	// IssuerURL is the IdP's issuer URL, e.g.
	// https://idp.example.com/realms/levee. Discovery is performed
	// against <IssuerURL>/.well-known/openid-configuration.
	IssuerURL string
	// ClientID is the OAuth2 client id LEVEE is registered as in the IdP.
	ClientID string
	// Audience is the expected "aud" claim of accepted tokens. Defaults
	// to ClientID when empty; set explicitly when the IdP issues access
	// tokens for a distinct API audience.
	Audience string
	// UsernameClaim names the claim used as the authenticated subject
	// (default "preferred_username", falling back to "email" then "sub").
	UsernameClaim string
	// RoleClaim optionally names a claim carrying the caller's roles or
	// groups (string, []string, or comma-separated string).
	RoleClaim string
	// RoleMap optionally maps IdP role values to LEVEE role names. Roles
	// not present in the map pass through unchanged.
	RoleMap map[string]string
}

// DefaultUsernameClaim is used when OIDCConfig.UsernameClaim is empty.
const DefaultUsernameClaim = "preferred_username"

// Identity is a successfully verified caller identity extracted from a
// validated JWT.
type Identity struct {
	// Subject is the authenticated username (from the configured claim).
	Subject string
	// Roles carries the (optionally mapped) role values, possibly empty.
	Roles []string
	// Issuer is the token's iss claim, as verified against the provider.
	Issuer string
	// Expiry is the token's exp claim.
	Expiry time.Time
}

// Verifier verifies JWTs against an OIDC provider's published keys. The
// zero value is not usable; construct with NewVerifier. A nil *Verifier is
// a valid "disabled" value: all accessors are nil-safe.
type Verifier struct {
	cfg      OIDCConfig
	verifier *oidc.IDTokenVerifier
	authURL  string
	tokenURL string
}

// NewVerifier performs OIDC discovery against cfg.IssuerURL and returns a
// ready verifier. It returns (nil, nil) when cfg.Enabled is false. Discovery
// failure is an error: a daemon configured for OIDC must not start unable
// to verify the tokens it is configured to accept.
func NewVerifier(ctx context.Context, cfg OIDCConfig) (*Verifier, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if strings.TrimSpace(cfg.IssuerURL) == "" {
		return nil, fmt.Errorf("oidc: issuer_url is required when enabled")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, fmt.Errorf("oidc: client_id is required when enabled")
	}

	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery for issuer %q: %w", cfg.IssuerURL, err)
	}

	aud := cfg.Audience
	if aud == "" {
		aud = cfg.ClientID
	}

	v := &Verifier{
		cfg:      cfg,
		verifier: provider.Verifier(&oidc.Config{ClientID: aud}),
	}
	// The frontend login flow needs the authorization and token endpoints;
	// surface them from the same discovery document.
	var disc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := provider.Claims(&disc); err == nil {
		v.authURL = disc.AuthorizationEndpoint
		v.tokenURL = disc.TokenEndpoint
	}
	return v, nil
}

// Enabled reports whether the verifier is configured. Nil-safe.
func (v *Verifier) Enabled() bool { return v != nil && v.verifier != nil }

// IssuerURL returns the configured issuer URL. Nil-safe.
func (v *Verifier) IssuerURL() string {
	if v == nil {
		return ""
	}
	return v.cfg.IssuerURL
}

// ClientID returns the configured OAuth2 client id. Nil-safe.
func (v *Verifier) ClientID() string {
	if v == nil {
		return ""
	}
	return v.cfg.ClientID
}

// Endpoints returns the IdP's authorization and token endpoints learned
// during discovery. Nil-safe; both are empty when disabled.
func (v *Verifier) Endpoints() (authorizeURL, tokenURL string) {
	if v == nil {
		return "", ""
	}
	return v.authURL, v.tokenURL
}

// LooksLikeJWT reports whether token has the three-segment JWT shape.
// Used to decide whether a presented bearer token is a candidate for OIDC
// verification at all; static tokens are always checked first.
func LooksLikeJWT(token string) bool {
	return strings.Count(token, ".") == 2
}

// Verify validates raw as a JWT issued by the configured provider: signature
// against the provider's JWKS, plus issuer, audience and expiry checks. On
// success it extracts the subject and roles per the verifier configuration.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Identity, error) {
	if !v.Enabled() {
		return nil, fmt.Errorf("oidc: verifier disabled")
	}
	tok, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}

	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("oidc: parse claims: %w", err)
	}

	subject := subjectFromClaims(claims, v.cfg.UsernameClaim, tok.Subject)
	if subject == "" {
		return nil, fmt.Errorf("oidc: no usable subject claim")
	}

	return &Identity{
		Subject: subject,
		Roles:   mapRoles(extractRoles(claims, v.cfg.RoleClaim), v.cfg.RoleMap),
		Issuer:  tok.Issuer,
		Expiry:  tok.Expiry,
	}, nil
}

// subjectFromClaims resolves the authenticated username: the configured
// claim first, then email, then the token's sub.
func subjectFromClaims(claims map[string]any, configured, sub string) string {
	name := configured
	if name == "" {
		name = DefaultUsernameClaim
	}
	if s := claimString(claims, name); s != "" {
		return s
	}
	if name != "email" {
		if s := claimString(claims, "email"); s != "" {
			return s
		}
	}
	return sub
}

// claimString returns the named claim when it holds a non-empty string.
func claimString(claims map[string]any, name string) string {
	if name == "" {
		return ""
	}
	s, _ := claims[name].(string)
	return strings.TrimSpace(s)
}

// extractRoles reads the role claim, tolerating the common shapes IdPs use:
// a []string / []any of strings, a single string, or a comma-separated
// string. Returns nil when the claim is absent or empty.
func extractRoles(claims map[string]any, name string) []string {
	if name == "" {
		return nil
	}
	raw, ok := claims[name]
	if !ok {
		return nil
	}
	var out []string
	switch v := raw.(type) {
	case []any:
		for _, el := range v {
			if s, ok := el.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case []string:
		for _, s := range v {
			if strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case string:
		for _, part := range strings.Split(v, ",") {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// mapRoles applies the configured IdP-role → LEVEE-role mapping. Roles not
// present in the map pass through unchanged; an empty map is a no-op.
func mapRoles(roles []string, roleMap map[string]string) []string {
	if len(roleMap) == 0 || len(roles) == 0 {
		return roles
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		if mapped, ok := roleMap[r]; ok {
			out = append(out, mapped)
		} else {
			out = append(out, r)
		}
	}
	return out
}
