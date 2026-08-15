// Package verify implements LEVEE's verification gate framework.
//
// This file implements SLOGate (design doc section 4.4.5, MVP task T029).
// An SLOGate queries a Prometheus HTTP API for a scalar metric, compares the
// returned value against a threshold using one of the five comparison
// operators (lt, le, gt, ge, eq) and passes when the comparison holds. It is
// bound to the post_batch phase: an SLO probe after each batch is the
// canonical use case. The gate retries on query failure or comparison
// mismatch up to the configured retry count, with a fixed delay between
// attempts. The Prometheus client is a thin net/http wrapper so that no
// third-party Prometheus dependency is introduced.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nexus/levee/internal/log"
)

// SLOGate default tuning. The defaults are conservative so that a
// misconfigured gate fails fast rather than hanging the pipeline.
const (
	// DefaultSLOTimeout is the default per-attempt timeout for an SLOGate
	// Prometheus query when WithSLOTimeout is not used.
	DefaultSLOTimeout = 15 * time.Second

	// DefaultSLORetries is the default retry count (additional attempts
	// after the first failure).
	DefaultSLORetries = 2

	// DefaultSLORetryDelay is the default delay between SLOGate retries.
	DefaultSLORetryDelay = 2 * time.Second

	// DefaultSLOSource is the default Prometheus URL when WithSLOSource is
	// not used. It points at the in-process default that tests override.
	DefaultSLOSource = "http://localhost:9090"
)

// SLOCompare is the comparison operator used by SLOGate. The string values
// are the stable identifiers used in LEVEELang source and the audit trail.
type SLOCompare string

const (
	// CompareLT passes when value < threshold.
	CompareLT SLOCompare = "lt"
	// CompareLE passes when value <= threshold.
	CompareLE SLOCompare = "le"
	// CompareGT passes when value > threshold.
	CompareGT SLOCompare = "gt"
	// CompareGE passes when value >= threshold.
	CompareGE SLOCompare = "ge"
	// CompareEQ passes when value == threshold.
	CompareEQ SLOCompare = "eq"
)

// sloGateOption configures an SLOGate at construction time.
type sloGateOption func(*SLOGate)

// SLOGateOption is the public alias for the functional-option type. We
// expose it so that callers can build typed option lists, while keeping the
// internal option receiver unexported to discourage ad-hoc mutation.
type SLOGateOption = sloGateOption

// WithSLOTimeout sets the per-attempt timeout for the Prometheus query.
func WithSLOTimeout(d time.Duration) SLOGateOption {
	return func(g *SLOGate) { g.timeout = d }
}

// WithSLORetries sets the number of additional attempts after the first
// failure.
func WithSLORetries(n int) SLOGateOption {
	return func(g *SLOGate) { g.retries = n }
}

// WithSLORetryDelay sets the delay between retry attempts.
func WithSLORetryDelay(d time.Duration) SLOGateOption {
	return func(g *SLOGate) { g.retryDelay = d }
}

// WithSLOSource sets the Prometheus base URL (e.g. "http://prom:9090").
func WithSLOSource(u string) SLOGateOption {
	return func(g *SLOGate) { g.source = u }
}

// WithSLOHTTPClient sets a custom *http.Client. It is primarily intended for
// tests that want to inject a stub transport without spinning up an
// httptest.Server, but can also be used in production to tune connection
// pooling.
func WithSLOHTTPClient(c *http.Client) SLOGateOption {
	return func(g *SLOGate) { g.httpClient = c }
}

// SLOGate queries a Prometheus instance for a scalar metric and compares the
// result against a threshold. It is bound to the post_batch phase. The gate
// is safe for concurrent use: all mutable state is confined to a single
// Check call.
type SLOGate struct {
	name       string
	query      string
	source     string
	threshold  float64
	compare    SLOCompare
	timeout    time.Duration
	retries    int
	retryDelay time.Duration
	// httpClient is the *http.Client used to query Prometheus. It is
	// initialised to http.DefaultClient when not set via an option.
	httpClient *http.Client
}

