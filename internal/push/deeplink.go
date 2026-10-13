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

// DefaultConsumedRetention is how long a spent token keeps its binding
// resolvable. A one-tap link is opened asynchronously — often long after the
// tap that settled it — and that second open still needs to name the run it
// already acted on, or the operator reads "invalid token" and taps again.
// A day covers "I'll look at it tomorrow" without growing the map forever.
const DefaultConsumedRetention = 24 * time.Hour

// deepLinkEntry is the internal record stored for each generated token.
// consumedAt is zero while the token is still live.
type deepLinkEntry struct {
	runID      string
	userID     string
	action     string
	issuedAt   time.Time
	expiresAt  time.Time
	consumedAt time.Time
}

// Binding is what a deep-link token points at, as reported by
// LookupBinding. ConsumedAt is zero while the token is still live; a spent
// token keeps its binding (and its spend time) for the retention window.
type Binding struct {
	RunID      string
	UserID     string
	Action     string
	ExpiresAt  time.Time
	ConsumedAt time.Time
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
//
// Spending a token (a successful validation, or an expiry discovered on
// read) moves its entry from tokens to consumed rather than dropping it: the
// binding stays resolvable for the retention window so a re-opened link can
// be answered with "which run did I already decide". Only a token this
// generator never issued is ever unknown.
type DeepLinkGenerator struct {
	scheme  string
	baseURL string
	ttl     time.Duration

	// consumedRetention is how long a retired entry stays resolvable. It is
	// a field, not a constant, so tests can shrink it.
	consumedRetention time.Duration

	mu       sync.Mutex
	tokens   map[string]deepLinkEntry
	consumed map[string]deepLinkEntry
}

// NewDeepLinkGenerator builds a generator with the default TTL.
//
//   - scheme:  URL scheme, e.g. "levee".
//   - baseURL: fallback web URL used for non-mobile clients, e.g.
//     "https://levee.example.com". May be empty when not needed.
func NewDeepLinkGenerator(scheme, baseURL string) *DeepLinkGenerator {
	return &DeepLinkGenerator{
		scheme:            scheme,
		baseURL:           baseURL,
		ttl:               DefaultDeepLinkTTL,
		consumedRetention: DefaultConsumedRetention,
		tokens:            make(map[string]deepLinkEntry),
		consumed:          make(map[string]deepLinkEntry),
	}
}

// SetTTL overrides the token TTL. Intended for tests that want short-lived
// tokens; production callers should use the default.
func (g *DeepLinkGenerator) SetTTL(ttl time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ttl = ttl
}

// SetConsumedRetention overrides how long a spent token keeps its binding
// resolvable. Intended for tests; production callers keep the default.
func (g *DeepLinkGenerator) SetConsumedRetention(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.consumedRetention = d
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
//
// Both failure paths still retire the entry into the consumed map, so the
// binding stays resolvable through LookupBinding: an expired token that was
// already approved through another channel has a real answer ("run X, decided
// by Y"), while a token this generator never issued stays unknown.
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
	delete(g.tokens, token)
	if time.Now().After(entry.expiresAt) {
		// Expired before it was ever tapped: no spend time to report, but the
		// binding still answers "which run did this link point at".
		g.retire(token, entry)
		return "", "", "", ErrTokenExpired
	}
	entry.consumedAt = time.Now()
	g.retire(token, entry)
	log.Info("push: deeplink token consumed",
		"run_id", entry.runID, "user_id", entry.userID, "action", entry.action)
	return entry.runID, entry.userID, entry.action, nil
}

// LookupBinding reports the binding of a token without mutating anything.
// Live and already-spent tokens both bind; only a token this generator never
// issued — or one whose retention has elapsed — is unknown. It is the read
// side of ValidateToken, and it exists so a client that re-opens a spent link
// can be told which run it already acted on instead of just "no".
func (g *DeepLinkGenerator) LookupBinding(token string) (Binding, bool) {
	if token == "" {
		return Binding{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if entry, ok := g.tokens[token]; ok {
		return entry.binding(), true
	}
	entry, ok := g.consumed[token]
	if !ok {
		return Binding{}, false
	}
	if g.retentionElapsed(entry) {
		// Retention elapsed: the link is older than any useful answer.
		delete(g.consumed, token)
		return Binding{}, false
	}
	return entry.binding(), true
}

// retentionElapsed reports whether a retired entry is too old to still name
// its run: later than the retention window measured from when the entry
// became inert. The caller must hold g.mu.
func (g *DeepLinkGenerator) retentionElapsed(entry deepLinkEntry) bool {
	return time.Now().After(entry.spentAt().Add(g.consumedRetention))
}

// retire moves a spent token's entry into the consumed map. The caller must
// hold g.mu.
func (g *DeepLinkGenerator) retire(token string, entry deepLinkEntry) {
	g.consumed[token] = entry
}

// GenerateToken returns a fresh 32-byte hex-encoded random string. It is
// exposed so callers can mint ad-hoc tokens (e.g. for non-approval flows)
// without going through the action-specific helpers. The token is not stored
// by this call; storage is the caller's responsibility. Deeplink tokens are
// one-time authorization credentials, so a crypto/rand failure is returned
// as an error — never a timestamp-derived fallback (SA-012).
func (g *DeepLinkGenerator) GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("push: deeplink: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// CleanupExpired removes all expired tokens from the in-memory store and
// returns the number removed. Intended to be called periodically by a
// background goroutine; not strictly required for correctness because
// ValidateToken also garbage-collects on read. Spent tokens are swept on the
// same pass once their retention (own TTL plus the consumed window) has
// elapsed, so the consumed map stays bounded by the links of roughly the last
// day rather than growing for the life of the process.
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
	for tok, entry := range g.consumed {
		if g.retentionElapsed(entry) {
			delete(g.consumed, tok)
			removed++
		}
	}
	return removed
}

// --- internal helpers -------------------------------------------------------

// binding projects the entry into its exportable form.
func (e deepLinkEntry) binding() Binding {
	return Binding{
		RunID:      e.runID,
		UserID:     e.userID,
		Action:     e.action,
		ExpiresAt:  e.expiresAt,
		ConsumedAt: e.consumedAt,
	}
}

// spentAt is when the entry became inert: the tap that consumed it, or its
// own expiry when it aged out before anyone tapped it.
func (e deepLinkEntry) spentAt() time.Time {
	if !e.consumedAt.IsZero() {
		return e.consumedAt
	}
	return e.expiresAt
}

// generateLink is the shared body for the action-specific helpers. It mints a
// token, stores the entry and builds the URL.
func (g *DeepLinkGenerator) generateLink(action, runID, userID string) (*DeepLink, error) {
	if runID == "" {
		return nil, fmt.Errorf("push: deeplink: empty run id")
	}
	if action != ActionShow && userID == "" {
		return nil, fmt.Errorf("push: deeplink: empty user id for action %q", action)
	}

	token, err := g.GenerateToken()
	if err != nil {
		return nil, err
	}
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
