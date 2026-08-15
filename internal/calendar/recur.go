// Recurring-window expansion for the LEVEE change calendar.
//
// This file implements a self-contained 5-field cron parser (min hour day
// month weekday) so the calendar subsystem has zero external dependencies.
// The parser supports the standard syntax:
//
//   - "*"           — any value
//   - "N"           — a single value
//   - "A-B"         — an inclusive range
//   - "A,B,C"       — a list of values / ranges
//   - "*/S"         — every S-th value within the field's full range
//   - "A-B/S"       — every S-th value within [A, B]
//
// Day-of-month and day-of-week are OR-combined per POSIX cron: when both
// are restricted (neither is "*"), a date matches if either field matches.
//
// The parser deliberately rejects named tokens (MON, JAN, @reboot, L, W, #)
// to keep the implementation small. LEVEE windows only need numeric cron.

package calendar

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// =========================================================================
// CronSchedule
// =========================================================================

// CronSchedule is a parsed 5-field cron expression. Each field is a sorted
// slice of allowed values within the field's natural range. domRestricted and
// dowRestricted record whether the original field was something other than
// "*", which affects the day-of-month / day-of-week OR-combination rule.
type CronSchedule struct {
	minute         []int
	hour           []int
	dom            []int // day of month, 1..31
	month          []int // 1..12
	dow            []int // day of week, 0..6 (0 = Sunday)
	domRestricted  bool
	dowRestricted  bool
	original       string
}

// String returns the original cron expression.
func (c *CronSchedule) String() string { return c.original }

