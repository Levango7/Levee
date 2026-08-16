package alert

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSeverityString covers the Severity.String mapping.
func TestSeverityString(t *testing.T) {
	cases := []struct {
		sev  Severity
		want string
	}{
		{SeverityInfo, "info"},
		{SeverityWarning, "warning"},
		{SeverityCritical, "critical"},
		{Severity(99), "unknown"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.sev.String())
	}
}

// TestParseSeverity covers case-insensitive parsing and the error path.
func TestParseSeverity(t *testing.T) {
	cases := []struct {
		in     string
		want   Severity
		wantOk bool
	}{
		{"info", SeverityInfo, true},
		{"INFO", SeverityInfo, true},
		{"informational", SeverityInfo, true},
		{"warning", SeverityWarning, true},
		{"WARN", SeverityWarning, true},
		{"critical", SeverityCritical, true},
		{"crit", SeverityCritical, true},
		{"bogus", SeverityInfo, false},
		{"", SeverityInfo, false},
	}
	for _, c := range cases {
		got, err := ParseSeverity(c.in)
		if c.wantOk {
			require.NoError(t, err, "input %q", c.in)
			assert.Equal(t, c.want, got, "input %q", c.in)
		} else {
			assert.Error(t, err, "input %q", c.in)
		}
	}
}

// TestAlertStatusString covers AlertStatus.String.
func TestAlertStatusString(t *testing.T) {
	assert.Equal(t, "firing", StatusFiring.String())
	assert.Equal(t, "resolved", StatusResolved.String())
	assert.Equal(t, "unknown", AlertStatus(99).String())
}

// TestParseAlertStatus covers the status parser.
func TestParseAlertStatus(t *testing.T) {
	st, err := ParseAlertStatus("firing")
	require.NoError(t, err)
	assert.Equal(t, StatusFiring, st)

	st, err = ParseAlertStatus("RESOLVED")
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, st)

	_, err = ParseAlertStatus("weird")
	assert.Error(t, err)
}

// TestNewAlert verifies defaults and fingerprint population.
func TestNewAlert(t *testing.T) {
	now := time.Now()
	a := NewAlert("prometheus", "HighCpu", SeverityWarning, map[string]string{"host": "n1"}, now)
	assert.Equal(t, "prometheus", a.Source)
	assert.Equal(t, "HighCpu", a.Title)
	assert.Equal(t, SeverityWarning, a.Severity)
	assert.Equal(t, StatusFiring, a.Status)
	assert.NotEmpty(t, a.Fingerprint)
	assert.Equal(t, a.Fingerprint, a.ID)
	assert.True(t, a.IsFiring())
}

// TestAlertValidate covers every validation branch.
func TestAlertValidate(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		a    *Alert
		ok   bool
	}{
		{"good", &Alert{Source: "x", Title: "t", StartsAt: now, Severity: SeverityInfo}, true},
		{"nil", nil, false},
		{"empty source", &Alert{Title: "t", StartsAt: now}, false},
		{"empty title", &Alert{Source: "x", StartsAt: now}, false},
		{"zero starts", &Alert{Source: "x", Title: "t"}, false},
		{"bad severity", &Alert{Source: "x", Title: "t", StartsAt: now, Severity: Severity(7)}, false},
	}
	for _, c := range cases {
		err := c.a.Validate()
		if c.ok {
			assert.NoError(t, err, c.name)
		} else {
			assert.Error(t, err, c.name)
			assert.True(t, errors.Is(err, ErrInvalidAlert), c.name)
		}
	}
}

// TestGenerateFingerprintStable ensures the same inputs produce the same
// fingerprint and label order does not matter.
func TestGenerateFingerprintStable(t *testing.T) {
	a := &Alert{Source: "s", Title: "t", Severity: SeverityWarning, Labels: map[string]string{"a": "1", "b": "2"}}
	b := &Alert{Source: "s", Title: "t", Severity: SeverityWarning, Labels: map[string]string{"b": "2", "a": "1"}}
	assert.Equal(t, a.GenerateFingerprint(), b.GenerateFingerprint())

	// Different title -> different fingerprint.
	c := &Alert{Source: "s", Title: "u", Severity: SeverityWarning, Labels: a.Labels}
	assert.NotEqual(t, a.GenerateFingerprint(), c.GenerateFingerprint())
}

// TestAlertString covers the String formatter and the nil path.
func TestAlertString(t *testing.T) {
	a := &Alert{ID: "id", Source: "s", Severity: SeverityCritical, Title: "T", Status: StatusFiring, StartsAt: time.Unix(0, 0).UTC()}
	s := a.String()
	assert.Contains(t, s, "id")
	assert.Contains(t, s, "s")
	assert.Contains(t, s, "T")

	var nilA *Alert
	assert.Equal(t, "<nil alert>", nilA.String())
}

// TestAlertDuration covers both firing and resolved paths.
func TestAlertDuration(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	a := &Alert{StartsAt: start, Status: StatusFiring}
	d := a.Duration()
	assert.InDelta(t, time.Hour.Seconds(), d.Seconds(), 1)

	a.EndsAt = start.Add(30 * time.Minute)
	assert.Equal(t, 30*time.Minute, a.Duration())

	var nilA *Alert
	assert.Equal(t, time.Duration(0), nilA.Duration())
}

// TestAlertMarshalJSON ensures the fingerprint is filled when missing.
func TestAlertMarshalJSON(t *testing.T) {
	a := &Alert{Source: "s", Title: "t", Severity: SeverityInfo, StartsAt: time.Now()}
	data, err := json.Marshal(a)
	require.NoError(t, err)
	var back Alert
	require.NoError(t, json.Unmarshal(data, &back))
	assert.NotEmpty(t, back.Fingerprint)
	assert.Equal(t, back.Fingerprint, back.ID)
}
