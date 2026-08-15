package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestWebhook builds a WebhookNotifier pointing at srv.URL with fast retry
// settings suitable for tests.
func newTestWebhook(t *testing.T, name, url, secret string, maxRetries int, retryDelay, timeout time.Duration) *WebhookNotifier {
	t.Helper()
	w, err := NewWebhookNotifier(WebhookConfig{
		Name:       name,
		URL:        url,
		Secret:     secret,
		MaxRetries: maxRetries,
		RetryDelay: retryDelay,
		Timeout:    timeout,
	})
	require.NoError(t, err)
	return w
}

// drainBody reads and discards the request body so the server can detect
// connection reuse issues. It is used in handlers that do not need the body.
func drainBody(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
}

// --- Construction ----------------------------------------------------------

// TestNewWebhookNotifier_Validation verifies that empty Name and URL are
// rejected with the appropriate sentinel errors.
func TestNewWebhookNotifier_Validation(t *testing.T) {
	_, err := NewWebhookNotifier(WebhookConfig{Name: "", URL: "http://example.com/hook"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmptyName), "want ErrEmptyName, got %v", err)

	_, err = NewWebhookNotifier(WebhookConfig{Name: "hook", URL: ""})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmptyURL), "want ErrEmptyURL, got %v", err)
}

// TestNewWebhookNotifier_Defaults verifies that zero/ negative config values
// fall back to the documented defaults.
func TestNewWebhookNotifier_Defaults(t *testing.T) {
	w, err := NewWebhookNotifier(WebhookConfig{Name: "d", URL: "http://x"})
	require.NoError(t, err)
	assert.Equal(t, defaultWebhookMaxRetries, w.maxRetries)
	assert.Equal(t, defaultWebhookRetryDelay, w.retryDelay)
	assert.Equal(t, defaultWebhookTimeout, w.timeout)
	assert.NotNil(t, w.client)
}

// --- Name ------------------------------------------------------------------

// TestWebhookName verifies that Name returns the configured channel name.
func TestWebhookName(t *testing.T) {
	w := newTestWebhook(t, "my-hook", "http://example.com/hook", "", 0, 0, 0)
	assert.Equal(t, "my-hook", w.Name())
}

// --- Send success ----------------------------------------------------------

// TestWebhookSend_Success verifies a happy-path POST: the method, path, JSON
// body, Content-Type and X-Levee-Event headers are all correct.
func TestWebhookSend_Success(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   []byte
		gotCT     string
		gotEvent  string
		gotSig    string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		gotCT = r.Header.Get(HeaderContentType)
		gotEvent = r.Header.Get(HeaderEvent)
		gotSig = r.Header.Get(HeaderSignature)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := newTestWebhook(t, "test", srv.URL, "", 0, 10*time.Millisecond, 5*time.Second)
	msg := validMessage()

	require.NoError(t, wh.Send(context.Background(), msg))

	// 1. method & path
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/", gotPath)

	// 10. body is valid JSON that round-trips to the same Message
	var decoded Message
	require.NoError(t, json.Unmarshal(gotBody, &decoded))
	assert.Equal(t, msg.ID, decoded.ID)
	assert.Equal(t, msg.Event, decoded.Event)
	assert.Equal(t, msg.RunID, decoded.RunID)
	assert.Equal(t, msg.Title, decoded.Title)
	assert.Equal(t, msg.Body, decoded.Body)

	// 11. X-Levee-Event header carries the event
	assert.Equal(t, msg.Event, gotEvent)

	// 12. Content-Type header
	assert.Equal(t, ContentTypeJSON, gotCT)

	// 3. no secret => no signature header
	assert.Empty(t, gotSig)
}

// --- Signing ---------------------------------------------------------------

// TestWebhookSend_WithSignature verifies that when a secret is configured the
// X-Levee-Signature header carries the correct "sha256=<hex>" HMAC.
func TestWebhookSend_WithSignature(t *testing.T) {
	var (
		gotSig  string
		gotBody []byte
	)
	secret := "topsecret"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get(HeaderSignature)
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := newTestWebhook(t, "signed", srv.URL, secret, 0, 0, 0)
	require.NoError(t, wh.Send(context.Background(), validMessage()))

	// Recompute the expected signature over the exact bytes the server saw.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	want := SignaturePrefix + hex.EncodeToString(mac.Sum(nil))
	assert.Equal(t, want, gotSig)
	assert.True(t, len(gotSig) > len(SignaturePrefix))
}

