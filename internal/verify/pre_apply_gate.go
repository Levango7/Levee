
// Package verify implements LEVEE's verification gate framework.
//
// This file implements PreApplySLOGate (design doc section 4.4.5, F14 SLO gate
// three-stage timing). A PreApplySLOGate runs at the pre_apply phase: it
// queries one or more Prometheus SLO metrics before any change is applied and
// confirms that the system is healthy enough to proceed. It is the first leg
// of the three-stage SLO timing (pre_apply → post_batch/post_apply →
// grace_period).
//
// The gate reuses the Prometheus query plumbing from slo_gate.go (the
// promResponse / promData / promSample types and the parseSampleValue helper)
// so that no second Prometheus client is introduced. It supports a configurable
// failure policy (block / warn / skip) so that a flaky Prometheus does not
// hard-abort every run, and a MultiSLO configuration so that a single gate can
// require several metrics to be within threshold simultaneously.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/nexus/levee/internal/log"
)

// SLOOnFailure is the policy applied when a PreApplySLOGate cannot determine
// that the SLO is healthy — either because the query failed or because the
// metric breached the threshold. The string values are stable identifiers used
// in LEVEELang source and the audit trail.
type SLOOnFailure string

const (
	// OnFailureBlock (default) treats any query error or threshold breach as a
	// gate failure, which aborts the apply.
	OnFailureBlock SLOOnFailure = "block"

	// OnFailureWarn treats the breach as a warning: the gate still reports
	// Passed == false so that the manager records it, but the message is
	// phrased as a warning and the Details carry a "policy" field so that
	// upstream consumers can choose to continue. The manager itself treats
	// warn the same as block for the purpose of stopping the phase; the
	// difference is purely informational for the audit trail. Callers that
	// want a true non-blocking warning should use OnFailureSkip.
	OnFailureWarn SLOOnFailure = "warn"

	// OnFailureSkip makes the gate return Passed == true with a "skipped"
	// reason whenever the query fails or the threshold is breached. It is
	// intended for non-critical SLOs where a flaky Prometheus must not block
	// the run.
	OnFailureSkip SLOOnFailure = "skip"
)

// parseOnFailure coerces a string to an SLOOnFailure. Unknown values default
// to OnFailureBlock so that a typo does not silently turn into a permissive
// policy.
func parseOnFailure(s string) SLOOnFailure {
	switch s {
	case "block":
		return OnFailureBlock
	case "warn":
		return OnFailureWarn
	case "skip":
		return OnFailureSkip
	default:
		return OnFailureBlock
	}
}

// SLOQuerySpec describes a single Prometheus SLO query inside a MultiSLO gate.
// All fields are required: a missing query or an unknown compare operator is
// treated as a configuration error and causes the gate to fail.
type SLOQuerySpec struct {
	// Query is the PromQL instant-query expression, e.g.
	// "rate(http_errors_total[5m]) / rate(http_requests_total[5m])".
	Query string `json:"query" yaml:"query"`

	// Threshold is the value the query result is compared against.
	Threshold float64 `json:"threshold" yaml:"threshold"`

	// Compare is the comparison operator: "lt", "le", "gt", "ge", "eq".
	// Unknown values are coerced to "lt" at construction time.
	Compare string `json:"compare" yaml:"compare"`

	// Label is an optional human-readable name for the query, used in log
	// messages and the audit trail. When empty the query string is used.
	Label string `json:"label,omitempty" yaml:"label,omitempty"`
}

// preApplyOption configures a PreApplySLOGate at construction time.
type preApplyOption func(*PreApplySLOGate)

// PreApplySLOGateOption is the public alias for the functional-option type.
type PreApplySLOGateOption = preApplyOption

// WithPreApplyTimeout sets the per-attempt timeout for every Prometheus query
// issued by the gate.
func WithPreApplyTimeout(d time.Duration) PreApplySLOGateOption {
	return func(g *PreApplySLOGate) { g.timeout = d }
}

// WithPreApplyRetries sets the number of additional attempts after the first
// failure for each query.
func WithPreApplyRetries(n int) PreApplySLOGateOption {
	return func(g *PreApplySLOGate) { g.retries = n }
}

