package dsl

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 2026-09-28 is a Monday, 2026-09-29 a Tuesday, 2026-10-03 a Saturday.

func at(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

func TestWindowCheck_ClockGrammar(t *testing.T) {
	cases := []struct {
		name      string
		start, en string
		wantCode  string
		wantMsg   string
	}{
		{"both valid", "02:00", "06:00", "", ""},
		{"single digit hour", "2:00", "06:00", "LE020", "HH:MM"},
		{"seconds present", "02:00:00", "06:00", "LE020", "HH:MM"},
		{"hour out of range", "25:00", "06:00", "LE020", "24-hour"},
		{"minute out of range", "02:60", "06:00", "LE020", "24-hour"},
		{"not a clock", "0200hrs", "06:00", "LE020", "HH:MM"},
		{"rfc3339 start", "2024-01-01T02:00:00Z", "06:00", "LE020", "HH:MM"},
		{"missing end", "02:00", "", "LE020", "required"},
		{"missing start", "", "06:00", "LE020", "required"},
		{"equal bounds", "02:00", "02:00", "LE020", "00:00-23:59"},
		{"cross midnight is legal", "23:00", "02:00", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := ChangeWindow{Start: tc.start, End: tc.en}.Check()
			if tc.wantCode == "" {
				assert.Empty(t, issues, "declaration must be usable")
				return
			}
			require.NotEmpty(t, issues)
			assert.Equal(t, tc.wantCode, issues[0].Code)
			assert.Contains(t, issues[0].Message, tc.wantMsg,
				"the refusal has to name what the author wrote wrong")
		})
	}
}

func TestWindowCheck_OnlyStartEndMattersForDeclaration(t *testing.T) {
	// A timezone or a day list without bounds cannot be judged: it is either a
	// typo or a half-written window, and silently unconstrained is the worst
	// reading of it.
	issues := ChangeWindow{Timezone: "Asia/Shanghai", Days: []string{"Sat"}}.Check()
	require.Len(t, issues, 1)
	assert.Equal(t, "LE020", issues[0].Code)
	assert.Contains(t, issues[0].Message, "constrains nothing")

	// Nothing declared at all is a legal, unbounded workflow.
	assert.Empty(t, ChangeWindow{}.Check())
}

func TestWindowCheck_Timezone(t *testing.T) {
	// A valid IANA zone and the empty default are both usable.
	assert.Empty(t, ChangeWindow{Start: "02:00", End: "06:00", Timezone: "Asia/Shanghai"}.Check())
	assert.Empty(t, ChangeWindow{Start: "02:00", End: "06:00"}.Check())

	issues := ChangeWindow{Start: "02:00", End: "06:00", Timezone: "Mars/Olympus_Mons"}.Check()
	require.NotEmpty(t, issues)
	assert.Equal(t, "LE021", issues[0].Code)
	assert.Contains(t, issues[0].Message, "IANA")
}

func TestWindowCheck_Days(t *testing.T) {
	// The spec's vocabulary, and case-insensitive acceptance of it.
	for _, days := range [][]string{{"Sat", "Sun"}, {"sat", "SUN"}, {"Mon"}} {
		assert.Empty(t, ChangeWindow{Start: "02:00", End: "06:00", Days: days}.Check(),
			"%v must be accepted", days)
	}

	issues := ChangeWindow{Start: "02:00", End: "06:00", Days: []string{"sunday"}}.Check()
	require.NotEmpty(t, issues)
	assert.Equal(t, "LE003", issues[0].Code)
	assert.Contains(t, issues[0].Message, "weekday",
		"a day name that matches nothing would leave a window that never opens")
}