// TestWebhookSend_NoSignature verifies that without a secret no signature
// header is sent.
func TestWebhookSend_NoSignature(t *testing.T) {
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get(HeaderSignature)
		drainBody(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := newTestWebhook(t, "nosig", srv.URL, "", 0, 0, 0)
	require.NoError(t, wh.Send(context.Background(), validMessage()))
	assert.Empty(t, gotSig)
}

// --- Retry -----------------------------------------------------------------

// TestWebhookSend_RetrySuccess verifies that the notifier retries after 5xx
// responses and succeeds once the server returns 2xx.
func TestWebhookSend_RetrySuccess(t *testing.T) {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainBody(r)
		n := atomic.AddInt64(&count, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := newTestWebhook(t, "retry", srv.URL, "", 3, 5*time.Millisecond, 0)
	require.NoError(t, wh.Send(context.Background(), validMessage()))
	assert.Equal(t, int64(3), atomic.LoadInt64(&count), "should succeed on 3rd attempt")
}

// TestWebhookSend_RetryExhausted verifies that after exhausting all retries the
// error wraps both ErrMaxRetries and the last underlying error.
func TestWebhookSend_RetryExhausted(t *testing.T) {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainBody(r)
		atomic.AddInt64(&count, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	wh := newTestWebhook(t, "retry", srv.URL, "", 2, 5*time.Millisecond, 0)
	err := wh.Send(context.Background(), validMessage())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMaxRetries), "want ErrMaxRetries, got %v", err)
	assert.True(t, errors.Is(err, ErrNon2xxResponse), "want ErrNon2xxResponse, got %v", err)
	// maxRetries=2 => 1 initial + 2 retries = 3 attempts
	assert.Equal(t, int64(3), atomic.LoadInt64(&count))
}

// TestWebhookSend_NoRetryOnSuccess verifies that a first-attempt success does
// not trigger any retry.
func TestWebhookSend_NoRetryOnSuccess(t *testing.T) {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainBody(r)
		atomic.AddInt64(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := newTestWebhook(t, "ok", srv.URL, "", 5, 5*time.Millisecond, 0)
	require.NoError(t, wh.Send(context.Background(), validMessage()))
	assert.Equal(t, int64(1), atomic.LoadInt64(&count))
}

// --- Invalid URL -----------------------------------------------------------

// TestWebhookSend_InvalidURL verifies that an unreachable URL yields an error.
// The .invalid TLD (RFC 6761) is guaranteed never to resolve. MaxRetries=-1
// disables retries so the test fails fast on a single attempt.
func TestWebhookSend_InvalidURL(t *testing.T) {
	wh := newTestWebhook(t, "bad", "http://levee-nonexistent.invalid/hook", "", -1, 0, 2*time.Second)
	err := wh.Send(context.Background(), validMessage())
	require.Error(t, err)
}

// --- Context cancellation --------------------------------------------------

// TestWebhookSend_ContextCancelled verifies that cancelling the context during
// the retry delay stops retrying immediately and surfaces context.Canceled.
func TestWebhookSend_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainBody(r)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	wh := newTestWebhook(t, "cancel", srv.URL, "", 10, 500*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- wh.Send(ctx, validMessage())
	}()

	// Let the first (fast) attempt fail, then cancel during the retry delay.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled), "want context.Canceled, got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not return after context cancel")
	}
}

// TestWebhookSend_PreCancelledContext verifies that a pre-cancelled context
// fails fast without hitting the network more than necessary.
func TestWebhookSend_PreCancelledContext(t *testing.T) {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainBody(r)
		atomic.AddInt64(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := newTestWebhook(t, "pre", srv.URL, "", 3, 5*time.Millisecond, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := wh.Send(ctx, validMessage())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

// --- Timeout ---------------------------------------------------------------

// TestWebhookSend_Timeout verifies that a slow server triggers a timeout error
// when the configured Timeout is exceeded. MaxRetries=-1 disables retries so
// the test only waits for a single timeout.
func TestWebhookSend_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainBody(r)
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := newTestWebhook(t, "slow", srv.URL, "", -1, 0, 50*time.Millisecond)
	err := wh.Send(context.Background(), validMessage())
	require.Error(t, err)
	// http.Client.Timeout surfaces as context.DeadlineExceeded.
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "want DeadlineExceeded, got %v", err)
}

// --- Integration with NotificationManager -----------------------------------

// TestWebhookNotifier_IntegrationWithManager verifies that a WebhookNotifier can
// be registered with the NotificationManager and receives fanned-out messages.
func TestWebhookNotifier_IntegrationWithManager(t *testing.T) {
	var (
		gotBody  []byte
		gotEvent string
		count    int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotEvent = r.Header.Get(HeaderEvent)
		atomic.AddInt64(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := newTestWebhook(t, "mgr-hook", srv.URL, "", 0, 0, 0)

	mgr := NewNotificationManager()
	require.NoError(t, mgr.Register(wh))

	msg := validMessage()
	require.NoError(t, mgr.Notify(context.Background(), msg))

	assert.Equal(t, int64(1), atomic.LoadInt64(&count))
	assert.Equal(t, msg.Event, gotEvent)

	var decoded Message
	require.NoError(t, json.Unmarshal(gotBody, &decoded))
	assert.Equal(t, msg.ID, decoded.ID)
}
