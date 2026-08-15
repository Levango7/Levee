package calendar

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =========================================================================
// Pure-function conflict detection
// =========================================================================

func TestDetectConflicts_NoOverlap(t *testing.T) {
	cand := sampleWindow("cand")
	cand.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	cand.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	cand.TargetLabels = []string{"web"}

	other := sampleWindow("other")
	other.StartTime = mustParseTime(t, "2026-08-16T14:00:00Z")
	other.EndTime = mustParseTime(t, "2026-08-16T16:00:00Z")
	other.TargetLabels = []string{"web"}

	conflicts := detectConflicts(cand, []*Window{other})
	assert.Empty(t, conflicts)
}

func TestDetectConflicts_OverlapButDisjointTargets(t *testing.T) {
	cand := sampleWindow("cand")
	cand.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	cand.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	cand.TargetLabels = []string{"web"}

	other := sampleWindow("other")
	other.StartTime = mustParseTime(t, "2026-08-16T11:00:00Z")
	other.EndTime = mustParseTime(t, "2026-08-16T13:00:00Z")
	other.TargetLabels = []string{"db"}

	conflicts := detectConflicts(cand, []*Window{other})
	assert.Empty(t, conflicts, "overlapping windows with disjoint targets do not conflict")
}

func TestDetectConflicts_OverlapSharedTarget(t *testing.T) {
	cand := sampleWindow("cand")
	cand.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	cand.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	cand.TargetLabels = []string{"web", "db"}

	other := sampleWindow("other")
	other.StartTime = mustParseTime(t, "2026-08-16T11:00:00Z")
	other.EndTime = mustParseTime(t, "2026-08-16T13:00:00Z")
	other.TargetLabels = []string{"db", "cache"}

	conflicts := detectConflicts(cand, []*Window{other})
	require.Len(t, conflicts, 1)
	c := conflicts[0]
	assert.Equal(t, "cand", c.WindowAID)
	assert.Equal(t, "other", c.WindowBID)
	assert.True(t, mustParseTime(t, "2026-08-16T11:00:00Z").Equal(c.OverlapStart))
	assert.True(t, mustParseTime(t, "2026-08-16T12:00:00Z").Equal(c.OverlapEnd))
	assert.Equal(t, []string{"db"}, c.SharedTargets)
}

func TestDetectConflicts_MultipleSharedTargetsSorted(t *testing.T) {
	cand := sampleWindow("cand")
	cand.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	cand.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	cand.TargetLabels = []string{"db", "web", "cache"}

	other := sampleWindow("other")
	other.StartTime = mustParseTime(t, "2026-08-16T11:00:00Z")
	other.EndTime = mustParseTime(t, "2026-08-16T13:00:00Z")
	other.TargetLabels = []string{"web", "db"}

	conflicts := detectConflicts(cand, []*Window{other})
	require.Len(t, conflicts, 1)
	assert.Equal(t, []string{"db", "web"}, conflicts[0].SharedTargets)
}

func TestDetectConflicts_MultipleConflictingWindows(t *testing.T) {
	cand := sampleWindow("cand")
	cand.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	cand.EndTime = mustParseTime(t, "2026-08-16T14:00:00Z")
	cand.TargetLabels = []string{"web"}

	mkOther := func(id string, start, end string) *Window {
		w := sampleWindow(id)
		w.StartTime = mustParseTime(t, start)
		w.EndTime = mustParseTime(t, end)
		w.TargetLabels = []string{"web"}
		return w
	}
	existing := []*Window{
		mkOther("a", "2026-08-16T09:00:00Z", "2026-08-16T11:00:00Z"),
		mkOther("b", "2026-08-16T11:30:00Z", "2026-08-16T13:00:00Z"),
		mkOther("c", "2026-08-16T15:00:00Z", "2026-08-16T16:00:00Z"), // no overlap
	}

	conflicts := detectConflicts(cand, existing)
	require.Len(t, conflicts, 2, "should detect 2 overlapping windows")
	// Sorted by OverlapStart ascending.
	assert.Equal(t, "a", conflicts[0].WindowBID)
	assert.Equal(t, "b", conflicts[1].WindowBID)
}

func TestDetectConflicts_SkipsSelf(t *testing.T) {
	cand := sampleWindow("self")
	cand.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	cand.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	cand.TargetLabels = []string{"web"}

	// An existing window with the same ID should not conflict with itself.
	conflicts := detectConflicts(cand, []*Window{cand})
	assert.Empty(t, conflicts)
}

func TestDetectConflicts_DedupesPairs(t *testing.T) {
	cand := sampleWindow("cand")
	cand.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	cand.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	cand.TargetLabels = []string{"web", "db"}

	other := sampleWindow("other")
	other.StartTime = mustParseTime(t, "2026-08-16T11:00:00Z")
	other.EndTime = mustParseTime(t, "2026-08-16T13:00:00Z")
	other.TargetLabels = []string{"web", "db"}

	// Even though both "web" and "db" match, only one conflict should be
	// emitted for the (cand, other) pair.
	conflicts := detectConflicts(cand, []*Window{other})
	require.Len(t, conflicts, 1)
	assert.ElementsMatch(t, []string{"db", "web"}, conflicts[0].SharedTargets)
}

