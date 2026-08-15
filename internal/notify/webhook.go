// webhook.go implements the WebhookNotifier channel. It delivers notifications
// over HTTP POST with optional HMAC-SHA256 signing and automatic retries on
// transient failures. WebhookNotifier implements the Notifier interface and
// can be registered with NotificationManager.

package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrEmptyURL is returned when a webhook notifier is constructed without a
	// target URL.
	ErrEmptyURL = errors.New("notify: empty webhook url")
	// ErrEmptyName is returned when a webhook notifier is constructed without a
	// channel name.
	ErrEmptyName = errors.New("notify: empty webhook name")
	// ErrWebhookFailed is the generic sentinel for a failed webhook request.
	ErrWebhookFailed = errors.New("notify: webhook request failed")
	// ErrNon2xxResponse is returned when the webhook endpoint responds with a
	// non-2xx status code.
	ErrNon2xxResponse = errors.New("notify: non-2xx response")
	// ErrMaxRetries is returned when all retry attempts have been exhausted
	// without success.
	ErrMaxRetries = errors.New("notify: max retries exceeded")
)

// --- Constants --------------------------------------------------------------

const (
	// HeaderSignature is the HTTP header carrying the HMAC signature.
	HeaderSignature = "X-Levee-Signature"
	// HeaderEvent is the HTTP header carrying the triggering event name.
	HeaderEvent = "X-Levee-Event"
	// HeaderContentType is the HTTP content-type header name.
	HeaderContentType = "Content-Type"
	// ContentTypeJSON is the JSON content type used for webhook payloads.
	ContentTypeJSON = "application/json"
	// SignaturePrefix is the prefix of the signature header value, yielding
	// "sha256=<hex>".
	SignaturePrefix = "sha256="

	// defaultWebhookMaxRetries is the default number of retry attempts.
	defaultWebhookMaxRetries = 3
	// defaultWebhookRetryDelay is the default delay between retry attempts.
	defaultWebhookRetryDelay = 1 * time.Second
	// defaultWebhookTimeout is the default per-request timeout.
	defaultWebhookTimeout = 10 * time.Second
)

// --- Config & Notifier ------------------------------------------------------

// WebhookConfig is the configuration for a WebhookNotifier.
type WebhookConfig struct {
	Name       string        // channel name (required)
	URL        string        // webhook URL (required)
	Secret     string        // HMAC-SHA256 signing secret (optional)
	MaxRetries int           // max retry attempts after the initial request
	RetryDelay time.Duration // delay between retry attempts
	Timeout    time.Duration // per-request timeout
}

// WebhookNotifier delivers notifications over HTTP POST to a fixed URL. It
// implements the Notifier interface and is safe for concurrent use: each Send
// call is independent and the underlying http.Client is goroutine-safe.
type WebhookNotifier struct {
	name       string
	url        string
	secret     string
	client     *http.Client
	maxRetries int
	retryDelay time.Duration
	timeout    time.Duration
}

// NewWebhookNotifier constructs a WebhookNotifier from the given config. It
// validates that both Name and URL are non-empty and applies defaults for
// MaxRetries, RetryDelay and Timeout when they are not set (zero or negative).
func NewWebhookNotifier(cfg WebhookConfig) (*WebhookNotifier, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("notify: new webhook: %w", ErrEmptyName)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("notify: new webhook: %w", ErrEmptyURL)
	}

	// Zero values fall back to the documented defaults. This follows the
	// common Go config idiom where an unset field means "use the default".
	// Callers that want to disable retries can set MaxRetries to a negative
	// value, which is clamped to zero.
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = defaultWebhookMaxRetries
	} else if maxRetries < 0 {
		maxRetries = 0
	}
	retryDelay := cfg.RetryDelay
	if retryDelay <= 0 {
		retryDelay = defaultWebhookRetryDelay
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultWebhookTimeout
	}

	return &WebhookNotifier{
		name:       cfg.Name,
		url:        cfg.URL,
		secret:     cfg.Secret,
		client:     &http.Client{Timeout: timeout},
		maxRetries: maxRetries,
		retryDelay: retryDelay,
		timeout:    timeout,
	}, nil
}

// Name returns the channel name, satisfying the Notifier interface.
func (w *WebhookNotifier) Name() string {
	return w.name
}

// computeSignature returns the hex-encoded HMAC-SHA256 of payload keyed by
// secret. It is deterministic and allocation-light.
func computeSignature(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// Send delivers msg to the configured webhook URL. The message is serialized to
// JSON and POSTed with the appropriate headers. When a signing secret is
// configured the HMAC-SHA256 of the payload is sent in the X-Levee-Signature
// header. On failure the request is retried up to MaxRetries times with a fixed
// RetryDelay between attempts. The context is honoured: cancellation or
// deadline expiry stops retrying immediately.
func (w *WebhookNotifier) Send(ctx context.Context, msg Message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("notify: webhook marshal: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= w.maxRetries; attempt++ {
		// Before every retry (attempt > 0) wait for retryDelay while
		// respecting context cancellation.
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("notify: webhook send: %w", ctx.Err())
			case <-time.After(w.retryDelay):
			}
		}

		lastErr = w.doRequest(ctx, payload, msg.Event)
		if lastErr == nil {
			if attempt > 0 {
				log.Info("notify: webhook recovered",
					"notifier", w.name,
					"attempt", attempt)
			}
			return nil
		}

		log.Warn("notify: webhook attempt failed",
			"notifier", w.name,
			"attempt", attempt,
			"err", lastErr)

		// If the context is already cancelled there is no point retrying.
		if ctx.Err() != nil {
			return fmt.Errorf("notify: webhook send: %w", ctx.Err())
		}
	}

	// All attempts exhausted. Wrap both ErrMaxRetries and the last concrete
	// error so callers can use errors.Is to detect either condition.
	return fmt.Errorf("notify: webhook %q: %w (last: %w)",
		w.name, ErrMaxRetries, lastErr)
}

// doRequest performs a single HTTP POST. It returns nil on a 2xx response and a
// wrapped sentinel error otherwise. The response body is fully drained and
// closed to allow underlying connection reuse.
func (w *WebhookNotifier) doRequest(ctx context.Context, payload []byte, event string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("notify: webhook build request: %w", err)
	}

	req.Header.Set(HeaderContentType, ContentTypeJSON)
	req.Header.Set(HeaderEvent, event)
	if w.secret != "" {
		req.Header.Set(HeaderSignature, SignaturePrefix+computeSignature(w.secret, payload))
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: webhook do: %w", err)
	}
	defer resp.Body.Close()
	// Drain the body so the connection can be reused by the keep-alive pool.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("notify: webhook status %d: %w", resp.StatusCode, ErrNon2xxResponse)
	}
	return nil
}
