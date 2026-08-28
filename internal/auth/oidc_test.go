package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureIdP is a minimal in-process OpenID Connect provider: a discovery
// document plus a JWKS endpoint backed by a fresh RSA key. It exists so the
// verifier tests never touch a real IdP.
type fixtureIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	issuer string
}

func newFixtureIdP(t *testing.T) *fixtureIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	idp := &fixtureIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.issuer,
			"jwks_uri":                              idp.issuer + "/keys",
			"authorization_endpoint":                idp.issuer + "/authorize",
			"token_endpoint":                        idp.issuer + "/token",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       &idp.key.PublicKey,
			KeyID:     "test-key",
			Algorithm: "RS256",
			Use:       "sig",
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	})
	idp.srv = httptest.NewServer(mux)
	idp.issuer = idp.srv.URL
	t.Cleanup(idp.srv.Close)
	return idp
}

// sign issues a compact JWT with the given claims, signed by the fixture key.
func (f *fixtureIdP) sign(t *testing.T, claims map[string]any) string {
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

func (f *fixtureIdP) standardClaims(aud string) map[string]any {
	return map[string]any{
		"iss":                f.issuer,
		"sub":                "user-123",
		"aud":                aud,
		"exp":                time.Now().Add(time.Hour).Unix(),
		"iat":                time.Now().Unix(),
		"preferred_username": "alice",
	}
}

func newTestVerifier(t *testing.T, idp *fixtureIdP, mutate func(*OIDCConfig)) *Verifier {
	t.Helper()
	cfg := OIDCConfig{
		Enabled:   true,
		IssuerURL: idp.issuer,
		ClientID:  "levee",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	v, err := NewVerifier(context.Background(), cfg)
	require.NoError(t, err)
	require.NotNil(t, v)
	return v
}

func TestNewVerifier_Disabled(t *testing.T) {
	v, err := NewVerifier(context.Background(), OIDCConfig{Enabled: false})
	assert.NoError(t, err)
	assert.Nil(t, v)
	assert.False(t, v.Enabled()) // nil-safe
	assert.Empty(t, v.IssuerURL())
	assert.Empty(t, v.ClientID())
	auth, tok := v.Endpoints()
	assert.Empty(t, auth)
	assert.Empty(t, tok)
}

func TestNewVerifier_MissingFields(t *testing.T) {
	_, err := NewVerifier(context.Background(), OIDCConfig{Enabled: true, ClientID: "x"})
	assert.ErrorContains(t, err, "issuer_url")
	_, err = NewVerifier(context.Background(), OIDCConfig{Enabled: true, IssuerURL: "https://idp.example"})
	assert.ErrorContains(t, err, "client_id")
}

func TestNewVerifier_DiscoveryFailure(t *testing.T) {
	// A server that is closed immediately cannot serve discovery.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := NewVerifier(context.Background(), OIDCConfig{Enabled: true, IssuerURL: url, ClientID: "x"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "discovery")
}

func TestNewVerifier_ExposesEndpoints(t *testing.T) {
	idp := newFixtureIdP(t)
	v := newTestVerifier(t, idp, nil)
	authURL, tokenURL := v.Endpoints()
	assert.Equal(t, idp.issuer+"/authorize", authURL)
	assert.Equal(t, idp.issuer+"/token", tokenURL)
	assert.Equal(t, "levee", v.ClientID())
	assert.Equal(t, idp.issuer, v.IssuerURL())
}

func TestVerify_ValidToken(t *testing.T) {
	idp := newFixtureIdP(t)
	v := newTestVerifier(t, idp, func(c *OIDCConfig) {
		c.RoleClaim = "roles"
		c.RoleMap = map[string]string{"levee-admin": "admin"}
	})
	claims := idp.standardClaims("levee")
	claims["roles"] = []string{"levee-admin", "viewer"}

	id, err := v.Verify(context.Background(), idp.sign(t, claims))
	require.NoError(t, err)
	assert.Equal(t, "alice", id.Subject)
	assert.Equal(t, []string{"admin", "viewer"}, id.Roles)
	assert.Equal(t, idp.issuer, id.Issuer)
	assert.True(t, id.Expiry.After(time.Now()))
}

func TestVerify_SubjectFallbackEmailThenSub(t *testing.T) {
	idp := newFixtureIdP(t)
	v := newTestVerifier(t, idp, nil)

	// No preferred_username: falls back to email.
	claims := idp.standardClaims("levee")
	delete(claims, "preferred_username")
	claims["email"] = "alice@example.com"
	id, err := v.Verify(context.Background(), idp.sign(t, claims))
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", id.Subject)

	// Neither claim: falls back to sub.
	claims = idp.standardClaims("levee")
	delete(claims, "preferred_username")
	id, err = v.Verify(context.Background(), idp.sign(t, claims))
	require.NoError(t, err)
	assert.Equal(t, "user-123", id.Subject)
}

func TestVerify_WrongAudience(t *testing.T) {
	idp := newFixtureIdP(t)
	v := newTestVerifier(t, idp, nil)
	_, err := v.Verify(context.Background(), idp.sign(t, idp.standardClaims("other-api")))
	require.Error(t, err)
	assert.ErrorContains(t, err, "audience")
}

func TestVerify_Expired(t *testing.T) {
	idp := newFixtureIdP(t)
	v := newTestVerifier(t, idp, nil)
	claims := idp.standardClaims("levee")
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	_, err := v.Verify(context.Background(), idp.sign(t, claims))
	require.Error(t, err)
	assert.ErrorContains(t, err, "expired")
}

func TestVerify_BadSignature(t *testing.T) {
	idp := newFixtureIdP(t)
	v := newTestVerifier(t, idp, nil)

	// A second key not present in the fixture JWKS.
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: otherKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), "unknown-kid"),
	)
	require.NoError(t, err)
	payload, err := json.Marshal(idp.standardClaims("levee"))
	require.NoError(t, err)
	obj, err := signer.Sign(payload)
	require.NoError(t, err)
	raw, err := obj.CompactSerialize()
	require.NoError(t, err)

	_, err = v.Verify(context.Background(), raw)
	require.Error(t, err)
}

