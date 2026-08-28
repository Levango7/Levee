// auth_oidc_test.go covers the OIDC credential source wired into the
// multi-source auth stack: AuthTokens.Resolve dual-mode (static first, then
// JWT verification), subject/role injection through the gRPC interceptors,
// the REST auth middleware and the public /system/auth-info descriptor.
package grpc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/auth"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpcpkg "google.golang.org/grpc"
)

// oidcFixture is a minimal in-process OpenID Connect provider used to keep
// these tests off the network. (Package-local copy of the fixture in
// internal/auth: the original is unexported test infrastructure there.)
type oidcFixture struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	issuer string
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	fx := &oidcFixture{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                fx.issuer,
			"jwks_uri":                              fx.issuer + "/keys",
			"authorization_endpoint":                fx.issuer + "/authorize",
			"token_endpoint":                        fx.issuer + "/token",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       &fx.key.PublicKey,
			KeyID:     "test-key",
			Algorithm: "RS256",
			Use:       "sig",
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	})
	fx.srv = httptest.NewServer(mux)
	fx.issuer = fx.srv.URL
	t.Cleanup(fx.srv.Close)
	return fx
}

// sign issues a compact JWT signed by the fixture key.
func (f *oidcFixture) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), "test-key"),
	)
	require.NoError(t, err)
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	obj, err := signer.Sign(payload)
	require.NoError(t, err)
	raw, err := obj.CompactSerialize()
	require.NoError(t, err)
	return raw
}

// newTestOIDCVerifier builds a verifier against the fixture with the given
// config mutations applied.
func newTestOIDCVerifier(t *testing.T, fx *oidcFixture, mutate func(*auth.OIDCConfig)) *auth.Verifier {
	t.Helper()
	cfg := auth.OIDCConfig{Enabled: true, IssuerURL: fx.issuer, ClientID: "levee"}
	if mutate != nil {
		mutate(&cfg)
	}
	v, err := auth.NewVerifier(context.Background(), cfg)
	require.NoError(t, err)
	require.NotNil(t, v)
	return v
}

// validOIDCToken returns a fresh JWT the fixture verifier accepts.
func (f *oidcFixture) validOIDCToken(t *testing.T) string {
	t.Helper()
	return f.sign(t, map[string]any{
		"iss":                f.issuer,
		"sub":                "user-123",
		"aud":                "levee",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"iat":                time.Now().Unix(),
		"preferred_username": "sso-alice",
		"roles":              []string{"operator"},
	})
}

// --- AuthTokens.Resolve dual-mode --------------------------------------------

func TestAuthTokens_Resolve_OIDCDualMode(t *testing.T) {
	fx := newOIDCFixture(t)
	verifier := newTestOIDCVerifier(t, fx, func(c *auth.OIDCConfig) {
		c.RoleClaim = "roles"
	})
	tokens := AuthTokens{
		Legacy: "shared",
		Named:  []TokenIdentity{{Token: "alice-secret", Subject: "alice"}},
		OIDC:   verifier,
	}

	// Static tokens win over OIDC and carry no roles.
	id, ok := tokens.Resolve(context.Background(), "alice-secret")
	require.True(t, ok)
	assert.Equal(t, "alice", id.Subject)
	assert.Empty(t, id.Roles)

	// A JWT with matching claims resolves through the OIDC verifier.
	id, ok = tokens.Resolve(context.Background(), fx.validOIDCToken(t))
	require.True(t, ok, "a valid JWT must authenticate even though static tokens are configured")
	assert.Equal(t, "sso-alice", id.Subject)
	assert.Equal(t, []string{"operator"}, id.Roles)

	// A JWT signed for another issuer/audience is rejected even though it
	// has JWT shape.
	_, ok = tokens.Resolve(context.Background(), fx.sign(t, map[string]any{
		"iss": fx.issuer, "sub": "x", "aud": "other-api",
		"exp": time.Now().Add(time.Hour).Unix(),
	}))
	assert.False(t, ok, "wrong-audience JWT must not authenticate")

	// A non-JWT token matching no static entry is rejected without an OIDC
	// round-trip.
	_, ok = tokens.Resolve(context.Background(), "plain-unknown")
	assert.False(t, ok)
}

func TestAuthTokens_Enabled_OIDC(t *testing.T) {
	fx := newOIDCFixture(t)
	verifier := newTestOIDCVerifier(t, fx, nil)

	assert.True(t, AuthTokens{OIDC: verifier}.Enabled(),
		"a configured OIDC verifier alone must enable authentication")
	assert.False(t, AuthTokens{}.Enabled())
	assert.False(t, AuthTokens{OIDC: nil}.Enabled(),
		"explicit nil verifier must keep auth disabled")
}

