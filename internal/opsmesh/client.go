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
	"strconv"
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
	// ErrQueryFailed is returned when the platform's Prometheus proxy reports
	// a non-success status for the query.
	ErrQueryFailed = errors.New("opsmesh: query failed")
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
// POST /api/v1/alerts/{alertID}/resolution (docs/opsmesh-integration-design.md
// §6.2).
//
// PLATFORM GAP, stated rather than guessed: the OpsMesh platform today
// implements POST /api/v1/alerts/{id}/ack and /silence — there is no
// /resolution endpoint (verified against the platform source). Reporting
// against such a deployment returns a 404 which callers log and treat as
// non-fatal; the alert still exists platform-side, unresolved. Routing this
// report at /ack instead was considered and rejected: ack carries no outcome
// payload and it suppresses the platform's own escalation semantics — an
// automation silently acknowledging alerts is a platform-side policy decision
// that must not be made unilaterally from this client. Until the platform
// grows the endpoint per the design, this method is the correct client for
// the agreed contract.
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

// GetTopology fetches the platform's asset/service catalog graph for a
// tenant: GET /api/v1/catalog/topology?tenantID={tenantID} (the platform's
// real endpoint — the `service`-keyed sketch in the integration design was
// never implemented; see this package's types.go). The whole graph is
// returned; callers match their own pivot (the diagnosis topology stage does
// this through topology.FindNode). The returned CatalogGraph is non-nil on a
// nil error.
//
// An empty tenantID omits the parameter, which the platform treats as its
// default tenant.
func (c *OpsMeshClient) GetTopology(ctx context.Context, tenantID string) (*CatalogGraph, error) {
	path := apiPrefix + "/catalog/topology"
	if tenantID != "" {
		q := url.Values{}
		q.Set("tenantID", tenantID)
		path += "?" + q.Encode()
	}

	var graph CatalogGraph
	if err := c.doGet(ctx, path, &graph); err != nil {
		return nil, err
	}
	return &graph, nil
}

// GetMetrics runs a PromQL query through the platform's Prometheus proxy:
// POST /api/v1/prometheus/query {"query": "..."} (aio-svc's real endpoint).
// The platform evaluates at the instant of the call — that is its contract,
// so this client carries no time range. The Prometheus envelope is decoded
// and normalized into Metrics (one MetricSample per series, value parsed,
// timestamp decoded).
//
// An empty query is rejected with ErrEmptyQuery; a non-"success" envelope
// status is rejected with ErrQueryFailed.
func (c *OpsMeshClient) GetMetrics(ctx context.Context, query string) (*Metrics, error) {
	if query == "" {
		return nil, fmt.Errorf("opsmesh: get metrics: %w", ErrEmptyQuery)
	}

	var resp PrometheusResponse
	if err := c.doPostDecoding(ctx, apiPrefix+"/prometheus/query", map[string]string{"query": query}, &resp); err != nil {
		return nil, err
	}
	return normalizeMetrics(query, &resp)
}

// Ping issues GET /healthz against the platform root and returns nil when it
// reports a 2xx status. It is the lightweight liveness probe used by the
// levee health subsystem (`/healthz` is the platform's registered probe —
// verified against its source; the former /api/v1/health path never existed).
func (c *OpsMeshClient) Ping(ctx context.Context) error {
	return c.doGet(ctx, "/healthz", nil)
}

// normalizeMetrics converts a Prometheus instant-vector envelope into the
// normalized Metrics view. Malformed series (a value pair that is not
// [timestamp, "value"]) are skipped rather than failing the whole query —
// one odd series must not blank out the evidence.
func normalizeMetrics(query string, resp *PrometheusResponse) (*Metrics, error) {
	if resp.Status != "success" {
		return nil, fmt.Errorf("opsmesh: get metrics: %w: platform status %q", ErrQueryFailed, resp.Status)
	}
	out := &Metrics{Query: query, Series: make([]MetricSample, 0, len(resp.Data.Result))}
	for _, s := range resp.Data.Result {
		if len(s.Value) != 2 {
			continue
		}
		ts, ok := s.Value[0].(float64)
		if !ok {
			continue
		}
		valStr, ok := s.Value[1].(string)
		if !ok {
			continue
		}
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		out.Series = append(out.Series, MetricSample{
			Labels:    s.Metric,
			Value:     val,
			Timestamp: time.Unix(int64(ts), int64((ts-float64(int64(ts)))*1e9)).UTC(),
		})
	}
	return out, nil
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

// doPostDecoding is the shared POST helper for endpoints that return a JSON
// body (the Prometheus proxy). It marshals body, performs the request and
// decodes the response into out on a 2xx.
func (c *OpsMeshClient) doPostDecoding(ctx context.Context, path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("opsmesh: marshal: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(payload))
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