func TestWindowOpenAt_SameDayWindowBoundaries(t *testing.T) {
	w := ChangeWindow{Start: "02:00", End: "06:00"}

	open, err := w.OpenAt(at(2026, time.September, 28, 2, 0))
	require.NoError(t, err)
	assert.True(t, open, "start is inclusive")

	open, err = w.OpenAt(at(2026, time.September, 28, 5, 59))
	require.NoError(t, err)
	assert.True(t, open)

	open, err = w.OpenAt(at(2026, time.September, 28, 6, 0))
	require.NoError(t, err)
	assert.False(t, open, "end is exclusive")

	open, err = w.OpenAt(at(2026, time.September, 28, 1, 59))
	require.NoError(t, err)
	assert.False(t, open)
}

func TestWindowOpenAt_CrossMidnightWindowOwnsItsOpeningDay(t *testing.T) {
	// The spec's weekday-night window (leveelang-spec.md §4.2 example):
	// 23:00-02:00 on Mon..Fri. days names the day the window OPENS on.
	w := ChangeWindow{
		Start: "23:00", End: "02:00",
		Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri"},
	}

	inside := []time.Time{
		at(2026, time.September, 28, 23, 30), // Mon evening
		at(2026, time.September, 29, 1, 0),   // Tue 01:00 — the tail of Monday
		at(2026, time.October, 2, 23, 45),    // Fri evening
		at(2026, time.October, 3, 1, 30),     // Sat 01:30 — the tail of Friday
	}
	for _, ts := range inside {
		open, err := w.OpenAt(ts)
		require.NoError(t, err)
		assert.True(t, open, "%s must be inside the weekday night window", ts.Format(time.RFC3339))
	}

	outside := []time.Time{
		at(2026, time.September, 28, 22, 59), // Mon 22:59 — window has not opened
		at(2026, time.September, 28, 1, 0),   // Mon 01:00 — Sunday's tail, Sunday is not listed
		at(2026, time.October, 3, 23, 30),    // Sat evening — not a listed day
		at(2026, time.October, 4, 1, 0),      // Sun 01:00 — the tail of Saturday
		at(2026, time.October, 5, 12, 0),     // Mon midday
	}
	for _, ts := range outside {
		open, err := w.OpenAt(ts)
		require.NoError(t, err)
		assert.False(t, open, "%s must be outside the weekday night window", ts.Format(time.RFC3339))
	}
}

func TestWindowOpenAt_TimezoneDrivesBothClockAndDay(t *testing.T) {
	// 02:00-06:00 Asia/Shanghai. 2026-09-28 20:00 UTC is 2026-09-29 04:00 in
	// Shanghai: inside the hours AND a listed day, while UTC says it is
	// Monday 20:00 and outside. Judging this in UTC would pass one half and
	// fail the other, so both are asserted.
	w := ChangeWindow{Start: "02:00", End: "06:00", Timezone: "Asia/Shanghai", Days: []string{"Tue"}}

	open, err := w.OpenAt(at(2026, time.September, 28, 20, 0))
	require.NoError(t, err)
	assert.True(t, open, "04:00 Tuesday in Shanghai is inside a Tuesday 02:00-06:00 window")

	open, err = w.OpenAt(at(2026, time.September, 28, 3, 0))
	require.NoError(t, err)
	assert.False(t, open, "03:00 UTC is 11:00 in Shanghai — the window is shut")

	// 2026-09-28 17:30 UTC is 09:30 Monday in Shanghai: wrong hour, and the
	// day the window belongs to is Monday, not in days.
	open, err = w.OpenAt(at(2026, time.September, 28, 17, 30))
	require.NoError(t, err)
	assert.False(t, open)
}

func TestWindowOpenAt_UndeclaredWindowIsAlwaysOpen(t *testing.T) {
	w := ChangeWindow{}
	for _, ts := range []time.Time{
		at(2026, time.September, 28, 0, 0),
		at(2026, time.October, 3, 15, 4),
		time.Now(),
	} {
		open, err := w.OpenAt(ts)
		require.NoError(t, err)
		assert.True(t, open, "no window means no constraint (§4.2)")
	}
}

