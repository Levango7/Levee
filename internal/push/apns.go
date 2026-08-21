package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// APNs endpoint hosts. The development host is used for sandbox builds; the
// production host is used for App Store / TestFlight builds.
const (
	APNsEndpointDevelopment = "https://api.development.push.apple.com"
	APNsEndpointProduction  = "https://api.push.apple.com"

	// apnsTokenTTL is the maximum lifetime of a provider JWT per Apple docs.
	// We refresh well before the documented 1h ceiling to avoid edge cases.
	apnsTokenTTL = 30 * time.Minute

	// apnsRequestTimeout is the per-request HTTP timeout. APNs is a real-time
	// service; long waits indicate a problem.
	apnsRequestTimeout = 30 * time.Second
)

// APNSClient sends notifications to Apple Push Notification Service over HTTP/2
// using a provider JWT signed with an ES256 key. The JWT is cached and refreshed
// automatically before expiry. A client is safe for concurrent use.
type APNSClient struct {
	teamID     string
	keyID      string
	privateKey *ecdsa.PrivateKey
	bundleID   string
	endpoint   string
	httpClient *http.Client

	// JWT cache. Protected by mu.
	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// APNSNotification is the payload sent to a single iOS device. The Alert,
// Badge, Sound and Category fields map directly to the APNs alert dictionary;
// CustomData is serialised under a top-level "data" key for app-side handling.
type APNSNotification struct {
	// DeviceToken is the hex-encoded APNs device token.
	DeviceToken string `json:"device_token"`
	// Alert is the visible notification body.
	Alert APNSAlert `json:"alert"`
	// Badge is the home-screen badge counter.
	Badge int `json:"badge,omitempty"`
	// Sound is the sound file name (default "default").
	Sound string `json:"sound,omitempty"`
	// Category identifies the interactive notification category registered by
	// the app, used to display approve / reject buttons.
	Category string `json:"category,omitempty"`
	// CustomData carries arbitrary key-value pairs delivered to the app.
	CustomData map[string]interface{} `json:"custom_data,omitempty"`
}

// APNSAlert is the visible title/body pair shown in the notification banner.
type APNSAlert struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

// apnsPayload is the wire-level JSON sent to APNs. It mirrors the structure
// documented at https://developer.apple.com/documentation/usernotifications/
// setting-up-a-remote-notification-server/generating-a-remote-notification.
type apnsPayload struct {
	APS struct {
		Alert    APNSAlert `json:"alert"`
		Badge    int       `json:"badge,omitempty"`
		Sound    string    `json:"sound,omitempty"`
		Category string    `json:"category,omitempty"`
	} `json:"aps"`
}

// NewAPNSClient builds an APNSClient from the Apple developer credentials.
//
//   - teamID:     Apple Developer team identifier (10 chars).
//   - keyID:      Identifier of the .p8 signing key (10 chars).
//   - bundleID:   App bundle identifier (e.g. com.example.levee).
//   - privateKey: PEM-encoded .p8 private key contents.
//   - production: Selects the production endpoint when true, development otherwise.
//
// Returns an error when the private key cannot be parsed as a PEM-encoded ECDSA
// P-256 key. The returned client is ready to use; no network I/O happens at
// construction time.
func NewAPNSClient(teamID, keyID, bundleID string, privateKey []byte, production bool) (*APNSClient, error) {
	if teamID == "" || keyID == "" || bundleID == "" {
		return nil, fmt.Errorf("push: apns: teamID, keyID and bundleID are required")
	}
	key, err := parseECDSAPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("push: apns: parse private key: %w", err)
	}
	endpoint := APNsEndpointDevelopment
	if production {
		endpoint = APNsEndpointProduction
	}
	return &APNSClient{
		teamID:     teamID,
		keyID:      keyID,
		privateKey: key,
		bundleID:   bundleID,
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: apnsRequestTimeout},
	}, nil
}

