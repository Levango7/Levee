// Package verify implements LEVEE's verification gate framework.
//
// This file implements GracePeriodGate (design doc section 4.4.5, F14 SLO gate
// three-stage timing). A GracePeriodGate runs at the grace_period phase: after
// the post_apply phase finishes, it waits for a configurable cool-down
// duration and then re-queries one or more Prometheus SLO metrics to detect
// delayed regressions that only surface after the apply completes (e.g. cache
// warm-up, connection-pool drift, slow error-rate creep).
//
// The cool-down is implemented with sleepCtx so that it is cancellable via the
// caller's context: a run that is aborted mid-wait does not leak a goroutine.
// Setting Duration to 0 makes the gate a no-op pass, which lets non-critical
// workflows opt out of the grace period without removing the gate from their
// configuration.
//
// The gate reuses the Prometheus query plumbing from slo_gate.go and the
// MultiSLO + on-failure policy from pre_apply_gate.go so that the three SLO
// gate variants share a single behavioural contract.
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

// GracePeriod default tuning. The defaults are conservative so that a
// misconfigured gate fails fast rather than hanging the pipeline.
const (
	// DefaultGracePeriodDuration is the default cool-down between post_apply
	// finishing and the SLO re-query. Five minutes is long enough to catch
	// most delayed regressions while short enough to keep the pipeline
	// responsive.
	DefaultGracePeriodDuration = 5 * time.Minute

	// MaxGracePeriodDuration caps the configurable cool-down. Thirty minutes
	// is the upper bound beyond which the gate refuses to start: a longer
	// wait would block the pipeline for too long and is almost always a
	// configuration mistake.
	MaxGracePeriodDuration = 30 * time.Minute
)

// GracePeriodConfig captures the user-facing configuration of a GracePeriodGate.
// It is exposed so that LEVEELang parsers and config loaders can build a gate
// from a plain struct without depending on the functional-option constructors.
type GracePeriodConfig struct {
	// Duration is the cool-down between post_apply finishing and the SLO
	// re-query. A value of 0 makes the gate a no-op pass. Values above
	// MaxGracePeriodDuration are clamped at construction time.
	Duration time.Duration `json:"duration" yaml:"duration"`

	// SLOQueries is the list of SLO queries to issue after the cool-down.
	// At least one query is required when Duration > 0; an empty slice with
	// a positive Duration is a configuration error and the gate fails on
	// the first Check.
	SLOQueries []SLOQuerySpec `json:"slo_queries" yaml:"slo_queries"`

	// OnFailure is the policy applied when a query fails or a threshold is
	// breached. It accepts the same values as PreApplySLOGate: "block"
	// (default), "warn", "skip".
	OnFailure string `json:"on_failure" yaml:"on_failure"`
}

// gracePeriodOption configures a GracePeriodGate at construction time.
type gracePeriodOption func(*GracePeriodGate)

// GracePeriodGateOption is the public alias for the functional-option type.
type GracePeriodGateOption = gracePeriodOption

// WithGracePeriodTimeout sets the per-attempt timeout for every Prometheus
// query issued by the gate.
func WithGracePeriodTimeout(d time.Duration) GracePeriodGateOption {
	return func(g *GracePeriodGate) { g.timeout = d }
}

// WithGracePeriodRetries sets the number of additional attempts after the
// first failure for each query.
func WithGracePeriodRetries(n int) GracePeriodGateOption {
	return func(g *GracePeriodGate) { g.retries = n }
}

// WithGracePeriodRetryDelay sets the delay between retry attempts.
func WithGracePeriodRetryDelay(d time.Duration) GracePeriodGateOption {
	return func(g *GracePeriodGate) { g.retryDelay = d }
}

// WithGracePeriodSource sets the Prometheus base URL.
func WithGracePeriodSource(u string) GracePeriodGateOption {
	return func(g *GracePeriodGate) { g.source = u }
}

// WithGracePeriodHTTPClient sets a custom *http.Client.
func WithGracePeriodHTTPClient(c *http.Client) GracePeriodGateOption {
	return func(g *GracePeriodGate) { g.httpClient = c }
}

// WithGracePeriodOnFailure overrides the on-failure policy.
func WithGracePeriodOnFailure(p SLOOnFailure) GracePeriodGateOption {
	return func(g *GracePeriodGate) { g.onFailure = p }
}