// NewSLOGate returns an SLOGate with the given name, query, threshold and
// comparison operator. The phase is always PhasePostBatch. Override the
// defaults (source, timeout, retries, retry delay, http client) with the
// provided options.
//
// compare must be one of "lt", "le", "gt", "ge", "eq" (case-insensitive).
// An unknown operator is coerced to CompareLT at construction time and the
// mismatch is recorded in the result Details on the first Check.
func NewSLOGate(name string, query string, threshold float64, compare string, opts ...SLOGateOption) *SLOGate {
	g := &SLOGate{
		name:       name,
		query:      query,
		source:     DefaultSLOSource,
		threshold:  threshold,
		compare:    parseCompare(compare),
		timeout:    DefaultSLOTimeout,
		retries:    DefaultSLORetries,
		retryDelay: DefaultSLORetryDelay,
		httpClient: http.DefaultClient,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Name returns the gate's unique identifier.
func (g *SLOGate) Name() string { return g.name }

// Phase returns PhasePostBatch; an SLOGate always runs post-batch.
func (g *SLOGate) Phase() GatePhase { return PhasePostBatch }

// Check queries Prometheus, parses the scalar result and compares it against
// the threshold. It retries up to g.retries times on failure (query error or
// comparison mismatch), with g.retryDelay between attempts. The per-attempt
// timeout is g.timeout; the caller's ctx deadline is also honoured.
func (g *SLOGate) Check(ctx context.Context, input GateInput) (GateResult, error) {
	if err := ctx.Err(); err != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("slo gate %q cancelled before run: %v", g.name, err),
			Details: map[string]any{
				"gate":   "slo",
				"name":   g.name,
				"reason": "context_cancelled",
				"cause":  err.Error(),
			},
		}, nil
	}

	var lastResult GateResult
	for attempt := 0; attempt <= g.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			lastResult = GateResult{
				Passed:  false,
				Message: fmt.Sprintf("slo gate %q cancelled on attempt %d: %v", g.name, attempt+1, err),
				Details: map[string]any{
					"gate":    "slo",
					"name":    g.name,
					"attempt": attempt + 1,
					"reason":  "context_cancelled",
					"cause":   err.Error(),
				},
			}
			return lastResult, nil
		}

		result, err := g.runOnce(ctx, attempt+1)
		if err != nil {
			lastResult = result
			if ctx.Err() != nil {
				return lastResult, nil
			}
			log.Warn("slo gate attempt failed",
				"gate", g.name,
				"attempt", attempt+1,
				"err", err)
		} else if result.Passed {
			return result, nil
		} else {
			lastResult = result
			log.Debug("slo gate attempt mismatch",
				"gate", g.name,
				"attempt", attempt+1,
				"message", result.Message)
		}

		if attempt < g.retries {
			if !sleepCtx(ctx, g.retryDelay) {
				lastResult = GateResult{
					Passed:  false,
					Message: fmt.Sprintf("slo gate %q cancelled during retry delay", g.name),
					Details: map[string]any{
						"gate":    "slo",
						"name":    g.name,
						"attempt": attempt + 1,
						"reason":  "context_cancelled",
					},
				}
				return lastResult, nil
			}
		}
	}

	return lastResult, nil
}

// runOnce executes a single Prometheus query and compares the result.
func (g *SLOGate) runOnce(ctx context.Context, attempt int) (GateResult, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	start := time.Now()
	value, err := g.queryProm(attemptCtx)
	latency := time.Since(start)

	details := map[string]any{
		"gate":      "slo",
		"name":      g.name,
		"attempt":   attempt,
		"query":     g.query,
		"source":    g.source,
		"threshold": g.threshold,
		"compare":   string(g.compare),
		"latency":   latency.String(),
	}

	if err != nil {
		details["reason"] = "query_error"
		details["cause"] = err.Error()
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("slo gate %q query failed on attempt %d: %v", g.name, attempt, err),
			Details: details,
		}, err
	}

	details["value"] = value
	passed, msg := g.compareValue(value)
	if !passed {
		details["reason"] = "threshold_exceeded"
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("slo gate %q %s on attempt %d", g.name, msg, attempt),
			Details: details,
		}, nil
	}

	details["reason"] = "within_threshold"
	return GateResult{
		Passed:  true,
		Message: fmt.Sprintf("slo gate %q passed on attempt %d: %s", g.name, attempt, msg),
		Details: details,
	}, nil
}