func TestDetectConflicts_NilCandidate(t *testing.T) {
	// The pure function panics on nil; the service method returns an error.
	assert.Panics(t, func() { _ = detectConflicts(nil, nil) })
}

// =========================================================================
// Service-level DetectConflicts
// =========================================================================

func TestService_DetectConflicts(t *testing.T) {
	store := newTestStore(t)
	svc := NewCalendarService(store)
	ctx := context.Background()

	existing := sampleWindow("existing")
	existing.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	existing.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	existing.TargetLabels = []string{"web"}
	require.NoError(t, svc.CreateWindow(ctx, existing))

	cand := sampleWindow("cand")
	cand.StartTime = mustParseTime(t, "2026-08-16T11:00:00Z")
	cand.EndTime = mustParseTime(t, "2026-08-16T13:00:00Z")
	cand.TargetLabels = []string{"web"}

	conflicts, err := svc.DetectConflicts(ctx, cand)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "existing", conflicts[0].WindowBID)
}

func TestService_DetectConflicts_NilCandidate(t *testing.T) {
	store := newTestStore(t)
	svc := NewCalendarService(store)
	_, err := svc.DetectConflicts(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil candidate")
}

// =========================================================================
// CheckWindowForPlan
// =========================================================================

func TestService_CheckWindowForPlan(t *testing.T) {
	store := newTestStore(t)
	svc := NewCalendarService(store)
	ctx := context.Background()

	// Existing window 10:00..12:00 on "web".
	existing := sampleWindow("existing")
	existing.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	existing.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	existing.TargetLabels = []string{"web"}
	require.NoError(t, svc.CreateWindow(ctx, existing))

	// Plan a change at 11:00 for 1 hour on "web" — should conflict.
	conflicts, err := svc.CheckWindowForPlan(ctx, []string{"web"},
		mustParseTime(t, "2026-08-16T11:00:00Z"), time.Hour)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)

	// Plan a change at 13:00 — no conflict.
	conflicts2, err := svc.CheckWindowForPlan(ctx, []string{"web"},
		mustParseTime(t, "2026-08-16T13:00:00Z"), time.Hour)
	require.NoError(t, err)
	assert.Empty(t, conflicts2)

	// Plan a change on a different target — no conflict.
	conflicts3, err := svc.CheckWindowForPlan(ctx, []string{"db"},
		mustParseTime(t, "2026-08-16T11:00:00Z"), time.Hour)
	require.NoError(t, err)
	assert.Empty(t, conflicts3)
}

func TestService_CheckWindowForPlan_DefaultDuration(t *testing.T) {
	store := newTestStore(t)
	svc := NewCalendarService(store)
	ctx := context.Background()

	existing := sampleWindow("existing")
	existing.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	existing.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	existing.TargetLabels = []string{"web"}
	require.NoError(t, svc.CreateWindow(ctx, existing))

	// duration <= 0 should be treated as 1 second.
	conflicts, err := svc.CheckWindowForPlan(ctx, []string{"web"},
		mustParseTime(t, "2026-08-16T11:00:00Z"), 0)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
}

// =========================================================================
// Helpers
// =========================================================================

func TestOverlapInterval(t *testing.T) {
	a := mustParseTime(t, "2026-08-16T10:00:00Z")
	b := mustParseTime(t, "2026-08-16T12:00:00Z")
	c := mustParseTime(t, "2026-08-16T11:00:00Z")
	d := mustParseTime(t, "2026-08-16T13:00:00Z")

	// Overlapping.
	start, end, ok := overlapInterval(a, b, c, d)
	require.True(t, ok)
	assert.True(t, c.Equal(start))
	assert.True(t, b.Equal(end))

	// Disjoint.
	_, _, ok = overlapInterval(a, b, d, d.Add(time.Hour))
	assert.False(t, ok)

	// Touching at a single point (half-open => no overlap).
	_, _, ok = overlapInterval(a, b, b, d)
	assert.False(t, ok)
}

func TestIntersectSorted(t *testing.T) {
	got := intersectSorted([]string{"db", "web"}, []string{"web", "db", "cache"})
	assert.Equal(t, []string{"db", "web"}, got)

	got = intersectSorted([]string{"a"}, []string{"b"})
	assert.Empty(t, got)

	got = intersectSorted(nil, []string{"b"})
	assert.Empty(t, got)
}

func TestUniqueSorted(t *testing.T) {
	got := uniqueSorted([]string{"b", "a", "b", "c", "a"})
	assert.Equal(t, []string{"a", "b", "c"}, got)
}

func TestPairKey(t *testing.T) {
	assert.Equal(t, pairKey("a", "b"), pairKey("b", "a"))
	assert.NotEqual(t, pairKey("a", "b"), pairKey("a", "c"))
}