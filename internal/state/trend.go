// trend.go — the change-outcome trend the console's charts read.
//
// WHY THE BUCKETING IS IN GO AND NOT IN SQL (measured, not assumed): modernc
// sqlite stores a time.Time column as RFC3339 TEXT ("2026-10-12T23:30:00Z").
// Against that shape:
//
//   - strftime('%Y-%m-%d', created_at)            -> NULL
//   - strftime(..., '+480 minutes')               -> NULL
//   - datetime(replace(...))                      -> empty
//   - substr(created_at, 1, 10)                   -> works, UTC day only
//
// So SQLite can bucket by UTC day and nothing finer, while PG could do it
// properly in SQL (to_timestamp + interval). Two dialects bucketing the same
// rows on different day boundaries is a worse outcome than one Go function
// doing it identically for both: a UTC+8 operator's 00:00–08:00 activity would
// land under the wrong date, which is exactly the change window they care
// about. Both stores therefore return the WINDOW's rows — the window is an
// indexed range predicate on both backends (idx_runs_created_at on PG) — and
// BucketRuns buckets them in the caller's timezone.
//
// The window is bounded and capped: reading N rows to bucket them is O(N), so
// the filter carries a Limit and the response reports whether the cap was hit
// (a chart that silently drops rows is a chart that lies about volume).
package state

import (
	"context"
	"time"

	"github.com/nexus/levee/internal/runstatus"
)

// Outcome vocabulary. Five series, because they are the five questions an
// operator actually asks of a trend: what succeeded, what failed, what had to
// be rolled back, what is still moving, and what a human stopped on purpose.
// The fifteen run statuses roll up into these in OutcomeOf — a chart with one
// series per status would need fifteen legend entries and answer none of them.
const (
	OutcomeSuccess    = "success"
	OutcomeFailed     = "failed"
	OutcomeRolledBack = "rolled_back"
	OutcomeActive     = "active"
	OutcomeStopped    = "stopped"
)

// TrendOutcomes is the render order: worst first, so a stacked bar's top edge
// is the good news and the eye lands on trouble. It is also the legend order.
var TrendOutcomes = []string{
	OutcomeSuccess,
	OutcomeActive,
	OutcomeRolledBack,
	OutcomeFailed,
	OutcomeStopped,
}

// OutcomeOf rolls one run status into the trend vocabulary. An unrecognised
// value lands in OutcomeActive rather than a sixth bucket: an unknown status is
// "something we do not have a name for yet", not a failure or a success, and
// inventing a bucket per new status would make the chart's shape change under
// the operator when a status is added.
func OutcomeOf(status string) string {
	switch status {
	case runstatus.StatusCompleted:
		return OutcomeSuccess
	case runstatus.StatusFailed, runstatus.StatusInterrupted, runstatus.StatusRollbackIncomplete:
		return OutcomeFailed
	case runstatus.StatusRolledBack, runstatus.StatusRolledBackPartial:
		return OutcomeRolledBack
	case runstatus.StatusRunning, runstatus.StatusApproved, runstatus.StatusPaused,
		runstatus.StatusPending, runstatus.StatusPlanned, runstatus.StatusDraft:
		return OutcomeActive
	case runstatus.StatusCancelled, runstatus.StatusRejected, runstatus.StatusArchived:
		return OutcomeStopped
	default:
		return OutcomeActive
	}
}

// RunTrendFilter narrows the trend window. All fields are optional; the zero
// value means "last 14 days, every environment, no tenant predicate".
type RunTrendFilter struct {
	// Days is the window length ending at the caller's local today. <= 0
	// means the default (14). A window longer than 365 days is clamped: a
	// day-bucketed chart beyond a year is unreadable and the row scan behind
	// it grows without telling the operator anything.
	Days int
	// Env restricts the trend to one environment. Empty means all
	// environments — which the gateway only allows a caller entitled to see
	// all of them (see rest_trend.go).
	Env string
	// TenantID restricts results to one owning tenant. Empty means no tenant
	// predicate (single-tenant path); internal/tenant always sets it.
	TenantID string
	// OffsetMinutes is the caller's timezone offset in the JS
	// getTimezoneOffset() convention: UTC minus local, so UTC+8 passes -480.
	// It decides the DAY BOUNDARY only; stored timestamps are absolute.
	OffsetMinutes int
	// Limit caps how many rows the store reads to bucket. <= 0 means the
	// default (DefaultRunTrendLimit).
	Limit int
}