// WithPreApplyRetryDelay sets the delay between retry attempts.
func WithPreApplyRetryDelay(d time.Duration) PreApplySLOGateOption {
	return func(g *PreApplySLOGate) { g.retryDelay = d }
}

// WithPreApplySource sets the Prometheus base URL.
func WithPreApplySource(u string) PreApplySLOGateOption {
	return func(g *PreApplySLOGate) { g.source = u }
}

// WithPreApplyHTTPClient sets a custom *http.Client. It is primarily intended
// for tests that want to inject a stub transport without spinning up an
// httptest.Server.
func WithPreApplyHTTPClient(c *http.Client) PreApplySLOGateOption {
	return func(g *PreApplySLOGate) { g.httpClient = c }
}

// WithPreApplyOnFailure overrides the on-failure policy.
func WithPreApplyOnFailure(p SLOOnFailure) PreApplySLOGateOption {
	return func(g *PreApplySLOGate) { g.onFailure = p }
}

// WithPreApplyBaselineWindow sets the look-back window used to annotate the
// queries. The gate does not modify the user-supplied PromQL; the window is
// only recorded in the audit trail so that operators can see which baseline
// the gate was configured against. A value of 0 (the default) means "not
// set".
func WithPreApplyBaselineWindow(d time.Duration) PreApplySLOGateOption {
	return func(g *PreApplySLOGate) { g.baselineWindow = d }
}

// PreApplySLOGate queries one or more Prometheus SLO metrics before apply
// starts and confirms the system is healthy. It is bound to the PhasePreApply
// phase. The gate is safe for concurrent use: all mutable state is confined
// to a single Check call.
//
// When multiple SLOQuerySpec entries are configured (MultiSLO) the gate
// passes only when every query is within its threshold. A failure of any one
// query is subject to the on-failure policy: block aborts the apply, warn
// records a warning but still fails the gate, skip treats the failure as a
// pass so that a flaky query does not block the run.
type PreApplySLOGate struct {
	name           string
	queries        []SLOQuerySpec
	source         string
	onFailure      SLOOnFailure
	timeout        time.Duration
	retries        int
	retryDelay     time.Duration
	baselineWindow time.Duration
	httpClient     *http.Client
}

// NewPreApplySLOGate returns a PreApplySLOGate with the given name and SLO
// query specifications. The phase is always PhasePreApply. At least one query
// must be supplied; an empty slice yields a gate that fails at construction
// time with a configuration error recorded in the first Check result.
//
// Override the defaults (source, timeout, retries, retry delay, http client,
// on-failure policy, baseline window) with the provided options.
func NewPreApplySLOGate(name string, queries []SLOQuerySpec, opts ...PreApplySLOGateOption) *PreApplySLOGate {
	g := &PreApplySLOGate{
		name:       name,
		queries:    queries,
		source:     DefaultSLOSource,
		onFailure:  OnFailureBlock,
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
func (g *PreApplySLOGate) Name() string { return g.name }

// Phase returns PhasePreApply; a PreApplySLOGate always runs before apply.
func (g *PreApplySLOGate) Phase() GatePhase { return PhasePreApply }

// Check queries every configured SLO and returns Passed == true only when all
// of them are within their thresholds. Per-query retries honour g.retries and
// g.retryDelay; the per-attempt timeout is g.timeout. The caller's ctx
// deadline is also honoured.
//
// The on-failure policy is applied per query: a query that ultimately fails
// (after retries) is mapped through the policy to decide the gate result.
func (g *PreApplySLOGate) Check(ctx context.Context, input GateInput) (GateResult, error) {
	if err := ctx.Err(); err != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("pre_apply slo gate %q cancelled before run: %v", g.name, err),
			Details: map[string]any{
				"gate":   "pre_apply_slo",
				"name":   g.name,
				"reason": "context_cancelled",
				"cause":  err.Error(),
			},
		}, nil
	}

	details := map[string]any{
		"gate":            "pre_apply_slo",
		"name":            g.name,
		"source":          g.source,
		"on_failure":      string(g.onFailure),
		"query_count":     len(g.queries),
		"baseline_window": g.baselineWindow.String(),
	}

	if len(g.queries) == 0 {
		details["reason"] = "no_queries_configured"
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("pre_apply slo gate %q has no queries configured", g.name),
			Details: details,
		}, fmt.Errorf("pre_apply slo gate %q: no queries configured", g.name)
	}

	// Run every query. We keep the per-query results so that the audit trail
	// can show which SLO passed and which failed.
	perQuery := make([]map[string]any, 0, len(g.queries))
	firstFailure := ""
	for i, spec := range g.queries {
		if err := ctx.Err(); err != nil {
			details["reason"] = "context_cancelled"
			details["cause"] = err.Error()
			details["queries"] = perQuery
			return GateResult{
				Passed:  false,
				Message: fmt.Sprintf("pre_apply slo gate %q cancelled mid-run: %v", g.name, err),
				Details: details,
			}, nil
		}

		qr := g.runQuery(ctx, spec, i+1)
		perQuery = append(perQuery, qr)

		if !qr["passed"].(bool) {
			if firstFailure == "" {
				firstFailure = fmt.Sprintf("query %d (%s) failed: %s", i+1, queryLabel(spec), qr["message"].(string))
			}
			log.Warn("pre_apply slo query failed",
				"gate", g.name,
				"query_index", i+1,
				"query", spec.Query,
				"message", qr["message"])
		}
	}

	details["queries"] = perQuery

	if firstFailure == "" {
		details["reason"] = "all_queries_within_threshold"
		return GateResult{
			Passed:  true,
			Message: fmt.Sprintf("pre_apply slo gate %q passed: %d/%d queries within threshold", g.name, len(g.queries), len(g.queries)),
			Details: details,
		}, nil
	}

	details["reason"] = "threshold_breached_or_query_failed"
	return g.applyPolicy(ctx, details, "pre_apply slo gate %q failed: %s", fmt.Errorf("%s", firstFailure)), nil
}

