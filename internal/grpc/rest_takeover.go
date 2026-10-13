// rest_takeover.go — the takeover event timeline the cluster page reads.
//
// REST-only, for the same reason as /system/batch-status and
// /system/change-trend: the payload is a timeline's rows, which is a view
// concern, and a protoc regeneration per shape change is not a trade worth
// making. The alternative — extending ListAuditTraces with an `action` filter
// — would put a cluster-observability query into the audit RPC's contract, and
// that RPC's consumers (the audit page) do not want the extra axis.
//
// Why this exists at all: internal/takeover settles an expired run by CAS'ing
// it running→interrupted under a takeover lock, and records a trace row
// (event "interrupted_by_takeover", actor "cluster-takeover"). state.TraceFilter
// can already filter on Event, so the events are queryable — but nothing
// QUERIED them: grep found no producer for that event outside the takeover
// package itself, and no read route in the gateway. The operator therefore had
// no way to see that a run was interrupted BY THE CLUSTER rather than by a
// human or a failure, which is the difference between "the executor died and
// the cluster noticed" and "someone cancelled it" — exactly the question the
// cluster page exists to answer.

package grpc

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nexus/levee/internal/state"
)

// TakeoverEvent is one settled takeover, flattened for a timeline row.
//
// It is a projection of state.Trace, not the trace itself: the timeline shows
// when, which run, and who settled it — the trace's chain columns (prev_hash /
// curr_hash) belong to the audit page, which is where the operator goes when
// they want to verify the chain rather than read the sequence.
type TakeoverEvent struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	Actor     string `json:"actor"`
	Timestamp int64  `json:"timestamp"`
	// Status is the run's current status, read alongside the event. An
	// interrupted run that has since been resumed is a different story from one
	// still sitting interrupted, and the timeline must not imply the latter when
	// it is the former.
	Status string `json:"status"`
}

// handleTakeoverEvents serves GET /system/takeover-events.
//
// Authorisation follows the read rule every other list endpoint uses: reads
// require `view`, judged in permission.default_env (the cluster registry is a
// deployment-wide resource, exactly like the node list it sits beside on the
// cluster page).
//
// The window is bounded and the row count is capped, like the trend: this reads
// the trace table, whose size is proportional to the deployment's history. A
// window longer than the cap is clamped rather than refused, because the
// caller asked for "recent events" and "recent" is still true when clamped.
func (gw *Gateway) handleTakeoverEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if gw.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no store configured")
		return
	}
	if err := authorizeResourceRead(ctx, gw.authz, gw.authz.ResolveEnv(""), "GetTakeoverEvents"); err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	}

	q := r.URL.Query()
	days := 7
	if raw := strings.TrimSpace(q.Get("days")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeJSONError(w, http.StatusBadRequest, "days must be a positive integer")
			return
		}
		days = n
	}
	if days > maxTakeoverWindowDays {
		days = maxTakeoverWindowDays
	}
	limit := defaultTakeoverLimit
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > maxTakeoverLimit {
		limit = maxTakeoverLimit
	}

	since := time.Now().UTC().AddDate(0, 0, -days)
	traces, err := gw.store.ListTraces(ctx, state.TraceFilter{Event: state.EventTakeoverInterrupted, Limit: limit})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list takeover traces: "+err.Error())
		return
	}

	// The run's own status, so a resumed run is not reported as still
	// interrupted. Fetched per event because the trace row does not carry the
	// run's CURRENT state (it records the moment of settlement, not the
	// aftermath). A run that has since been deleted reads as unknown rather
	// than being dropped: the takeover happened, and hiding it because the run
	// row is gone would make the timeline lie by omission.
	type runStatus struct {
		status string
		found  bool
	}
	statusByRun := make(map[string]runStatus, len(traces))
	for _, t := range traces {
		if _, ok := statusByRun[t.RunID]; ok {
			continue
		}
		st := runStatus{status: "unknown"}
		if run, rerr := gw.store.GetRun(ctx, t.RunID); rerr == nil && run != nil {
			st = runStatus{status: run.Status, found: true}
		}
		statusByRun[t.RunID] = st
	}

	events := make([]TakeoverEvent, 0, len(traces))
	for _, t := range traces {
		if t.Timestamp.Before(since) {
			continue
		}
		st := statusByRun[t.RunID]
		events = append(events, TakeoverEvent{
			ID:        t.ID,
			RunID:     t.RunID,
			Actor:     t.Actor,
			Timestamp: t.Timestamp.Unix(),
			Status:    st.status,
		})
	}

	writeJSON(w, takeoverEventsPayload{
		Events:     events,
		Days:       days,
		WindowFrom: since.Unix(),
	})
}

const (
	// defaultTakeoverWindowDays is the window when the caller asks for none.
	defaultTakeoverWindowDays = 7
	// maxTakeoverWindowDays bounds the scan: the trace table is unbounded.
	maxTakeoverWindowDays = 90
	// defaultTakeoverLimit / maxTakeoverLimit bound the row count. The timeline
	// shows the most recent settlements; a page that shows more is a log dump,
	// not a timeline.
	defaultTakeoverLimit = 100
	maxTakeoverLimit     = 500
)

type takeoverEventsPayload struct {
	Events     []TakeoverEvent `json:"events"`
	Days       int             `json:"days"`
	WindowFrom int64           `json:"window_from"`
}