// --- gRPC interceptor subject+role injection ----------------------------------

func TestAuthInterceptorFor_OIDCTokenInjection(t *testing.T) {
	fx := newOIDCFixture(t)
	verifier := newTestOIDCVerifier(t, fx, func(c *auth.OIDCConfig) {
		c.RoleClaim = "roles"
	})
	tokens := AuthTokens{OIDC: verifier}
	interceptor := AuthInterceptorFor(tokens)

	var seenActor string
	var seenRoles []string
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		seenActor = actorFromCtx(ctx)
		seenRoles = RolesFromContext(ctx)
		return "ok", nil
	}
	info := &grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}
	resp, err := interceptor(authCtx(fx.validOIDCToken(t)), "req", info, handler)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp)
	assert.Equal(t, "sso-alice", seenActor, "verified subject must reach the handler")
	assert.Equal(t, []string{"operator"}, seenRoles, "verified roles must reach the handler")
}

func TestAuthInterceptorFor_RejectsInvalidJWT(t *testing.T) {
	fx := newOIDCFixture(t)
	verifier := newTestOIDCVerifier(t, fx, nil)
	interceptor := AuthInterceptorFor(AuthTokens{OIDC: verifier})

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		t.Fatal("handler should not be called")
		return nil, nil
	}
	info := &grpcpkg.UnaryServerInfo{FullMethod: "/test/Method"}
	_, err := interceptor(authCtx(strings.Repeat("A", 60)+".eyJzdWIiOiJ4In0.c2ln"), "req", info, handler)
	require.Error(t, err, "a JWT-shaped but unverifiable token must be rejected")
}

// --- REST middleware + auth-info ----------------------------------------------

func TestRESTAuthMiddleware_OIDCToken(t *testing.T) {
	fx := newOIDCFixture(t)
	verifier := newTestOIDCVerifier(t, fx, func(c *auth.OIDCConfig) {
		c.RoleClaim = "roles"
	})
	cfg := ServeGatewayConfig{
		AuthToken: "shared",
		OIDC:      verifier,
	}
	_, srv, _ := startTestGatewayFull(t, cfg)

	body := `{"label":"sso-change"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/changes", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+fx.validOIDCToken(t))
	req.Header.Set("X-Acting-As", "mallory")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer readAndClose(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var created struct {
		ID        string `json:"id"`
		CreatedBy string `json:"createdBy"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	assert.Equal(t, "sso-alice", created.CreatedBy,
		"OIDC-verified subject must win over the X-Acting-As assertion")
}

func TestRESTSystemAuthInfo(t *testing.T) {
	fx := newOIDCFixture(t)

	t.Run("oidc enabled exposes discovery fields without auth", func(t *testing.T) {
		verifier := newTestOIDCVerifier(t, fx, nil)
		_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{
			AuthToken: "shared",
			OIDC:      verifier,
		})

		resp, err := http.Get(srv.URL + "/system/auth-info")
		require.NoError(t, err)
		defer readAndClose(resp)
		require.Equal(t, http.StatusOK, resp.StatusCode,
			"auth-info must be reachable without a bearer token")

		var info struct {
			OIDCEnabled  bool   `json:"oidcEnabled"`
			IssuerURL    string `json:"issuerUrl"`
			ClientID     string `json:"clientId"`
			AuthorizeURL string `json:"authorizeUrl"`
			TokenURL     string `json:"tokenUrl"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&info))
		assert.True(t, info.OIDCEnabled)
		assert.Equal(t, fx.issuer, info.IssuerURL)
		assert.Equal(t, "levee", info.ClientID)
		assert.Equal(t, fx.issuer+"/authorize", info.AuthorizeURL)
		assert.Equal(t, fx.issuer+"/token", info.TokenURL)
	})

	t.Run("oidc disabled reports false without discovery fields", func(t *testing.T) {
		_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{AuthToken: "shared"})

		resp, err := http.Get(srv.URL + "/system/auth-info")
		require.NoError(t, err)
		defer readAndClose(resp)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var raw map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&raw))
		assert.Equal(t, false, raw["oidcEnabled"])
		assert.NotContains(t, raw, "authorizeUrl", "no IdP endpoints may leak when OIDC is off")
	})
}
