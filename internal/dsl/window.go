// window.go gives the LEVEELang change window (§4.2 of leveelang-spec.md) a
// grammar and a judgement. Both were missing: nothing in the repository read
// Workflow.Window, so `levee compile` accepted start "25:99" and timezone
// "Mars/Olympus_Mons", and a workflow declaring "02:00-06:00 Sun" planned and
// applied at Tuesday 22:00 without a word.
//
// Two entry points consume this file and must not disagree: internal/dsl's
// validator (is the declaration usable?) and the plan-time gate in
// internal/wiring (is this instant inside it?). The spec puts the check at
// plan time — "plan 时刻不在窗口内则阻断，不进审批" — and exempts rollback
// outright, so a failed change can never be stranded by a closed window.
package dsl

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	// Embed the IANA database. The gate fails closed when a timezone cannot
	// be resolved, and a container without /usr/share/zoneinfo (the scratch
	// dist stage, a distroless host, a busybox image) would otherwise refuse
	// every change that declares a non-UTC window.
	_ "time/tzdata"
)

// ErrWindowClosed marks a plan refused because the declared change window is
// shut. It travels through the wiring and gRPC layers as a wrapped error so
// each entry point can report a deliberate refusal (FailedPrecondition)
// instead of a fault (Internal).
var ErrWindowClosed = errors.New("change window is closed")

// windowDays is the spec's day vocabulary (§4.2: ["Mon","Tue",...]).
var windowDays = map[string]time.Weekday{
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
	"sun": time.Sunday,
}

// clock is a wall-clock minute of day, 0-1439.
type clock int

func (c clock) String() string { return fmt.Sprintf("%02d:%02d", int(c)/60, int(c)%60) }

// parsedWindow is a usable declaration: minutes, zone and owning days.
type parsedWindow struct {
	start, end    clock
	loc           *time.Location
	days          []time.Weekday
	spansMidnight bool
}

// Declared reports whether the workflow means to constrain time. A window
// block that names neither bound constrains nothing.
func (w ChangeWindow) Declared() bool {
	return strings.TrimSpace(w.Start) != "" || strings.TrimSpace(w.End) != ""
}

// Check lists every grammar problem in a declaration. An empty result means
// the window is usable; Declared()==false is not a problem, it is no window.
func (w ChangeWindow) Check() []ValidationError {
	if !w.Declared() {
		if strings.TrimSpace(w.Timezone) != "" || len(w.Days) > 0 {
			return []ValidationError{{
				Code: codeWindowClock, Field: "window.start",
				Message: "window declares timezone/days but no start and end: a window " +
					"without bounds constrains nothing, so planning would ignore it",
			}}
		}
		return nil
	}

	var issues []ValidationError
	s, serr := parseWindowClock(w.Start, "window.start")
	e, eerr := parseWindowClock(w.End, "window.end")
	if serr != nil {
		issues = append(issues, *serr)
	}
	if eerr != nil {
		issues = append(issues, *eerr)
	}
	if serr == nil && eerr == nil && *s == *e {
		issues = append(issues, ValidationError{
			Code:  codeWindowClock,
			Field: "window.end",
			Message: fmt.Sprintf("window start and end are both %s: an empty window that is open "+
				"nowhere reads the same as an all-day window that is open everywhere. For an "+
				"all-day window declare %s (see the spec's §4.2 example)", s, "00:00-23:59"),
		})
	}
	if _, zerr := windowLocation(w.Timezone); zerr != nil {
		issues = append(issues, *zerr)
	}
	issues = append(issues, checkWindowDays(w.Days)...)
	return issues
}

// OpenAt reports whether t falls inside the window. An undeclared window is
// open at every instant; an unusable declaration returns an error and callers
// must treat that as closed, because "cannot judge" must not become
// "automatically allowed" — that is how a declared constraint turns inert
// again.
func (w ChangeWindow) OpenAt(t time.Time) (bool, error) {
	if !w.Declared() {
		return true, nil
	}
	pw, issues := w.parse()
	if len(issues) > 0 {
		return false, fmt.Errorf("change window %q cannot be judged: %s",
			w.Describe(), issues[0].Message)
	}

	local := t.In(pw.loc)
	m := clock(local.Hour()*60 + local.Minute())

	// Wall-clock semantics in the declared zone: that is what a maintenance
	// window means to the operator who wrote it. A DST day that skips
	// 02:00-03:00 opens the window at the first instant past the gap; a
	// fall-back day that repeats it leaves the window open across the repeat.
	hourOpen := m >= pw.start && m < pw.end
	if pw.spansMidnight {
		// Evening half belongs to today, morning half to a listed day's tail.
		hourOpen = m >= pw.start || m < pw.end
	}
	if len(pw.days) == 0 {
		return hourOpen, nil
	}

	owner := local.Weekday()
	if pw.spansMidnight && m < pw.end {
		// 01:00 Tuesday inside "Mon 23:00 - Tue 02:00" belongs to Monday.
		owner = local.AddDate(0, 0, -1).Weekday()
	}
	for _, d := range pw.days {
		if d == owner {
			return hourOpen, nil
		}
	}
	return false, nil
}

