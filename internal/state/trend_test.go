package state

import (
	"context"
	"testing"
	"time"

	"github.com/nexus/levee/internal/runstatus"
)

// The trend's worth is entirely in its day boundaries and its outcome
// rollup, so those are what these tests pin. The bucketing lives in Go
// (trend.go explains why it cannot live in SQL on SQLite), which makes it
// testable without a database — and the window predicate is exercised against
// a real SQLite store below, because that is where the driver's storage format
// could have surprised us (it did: see TestProbeTimeStorage's findings).
func TestBucketRuns_DayBoundariesFollowTheCallersTimezone(t *testing.T) {
	// Three runs whose UTC day and UTC+8 day differ — the whole reason the
	// bucketing is not done in SQL (UTC day) on one backend and in SQL on the
	// other. A UTC+8 operator's 00:00-08:00 activity is their working day, and
	// a UTC bucket would file it under yesterday.
	//   23:30 UTC 10-12 -> 07:30 10-13 local
	//   01:30 UTC 10-13 -> 09:30 10-13 local
	//   20:00 UTC 10-13 -> 04:00 10-14 local
	runs := []*Run{
		{Status: runstatus.StatusCompleted, CreatedAt: time.Date(2026, 10, 12, 23, 30, 0, 0, time.UTC)},
		{Status: runstatus.StatusFailed, CreatedAt: time.Date(2026, 10, 13, 1, 30, 0, 0, time.UTC)},
		{Status: runstatus.StatusCompleted, CreatedAt: time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)},
	}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	utc := BucketRuns(runs, 3, 0, now)
	if utc[0].Day != "2026-10-12" || utc[0].Total != 1 {
		t.Fatalf("utc: want 1 run on 10-12, got %+v", utc[0])
	}
	if utc[1].Day != "2026-10-13" || utc[1].Total != 2 {
		t.Fatalf("utc: want 2 runs on 10-13, got %+v", utc[1])
	}

	cst := BucketRuns(runs, 3, -480, now) // UTC+8 passes -480 (JS convention)
	if cst[1].Day != "2026-10-13" || cst[1].Total != 2 {
		t.Fatalf("utc+8: want 2 runs on 10-13, got %+v", cst[1])
	}
	if cst[2].Day != "2026-10-14" || cst[2].Total != 1 {
		t.Fatalf("utc+8: want the 04:00 run on 10-14, got %+v", cst[2])
	}
}

func TestBucketRuns_EmptyDaysArePresent(t *testing.T) {
	// A gap in a trend chart is information: "nothing ran that day" must not
	// disappear just because it has no rows.
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	buckets := BucketRuns(nil, 5, 0, now)
	if len(buckets) != 5 {
		t.Fatalf("want 5 buckets for a 5-day window, got %d", len(buckets))
	}
	for i, b := range buckets {
		if b.Total != 0 || len(b.Outcomes) != 0 {
			t.Fatalf("bucket %d should be empty: %+v", i, b)
		}
	}
	// Consecutive days, ending today.
	if buckets[4].Day != "2026-10-14" || buckets[0].Day != "2026-10-10" {
		t.Fatalf("window does not end today: %s .. %s", buckets[0].Day, buckets[4].Day)
	}
}

func TestBucketRuns_IgnoresRowsOutsideTheWindow(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	runs := []*Run{
		{Status: runstatus.StatusCompleted, CreatedAt: now.Add(-24 * time.Hour)},
		{Status: runstatus.StatusCompleted, CreatedAt: now.Add(-30 * 24 * time.Hour)}, // too old
		{Status: runstatus.StatusCompleted, CreatedAt: now.Add(48 * time.Hour)},       // during the scan
	}
	buckets := BucketRuns(runs, 3, 0, now)
	total := 0
	for _, b := range buckets {
		total += b.Total
	}
	if total != 1 {
		t.Fatalf("want only the in-window row counted, got %d", total)
	}
}

func TestOutcomeOf_RollsEveryStatusIntoFiveSeries(t *testing.T) {
	cases := map[string]string{
		runstatus.StatusCompleted:          OutcomeSuccess,
		runstatus.StatusFailed:             OutcomeFailed,
		runstatus.StatusInterrupted:        OutcomeFailed,
		runstatus.StatusRollbackIncomplete: OutcomeFailed,
		runstatus.StatusRolledBack:         OutcomeRolledBack,
		runstatus.StatusRolledBackPartial:  OutcomeRolledBack,
		runstatus.StatusRunning:            OutcomeActive,
		runstatus.StatusApproved:           OutcomeActive,
		runstatus.StatusPaused:             OutcomeActive,
		runstatus.StatusPending:            OutcomeActive,
		runstatus.StatusPlanned:            OutcomeActive,
		runstatus.StatusDraft:              OutcomeActive,
		runstatus.StatusCancelled:          OutcomeStopped,
		runstatus.StatusRejected:           OutcomeStopped,
		runstatus.StatusArchived:           OutcomeStopped,
	}
	for status, want := range cases {
		if got := OutcomeOf(status); got != want {
			t.Errorf("OutcomeOf(%q) = %q, want %q", status, got, want)
		}
	}
	// An unknown status is "something we cannot name yet", not a success or a
	// failure: inventing a bucket per new status would change the chart's shape
	// under the operator when a status is added.
	if got := OutcomeOf("brand-new-status"); got != OutcomeActive {
		t.Errorf("unknown status should land in %q, got %q", OutcomeActive, got)
	}
}

