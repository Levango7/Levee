package push

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// Deep-link action constants. They appear as the path component of the
// generated URL (e.g. "levee://approve/run-123?token=xxx").
const (
	ActionApprove = "approve"
	ActionReject  = "reject"
	ActionShow    = "show"
	ActionList    = "list"
)

// DefaultDeepLinkTTL is the lifetime of one-tap approval tokens. After this
// duration the token is rejected with ErrTokenExpired.
const DefaultDeepLinkTTL = 30 * time.Minute

// deepLinkEntry is the internal record stored for each generated token.
type deepLinkEntry struct {
	runID     string
	userID    string
	action    string
	issuedAt  time.Time
	expiresAt time.Time
}

// DeepLink is a fully-resolved deep link ready to be sent to a mobile device.
type DeepLink struct {
	URL        string    `json:"url"`
	Action     string    `json:"action"`
	ResourceID string    `json:"resource_id"`
	Token      string    `json:"token"`
	Expiry     time.Time `json:"expiry"`
}

// DeepLinkGenerator builds and validates one-tap approval deep links. Tokens
// are random 32-byte strings stored in an in-memory map with a TTL. A
// generator is safe for concurrent use.
type DeepLinkGenerator struct {
	scheme  string
	baseURL string
	ttl     time.Duration

	mu     sync.Mutex
	tokens map[string]deepLinkEntry
}

// NewDeepLinkGenerator builds a generator with the default TTL.
//
//   - scheme:  URL scheme, e.g. "levee".
//   - baseURL: fallback web URL used for non-mobile clients, e.g.
//     "https://levee.example.com". May be empty when not needed.
func NewDeepLinkGenerator(scheme, baseURL string) *DeepLinkGenerator {
	return &DeepLinkGenerator{
		scheme:  scheme,
		baseURL: baseURL,
		ttl:     DefaultDeepLinkTTL,
		tokens:  make(map[string]deepLinkEntry),
	}
}

// SetTTL overrides the token TTL. Intended for tests that want short-lived
// tokens; production callers should use the default.
func (g *DeepLinkGenerator) SetTTL(ttl time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ttl = ttl
}

// GenerateApprovalLink creates a one-tap approval deep link for the given run
// and user. The token is single-use: once consumed by ValidateToken it is
// removed from the in-memory store.
func (g *DeepLinkGenerator) GenerateApprovalLink(runID, userID string) (*DeepLink, error) {
	return g.generateLink(ActionApprove, runID, userID)
}

// GenerateRejectLink creates a one-tap reject deep link.
func (g *DeepLinkGenerator) GenerateRejectLink(runID, userID string) (*DeepLink, error) {
	return g.generateLink(ActionReject, runID, userID)
}

// GenerateShowLink creates a deep link that opens the change detail view. No
// per-user token is required because show links are non-mutating; we still
// issue a token so the mobile app can validate origin, but it is bound to the
// empty user id and any user may consume it.
func (g *DeepLinkGenerator) GenerateShowLink(runID string) (*DeepLink, error) {
	return g.generateLink(ActionShow, runID, "")
}

// ValidateToken consumes a one-tap token and returns the bound (runID, userID,
// action). A token may be consumed only once; a second call returns
// ErrInvalidToken. Expired tokens return ErrTokenExpired. Unknown tokens
// return ErrInvalidToken.
func (g *DeepLinkGenerator) ValidateToken(token string) (runID, userID, action string, err error) {
	if token == "" {
		return "", "", "", ErrInvalidToken
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	entry, ok := g.tokens[token]
	if !ok {
		return "", "", "", ErrInvalidToken
	}
	if time.Now().After(entry.expiresAt) {
		delete(g.tokens, token)
		return "", "", "", ErrTokenExpired
	}
	// Single-use: remove the token after a successful read.
	delete(g.tokens, token)
	log.Info("push: deeplink token consumed",
		"run_id", entry.runID, "user_id", entry.userID, "action", entry.action)
	return entry.runID, entry.userID, entry.action, nil
}

// GenerateToken returns a fresh 32-byte hex-encoded random string. It is
// exposed so callers can mint ad-hoc tokens (e.g. for non-approval flows)
// without going through the action-specific helpers. The token is not stored
// by this call; storage is the caller's responsibility.
func (g *DeepLinkGenerator) GenerateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a timestamp-based token. rand.Read failing is
		// extraordinary; we still want a non-empty unique-ish string.
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// CleanupExpired removes all expired tokens from the in-memory store and
// returns the number removed. Intended to be called periodically by a
// background goroutine; not strictly required for correctness because
// ValidateToken also garbage-collects on read.
func (g *DeepLinkGenerator) CleanupExpired() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	removed := 0
	for tok, entry := range g.tokens {
		if now.After(entry.expiresAt) {
			delete(g.tokens, tok)
			removed++
		}
	}
	return removed
}

// --- internal helpers -------------------------------------------------------

// generateLink is the shared body for the action-specific helpers. It mints a
// token, stores the entry and builds the URL.
func (g *DeepLinkGenerator) generateLink(action, runID, userID string) (*DeepLink, error) {
	if runID == "" {
		return nil, fmt.Errorf("push: deeplink: empty run id")
	}
	if action != ActionShow && userID == "" {
		return nil, fmt.Errorf("push: deeplink: empty user id for action %q", action)
	}

	token := g.GenerateToken()
	now := time.Now()
	exp := now.Add(g.currentTTL())

	g.mu.Lock()
	g.tokens[token] = deepLinkEntry{
		runID:     runID,
		userID:    userID,
		action:    action,
		issuedAt:  now,
		expiresAt: exp,
	}
	g.mu.Unlock()

	link := &DeepLink{
		Action:     action,
		ResourceID: runID,
		Token:      token,
		Expiry:     exp,
		URL:        g.buildURL(action, runID, token),
	}
	log.Info("push: deeplink generated",
		"action", action, "run_id", runID, "user_id", userID, "expires_at", exp)
	return link, nil
}

// currentTTL returns the configured TTL under the lock.
func (g *DeepLinkGenerator) currentTTL() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ttl
}

// buildURL constructs the deep-link URL. For action=show with a baseURL, we
// emit an https URL so that desktop / non-mobile clients can still open it.
// For other actions we emit the scheme://action/runID?token=xxx form.
func (g *DeepLinkGenerator) buildURL(action, runID, token string) string {
	q := url.Values{}
	q.Set("token", token)
	if g.baseURL != "" && action == ActionShow {
		return fmt.Sprintf("%s/changes/%s?%s",
			strings.TrimRight(g.baseURL, "/"), url.PathEscape(runID), q.Encode())
	}
	scheme := g.scheme
	if scheme == "" {
		scheme = "levee"
	}
	return fmt.Sprintf("%s://%s/%s?%s",
		scheme, action, url.PathEscape(runID), q.Encode())
}
