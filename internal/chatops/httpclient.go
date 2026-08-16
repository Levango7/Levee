// httpclient.go provides a minimal, retry-aware HTTP POST helper shared by
// the platform adapters. It deliberately uses only net/http so the ChatOps
// layer stays free of third-party HTTP client dependencies (per the project
// coding guidelines).

package chatops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrEmptyWebhookURL is returned when an adapter is constructed without a
	// webhook URL.
	ErrEmptyWebhookURL = errors.New("chatops: empty webhook url")
	// ErrHTTPFailed is the generic sentinel for a failed HTTP request.
	ErrHTTPFailed = errors.New("chatops: http request failed")
	// ErrNon2xx is returned when the webhook endpoint responds with a non-2xx
	// status code.
	ErrNon2xx = errors.New("chatops: non-2xx response")
)

// --- Defaults --------------------------------------------------------------

const (
	defaultHTTPTimeout    = 10 * time.Second
	defaultHTTPMaxRetries = 2
	defaultHTTPRetryDelay = 500 * time.Millisecond
)

// httpOptions tunes the shared doPost helper.
type httpOptions struct {
	timeout    time.Duration
	maxRetries int
	retryDelay time.Duration
	headers    map[string]string
}

// newHTTPClient returns an http.Client with the given timeout (default
// applied when zero / negative).
func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	return &http.Client{Timeout: timeout}
}

// doPost sends payload (already JSON-encoded) to url with the given headers
// and retry policy. It returns nil on a 2xx response. The context is
// honoured between retries.
func doPost(ctx context.Context, client *http.Client, url string, payload []byte, opts httpOptions) error {
	if client == nil {
		client = newHTTPClient(opts.timeout)
	}
	maxRetries := opts.maxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	retryDelay := opts.retryDelay
	if retryDelay <= 0 {
		retryDelay = defaultHTTPRetryDelay
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("chatops: http post: %w", ctx.Err())
			case <-time.After(retryDelay):
			}
		}

		lastErr = doOnePost(ctx, client, url, payload, opts.headers)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("chatops: http post: %w", ctx.Err())
		}
	}
	return fmt.Errorf("chatops: http post %q: %w (last: %v)", url, ErrHTTPFailed, lastErr)
}

// doOnePost performs a single HTTP POST.
func doOnePost(ctx context.Context, client *http.Client, url string, payload []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("chatops: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("chatops: do request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("chatops: status %d: %w", resp.StatusCode, ErrNon2xx)
	}
	return nil
}

// postJSON marshals v and posts it. It is a thin convenience wrapper around
// doPost for adapters that do not need to pre-encode the payload.
func postJSON(ctx context.Context, client *http.Client, url string, v any, opts httpOptions) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("chatops: marshal: %w", err)
	}
	return doPost(ctx, client, url, payload, opts)
}