// DefaultRunTrendDays is the window the console asks for.
const DefaultRunTrendDays = 14

// DefaultRunTrendLimit caps the row scan. The trend is a trend, not a ledger:
// past this many runs in the window the endpoint says so instead of quietly
// charting a sample of the busiest days.
const DefaultRunTrendLimit = 20000

// MaxRunTrendDays is the clamp from RunTrendFilter.Days.
const MaxRunTrendDays = 365

// RunTrendBucket is one local day's counts, split by outcome.
type RunTrendBucket struct {
	Day      string         `json:"day"`
	Total    int            `json:"total"`
	Outcomes map[string]int `json:"outcomes"`
}

// dayKey is the bucket key in the caller's timezone.
func dayKey(t time.Time, offsetMinutes int) string {
	return t.In(fixedZone(offsetMinutes)).Format("2006-01-02")
}

// fixedZone builds the zone the caller's offset describes. A fixed zone is the
// right tool here: the offset was measured once, on the browser, and a chart
// whose day boundary moves with the server's DST rules would be worse than one
// that follows the operator.
func fixedZone(offsetMinutes int) *time.Location {
	return time.FixedZone("chart", -offsetMinutes*60)
}

// startOfWindow returns the first instant of the window: local midnight, Days
// days ago (i.e. the current day is included).
func startOfWindow(now time.Time, days, offsetMinutes int) time.Time {
	loc := fixedZone(offsetMinutes)
	local := now.In(loc)
	y, m, d := local.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, loc)
	return midnight.AddDate(0, 0, -(days - 1)).UTC()
}

// buildTrend is the shared half of RunTrend: normalise the window, run the
// backend's query, bucket the rows. Both stores call it so the window maths
// (and therefore the day boundaries) cannot drift between dialects.
//
// It returns the buckets, whether the row scan hit the cap, and any query
// error. `now` is injectable so tests do not depend on the wall clock.
func buildTrend(ctx context.Context, f RunTrendFilter, now time.Time, query func(context.Context, RunTrendFilter, time.Time) ([]*Run, bool, error)) ([]RunTrendBucket, bool, error) {
	days := f.Days
	if days <= 0 {
		days = DefaultRunTrendDays
	}
	if days > MaxRunTrendDays {
		days = MaxRunTrendDays
	}
	start := startOfWindow(now, days, f.OffsetMinutes)
	runs, truncated, err := query(ctx, f, start)
	if err != nil {
		return nil, false, err
	}
	return BucketRuns(runs, days, f.OffsetMinutes, now), truncated, nil
}

// BucketRuns rolls the window's runs into one bucket per local day, INCLUDING
// the days with no runs: a gap in a trend chart is information ("nothing ran
// that day"), and a chart that only drew days with data would read as a
// continuous series.
//
// runs may arrive in any order and may include rows outside the window (the
// store's window predicate and this function use the same clock, but a row
// written between the query and the bucketing must not be dropped silently) —
// out-of-window rows are ignored, and `now` decides what "today" is so the
// caller can pin it in tests.
func BucketRuns(runs []*Run, days, offsetMinutes int, now time.Time) []RunTrendBucket {
	if days <= 0 {
		days = DefaultRunTrendDays
	}
	if days > MaxRunTrendDays {
		days = MaxRunTrendDays
	}
	start := startOfWindow(now, days, offsetMinutes)

	// Index the buckets by day key so lookups do not scan.
	buckets := make([]RunTrendBucket, 0, days)
	index := make(map[string]int, days)
	loc := fixedZone(offsetMinutes)
	for i := 0; i < days; i++ {
		day := start.AddDate(0, 0, i).In(loc).Format("2006-01-02")
		index[day] = len(buckets)
		buckets = append(buckets, RunTrendBucket{
			Day:      day,
			Outcomes: map[string]int{},
		})
	}

	for _, r := range runs {
		if r == nil || r.CreatedAt.Before(start) {
			continue
		}
		k := dayKey(r.CreatedAt, offsetMinutes)
		pos, ok := index[k]
		if !ok {
			continue // after "today" — a row written during the scan
		}
		b := &buckets[pos]
		b.Total++
		b.Outcomes[OutcomeOf(r.Status)]++
	}
	return buckets
}