// ParseCron parses a 5-field cron expression into a CronSchedule. Supported
// syntax is documented in the package comment. The expression must contain
// exactly five whitespace-separated fields.
func ParseCron(expr string) (*CronSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("calendar: cron %q: expected 5 fields, got %d", expr, len(fields))
	}
	sched := &CronSchedule{original: expr}

	var err error
	sched.minute, err = parseField(fields[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("calendar: cron %q: minute field: %w", expr, err)
	}
	sched.hour, err = parseField(fields[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("calendar: cron %q: hour field: %w", expr, err)
	}
	sched.dom, err = parseField(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("calendar: cron %q: day-of-month field: %w", expr, err)
	}
	sched.month, err = parseField(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("calendar: cron %q: month field: %w", expr, err)
	}
	// Day of week: cron allows 0-7 where both 0 and 7 mean Sunday. We
	// normalise to 0..6.
	dow, err := parseField(fields[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("calendar: cron %q: day-of-week field: %w", expr, err)
	}
	normalisedDow := make([]int, 0, len(dow))
	seen := make(map[int]bool, len(dow))
	for _, d := range dow {
		if d == 7 {
			d = 0
		}
		if !seen[d] {
			seen[d] = true
			normalisedDow = append(normalisedDow, d)
		}
	}
	sort.Ints(normalisedDow)
	sched.dow = normalisedDow

	sched.domRestricted = fields[2] != "*"
	sched.dowRestricted = fields[4] != "*"
	return sched, nil
}

// parseField parses a single cron field into a sorted slice of allowed values
// within [min, max]. Supports "*", "N", "A-B", "A,B,C", "*/S", "A-B/S".
func parseField(field string, min, max int) ([]int, error) {
	if field == "" {
		return nil, fmt.Errorf("empty field")
	}
	var result []int
	seen := make(map[int]bool)

	parts := strings.Split(field, ",")
	for _, part := range parts {
		vals, err := parseFieldPart(part, min, max)
		if err != nil {
			return nil, err
		}
		for _, v := range vals {
			if v < min || v > max {
				return nil, fmt.Errorf("value %d out of range [%d, %d]", v, min, max)
			}
			if !seen[v] {
				seen[v] = true
				result = append(result, v)
			}
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("field %q produced no values", field)
	}
	sort.Ints(result)
	return result, nil
}

// parseFieldPart parses one comma-separated component of a cron field.
func parseFieldPart(part string, min, max int) ([]int, error) {
	// Split off the step suffix (/S).
	rangePart := part
	step := 1
	if idx := strings.Index(part, "/"); idx >= 0 {
		rangePart = part[:idx]
		s, err := strconv.Atoi(part[idx+1:])
		if err != nil {
			return nil, fmt.Errorf("invalid step %q: %w", part[idx+1:], err)
		}
		if s <= 0 {
			return nil, fmt.Errorf("step must be > 0, got %d", s)
		}
		step = s
	}

	var lo, hi int
	if rangePart == "*" {
		lo, hi = min, max
	} else if strings.Contains(rangePart, "-") {
		dash := strings.Index(rangePart, "-")
		a, err := strconv.Atoi(rangePart[:dash])
		if err != nil {
			return nil, fmt.Errorf("invalid range start %q: %w", rangePart[:dash], err)
		}
		b, err := strconv.Atoi(rangePart[dash+1:])
		if err != nil {
			return nil, fmt.Errorf("invalid range end %q: %w", rangePart[dash+1:], err)
		}
		if a > b {
			return nil, fmt.Errorf("range start %d > end %d", a, b)
		}
		lo, hi = a, b
	} else {
		n, err := strconv.Atoi(rangePart)
		if err != nil {
			return nil, fmt.Errorf("invalid value %q: %w", rangePart, err)
		}
		lo, hi = n, n
	}

	var out []int
	for v := lo; v <= hi; v += step {
		out = append(out, v)
	}
	return out, nil
}

// =========================================================================
// NextOccurrence
// =========================================================================

// NextOccurrence returns the earliest time strictly after `after` that
// matches the schedule. The result is in UTC. If no match exists within
// five years of `after` the function returns an error; this guards against
// pathological schedules (e.g. "0 0 30 2 *" — Feb 30 never exists) looping
// forever.
func NextOccurrence(schedule *CronSchedule, after time.Time) (time.Time, error) {
	if schedule == nil {
		return time.Time{}, fmt.Errorf("calendar: next occurrence: nil schedule")
	}
	after = after.UTC()

	// Start from the minute after `after`, truncated to the minute.
	t := after.Add(time.Minute).Truncate(time.Minute).UTC()
	deadline := after.Add(5 * 365 * 24 * time.Hour)

	for t.Before(deadline) {
		if schedule.matches(t) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("calendar: next occurrence: no match within 5 years of %s", after.Format(time.RFC3339))
}

// matches reports whether t satisfies all five cron fields.
func (c *CronSchedule) matches(t time.Time) bool {
	if !contains(c.minute, t.Minute()) {
		return false
	}
	if !contains(c.hour, t.Hour()) {
		return false
	}
	if !contains(c.month, int(t.Month())) {
		return false
	}
	// Day-of-month and day-of-week are OR-combined when both are restricted.
	domMatch := contains(c.dom, t.Day())
	dowMatch := contains(c.dow, int(t.Weekday()))
	if c.domRestricted && c.dowRestricted {
		if !domMatch && !dowMatch {
			return false
		}
	} else if c.domRestricted {
		if !domMatch {
			return false
		}
	} else if c.dowRestricted {
		if !dowMatch {
			return false
		}
	}
	return true
}

// contains reports whether xs contains v. xs is assumed sorted.
func contains(xs []int, v int) bool {
	idx := sort.SearchInts(xs, v)
	return idx < len(xs) && xs[idx] == v
}

// =========================================================================
// ExpandWindow
// =========================================================================

// ExpandWindow expands a recurring window into concrete instances whose
// [StartTime, EndTime) intervals intersect [from, to]. Non-recurring windows
// (CronExpr == "") are returned as a single instance if their interval
// intersects the range, otherwise nil.
//
// Each returned instance has a derived ID of the form "<baseID>#<n>" where n
// is the 1-based occurrence index, and StartTime/EndTime set to the concrete
// slot. All other fields (Name, TargetLabels, IsFrozen, etc.) are copied from
// the base window. CreatedAt/UpdatedAt are preserved from the base window.
//
// At most 1000 instances are returned to bound runtime for very dense
// schedules over very wide ranges.
func ExpandWindow(base *Window, from, to time.Time) ([]*Window, error) {
	if base == nil {
		return nil, fmt.Errorf("calendar: expand window: nil base")
	}
	from = from.UTC()
	to = to.UTC()
	if !from.Before(to) {
		return nil, fmt.Errorf("calendar: expand window: from %s must be before to %s",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	if base.CronExpr == "" {
		// Non-recurring: include if it overlaps [from, to).
		if base.EndTime.Before(from) || base.StartTime.After(to) {
			return nil, nil
		}
		copy := *base
		return []*Window{&copy}, nil
	}

	schedule, err := ParseCron(base.CronExpr)
	if err != nil {
		return nil, fmt.Errorf("calendar: expand window: %w", err)
	}

	dur := base.EndTime.Sub(base.StartTime)
	if dur <= 0 {
		dur = time.Hour
	}

	var out []*Window
	// We walk forward from `from - dur` so that an instance whose start is
	// before `from` but whose end extends into the range is still captured.
	cursor := from.Add(-dur).Truncate(time.Minute).UTC()
	for i := 1; i <= 1000; i++ {
		next, err := NextOccurrence(schedule, cursor)
		if err != nil {
			break
		}
		instStart := next
		instEnd := instStart.Add(dur)
		if !instStart.Before(to) {
			break
		}
		// Include if the instance overlaps [from, to).
		if !instEnd.Before(from) && instStart.Before(to) {
			inst := *base
			inst.ID = fmt.Sprintf("%s#%d", base.ID, i)
			inst.StartTime = instStart
			inst.EndTime = instEnd
			out = append(out, &inst)
		}
		cursor = next
	}
	return out, nil
}