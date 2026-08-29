// session_test.go covers the LEVEE-local session token manager: key
// validation, issue/verify round-trip, expiry, tampering and cross-manager
// isolation.
package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSessionManager_SecretRules(t *testing.T) {
	// Ephemeral key when no secret is configured.
	m, err := NewSessionManager("")
	require.NoError(t, err)
	assert.True(t, m.Ephemeral())

	// Too-short configured secret is rejected (would be forgeable).
	_, err = NewSessionManager("short")
	require.Error(t, err)
	assert.ErrorContains(t, err, "at least 32")

	// A real secret (GitHub secrets are 40 hex chars) is accepted.
	m, err = NewSessionManager(strings.Repeat("a", 40))
	require.NoError(t, err)
	assert.False(t, m.Ephemeral())
}

func TestSessionIssueVerify_RoundTrip(t *testing.T) {
	m, err := NewSessionManager(strings.Repeat("k", 32))
	require.NoError(t, err)

	raw, err := m.Issue("octocat", []string{"operator"}, "github")
	require.NoError(t, err)

	claims, err := m.VerifySession(raw)
	require.NoError(t, err)
	assert.Equal(t, "octocat", claims.Subject)
	assert.Equal(t, []string{"operator"}, claims.Roles)
	assert.Equal(t, "github", claims.Login)
	assert.True(t, claims.Expiry > time.Now().Unix())
}

func TestSessionVerify_Expired(t *testing.T) {
	m, err := NewSessionManager(strings.Repeat("k", 32))
	require.NoError(t, err)

	// Mint a token whose TTL started 13h ago: beyond SessionTTL.
	raw, err := m.issueAt("octocat", nil, "github", time.Now().Add(-13*time.Hour))
	require.NoError(t, err)
	_, err = m.VerifySession(raw)
	require.Error(t, err)
	assert.ErrorContains(t, err, "expired")
}

func TestSessionVerify_Tampered(t *testing.T) {
	m, err := NewSessionManager(strings.Repeat("k", 32))
	require.NoError(t, err)
	raw, err := m.Issue("octocat", nil, "github")
	require.NoError(t, err)

	// Flip a payload byte (middle segment) — signature must fail.
	parts := strings.Split(raw, ".")
	runes := []rune(parts[1])
	if runes[0] == 'A' {
		runes[0] = 'B'
	} else {
		runes[0] = 'A'
	}
	tampered := parts[0] + "." + string(runes) + "." + parts[2]
	_, err = m.VerifySession(tampered)
	require.Error(t, err)
}

func TestSessionVerify_WrongKey(t *testing.T) {
	m1, err := NewSessionManager(strings.Repeat("k", 32))
	require.NoError(t, err)
	m2, err := NewSessionManager(strings.Repeat("j", 32))
	require.NoError(t, err)

	raw, err := m1.Issue("octocat", nil, "github")
	require.NoError(t, err)
	_, err = m2.VerifySession(raw)
	require.Error(t, err, "a token signed by another secret must not verify")
}

func TestSessionManager_NilSafe(t *testing.T) {
	var m *SessionManager
	_, err := m.Issue("x", nil, "github")
	require.Error(t, err)
	_, err = m.VerifySession("a.b.c")
	require.Error(t, err)
	assert.False(t, m.Ephemeral())
}
