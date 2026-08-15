package calendar

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =========================================================================
// ParseCron
// =========================================================================

func TestParseCron_BasicFields(t *testing.T) {
	sched, err := ParseCron("0 2 * * *")
	require.NoError(t, err)
	assert.Equal(t, []int{0}, sched.minute)
	assert.Equal(t, []int{2}, sched.hour)
	assert.Equal(t, "0 2 * * *", sched.String())
	assert.False(t, sched.domRestricted)
	assert.False(t, sched.dowRestricted)
}

func TestParseCron_StarField(t *testing.T) {
	sched, err := ParseCron("* * * * *")
	require.NoError(t, err)
	assert.Len(t, sched.minute, 60)
	assert.Equal(t, 0, sched.minute[0])
	assert.Equal(t, 59, sched.minute[59])
}

func TestParseCron_Step(t *testing.T) {
	sched, err := ParseCron("*/15 * * * *")
	require.NoError(t, err)
	assert.Equal(t, []int{0, 15, 30, 45}, sched.minute)
}

func TestParseCron_Range(t *testing.T) {
	sched, err := ParseCron("0 9-17 * * 1-5")
	require.NoError(t, err)
	assert.Equal(t, []int{9, 10, 11, 12, 13, 14, 15, 16, 17}, sched.hour)
	assert.Equal(t, []int{1, 2, 3, 4, 5}, sched.dow)
	assert.True(t, sched.dowRestricted)
}

func TestParseCron_RangeWithStep(t *testing.T) {
	sched, err := ParseCron("0 0-23/4 * * *")
	require.NoError(t, err)
	assert.Equal(t, []int{0, 4, 8, 12, 16, 20}, sched.hour)
}

func TestParseCron_List(t *testing.T) {
	sched, err := ParseCron("0,30 0 * * *")
	require.NoError(t, err)
	assert.Equal(t, []int{0, 30}, sched.minute)
}

func TestParseCron_ListWithRanges(t *testing.T) {
	sched, err := ParseCron("0,15-20,45 * * * *")
	require.NoError(t, err)
	assert.Equal(t, []int{0, 15, 16, 17, 18, 19, 20, 45}, sched.minute)
}

func TestParseCron_DowNormalises7To0(t *testing.T) {
	sched, err := ParseCron("0 0 * * 7")
	require.NoError(t, err)
	assert.Equal(t, []int{0}, sched.dow, "7 should normalise to Sunday (0)")
}

func TestParseCron_DomAndDowBothRestricted(t *testing.T) {
	sched, err := ParseCron("0 0 1 * 1")
	require.NoError(t, err)
	assert.True(t, sched.domRestricted)
	assert.True(t, sched.dowRestricted)
}

func TestParseCron_Errors(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{"empty", ""},
		{"too few fields", "0 2 * *"},
		{"too many fields", "0 2 * * * *"},
		{"out of range minute", "60 * * * *"},
		{"out of range hour", "0 24 * * *"},
		{"out of range dom", "0 0 32 * *"},
		{"out of range month", "0 0 * 13 *"},
		{"invalid step zero", "*/0 * * * *"},
		{"invalid range", "0 5-1 * * *"},
		{"non-numeric", "a * * * *"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCron(tc.expr)
			require.Error(t, err)
		})
	}
}

// =========================================================================
// NextOccurrence
// =========================================================================

func TestNextOccurrence_DailyAt2AM(t *testing.T) {
	sched, err := ParseCron("0 2 * * *")
	require.NoError(t, err)

	after := mustParseTime(t, "2026-08-16T10:00:00Z")
	next, err := NextOccurrence(sched, after)
	require.NoError(t, err)
	expected := mustParseTime(t, "2026-08-17T02:00:00Z")
	assert.True(t, expected.Equal(next), "next should be 2026-08-17T02:00:00Z, got %s", next)
}

func TestNextOccurrence_Every15Min(t *testing.T) {
	sched, err := ParseCron("*/15 * * * *")
	require.NoError(t, err)

	after := mustParseTime(t, "2026-08-16T10:07:00Z")
	next, err := NextOccurrence(sched, after)
	require.NoError(t, err)
	expected := mustParseTime(t, "2026-08-16T10:15:00Z")
	assert.True(t, expected.Equal(next), "next should be 10:15, got %s", next)
}

func TestNextOccurrence_WeekdaysOnly(t *testing.T) {
	sched, err := ParseCron("0 9 * * 1-5")
	require.NoError(t, err)

	// 2026-08-16 is a Sunday.
	after := mustParseTime(t, "2026-08-15T10:00:00Z") // Saturday
	next, err := NextOccurrence(sched, after)
	require.NoError(t, err)
	expected := mustParseTime(t, "2026-08-17T09:00:00Z") // Monday
	assert.True(t, expected.Equal(next), "next should be Monday 09:00, got %s", next)
}