// runQuery executes a single SLO query spec with retries and returns a
// details map describing the outcome. The map always contains "passed"
// (bool), "query" (string), "threshold" (float64), "compare" (string),
// "label" (string), "message" (string) and, on success, "value" (float64).
func (g *PreApplySLOGate) runQuery(ctx context.Context, spec SLOQuerySpec, idx int) map[string]any {
	compare := parseCompare(spec.Compare)
	base := map[string]any{
		"index":     idx,
		"query":     spec.Query,
		"label":     queryLabel(spec),
		"threshold": spec.Threshold,
		"compare":   string(compare),
	}

	var lastValue float64
	var lastErr error
	var lastPassed bool
	var lastMsg string

	for attempt := 0; attempt <= g.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			base["passed"] = false
			base["message"] = fmt.Sprintf("cancelled on attempt %d: %v", attempt+1, err)
			base["reason"] = "context_cancelled"
			return base
		}

		value, err := g.queryPromOnce(ctx, spec.Query)
		if err != nil {
			lastErr = err
			lastPassed = false
			lastMsg = fmt.Sprintf("query error on attempt %d: %v", attempt+1, err)
			log.Debug("pre_apply slo query attempt error",
				"gate", g.name,
				"attempt", attempt+1,
				"err", err)
		} else {
			lastValue = value
			lastErr = nil
			passed, msg := compareValueGeneric(value, spec.Threshold, compare)
			lastPassed = passed
			lastMsg = msg
			if passed {
				base["passed"] = true
				base["value"] = value
				base["attempt"] = attempt + 1
				base["message"] = fmt.Sprintf("passed on attempt %d: %s", attempt+1, msg)
				base["reason"] = "within_threshold"
				return base
			}
			log.Debug("pre_apply slo query attempt mismatch",
				"gate", g.name,
				"attempt", attempt+1,
				"message", msg)
		}

		if attempt < g.retries {
			if !sleepCtx(ctx, g.retryDelay) {
				base["passed"] = false
				base["message"] = "cancelled during retry delay"
				base["reason"] = "context_cancelled"
				return base
			}
		}
	}

	base["passed"] = lastPassed
	if lastErr != nil {
		base["message"] = lastMsg
		base["reason"] = "query_error"
	} else {
		base["value"] = lastValue
		base["message"] = lastMsg
		base["reason"] = "threshold_exceeded"
	}
	return base
}