func TestVerify_WrongIssuer(t *testing.T) {
	idp := newFixtureIdP(t)
	v := newTestVerifier(t, idp, nil)
	claims := idp.standardClaims("levee")
	claims["iss"] = "https://evil.example.com"
	_, err := v.Verify(context.Background(), idp.sign(t, claims))
	require.Error(t, err)
}

func TestVerify_DisabledVerifier(t *testing.T) {
	var v *Verifier
	_, err := v.Verify(context.Background(), "a.b.c")
	require.Error(t, err)
}

func TestLooksLikeJWT(t *testing.T) {
	assert.True(t, LooksLikeJWT("eyJhbGciOi.eyJzdWIiOi.c2ln"))
	assert.False(t, LooksLikeJWT("plain-static-token"))
	assert.False(t, LooksLikeJWT("one.dot"))
	assert.False(t, LooksLikeJWT("a.b.c.d"))
	assert.False(t, LooksLikeJWT(""))
}

func TestExtractRoles_Shapes(t *testing.T) {
	claims := map[string]any{"roles": []any{"a", "b"}}
	assert.Equal(t, []string{"a", "b"}, extractRoles(claims, "roles"))

	claims = map[string]any{"roles": "a, b ,c"}
	assert.Equal(t, []string{"a", "b", "c"}, extractRoles(claims, "roles"))

	claims = map[string]any{"roles": "single"}
	assert.Equal(t, []string{"single"}, extractRoles(claims, "roles"))

	claims = map[string]any{}
	assert.Nil(t, extractRoles(claims, "roles"))
	assert.Nil(t, extractRoles(claims, ""))
}
