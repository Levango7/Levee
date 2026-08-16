package alert

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCustomAdapterName is a smoke test.
func TestCustomAdapterName(t *testing.T) {
	assert.Equal(t, "custom", NewCustomAdapter().Name())
}

// TestCustomAdapterParseSingle covers a single-object payload.
func TestCustomAdapterParseSingle(t *testing.T) {
	payload := `{
		"source": "custom",
		"severity": "critical",
		"title": "DiskFull",
		"description": "/ is 95% full",
		"labels": {"host": "node-1"},
		"starts_at": "2026-08-16T12:00:00Z",
		"status": "firing",
		"id": "ext-1"
	}`
	a := NewCustomAdapter()
	require.NoError(t, a.Validate([]byte(payload)))
	alerts, err := a.Parse([]byte(payload))
	require.NoError(t, err)
	require.Len(t, alerts, 1)

	got := alerts[0]
	assert.Equal(t, "custom", got.Source)
	assert.Equal(t, "DiskFull", got.Title)
	assert.Equal(t, SeverityCritical, got.Severity)
	assert.Equal(t, "ext-1", got.ID)
	assert.Equal(t, StatusFiring, got.Status)
	assert.Equal(t, "node-1", got.Labels["host"])
}

// TestCustomAdapterParseArray covers an array payload.
func TestCustomAdapterParseArray(t *testing.T) {
	payload := `[
		{"title":"A","severity":"info","starts_at":"2026-08-16T12:00:00Z"},
		{"title":"B","severity":"warning","starts_at":"2026-08-16T12:01:00Z"}
	]`
	alerts, err := NewCustomAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	require.Len(t, alerts, 2)
	assert.Equal(t, "A", alerts[0].Title)
	assert.Equal(t, "B", alerts[1].Title)
}

// TestCustomAdapterParseDefaults fills missing fields.
func TestCustomAdapterParseDefaults(t *testing.T) {
	payload := `{"title":"X"}`
	alerts, err := NewCustomAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, "custom", alerts[0].Source, "source defaults to adapter name")
	assert.Equal(t, SeverityWarning, alerts[0].Severity, "severity defaults to warning")
	assert.Equal(t, StatusFiring, alerts[0].Status)
	assert.False(t, alerts[0].StartsAt.IsZero(), "starts_at defaults to now")
	assert.Equal(t, alerts[0].Fingerprint, alerts[0].ID, "id defaults to fingerprint")
}

// TestCustomAdapterParseMissingTitle errors.
func TestCustomAdapterParseMissingTitle(t *testing.T) {
	_, err := NewCustomAdapter().Parse([]byte(`{"source":"x"}`))
	require.Error(t, err)
}

// TestCustomAdapterParseInvalidJSON errors.
func TestCustomAdapterParseInvalidJSON(t *testing.T) {
	a := NewCustomAdapter()
	_, err := a.Parse([]byte(`{not json`))
	require.Error(t, err)
	require.Error(t, a.Validate([]byte(`{not json`)))
}

// TestCustomAdapterParseResolved covers status=resolved.
func TestCustomAdapterParseResolved(t *testing.T) {
	payload := `{"title":"X","status":"resolved","starts_at":"2026-08-16T12:00:00Z","ends_at":"2026-08-16T12:05:00Z"}`
	alerts, err := NewCustomAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, alerts[0].Status)
}

// TestCustomAdapterParseSeverityUnknown falls back to warning.
func TestCustomAdapterParseSeverityUnknown(t *testing.T) {
	payload := `{"title":"X","severity":"bogus"}`
	alerts, err := NewCustomAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, SeverityWarning, alerts[0].Severity)
}

// TestCustomAdapterParseSourceOverride keeps an explicit source.
func TestCustomAdapterParseSourceOverride(t *testing.T) {
	payload := `{"title":"X","source":"myapp"}`
	alerts, err := NewCustomAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, "myapp", alerts[0].Source)
}

// TestCustomAdapterParseTimeFields parses RFC3339 timestamps.
func TestCustomAdapterParseTimeFields(t *testing.T) {
	payload := `{"title":"X","starts_at":"2026-08-16T12:00:00Z","ends_at":"2026-08-16T12:05:00Z"}`
	alerts, err := NewCustomAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	want, _ := time.Parse(time.RFC3339, "2026-08-16T12:00:00Z")
	assert.True(t, alerts[0].StartsAt.Equal(want))
}
