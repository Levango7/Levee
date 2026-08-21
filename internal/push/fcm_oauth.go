package push

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Google OAuth2 token endpoint. We use the public token endpoint; the JWT
// grant is signed with the service-account private key.
const (
	googleOAuth2TokenEndpoint = "https://oauth2.googleapis.com/token"
	googleFCMScope            = "https://www.googleapis.com/auth/firebase.messaging"
	oauth2JWTExpiry           = 1 * time.Hour
)

// serviceAccountKey is the subset of the Google service-account JSON we need
// to mint OAuth2 access tokens. Extra fields are ignored during unmarshalling.
type serviceAccountKey struct {
	Type                    string `json:"type"`
	ProjectID               string `json:"project_id"`
	PrivateKeyID            string `json:"private_key_id"`
	PrivateKey              string `json:"private_key"`
	ClientEmail             string `json:"client_email"`
	ClientID                string `json:"client_id"`
	AuthURI                 string `json:"auth_uri"`
	TokenURI                string `json:"token_uri"`
	AuthProviderX509CertURL string `json:"auth_provider_x509_cert_url"`
	ClientX509CertURL       string `json:"client_x509_cert_url"`
}

// getAccessToken mints an OAuth2 access token for the FCM scope using the
// service-account JWT-bearer grant flow. The token is cached by the caller
// (ensureAccessToken). The function performs a single HTTP POST to the Google
// token endpoint.
func (c *FCMClient) getAccessToken() (string, time.Time, error) {
	sa, err := parseServiceAccountKey(c.serviceAccountKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("push: fcm: parse service account: %w", err)
	}
	tokenURI := sa.TokenURI
	if tokenURI == "" {
		tokenURI = googleOAuth2TokenEndpoint
	}

	assertion, err := c.buildJWTAssertion(sa, tokenURI)
	if err != nil {
		return "", time.Time{}, err
	}

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("push: fcm: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("push: fcm: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("push: fcm: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("%w: token endpoint status %d: %s",
			ErrPushFailed, resp.StatusCode, string(body))
	}

	var tokResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tokResp); err != nil {
		return "", time.Time{}, fmt.Errorf("push: fcm: parse token response: %w", err)
	}
	if tokResp.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("push: fcm: empty access token in response")
	}
	expiresIn := time.Duration(tokResp.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = fcmTokenTTL
	}
	return tokResp.AccessToken, time.Now().Add(expiresIn), nil
}

// buildJWTAssertion constructs a signed JWT for the JWT-bearer grant. The
// header is RS256; the payload carries the standard claims (iss, scope, aud,
// iat, exp).
func (c *FCMClient) buildJWTAssertion(sa *serviceAccountKey, tokenURI string) (string, error) {
	key, err := parseRSAPrivateKey([]byte(sa.PrivateKey))
	if err != nil {
		return "", fmt.Errorf("push: fcm: parse rsa key: %w", err)
	}
	now := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": sa.PrivateKeyID}
	payload := map[string]any{
		"iss":   sa.ClientEmail,
		"scope": googleFCMScope,
		"aud":   tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(oauth2JWTExpiry).Unix(),
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("push: fcm: marshal jwt header: %w", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("push: fcm: marshal jwt payload: %w", err)
	}
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := headerB64 + "." + payloadB64

	// RS256 signs the SHA-256 hash of the signing input.
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("push: fcm: sign jwt: %w", err)
	}
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + sigB64, nil
}

// parseServiceAccountKey unmarshals the Google service-account JSON.
func parseServiceAccountKey(raw []byte) (*serviceAccountKey, error) {
	var sa serviceAccountKey
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if sa.PrivateKey == "" {
		return nil, errors.New("missing private_key")
	}
	if sa.ClientEmail == "" {
		return nil, errors.New("missing client_email")
	}
	return &sa, nil
}

// parseRSAPrivateKey decodes a PEM-encoded RSA private key (PKCS1 or PKCS8).
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, fmt.Errorf("PKCS8 key is %T, not *rsa.PrivateKey", key)
	}
	return nil, errors.New("PEM block is neither PKCS1 nor PKCS8 RSA")
}

// WithServiceAccountKeyForTest replaces the service-account key at runtime.
// Intended for unit tests that want to inject a synthetic key without
// re-constructing the client.
func (c *FCMClient) WithServiceAccountKeyForTest(key []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serviceAccountKey = key
}

// ServiceAccountKeyForTest exposes the configured service-account key for
// unit tests that need to inspect it.
func (c *FCMClient) ServiceAccountKeyForTest() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serviceAccountKey
}

// MakeAssertionForTest exposes the JWT assertion builder for unit tests that
// want to inspect the signed assertion without performing the token exchange.
func (c *FCMClient) MakeAssertionForTest() (string, error) {
	sa, err := parseServiceAccountKey(c.serviceAccountKey)
	if err != nil {
		return "", err
	}
	tokenURI := sa.TokenURI
	if tokenURI == "" {
		tokenURI = googleOAuth2TokenEndpoint
	}
	return c.buildJWTAssertion(sa, tokenURI)
}