// GracePeriodGate waits for a configurable cool-down after post_apply and then
// re-queries one or more Prometheus SLO metrics. It is bound to the
// PhaseGracePeriod phase. The gate is safe for concurrent use: all mutable
// state is confined to a single Check call.
//
// Execution flow:
//  1. If Duration == 0 the gate returns a no-op pass immediately.
//  2. Otherwise the gate sleeps for Duration, cancellable via ctx.
//  3. After the cool-down it issues every configured SLO query (with retries).
//  4. The gate passes only when every query is within its threshold; the
//     on-failure policy is applied to the first failure.
type GracePeriodGate struct {
	name       string
	config     GracePeriodConfig
	source     string
	onFailure  SLOOnFailure
	timeout    time.Duration
	retries    int
	retryDelay time.Duration
	httpClient *http.Client
}

// NewGracePeriodGate returns a GracePeriodGate built from cfg. The phase is
// always PhaseGracePeriod. Duration values above MaxGracePeriodDuration are
// clamped to the maximum; a negative Duration is treated as 0. Override the
// Prometheus tuning (source, timeout, retries, retry delay, http client,
// on-failure policy) with the provided options.
func NewGracePeriodGate(name string, cfg GracePeriodConfig, opts ...GracePeriodGateOption) *GracePeriodGate {
	// Clamp the duration into [0, MaxGracePeriodDuration].
	if cfg.Duration < 0 {
		cfg.Duration = 0
	}
	if cfg.Duration > MaxGracePeriodDuration {
		cfg.Duration = MaxGracePeriodDuration
	}

	g := &GracePeriodGate{
		name:       name,
		config:     cfg,
		source:     DefaultSLOSource,
		onFailure:  parseOnFailure(cfg.OnFailure),
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
func (g *GracePeriodGate) Name() string { return g.name }

// Phase returns PhaseGracePeriod; a GracePeriodGate always runs in the
// grace_period phase.
func (g *GracePeriodGate) Phase() GatePhase { return PhaseGracePeriod }

// Check waits for the configured cool-down and then queries every SLO. It
// returns Passed == true only when all queries are within their thresholds.
//
// When Duration == 0 the gate short-circuits to a no-op pass without issuing
// any query, so that non-critical workflows can opt out of the grace period
// by setting Duration: 0 in their configuration.
func (g *GracePeriodGate) Check(ctx context.Context, input GateInput) (GateResult, error) {
	if err := ctx.Err(); err != nil {
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("grace_period gate %q cancelled before run: %v", g.name, err),
			Details: map[string]any{
				"gate":   "grace_period",
				"name":   g.name,
				"reason": "context_cancelled",
				"cause":  err.Error(),
			},
		}, nil
	}

	details := map[string]any{
		"gate":        "grace_period",
		"name":        g.name,
		"source":      g.source,
		"on_failure":  string(g.onFailure),
		"duration_ms": g.config.Duration.Milliseconds(),
		"query_count": len(g.config.SLOQueries),
		"run_id":      input.RunID,
	}

	// Short-circuit on Duration == 0: no cool-down, no query, no failure.
	if g.config.Duration == 0 {
		details["reason"] = "duration_zero_skipped"
		return GateResult{
			Passed:  true,
			Message: fmt.Sprintf("grace_period gate %q skipped (duration=0)", g.name),
			Details: details,
		}, nil
	}

	// Cool-down. sleepCtx returns false as soon as ctx is cancelled so that
	// the gate does not leak a goroutine when the run is aborted mid-wait.
	// We do not hold any connection during the sleep: the Prometheus client
	// is only used after the sleep finishes.
	start := time.Now()
	if !sleepCtx(ctx, g.config.Duration) {
		details["reason"] = "cancelled_during_cooldown"
		details["cause"] = ctx.Err().Error()
		details["waited_ms"] = time.Since(start).Milliseconds()
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("grace_period gate %q cancelled during cool-down: %v", g.name, ctx.Err()),
			Details: details,
		}, nil
	}
	details["waited_ms"] = time.Since(start).Milliseconds()

	// Validate query configuration now that the cool-down has elapsed.
	if len(g.config.SLOQueries) == 0 {
		details["reason"] = "no_queries_configured"
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("grace_period gate %q has no queries configured", g.name),
			Details: details,
		}, fmt.Errorf("grace_period gate %q: no queries configured", g.name)
	}

	// Issue every query. The structure mirrors PreApplySLOGate.Check so that
	// the two gates share a behavioural contract.
	perQuery := make([]map[string]any, 0, len(g.config.SLOQueries))
	firstFailure := ""
	for i, spec := range g.config.SLOQueries {
		if err := ctx.Err(); err != nil {
			details["reason"] = "context_cancelled"
			details["cause"] = err.Error()
			details["queries"] = perQuery
			return GateResult{
				Passed:  false,
				Message: fmt.Sprintf("grace_period gate %q cancelled mid-run: %v", g.name, err),
				Details: details,
			}, nil
		}

		qr := g.runQuery(ctx, spec, i+1)
		perQuery = append(perQuery, qr)

		if !qr["passed"].(bool) {
			if firstFailure == "" {
				firstFailure = fmt.Sprintf("query %d (%s) failed: %s", i+1, queryLabel(spec), qr["message"].(string))
			}
			log.Warn("grace_period slo query failed",
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
			Message: fmt.Sprintf("grace_period gate %q passed: %d/%d queries within threshold", g.name, len(g.config.SLOQueries), len(g.config.SLOQueries)),
			Details: details,
		}, nil
	}

	details["reason"] = "threshold_breached_or_query_failed"
	return g.applyPolicy(details, "grace_period gate %q failed: %s", fmt.Errorf("%s", firstFailure)), nil
}

