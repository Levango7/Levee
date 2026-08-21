// client.go implements the OpsMesh integration client. OpsMesh is the
// observability and topology platform that levee integrates with bidirectionally:
// levee reports remediation outcomes back to OpsMesh through ReportResult and
// pulls topology and metric data from OpsMesh through GetTopology and
// GetMetrics.
//
// OpsMeshClient is created once at startup and is safe for concurrent use: all
// fields are read-only after construction and the underlying http.Client is
// goroutine-safe. The client never mutates its own state, so multiple goroutines
// can share a single instance without external synchronisation.
package opsmesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrNilResult is returned when ReportResult is called with a nil result
	// pointer.
	ErrNilResult = errors.New("opsmesh: nil result")
	// ErrEmptyAlertID is returned when an alert identifier is required but
	// empty.
	ErrEmptyAlertID = errors.New("opsmesh: empty alert id")
	// ErrEmptyQuery is returned when GetMetrics is called with an empty query
	// string.
	ErrEmptyQuery = errors.New("opsmesh: empty query")
	// ErrInvalidTimeRange is returned when GetMetrics is called with a time
	// range whose End is not strictly after its Start.
	ErrInvalidTimeRange = errors.New("opsmesh: invalid time range")
)

// --- Constants --------------------------------------------------------------

const (
	// userAgent is the value sent in the User-Agent header on every request so
	// OpsMesh can identify levee-side traffic in its access logs.
	userAgent = "levee-opsmesh-client/1.0"
	// apiPrefix is the versioned API prefix common to all endpoints.
	apiPrefix = "/api/v1"
	// defaultHTTPTimeout is the per-request timeout used when the caller does
	// not supply an http.Client.
	defaultHTTPTimeout = 30 * time.Second
)

// --- Config & construction --------------------------------------------------

// OpsMeshClientConfig is the configuration for NewOpsMeshClient. All fields are
// optional except BaseURL and APIKey which must be non-empty for the client to
// be useful; NewOpsMeshClient does not validate them so callers can build a
// client early and defer configuration to first use, matching the project-wide
// "construct cheaply, validate on use" idiom.
type OpsMeshClientConfig struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client // nil -> default with 30s timeout
	Logger     *slog.Logger // nil -> default slog.Default()
}

// OpsMeshClient is the HTTP client for the OpsMesh platform. It is immutable
// after construction and safe for concurrent use by multiple goroutines.
type OpsMeshClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	log        *slog.Logger
}

// NewOpsMeshClient constructs an OpsMeshClient from cfg. When HTTPClient is nil
// a new http.Client with a 30 second timeout is used. When Logger is nil
// slog.Default() is used. The returned client is ready to use and safe for
// concurrent access.
func NewOpsMeshClient(cfg OpsMeshClientConfig) *OpsMeshClient {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	lg := cfg.Logger
	if lg == nil {
		lg = slog.Default()
	}

	return &OpsMeshClient{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:     cfg.APIKey,
		httpClient: httpClient,
		log:        lg,
	}
}

// --- Public API -------------------------------------------------------------

// ReportResult posts a remediation outcome back to OpsMesh for the given alert.
// The result is JSON-encoded and sent to
// POST /api/v1/alerts/{alertID}/resolution.
//
// ReportResult returns an error wrapping one of the sentinel errors
// ErrEmptyAlertID or ErrNilResult when the inputs are invalid, and a wrapped
// network/HTTP error when the request fails.
func (c *OpsMeshClient) ReportResult(ctx context.Context, alertID string, result *FixResult) error {
	if alertID == "" {
		return fmt.Errorf("opsmesh: report result: %w", ErrEmptyAlertID)
	}
	if result == nil {
		return fmt.Errorf("opsmesh: report result: %w", ErrNilResult)
	}

	path := fmt.Sprintf("%s/alerts/%s/resolution", apiPrefix, url.PathEscape(alertID))
	return c.doPost(ctx, path, result)
}

// GetTopology fetches the service topology from OpsMesh for the given service
// name. It issues GET /api/v1/topology?service={service} and decodes the JSON
// response into a Topology. The returned Topology is non-nil on a nil error.
func (c *OpsMeshClient) GetTopology(ctx context.Context, service string) (*Topology, error) {
	q := url.Values{}
	q.Set("service", service)
	path := fmt.Sprintf("%s/topology?%s", apiPrefix, q.Encode())

	var topo Topology
	if err := c.doGet(ctx, path, &topo); err != nil {
		return nil, err
	}
	return &topo, nil
}