// queryPromOnce issues a single instant query against the Prometheus HTTP API
// for the given query string and returns the scalar value. It mirrors
// SLOGate.queryProm but accepts the query explicitly so that a MultiSLO gate
// can issue several different queries through the same client.
func (g *PreApplySLOGate) queryPromOnce(ctx context.Context, query string) (float64, error) {
	u, err := url.JoinPath(g.source, "/api/v1/query")
	if err != nil {
		return 0, fmt.Errorf("pre_apply slo gate %q: invalid source URL %q: %w", g.name, g.source, err)
	}

	attemptCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("pre_apply slo gate %q: build request: %w", g.name, err)
	}
	q := req.URL.Query()
	q.Set("query", query)
	req.URL.RawQuery = q.Encode()

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("pre_apply slo gate %q: http get: %w", g.name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("pre_apply slo gate %q: read body: %w", g.name, err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("pre_apply slo gate %q: prometheus returned status %d: %s",
			g.name, resp.StatusCode, truncateBody(body))
	}

	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, fmt.Errorf("pre_apply slo gate %q: decode response: %w", g.name, err)
	}
	if pr.Status != "success" {
		return 0, fmt.Errorf("pre_apply slo gate %q: prometheus status %q: %s",
			g.name, pr.Status, pr.Error)
	}

	value, err := pr.Data.scalarValue()
	if err != nil {
		return 0, fmt.Errorf("pre_apply slo gate %q: %w", g.name, err)
	}
	return value, nil
}

// applyPolicy maps a failure through the on-failure policy and returns the
// resulting GateResult. The error is preserved in Details.cause for the
// audit trail; the returned error is always nil because the policy decision
// is part of the result, not a transport-level error.
func (g *PreApplySLOGate) applyPolicy(_ context.Context, details map[string]any, format string, cause error) GateResult {
	details["policy"] = string(g.onFailure)
	details["cause"] = cause.Error()
	msg := fmt.Sprintf(format, g.name, cause)

	switch g.onFailure {
	case OnFailureSkip:
		// Skip: report a pass so that the manager continues. The original
		// failure is preserved in Details for the audit trail.
		details["reason"] = "skipped_by_policy"
		return GateResult{
			Passed:  true,
			Message: fmt.Sprintf("pre_apply slo gate %q skipped by policy (on_failure=skip): %v", g.name, cause),
			Details: details,
		}
	case OnFailureWarn:
		// Warn: still fail the gate (so the manager records it) but phrase
		// the message as a warning and tag the reason.
		details["reason"] = "warn_by_policy"
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("pre_apply slo gate %q WARNING (on_failure=warn): %v", g.name, cause),
			Details: details,
		}
	default:
		// Block (default): fail the gate.
		details["reason"] = "blocked_by_policy"
		return GateResult{
			Passed:  false,
			Message: msg,
			Details: details,
		}
	}
}

// queryLabel returns the label of a spec, falling back to the query string
// when the label is empty.
func queryLabel(spec SLOQuerySpec) string {
	if spec.Label != "" {
		return spec.Label
	}
	return spec.Query
}

// compareValueGeneric applies a comparison operator to value vs threshold
// outside the SLOGate struct. It is shared by PreApplySLOGate and
// GracePeriodGate so that both can avoid depending on SLOGate's receiver
// method.
func compareValueGeneric(value, threshold float64, op SLOCompare) (bool, string) {
	switch op {
	case CompareLT:
		if value < threshold {
			return true, fmt.Sprintf("value %g < threshold %g", value, threshold)
		}
		return false, fmt.Sprintf("value %g >= threshold %g", value, threshold)
	case CompareLE:
		if value <= threshold {
			return true, fmt.Sprintf("value %g <= threshold %g", value, threshold)
		}
		return false, fmt.Sprintf("value %g > threshold %g", value, threshold)
	case CompareGT:
		if value > threshold {
			return true, fmt.Sprintf("value %g > threshold %g", value, threshold)
		}
		return false, fmt.Sprintf("value %g <= threshold %g", value, threshold)
	case CompareGE:
		if value >= threshold {
			return true, fmt.Sprintf("value %g >= threshold %g", value, threshold)
		}
		return false, fmt.Sprintf("value %g < threshold %g", value, threshold)
	case CompareEQ:
		if value == threshold {
			return true, fmt.Sprintf("value %g == threshold %g", value, threshold)
		}
		return false, fmt.Sprintf("value %g != threshold %g", value, threshold)
	default:
		return false, fmt.Sprintf("unknown compare operator %q", op)
	}
}