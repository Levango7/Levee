package alert

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrometheusAdapterName is a smoke test.
func TestPrometheusAdapterName(t *testing.T) {
	a := NewPrometheusAdapter()
	assert.Equal(t, "prometheus", a.Name())
}

// TestPrometheusAdapterParse covers a representative Alertmanager payload.
func TestPrometheusAdapterParse(t *testing.T) {
	payload := `[{
		"status": "firing",
		"labels": {"alertname": "HighCpu", "severity": "warning", "host": "n1"},
		"annotations": {"summary": "CPU > 90%", "description": "load avg high"},
		"startsAt": "2026-08-16T12:00:00Z",
		"endsAt": "0001-01-01T00:00:00Z",
		"generatorURL": "http://prom/graph"
	}]`

	a := NewPrometheusAdapter()
	require.NoError(t, a.Validate([]byte(payload)))
	alerts, err := a.Parse([]byte(payload))
	require.NoError(t, err)
	require.Len(t, alerts, 1)

	got := alerts[0]
	assert.Equal(t, "prometheus", got.Source)
	assert.Equal(t, "HighCpu", got.Title)
	assert.Equal(t, SeverityWarning, got.Severity)
	assert.Equal(t, "CPU > 90%", got.Description)
	assert.Equal(t, StatusFiring, got.Status)
	assert.Equal(t, "n1", got.Labels["host"])
	assert.NotEmpty(t, got.Fingerprint)
	assert.Equal(t, got.Fingerprint, got.ID)
}

// TestPrometheusAdapterParseResolved covers status=resolved.
func TestPrometheusAdapterParseResolved(t *testing.T) {
	payload := `[{"status":"resolved","labels":{"alertname":"X"},"startsAt":"2026-08-16T12:00:00Z","endsAt":"2026-08-16T12:05:00Z"}]`
	alerts, err := NewPrometheusAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, alerts[0].Status)
	assert.False(t, alerts[0].EndsAt.IsZero())
}

// TestPrometheusAdapterParseFingerprintPrefersAlertmanager.
func TestPrometheusAdapterParseFingerprintPrefersAlertmanager(t *testing.T) {
	payload := `[{"status":"firing","labels":{"alertname":"X"},"startsAt":"2026-08-16T12:00:00Z","fingerprint":"abc123"}]`
	alerts, err := NewPrometheusAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, "abc123", alerts[0].Fingerprint)
}

// TestPrometheusAdapterParseMissingAlertname errors.
func TestPrometheusAdapterParseMissingAlertname(t *testing.T) {
	payload := `[{"status":"firing","labels":{},"startsAt":"2026-08-16T12:00:00Z"}]`
	_, err := NewPrometheusAdapter().Parse([]byte(payload))
	require.Error(t, err)
}

// TestPrometheusAdapterParseInvalidJSON errors.
func TestPrometheusAdapterParseInvalidJSON(t *testing.T) {
	_, err := NewPrometheusAdapter().Parse([]byte(`{not json`))
	require.Error(t, err)
	require.Error(t, NewPrometheusAdapter().Validate([]byte(`{not json`)))
}

// TestPrometheusAdapterParseSeverityFallback defaults to warning.
func TestPrometheusAdapterParseSeverityFallback(t *testing.T) {
	payload := `[{"status":"firing","labels":{"alertname":"X","severity":"bogus"},"startsAt":"2026-08-16T12:00:00Z"}]`
	alerts, err := NewPrometheusAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, SeverityWarning, alerts[0].Severity, "unknown severity falls back to warning")
}

// TestPrometheusAdapterParseDescriptionFallback uses description when summary
// is absent.
func TestPrometheusAdapterParseDescriptionFallback(t *testing.T) {
	payload := `[{"status":"firing","labels":{"alertname":"X"},"annotations":{"description":"d-only"},"startsAt":"2026-08-16T12:00:00Z"}]`
	alerts, err := NewPrometheusAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, "d-only", alerts[0].Description)
}

// TestPrometheusAdapterParseEmptyArray returns an empty slice.
func TestPrometheusAdapterParseEmptyArray(t *testing.T) {
	alerts, err := NewPrometheusAdapter().Parse([]byte(`[]`))
	require.NoError(t, err)
	assert.Empty(t, alerts)
}

// TestPrometheusAdapterEncodeReverse round-trips an Alert.
func TestPrometheusAdapterEncodeReverse(t *testing.T) {
	a := &Alert{
		Source:      "prometheus",
		Title:       "HighCpu",
		Severity:    SeverityCritical,
		Description: "CPU high",
		Labels:      map[string]string{"host": "n1"},
		StartsAt:    time.Now(),
		Status:      StatusFiring,
	}
	pa, err := NewPrometheusAdapter().EncodeReverse(a)
	require.NoError(t, err)
	assert.Equal(t, "HighCpu", pa.Labels["alertname"])
	assert.Equal(t, "critical", pa.Labels["severity"])
	assert.Equal(t, "CPU high", pa.Annotations["summary"])

	_, err = NewPrometheusAdapter().EncodeReverse(nil)
	assert.Error(t, err)
}

// TestPrometheusAdapterParseMultiple covers a multi-alert payload.
func TestPrometheusAdapterParseMultiple(t *testing.T) {
	payload := `[
		{"status":"firing","labels":{"alertname":"A"},"startsAt":"2026-08-16T12:00:00Z"},
		{"status":"firing","labels":{"alertname":"B"},"startsAt":"2026-08-16T12:01:00Z"}
	]`
	alerts, err := NewPrometheusAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	require.Len(t, alerts, 2)
	assert.Equal(t, "A", alerts[0].Title)
	assert.Equal(t, "B", alerts[1].Title)
}

// TestPrometheusAlertJSONTags ensures the struct tags are stable.
func TestPrometheusAlertJSONTags(t *testing.T) {
	pa := PrometheusAlert{Status: "firing"}
	data, err := json.Marshal(pa)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"status":"firing"`)
}