func TestTrendOutcomes_LegendOrderIsWorstLast(t *testing.T) {
	// The render order is the legend order; success first keeps a stacked bar's
	// readable end at the top and puts failure where the eye lands.
	want := []string{OutcomeSuccess, OutcomeActive, OutcomeRolledBack, OutcomeFailed, OutcomeStopped}
	if len(TrendOutcomes) != len(want) {
		t.Fatalf("want %d series, got %d", len(want), len(TrendOutcomes))
	}
	for i := range want {
		if TrendOutcomes[i] != want[i] {
			t.Fatalf("series %d = %q, want %q", i, TrendOutcomes[i], want[i])
		}
	}
}

// --- the window predicate, against a real store -----------------------------

func newTrendStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(context.Background(), t.TempDir()+"/trend.db")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedRun(t *testing.T, s *SQLiteStore, id, status, env string, at time.Time) {
	t.Helper()
	err := s.CreateRun(context.Background(), &Run{
		ID: id, WorkflowName: "wf", TemplateName: "tpl", Status: status,
		CreatedAt: at, UpdatedAt: at, Creator: "liwg", IncidentID: env,
	})
	if err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
}

func TestRunTrend_WindowAndCap(t *testing.T) {
	store := newTrendStore(t)
	// Seeded relative to the wall clock: RunTrend windows off time.Now(), and
	// hard-coded dates drift out of the window the moment the calendar moves.
	now := time.Now()
	seedRun(t, store, "r1", runstatus.StatusCompleted, "prod", now.Add(-1*time.Hour))
	seedRun(t, store, "r2", runstatus.StatusFailed, "prod", now.Add(-2*time.Hour))
	seedRun(t, store, "r3", runstatus.StatusCompleted, "dev", now.Add(-3*time.Hour))
	seedRun(t, store, "old", runstatus.StatusCompleted, "prod", now.AddDate(0, 0, -60))

	ctx := context.Background()

	buckets, truncated, err := store.RunTrend(ctx, RunTrendFilter{Days: 3, OffsetMinutes: 0})
	if err != nil {
		t.Fatalf("RunTrend: %v", err)
	}
	if truncated {
		t.Fatal("a 3-row window must not report truncation")
	}
	total := 0
	for _, b := range buckets {
		total += b.Total
	}
	if total != 3 {
		t.Fatalf("want 3 in-window runs, got %d", total)
	}

	// Environment filter: the environment is carried in incident_id (envOf in
	// the change service reads the same column), so filtering by env must not
	// silently return everything.
	buckets, _, err = store.RunTrend(ctx, RunTrendFilter{Days: 3, OffsetMinutes: 0, Env: "prod"})
	if err != nil {
		t.Fatalf("RunTrend(env): %v", err)
	}
	total = 0
	for _, b := range buckets {
		total += b.Total
	}
	if total != 2 {
		t.Fatalf("want 2 prod runs, got %d", total)
	}

	// The cap is reported, not hidden: more rows than the limit must come back
	// as truncated=true, and the buckets hold exactly the cap.
	buckets, truncated, err = store.RunTrend(ctx, RunTrendFilter{Days: 3, OffsetMinutes: 0, Limit: 2})
	if err != nil {
		t.Fatalf("RunTrend(limit): %v", err)
	}
	if !truncated {
		t.Fatal("a window of 3 rows with Limit=2 must report truncation")
	}
	total = 0
	for _, b := range buckets {
		total += b.Total
	}
	if total != 2 {
		t.Fatalf("truncated trend must still bucket exactly the cap, got %d", total)
	}
}

func TestRunTrend_EmptyWindow(t *testing.T) {
	store := newTrendStore(t)
	buckets, truncated, err := store.RunTrend(context.Background(), RunTrendFilter{Days: 7})
	if err != nil {
		t.Fatalf("RunTrend: %v", err)
	}
	if truncated {
		t.Fatal("an empty window is not a truncated one")
	}
	if len(buckets) != 7 {
		t.Fatalf("want 7 buckets, got %d", len(buckets))
	}
}
