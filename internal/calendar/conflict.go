// Conflict detection for the LEVEE change calendar.
//
// Two windows conflict when their [StartTime, EndTime) intervals overlap AND
// they share at least one target label. Conflicts are advisory: the engine
// does not hard-block creation of overlapping windows, but it surfaces them
// so a human approver can confirm before proceeding.
//
// To keep the MVP fast for the expected scale (tens to low hundreds of
// windows) we build an inverted index from target label -> window IDs at
// the start of each detection pass. Lookup is then O(k) per candidate where
// k is the number of windows sharing any of the candidate's labels.

package calendar

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Conflict describes an overlap between two windows that share at least one
// target label. SharedTargets is the intersection of the two windows'
// TargetLabels (sorted, deduplicated). OverlapStart/OverlapEnd describe the
// overlapping interval; OverlapStart is inclusive, OverlapEnd is exclusive.
type Conflict struct {
	WindowAID     string    `json:"window_a_id"`
	WindowBID     string    `json:"window_b_id"`
	OverlapStart  time.Time `json:"overlap_start"`
	OverlapEnd    time.Time `json:"overlap_end"`
	SharedTargets []string  `json:"shared_targets"`
}

// DetectConflicts returns all conflicts between the supplied candidate window
// and the windows already in the store. The candidate is treated as new: it is
// not compared against itself even if an identical ID already exists in the
// store. Recurring windows (CronExpr != "") are expanded into concrete
// instances within [candidate.StartTime, candidate.EndTime] before comparison
// so a weekly recurring window that lands inside the candidate is flagged.
//
// The returned slice is sorted by OverlapStart ascending. An empty slice
// means no conflicts.
func (s *CalendarService) DetectConflicts(ctx context.Context, candidate *Window) ([]Conflict, error) {
	if candidate == nil {
		return nil, fmt.Errorf("calendar: detect conflicts: nil candidate")
	}
	existing, err := s.store.ListWindows(ctx, WindowFilter{})
	if err != nil {
		return nil, fmt.Errorf("calendar: list windows for conflict check: %w", err)
	}
	return detectConflicts(candidate, existing), nil
}

// detectConflicts is the pure-function core of DetectConflicts. It is split
// out so tests can drive it without a store.
func detectConflicts(candidate *Window, existing []*Window) []Conflict {
	// Build inverted index: label -> window IDs that contain it.
	index := make(map[string][]*Window)
	for _, w := range existing {
		if w.ID == candidate.ID {
			continue
		}
		for _, label := range w.TargetLabels {
			index[label] = append(index[label], w)
		}
	}

	// Collect candidate windows to compare. For recurring candidates we
	// expand a single instance (the candidate itself) — full expansion is
	// the caller's responsibility via ExpandWindow when needed. Here we
	// only need to compare the candidate's own interval against existing
	// windows; recurring existing windows are expanded below.
	candStart := candidate.StartTime.UTC()
	candEnd := candidate.EndTime.UTC()

	// Gather unique candidate labels.
	candLabels := uniqueSorted(candidate.TargetLabels)

	var conflicts []Conflict
	seen := make(map[string]bool) // dedupe by (aID,bID) pair

	for _, label := range candLabels {
		for _, other := range index[label] {
			// Expand recurring `other` into concrete instances that could
			// overlap the candidate. For non-recurring windows this returns
			// a single instance equal to `other`.
			instances := expandForOverlap(other, candStart, candEnd)
			for _, inst := range instances {
				ovStart, ovEnd, ok := overlapInterval(candStart, candEnd, inst.StartTime, inst.EndTime)
				if !ok {
					continue
				}
				shared := intersectSorted(candidate.TargetLabels, inst.TargetLabels)
				if len(shared) == 0 {
					continue
				}
				pairKey := pairKey(candidate.ID, other.ID)
				if seen[pairKey] {
					continue
				}
				seen[pairKey] = true
				conflicts = append(conflicts, Conflict{
					WindowAID:     candidate.ID,
					WindowBID:     other.ID,
					OverlapStart:  ovStart,
					OverlapEnd:    ovEnd,
					SharedTargets: shared,
				})
			}
		}
	}

	sort.Slice(conflicts, func(i, j int) bool {
		if !conflicts[i].OverlapStart.Equal(conflicts[j].OverlapStart) {
			return conflicts[i].OverlapStart.Before(conflicts[j].OverlapStart)
		}
		return conflicts[i].WindowBID < conflicts[j].WindowBID
	})
	return conflicts
}

// CheckWindowForPlan validates a planned change against the calendar at plan
// time. It returns all conflicts between a hypothetical window covering
// [plannedTime, plannedTime+duration) on the given target labels and the
// existing windows in the store. The result is advisory: the plan phase
// surfaces conflicts to the user but does not hard-block.
//
// duration <= 0 is treated as an instantaneous change (1 second window).
func (s *CalendarService) CheckWindowForPlan(ctx context.Context, targetLabels []string, plannedTime time.Time, duration time.Duration) ([]Conflict, error) {
	if duration <= 0 {
		duration = time.Second
	}
	candidate := &Window{
		ID:           "__plan_candidate__",
		Name:         "plan-candidate",
		StartTime:    plannedTime.UTC(),
		EndTime:      plannedTime.UTC().Add(duration),
		TargetLabels: targetLabels,
	}
	return s.DetectConflicts(ctx, candidate)
}

// =========================================================================
// Helpers
// =========================================================================

// expandForOverlap returns concrete window instances derived from w that
// could overlap [candStart, candEnd). For non-recurring windows it returns
// the window itself if its interval intersects the candidate range, otherwise
// nil. For recurring windows it expands the cron schedule over the candidate
// range and returns each concrete instance.
func expandForOverlap(w *Window, candStart, candEnd time.Time) []*Window {
	if w.CronExpr == "" {
		// Non-recurring: include only if it could possibly overlap.
		if w.EndTime.Before(candStart) || w.StartTime.After(candEnd) {
			return nil
		}
		return []*Window{w}
	}
	// Recurring: expand instances over the candidate range. We expand a
	// slightly wider window to catch instances whose start is before
	// candStart but whose end (start + window duration) extends into the
	// candidate.
	dur := w.EndTime.Sub(w.StartTime)
	if dur <= 0 {
		dur = time.Hour
	}
	from := candStart.Add(-dur)
	to := candEnd
	instances, err := ExpandWindow(w, from, to)
	if err != nil {
		// If cron expansion fails, fall back to the raw window.
		return []*Window{w}
	}
	return instances
}

// overlapInterval computes the intersection of [aStart, aEnd) and
// [bStart, bEnd). It returns (start, end, true) when the intersection is
// non-empty, otherwise (_, _, false).
func overlapInterval(aStart, aEnd, bStart, bEnd time.Time) (time.Time, time.Time, bool) {
	start := aStart
	if bStart.After(start) {
		start = bStart
	}
	end := aEnd
	if bEnd.Before(end) {
		end = bEnd
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}

// uniqueSorted returns the unique elements of xs in sorted order.
func uniqueSorted(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// intersectSorted returns the sorted, deduplicated intersection of a and b.
// Both inputs may be unsorted; the result is always sorted.
func intersectSorted(a, b []string) []string {
	set := make(map[string]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	out := make([]string, 0)
	seen := make(map[string]bool, len(b))
	for _, x := range b {
		if _, ok := set[x]; ok && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// pairKey returns a stable, order-independent key for a pair of window IDs.
func pairKey(a, b string) string {
	if a < b {
		return a + "|" + b
	}
	return b + "|" + a
}