// Send delivers a single notification to APNs. It refreshes the provider JWT
// when needed, builds the JSON payload and POSTs to /3/device/<token>. A non-2xx
// response is converted to ErrPushFailed with the APNs reason string.
func (c *APNSClient) Send(ctx context.Context, notif APNSNotification) error {
	if notif.DeviceToken == "" {
		return ErrEmptyDeviceToken
	}
	body, err := c.marshalPayload(notif)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.deviceURL(notif.DeviceToken), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("push: apns: build request: %w", err)
	}
	if err := c.setAuthHeaders(req); err != nil {
		return err
	}
	req.Header.Set("apns-topic", c.bundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("push: apns: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return c.handleResponse(resp)
}

// SendBatch delivers a slice of notifications sequentially and returns a
// parallel slice of errors. A nil entry means the corresponding notification
// was delivered successfully. The sequential strategy keeps error attribution
// trivial and matches APNs' recommendation against high-throughput batch APIs.
func (c *APNSClient) SendBatch(ctx context.Context, notifs []APNSNotification) []error {
	errs := make([]error, len(notifs))
	for i, n := range notifs {
		errs[i] = c.Send(ctx, n)
	}
	return errs
}

// Close releases any resources held by the client. The standard http.Client
// has no Close method, so this is currently a no-op retained for future
// transport-level pooling and for satisfying io.Closer at call sites.
func (c *APNSClient) Close() error { return nil }

// --- internal helpers -------------------------------------------------------

// deviceURL builds the per-device POST URL.
func (c *APNSClient) deviceURL(token string) string {
	return fmt.Sprintf("%s/3/device/%s", c.endpoint, token)
}

// marshalPayload serialises the public APNSNotification into the APNs wire
// format, merging CustomData at the top level alongside "aps".
func (c *APNSClient) marshalPayload(notif APNSNotification) ([]byte, error) {
	p := apnsPayload{}
	p.APS.Alert = notif.Alert
	p.APS.Badge = notif.Badge
	p.APS.Sound = notif.Sound
	p.APS.Category = notif.Category

	// We marshal the aps struct first, then merge custom data at the top level
	// so the final document is {"aps": {...}, "run_id": "..."}. Doing it in
	// two steps avoids defining an extra exported type with a custom JSON
	// marshaller.
	apsBytes, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("push: apns: marshal aps: %w", err)
	}
	if len(notif.CustomData) == 0 {
		return apsBytes, nil
	}
	var combined map[string]json.RawMessage
	if err := json.Unmarshal(apsBytes, &combined); err != nil {
		return nil, fmt.Errorf("push: apns: reparse aps: %w", err)
	}
	for k, v := range notif.CustomData {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("push: apns: marshal custom %q: %w", k, err)
		}
		combined[k] = raw
	}
	return json.Marshal(combined)
}

// setAuthHeaders refreshes the cached JWT if needed and sets the
// "authorization: bearer <jwt>" header on the request.
func (c *APNSClient) setAuthHeaders(req *http.Request) error {
	tok, err := c.ensureToken()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "bearer "+tok)
	return nil
}

// ensureToken returns a valid JWT, refreshing it when the cached value is
// missing or within 5 minutes of expiry. The lock makes the refresh
// single-flight: concurrent senders reuse the same token.
func (c *APNSClient) ensureToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Add(5*time.Minute).Before(c.tokenExpiry) {
		return c.token, nil
	}
	tok, exp, err := c.generateToken()
	if err != nil {
		return "", err
	}
	c.token = tok
	c.tokenExpiry = exp
	return tok, nil
}

// handleResponse inspects the APNs HTTP response. A 2xx is success; anything
// else carries a JSON body with a "reason" field that we attach to ErrPushFailed.
func (c *APNSClient) handleResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	var apnsErr struct {
		Reason string `json:"reason"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &apnsErr)
	}
	reason := apnsErr.Reason
	if reason == "" {
		reason = strings.TrimSpace(string(body))
	}
	log.Warn("push: apns: non-2xx response",
		"status", resp.StatusCode, "reason", reason)
	return fmt.Errorf("%w: apns status %d: %s", ErrPushFailed, resp.StatusCode, reason)
}

// parseECDSAPrivateKey decodes a PEM-encoded ECDSA P-256 private key as
// exported by the Apple developer portal. It accepts both "EC PRIVATE KEY"
// (SEC1) and "PRIVATE KEY" (PKCS8) PEM blocks.
func parseECDSAPrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	// Try PKCS8 first, then SEC1.
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if ecKey, ok := key.(*ecdsa.PrivateKey); ok {
			return ecKey, nil
		}
		return nil, fmt.Errorf("PKCS8 key is %T, not *ecdsa.PrivateKey", key)
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New("PEM block is neither PKCS8 nor SEC1 ECDSA")
}
