package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// FCM endpoint constants. The HTTP v1 API is used (not the legacy API), which
// requires an OAuth2 access token derived from a service-account key.
const (
	FCMEndpointFormat = "https://fcm.googleapis.com/v1/projects/%s/messages:send"

	// fcmTokenTTL is the lifetime of the OAuth2 access token. Google service
	// account tokens last 1h; we refresh 5 minutes early to avoid edge cases.
	fcmTokenTTL = 55 * time.Minute

	// fcmRequestTimeout caps the per-request HTTP timeout.
	fcmRequestTimeout = 30 * time.Second
)

// FCMClient sends messages to Firebase Cloud Messaging over the HTTP v1 API
// using an OAuth2 access token derived from a service-account key. The token
// is cached and refreshed automatically. A client is safe for concurrent use.
type FCMClient struct {
	projectID         string
	serviceAccountKey []byte
	endpoint          string
	httpClient        *http.Client

	// OAuth2 token cache. Protected by mu.
	mu          sync.Mutex
	authToken   string
	tokenExpiry time.Time
}

// FCMMessage is the payload sent to a single Android / iOS device via FCM.
// Token is the registration token; Notification carries the visible title/body;
// Data is delivered to the app; Android and APNS optionally carry platform
// specific overrides.
type FCMMessage struct {
	Token        string            `json:"token,omitempty"`
	Notification *FCMNotification  `json:"notification,omitempty"`
	Data         map[string]string `json:"data,omitempty"`
	Android      *AndroidConfig    `json:"android,omitempty"`
	APNS         *APNSConfig       `json:"apns,omitempty"`
}

// FCMNotification is the visible alert shown by the FCM SDK.
type FCMNotification struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

// AndroidConfig carries Android-specific overrides such as priority and
// notification channel. It is a thin subset of the FCM HTTP v1 schema.
type AndroidConfig struct {
	Priority    string `json:"priority,omitempty"` // "normal" or "high"
	ClickAction string `json:"click_action,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
}

// APNSConfig carries iOS-specific overrides via FCM. We only expose the
// payload-as-string form used by the FCM v1 API.
type APNSConfig struct {
	Payload string `json:"payload,omitempty"` // raw JSON string per FCM v1
}

// NewFCMClient builds an FCMClient from the Google service-account credentials.
//
//   - projectID:         GCP project id that owns the FCM resource.
//   - serviceAccountKey: JSON contents of the service-account key file.
//
// The key is validated lazily on first Send to avoid network I/O at construction
// time. The returned client is ready to use.
func NewFCMClient(projectID string, serviceAccountKey []byte) *FCMClient {
	return &FCMClient{
		projectID:         projectID,
		serviceAccountKey: serviceAccountKey,
		endpoint:          fmt.Sprintf(FCMEndpointFormat, projectID),
		httpClient:        &http.Client{Timeout: fcmRequestTimeout},
	}
}

// Send delivers a single message to FCM. It refreshes the OAuth2 access token
// when needed, serialises the message and POSTs to the v1 endpoint. A non-2xx
// response is converted to ErrPushFailed with the FCM error detail.
func (c *FCMClient) Send(ctx context.Context, msg FCMMessage) error {
	if msg.Token == "" {
		return ErrEmptyDeviceToken
	}

	// The FCM v1 endpoint expects {"message": {...}}.
	body, err := json.Marshal(map[string]any{"message": msg})
	if err != nil {
		return fmt.Errorf("push: fcm: marshal message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("push: fcm: build request: %w", err)
	}
	tok, err := c.ensureAccessToken()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("push: fcm: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return c.handleResponse(resp)
}

// SendBatch delivers a slice of messages sequentially and returns a parallel
// slice of errors. A nil entry means the corresponding message was delivered
// successfully.
func (c *FCMClient) SendBatch(ctx context.Context, msgs []FCMMessage) []error {
	errs := make([]error, len(msgs))
	for i, m := range msgs {
		errs[i] = c.Send(ctx, m)
	}
	return errs
}

// Close releases any resources held by the client. Currently a no-op.
func (c *FCMClient) Close() error { return nil }

// --- internal helpers -------------------------------------------------------

// ensureAccessToken returns a valid OAuth2 access token, refreshing it when
// the cached value is missing or near expiry. The lock makes the refresh
// single-flight.
func (c *FCMClient) ensureAccessToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authToken != "" && time.Now().Add(5*time.Minute).Before(c.tokenExpiry) {
		return c.authToken, nil
	}
	tok, exp, err := c.getAccessToken()
	if err != nil {
		return "", err
	}
	c.authToken = tok
	c.tokenExpiry = exp
	return tok, nil
}

// handleResponse inspects the FCM HTTP response. A 2xx is success; anything
// else carries a JSON body with an "error" object that we attach to
// ErrPushFailed.
func (c *FCMClient) handleResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	var fcmErr struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &fcmErr)
	}
	detail := fcmErr.Error.Message
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	log.Warn("push: fcm: non-2xx response",
		"status", resp.StatusCode, "detail", detail)
	return fmt.Errorf("%w: fcm status %d: %s", ErrPushFailed, resp.StatusCode, detail)
}

// SetEndpointForTest overrides the FCM endpoint. It is intended only for unit
// tests that point the client at a httptest.Server. Calling it after Send has
// been invoked may race with token refresh; tests should call it once at setup.
func (c *FCMClient) SetEndpointForTest(endpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endpoint = endpoint
}

// SetHTTPClientForTest overrides the underlying http.Client. Intended for unit
// tests that want to inject a custom transport (e.g. to point at an in-process
// httptest.Server with TLS).
func (c *FCMClient) SetHTTPClientForTest(hc *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.httpClient = hc
}

// SetAccessTokenForTest injects a precomputed access token and expiry. Intended
// for unit tests that want to bypass the OAuth2 dance.
func (c *FCMClient) SetAccessTokenForTest(token string, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authToken = token
	c.tokenExpiry = expiry
}