// runQuery executes a single SLO query spec with retries and returns a
// details map describing the outcome. The structure mirrors
// PreApplySLOGate.runQuery so that the two gates produce comparable audit
// trails.
func (g *GracePeriodGate) runQuery(ctx context.Context, spec SLOQuerySpec, idx int) map[string]any {
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
			log.Debug("grace_period slo query attempt error",
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
			log.Debug("grace_period slo query attempt mismatch",
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

// queryPromOnce issues a single instant query against the Prometheus HTTP API.
func (g *GracePeriodGate) queryPromOnce(ctx context.Context, query string) (float64, error) {
	u, err := url.JoinPath(g.source, "/api/v1/query")
	if err != nil {
		return 0, fmt.Errorf("grace_period gate %q: invalid source URL %q: %w", g.name, g.source, err)
	}

	attemptCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("grace_period gate %q: build request: %w", g.name, err)
	}
	q := req.URL.Query()
	q.Set("query", query)
	req.URL.RawQuery = q.Encode()

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("grace_period gate %q: http get: %w", g.name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("grace_period gate %q: read body: %w", g.name, err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("grace_period gate %q: prometheus returned status %d: %s",
			g.name, resp.StatusCode, truncateBody(body))
	}

	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, fmt.Errorf("grace_period gate %q: decode response: %w", g.name, err)
	}
	if pr.Status != "success" {
		return 0, fmt.Errorf("grace_period gate %q: prometheus status %q: %s",
			g.name, pr.Status, pr.Error)
	}

	value, err := pr.Data.scalarValue()
	if err != nil {
		return 0, fmt.Errorf("grace_period gate %q: %w", g.name, err)
	}
	return value, nil
}

// applyPolicy maps a failure through the on-failure policy. It mirrors
// PreApplySLOGate.applyPolicy so that the two gates share a behavioural
// contract.
func (g *GracePeriodGate) applyPolicy(details map[string]any, format string, cause error) GateResult {
	details["policy"] = string(g.onFailure)
	details["cause"] = cause.Error()
	msg := fmt.Sprintf(format, g.name, cause)

	switch g.onFailure {
	case OnFailureSkip:
		details["reason"] = "skipped_by_policy"
		return GateResult{
			Passed:  true,
			Message: fmt.Sprintf("grace_period gate %q skipped by policy (on_failure=skip): %v", g.name, cause),
			Details: details,
		}
	case OnFailureWarn:
		details["reason"] = "warn_by_policy"
		return GateResult{
			Passed:  false,
			Message: fmt.Sprintf("grace_period gate %q WARNING (on_failure=warn): %v", g.name, cause),
			Details: details,
		}
	default:
		details["reason"] = "blocked_by_policy"
		return GateResult{
			Passed:  false,
			Message: msg,
			Details: details,
		}
	}
}
