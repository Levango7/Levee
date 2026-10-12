// rest_trend.go — the change-outcome trend the console's charts read.
//
// REST-only, exactly like /system/batch-status: the payload is a chart's
// (day × outcome) buckets, which is a view concern, and putting it in the proto
// would mean a protoc regeneration every time the chart changes shape.

package grpc

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/nexus/levee/internal/state"
)

// handleChangeTrend serves GET /system/change-trend.
//
// Authorisation follows the read rule the other list endpoints use
// (resource_authz.go): reads require `view`, and the window is scoped to ONE
// environment rather than narrowed by a predicate — because "which
// environments may this subject see" has no enumeration API, and answering it
// by totalling every environment would be the aggregate equivalent of the
// per-row leak the read gate exists to prevent. So:
//
//   - env given    -> that environment, judged like any other read (403 if not).
//   - env absent   -> permission.default_env, the same fallback the gates use.
//   - neither      -> every environment, which only a caller the deployment
//     does not restrict (no matrix, or an unprovable identity) reaches.
//
// The console therefore puts an environment selector on the chart rather than
// promising a single global series: "the trend of the environment I am looking
// at" is also the question an operator actually asks.
func (gw *Gateway) handleChangeTrend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if gw.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no store configured")
		return
	}

	q := r.URL.Query()
	// Resolve BEFORE the gate, and use the resolved value for the store filter
	// too: the gate judges permission.default_env when env is empty, so
	// filtering the aggregate by "" (every environment) would hand a caller
	// entitled to one environment every environment's counts under a gate that
	// only ever saw the default.
	env := gw.authz.ResolveEnv(strings.TrimSpace(q.Get("env")))
	if err := authorizeResourceRead(ctx, gw.authz, env, "GetChangeTrend"); err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	}

	filter := state.RunTrendFilter{Env: env}
	if days := strings.TrimSpace(q.Get("days")); days != "" {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			writeJSONError(w, http.StatusBadRequest, "days must be a positive integer")
			return
		}
		filter.Days = n
	}
	// The day boundary belongs to the OPERATOR, not to the server: a chart
	// bucketed on the server's timezone files a UTC+8 operator's night work
	// under the wrong date. The browser sends getTimezoneOffset() (UTC minus
	// local, so UTC+8 is -480) and the store converts it.
	if raw := strings.TrimSpace(q.Get("tz_offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < -840 || n > 840 {
			writeJSONError(w, http.StatusBadRequest, "tz_offset must be minutes within ±14h")
			return
		}
		filter.OffsetMinutes = n
	}

	buckets, truncated, err := gw.store.RunTrend(ctx, filter)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "change trend: "+err.Error())
		return
	}
	if buckets == nil {
		buckets = []state.RunTrendBucket{}
	}
	// The legend order travels with the payload so no client can invent its own
	// and drift from every other trend on the console.
	writeJSON(w, changeTrendPayload{
		Buckets:   buckets,
		Series:    state.TrendOutcomes,
		Truncated: truncated,
	})
}

// changeTrendPayload is what /system/change-trend returns.
type changeTrendPayload struct {
	Buckets   []state.RunTrendBucket `json:"buckets"`
	Series    []string               `json:"series"`
	Truncated bool                   `json:"truncated"`
}