// queryProm issues a single instant query against the Prometheus HTTP API
// and returns the scalar value. It expects the response to be a vector
// (type "vector") with at least one sample; the first sample's value is
// returned. Scalar results (type "scalar") are also accepted.
func (g *SLOGate) queryProm(ctx context.Context) (float64, error) {
	u, err := url.JoinPath(g.source, "/api/v1/query")
	if err != nil {
		return 0, fmt.Errorf("slo gate %q: invalid source URL %q: %w", g.name, g.source, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("slo gate %q: build request: %w", g.name, err)
	}
	q := req.URL.Query()
	q.Set("query", g.query)
	req.URL.RawQuery = q.Encode()

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("slo gate %q: http get: %w", g.name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("slo gate %q: read body: %w", g.name, err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("slo gate %q: prometheus returned status %d: %s",
			g.name, resp.StatusCode, truncateBody(body))
	}

	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, fmt.Errorf("slo gate %q: decode response: %w", g.name, err)
	}
	if pr.Status != "success" {
		return 0, fmt.Errorf("slo gate %q: prometheus status %q: %s",
			g.name, pr.Status, pr.Error)
	}

	value, err := pr.Data.scalarValue()
	if err != nil {
		return 0, fmt.Errorf("slo gate %q: %w", g.name, err)
	}
	return value, nil
}

// compareValue applies the configured comparison operator to value vs
// threshold. It returns (passed, humanReadableMessage).
func (g *SLOGate) compareValue(value float64) (bool, string) {
	switch g.compare {
	case CompareLT:
		if value < g.threshold {
			return true, fmt.Sprintf("value %g < threshold %g", value, g.threshold)
		}
		return false, fmt.Sprintf("value %g >= threshold %g", value, g.threshold)
	case CompareLE:
		if value <= g.threshold {
			return true, fmt.Sprintf("value %g <= threshold %g", value, g.threshold)
		}
		return false, fmt.Sprintf("value %g > threshold %g", value, g.threshold)
	case CompareGT:
		if value > g.threshold {
			return true, fmt.Sprintf("value %g > threshold %g", value, g.threshold)
		}
		return false, fmt.Sprintf("value %g <= threshold %g", value, g.threshold)
	case CompareGE:
		if value >= g.threshold {
			return true, fmt.Sprintf("value %g >= threshold %g", value, g.threshold)
		}
		return false, fmt.Sprintf("value %g < threshold %g", value, g.threshold)
	case CompareEQ:
		if value == g.threshold {
			return true, fmt.Sprintf("value %g == threshold %g", value, g.threshold)
		}
		return false, fmt.Sprintf("value %g != threshold %g", value, g.threshold)
	default:
		// Should not happen: parseCompare coerces unknowns to CompareLT.
		return false, fmt.Sprintf("unknown compare operator %q", g.compare)
	}
}

// parseCompare converts a string to an SLOCompare. Unknown values default to
// CompareLT so that a typo does not silently turn into a permissive gate.
func parseCompare(s string) SLOCompare {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "lt":
		return CompareLT
	case "le":
		return CompareLE
	case "gt":
		return CompareGT
	case "ge":
		return CompareGE
	case "eq":
		return CompareEQ
	default:
		return CompareLT
	}
}

// truncateBody returns at most the first 200 bytes of body as a string,
// suitable for embedding in an error message without overflowing the log.
func truncateBody(body []byte) string {
	const max = 200
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "...(truncated)"
}

// --- Prometheus response types --------------------------------------------
//
// These types model the subset of the Prometheus HTTP API response that
// SLOGate needs. The full schema is documented at
// https://prometheus.io/docs/prometheus/latest/querying/api/.

// promResponse is the top-level envelope of a Prometheus query response.
type promResponse struct {
	Status    string   `json:"status"`
	Data      promData `json:"data"`
	Error     string   `json:"error,omitempty"`
	ErrorType string   `json:"errorType,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// promData carries the result type and the result itself. The Result field
// is decoded lazily because the shape depends on ResultType.
type promData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

// scalarValue extracts a single float64 from the Prometheus result. It
// supports the two result types that an instant query can produce for a
// scalar expression: "scalar" ([ts, "value"]) and "vector"
// ([{"metric":{}, "value":[ts, "value"]}, ...]). For vectors the first
// sample's value is returned.
func (d promData) scalarValue() (float64, error) {
	switch d.ResultType {
	case "scalar":
		var s [2]any
		if err := json.Unmarshal(d.Result, &s); err != nil {
			return 0, fmt.Errorf("decode scalar: %w", err)
		}
		return parseSampleValue(s[1])
	case "vector":
		var samples []promSample
		if err := json.Unmarshal(d.Result, &samples); err != nil {
			return 0, fmt.Errorf("decode vector: %w", err)
		}
		if len(samples) == 0 {
			return 0, fmt.Errorf("empty vector result")
		}
		return parseSampleValue(samples[0].Value[1])
	default:
		return 0, fmt.Errorf("unsupported result type %q (want scalar or vector)", d.ResultType)
	}
}

// promSample is a single sample in a vector result.
type promSample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// parseSampleValue converts the value portion of a Prometheus sample (which
// is encoded as a string in the JSON response) to a float64.
func parseSampleValue(v any) (float64, error) {
	switch x := v.(type) {
	case string:
		var f float64
		_, err := fmt.Sscanf(x, "%g", &f)
		if err != nil {
			return 0, fmt.Errorf("parse value %q: %w", x, err)
		}
		return f, nil
	case float64:
		return x, nil
	case int64:
		return float64(x), nil
	default:
		return 0, fmt.Errorf("unexpected value type %T", v)
	}
}