func TestNextOccurrence_StrictlyAfter(t *testing.T) {
	sched, err := ParseCron("0 2 * * *")
	require.NoError(t, err)

	// Exactly at 02:00 — next should be the following day, not the same.
	after := mustParseTime(t, "2026-08-16T02:00:00Z")
	next, err := NextOccurrence(sched, after)
	require.NoError(t, err)
	expected := mustParseTime(t, "2026-08-17T02:00:00Z")
	assert.True(t, expected.Equal(next), "next should be next day, got %s", next)
}

func TestNextOccurrence_NilSchedule(t *testing.T) {
	_, err := NextOccurrence(nil, time.Now())
	require.Error(t, err)
}

func TestNextOccurrence_MonthlyFirst(t *testing.T) {
	sched, err := ParseCron("0 0 1 * *")
	require.NoError(t, err)

	after := mustParseTime(t, "2026-08-16T10:00:00Z")
	next, err := NextOccurrence(sched, after)
	require.NoError(t, err)
	expected := mustParseTime(t, "2026-09-01T00:00:00Z")
	assert.True(t, expected.Equal(next), "next should be Sep 1, got %s", next)
}

// =========================================================================
// ExpandWindow
// =========================================================================

func TestExpandWindow_NonRecurring_Overlapping(t *testing.T) {
	base := sampleWindow("win-1")
	base.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	base.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	base.CronExpr = ""

	from := mustParseTime(t, "2026-08-16T11:00:00Z")
	to := mustParseTime(t, "2026-08-16T13:00:00Z")
	got, err := ExpandWindow(base, from, to)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "win-1", got[0].ID)
}

func TestExpandWindow_NonRecurring_Disjoint(t *testing.T) {
	base := sampleWindow("win-1")
	base.StartTime = mustParseTime(t, "2026-08-16T10:00:00Z")
	base.EndTime = mustParseTime(t, "2026-08-16T12:00:00Z")
	base.CronExpr = ""

	from := mustParseTime(t, "2026-08-16T14:00:00Z")
	to := mustParseTime(t, "2026-08-16T16:00:00Z")
	got, err := ExpandWindow(base, from, to)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestExpandWindow_Recurring(t *testing.T) {
	base := sampleWindow("win-1")
	base.StartTime = mustParseTime(t, "2026-08-16T02:00:00Z")
	base.EndTime = mustParseTime(t, "2026-08-16T03:00:00Z")
	base.CronExpr = "0 2 * * *" // daily at 02:00

	from := mustParseTime(t, "2026-08-16T00:00:00Z")
	to := mustParseTime(t, "2026-08-19T00:00:00Z")
	got, err := ExpandWindow(base, from, to)
	require.NoError(t, err)
	require.Len(t, got, 3, "should expand to 3 daily instances")

	// Each instance has a derived ID.
	assert.Equal(t, "win-1#1", got[0].ID)
	assert.Equal(t, "win-1#2", got[1].ID)
	assert.Equal(t, "win-1#3", got[2].ID)

	// First instance at 2026-08-16T02:00:00Z.
	assert.True(t, mustParseTime(t, "2026-08-16T02:00:00Z").Equal(got[0].StartTime))
	assert.True(t, mustParseTime(t, "2026-08-17T02:00:00Z").Equal(got[1].StartTime))
	assert.True(t, mustParseTime(t, "2026-08-18T02:00:00Z").Equal(got[2].StartTime))

	// Duration preserved.
	for _, inst := range got {
		assert.Equal(t, time.Hour, inst.EndTime.Sub(inst.StartTime))
		assert.Equal(t, base.Name, inst.Name)
		assert.Equal(t, base.TargetLabels, inst.TargetLabels)
	}
}

func TestExpandWindow_Recurring_InvalidCron(t *testing.T) {
	base := sampleWindow("win-1")
	base.CronExpr = "not a cron"

	from := mustParseTime(t, "2026-08-16T00:00:00Z")
	to := mustParseTime(t, "2026-08-17T00:00:00Z")
	_, err := ExpandWindow(base, from, to)
	require.Error(t, err)
}

func TestExpandWindow_InvalidRange(t *testing.T) {
	base := sampleWindow("win-1")
	from := mustParseTime(t, "2026-08-17T00:00:00Z")
	to := mustParseTime(t, "2026-08-16T00:00:00Z")
	_, err := ExpandWindow(base, from, to)
	require.Error(t, err)
}

func TestExpandWindow_NilBase(t *testing.T) {
	from := mustParseTime(t, "2026-08-16T00:00:00Z")
	to := mustParseTime(t, "2026-08-17T00:00:00Z")
	_, err := ExpandWindow(nil, from, to)
	require.Error(t, err)
}

// =========================================================================
// Field parsing helpers
// =========================================================================

func TestParseField_StepRange(t *testing.T) {
	got, err := parseField("10-30/10", 0, 59)
	require.NoError(t, err)
	assert.Equal(t, []int{10, 20, 30}, got)
}

func TestParseField_OutOfRange(t *testing.T) {
	_, err := parseField("70", 0, 59)
	require.Error(t, err)
}

func TestParseField_Empty(t *testing.T) {
	_, err := parseField("", 0, 59)
	require.Error(t, err)
}
