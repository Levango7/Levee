// session.go implements LEVEE-local session tokens for browser SSO flows.
//
// GitHub's OAuth access token is opaque: it cannot be validated without an
// api.github.com round-trip per request, which would both leak a rate budget
// and make every API call depend on GitHub's availability. Instead, after
// the one-time code exchange the gateway mints a local signed token that
// carries the verified identity, and the browser presents that token on
// subsequent requests like any other bearer credential.
//
// The token format is a compact JWS (header.payload.signature) signed with
// HS256 via go-jose — the same library the test fixtures already use, so no
// new dependency is introduced. Verification is pure HMAC, no network.
package auth

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// SessionClaims is the payload of a LEVEE session token.
type SessionClaims struct {
	Subject string   `json:"sub"`
	Roles   []string `json:"roles,omitempty"`
	// Login is the SSO provider that authenticated the subject ("github");
	// recorded for audit trail clarity.
	Login string `json:"login,omitempty"`
	// IssuedAt / Expiry are Unix seconds.
	IssuedAt int64 `json:"iat"`
	Expiry   int64 `json:"exp"`
}

// SessionTTL is how long a session token stays valid. Browser flows can
// simply re-run the SSO redirect when it expires, so there is no need for
// refresh-token machinery.
const SessionTTL = 12 * time.Hour

// SessionManager issues and verifies session tokens under one HMAC secret.
type SessionManager struct {
	key       []byte
	ephemeral bool
}

// NewSessionManager returns a manager keyed by secret. A short secret makes
// the HMAC forgeable, so it is rejected up front; an empty secret selects a
// process-random key (sessions then survive until the next restart only,
// which is acceptable for single-node deployments).
func NewSessionManager(secret string) (*SessionManager, error) {
	if secret == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("session: generate ephemeral key: %w", err)
		}
		return &SessionManager{key: key, ephemeral: true}, nil
	}
	if len(secret) < 32 {
		return nil, errors.New("session: session_secret must be at least 32 chars (or empty for an ephemeral per-process key)")
	}
	return &SessionManager{key: []byte(secret)}, nil
}

// Ephemeral reports whether the manager keys sessions with a per-process
// random secret (true) or a configured one (false).
func (m *SessionManager) Ephemeral() bool { return m != nil && m.ephemeral }

// Issue mints a session token for the given identity.
func (m *SessionManager) Issue(subject string, roles []string, login string) (string, error) {
	if m == nil {
		return "", errors.New("session: manager not configured")
	}
	return m.issueAt(subject, roles, login, time.Now())
}

// issueAt is split out so tests can mint tokens anchored at an arbitrary
// time (expired-token fixtures) without exposing the hook publicly.
func (m *SessionManager) issueAt(subject string, roles []string, login string, now time.Time) (string, error) {
	if m == nil {
		return "", errors.New("session: manager not configured")
	}
	claims := SessionClaims{
		Subject:  subject,
		Roles:    roles,
		Login:    login,
		IssuedAt: now.Unix(),
		Expiry:   now.Add(SessionTTL).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("session: encode claims: %w", err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: m.key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", fmt.Errorf("session: init signer: %w", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", fmt.Errorf("session: sign: %w", err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		return "", fmt.Errorf("session: serialize: %w", err)
	}
	return raw, nil
}

// VerifySession validates a session token's signature and expiry and returns
// its claims. Revocation is out of scope by design: tokens are short-lived
// and the only legitimate holder is the browser that completed the SSO
// exchange; a stolen token expires on its own.
func (m *SessionManager) VerifySession(raw string) (*SessionClaims, error) {
	if m == nil {
		return nil, errors.New("session: manager not configured")
	}
	obj, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return nil, fmt.Errorf("session: parse: %w", err)
	}
	if len(obj.Signatures) != 1 {
		return nil, errors.New("session: expect exactly one signature")
	}
	payload, err := obj.Verify(m.key)
	if err != nil {
		return nil, fmt.Errorf("session: bad signature: %w", err)
	}
	claims := &SessionClaims{}
	if err := json.Unmarshal(payload, claims); err != nil {
		return nil, fmt.Errorf("session: decode claims: %w", err)
	}
	if claims.Expiry < time.Now().Unix() {
		return nil, errors.New("session: token expired")
	}
	if claims.Subject == "" {
		return nil, errors.New("session: token has no subject")
	}
	return claims, nil
}