// GetMetrics fetches monitoring metrics from OpsMesh for the given query and
// time range. It issues GET /api/v1/metrics?query={query}&start={start}&end={end}
// and decodes the JSON response into a Metrics. The returned Metrics is non-nil
// on a nil error.
//
// GetMetrics rejects an empty query with ErrEmptyQuery and a time range whose
// End is not strictly after Start with ErrInvalidTimeRange before any network
// activity.
func (c *OpsMeshClient) GetMetrics(ctx context.Context, query string, timeRange TimeRange) (*Metrics, error) {
	if query == "" {
		return nil, fmt.Errorf("opsmesh: get metrics: %w", ErrEmptyQuery)
	}
	if !timeRange.End.After(timeRange.Start) {
		return nil, fmt.Errorf("opsmesh: get metrics: %w", ErrInvalidTimeRange)
	}

	q := url.Values{}
	q.Set("query", query)
	q.Set("start", timeRange.Start.UTC().Format(time.RFC3339Nano))
	q.Set("end", timeRange.End.UTC().Format(time.RFC3339Nano))
	path := fmt.Sprintf("%s/metrics?%s", apiPrefix, q.Encode())

	var metrics Metrics
	if err := c.doGet(ctx, path, &metrics); err != nil {
		return nil, err
	}
	return &metrics, nil
}

// Ping issues GET /api/v1/health against OpsMesh and returns nil when the
// platform reports a 2xx status. It is the lightweight liveness probe used by
// the levee health subsystem.
func (c *OpsMeshClient) Ping(ctx context.Context) error {
	return c.doGet(ctx, apiPrefix+"/health", nil)
}

// --- Internal helpers -------------------------------------------------------

// doPost is the shared POST helper. It marshals body to JSON, attaches the
// standard headers, performs the request and returns nil on a 2xx response.
func (c *OpsMeshClient) doPost(ctx context.Context, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("opsmesh: marshal: %w", err)
	}

	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(payload))
	if err != nil {
		return err
	}

	return c.send(req, path)
}

// doGet is the shared GET helper. It performs the request and, when out is
// non-nil, decodes the JSON body into out. It returns nil on a 2xx response.
func (c *OpsMeshClient) doGet(ctx context.Context, path string, out any) error {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return c.send(req, path, out)
}

// newRequest builds an *http.Request with the standard headers attached. The
// returned request is ready to be passed to send.
func (c *OpsMeshClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	fullURL := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("opsmesh: build request %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

// send executes req, logs the outcome and, on a 2xx response, optionally
// decodes the JSON body into out. It always drains and closes the response
// body so the underlying connection can be reused.
func (c *OpsMeshClient) send(req *http.Request, path string, out ...any) error {
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	elapsed := time.Since(start)
	method := req.Method

	if err != nil {
		c.log.LogAttrs(context.Background(), slog.LevelWarn, "opsmesh request failed",
			slog.String("method", method),
			slog.String("path", path),
			slog.Duration("duration", elapsed),
			slog.String("err", err.Error()))
		return fmt.Errorf("opsmesh: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		c.log.LogAttrs(context.Background(), slog.LevelWarn, "opsmesh read body failed",
			slog.String("method", method),
			slog.String("path", path),
			slog.Int("status", resp.StatusCode),
			slog.Duration("duration", elapsed),
			slog.String("err", readErr.Error()))
		return fmt.Errorf("opsmesh: %s %s: read body: %w", method, path, readErr)
	}

	c.log.LogAttrs(context.Background(), slog.LevelInfo, "opsmesh request",
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", resp.StatusCode),
		slog.Duration("duration", elapsed))

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("opsmesh: %s %s: status %d: %s", method, path, resp.StatusCode, string(raw))
	}

	// Only decode when a target was supplied.
	if len(out) > 0 && out[0] != nil {
		if err := json.Unmarshal(raw, out[0]); err != nil {
			return fmt.Errorf("opsmesh: %s %s: unmarshal: %w", method, path, err)
		}
	}
	return nil
}