// Describe renders the declaration for an operator-facing message.
func (w ChangeWindow) Describe() string {
	zone := strings.TrimSpace(w.Timezone)
	if zone == "" {
		zone = "UTC"
	}
	days := "every day"
	if len(w.Days) > 0 {
		days = "on " + strings.Join(w.Days, ", ")
	}
	span := fmt.Sprintf("%s-%s", strings.TrimSpace(w.Start), strings.TrimSpace(w.End))
	if !w.Declared() {
		span = "(no bounds)"
	}
	return fmt.Sprintf("%s %s, %s", span, zone, days)
}

// parse is Check's success path: it returns the usable form, or the issues
// that make the declaration unusable.
func (w ChangeWindow) parse() (parsedWindow, []ValidationError) {
	issues := w.Check()
	if len(issues) > 0 {
		return parsedWindow{}, issues
	}
	s, _ := parseWindowClock(w.Start, "window.start")
	e, _ := parseWindowClock(w.End, "window.end")
	loc, _ := windowLocation(w.Timezone)
	if loc == nil {
		loc = time.UTC
	}
	pw := parsedWindow{start: *s, end: *e, loc: loc, spansMidnight: *e < *s}
	for _, name := range w.Days {
		if wd, ok := windowDays[strings.ToLower(strings.TrimSpace(name))]; ok {
			pw.days = append(pw.days, wd)
		}
	}
	return pw, nil
}

// parseWindowClock reads the spec's "HH:MM" 24-hour form. It is deliberately
// strict about the two-digit shape: "2:00" and "02:00:00" both look like a
// time to a human and both would silently widen or narrow a maintenance
// window if accepted by a parser that then re-rendered them differently.
func parseWindowClock(raw, field string) (*clock, *ValidationError) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil, &ValidationError{
			Code: codeWindowClock, Field: field,
			Message: fmt.Sprintf("%s is required when a change window is declared (§4.2)", field),
		}
	}
	parts := strings.Split(v, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return nil, &ValidationError{
			Code: codeWindowClock, Field: field,
			Message: fmt.Sprintf("%s %q is not HH:MM (24-hour, two digits each, e.g. \"02:00\")", field, raw),
		}
	}
	h, herr := strconv.Atoi(parts[0])
	m, merr := strconv.Atoi(parts[1])
	if herr != nil || merr != nil || h > 23 || m > 59 {
		return nil, &ValidationError{
			Code: codeWindowClock, Field: field,
			Message: fmt.Sprintf("%s %q is not a valid 24-hour time (hour 00-23, minute 00-59)", field, raw),
		}
	}
	c := clock(h*60 + m)
	return &c, nil
}

// windowLocation resolves the zone, defaulting to UTC as §4.2 requires.
func windowLocation(raw string) (*time.Location, *ValidationError) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(v)
	if err != nil {
		return nil, &ValidationError{
			Code: codeWindowZone, Field: "window.timezone",
			Message: fmt.Sprintf("window.timezone %q is not an IANA zone name (§4.2 lists e.g. "+
				"\"Asia/Shanghai\", \"America/New_York\"); an unresolvable zone would silently "+
				"shift or block the window", raw),
		}
	}
	return loc, nil
}

// checkWindowDays validates the day list against the spec vocabulary. A typo
// has to fail at compile: "sunday" matched against "Sun" would match nothing,
// and a window that never opens is indistinguishable from a broken deployment.
func checkWindowDays(days []string) []ValidationError {
	known := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	var issues []ValidationError
	for _, d := range days {
		if _, ok := windowDays[strings.ToLower(strings.TrimSpace(d))]; !ok {
			quoted := make([]string, 0, len(known))
			for _, name := range known {
				quoted = append(quoted, `"`+name+`"`)
			}
			issues = append(issues, ValidationError{
				Code: codeEnumIllegal, Field: "window.days",
				Message: fmt.Sprintf("window.days %q is not a weekday (§4.2 allows %s, case-insensitive)",
					d, strings.Join(quoted, ", ")),
			})
		}
	}
	return issues
}