func TestWindowOpenAt_UnusableDeclarationRefusesInsteadOfAllowing(t *testing.T) {
	// The plan path does not run the validator (only `levee compile` and the
	// conversation bridge do), so a garbage window can reach OpenAt. It must
	// refuse: defaulting "cannot judge" to "allowed" is how the declared
	// constraint becomes inert again.
	for _, bad := range []ChangeWindow{
		{Start: "25:99", End: "06:00"},
		{Start: "02:00", End: "Mars", Timezone: "UTC"},
		{Start: "02:00", End: "02:00"},
	} {
		open, err := bad.OpenAt(time.Now())
		require.Error(t, err, "%v must not be judged silently", bad)
		assert.False(t, open, "an undecidable window is not an open window")
		assert.Contains(t, err.Error(), "cannot be judged")
	}
}

func TestWindowDescribe(t *testing.T) {
	assert.Equal(t, "02:00-06:00 UTC, on Sat, Sun",
		ChangeWindow{Start: "02:00", End: "06:00", Days: []string{"Sat", "Sun"}}.Describe())
	assert.Contains(t, ChangeWindow{Start: "02:00", End: "06:00"}.Describe(), "every day")
	assert.Contains(t, ChangeWindow{}.Describe(), "(no bounds)")
}

// TestWindowIssuesCarryStableCodes guards the operator-facing contract: the
// codes are what the docs and `levee authz explain`-style tooling quote.
func TestWindowIssuesCarryStableCodes(t *testing.T) {
	issues := ChangeWindow{Start: "2:00", End: "24:00", Timezone: "Nope/Nada", Days: []string{"Funday"}}.Check()
	require.Len(t, issues, 4) // both clocks, the zone, the day
	seen := map[string]bool{}
	for _, is := range issues {
		seen[is.Code] = true
		assert.NotEmpty(t, is.Field, "every issue must name the field to fix")
	}
	for _, code := range []string{"LE020", "LE021", "LE003"} {
		assert.True(t, seen[code], "code %s must be produced", code)
	}
}

// TestValidatorActuallyCallsWindowCheck is the boundary half: Check() being
// correct is worthless if no admission path calls it. That is precisely how the
// whole window used to behave — parsed, documented, never consulted.
func TestValidatorActuallyCallsWindowCheck(t *testing.T) {
	windowErrs := func(w ChangeWindow) map[string]string {
		wf := &Workflow{
			Meta:    WorkflowMeta{Name: "window-via-validator"},
			Targets: []TargetGroup{{Name: "web", Hosts: []string{"h1"}}},
			Steps:   []Step{{Name: "restart", Module: "svc", Action: "restart"}},
			Window:  w,
		}
		out := map[string]string{}
		for _, e := range NewValidator().Validate(wf) {
			if strings.HasPrefix(e.Field, "window.") {
				out[e.Field] = e.Code
			}
		}
		return out
	}

	assert.Equal(t, map[string]string{
		"window.start":    "LE020",
		"window.timezone": "LE021",
		"window.days":     "LE003",
	}, windowErrs(ChangeWindow{Start: "25:99", End: "06:00", Timezone: "Mars/Olympus_Mons", Days: []string{"funday"}}),
		"Validate must surface each window defect under its own code and field")

	// The call must not over-reject: a usable window and an absent window are
	// both legal (§4.2 treats no window as no constraint).
	assert.Empty(t, windowErrs(ChangeWindow{Start: "23:00", End: "02:00", Days: []string{"Fri"}}))
	assert.Empty(t, windowErrs(ChangeWindow{}))

	// The strict entry point the conversation bridge uses has to agree, or the
	// two admission gates drift the way the batch-strategy vocabulary did.
	err := NewValidator().ValidateStrict(&Workflow{
		Meta:    WorkflowMeta{Name: "strict-window"},
		Targets: []TargetGroup{{Name: "web", Hosts: []string{"h1"}}},
		Steps:   []Step{{Name: "restart", Module: "svc", Action: "restart"}},
		Window:  ChangeWindow{Start: "2:00", End: "06:00"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LE020")
}
