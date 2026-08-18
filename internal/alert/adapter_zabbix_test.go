package alert

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestZabbixAdapterName is a smoke test.
func TestZabbixAdapterName(t *testing.T) {
	assert.Equal(t, "zabbix", NewZabbixAdapter().Name())
}

// TestZabbixAdapterParseSingle covers a single-object payload.
func TestZabbixAdapterParseSingle(t *testing.T) {
	payload := `{
		"event_id": "12345",
		"event_source": "trigger",
		"event_value": "1",
		"host": "web-server-01",
		"host_ip": "192.168.1.10",
		"trigger_id": "98765",
		"trigger_name": "CPU usage > 90% on web-server-01",
		"trigger_description": "CPU usage is too high",
		"trigger_severity": "4",
		"trigger_status": "PROBLEM",
		"trigger_url": "http://zabbix/triggers/98765",
		"item_lastvalue": "92.5",
		"datetime": "2026-08-16T12:00:00Z",
		"action": "PROBLEM"
	}`
	a := NewZabbixAdapter()
	require.NoError(t, a.Validate([]byte(payload)))
	alerts, err := a.Parse([]byte(payload))
	require.NoError(t, err)
	require.Len(t, alerts, 1)

	got := alerts[0]
	assert.Equal(t, "zabbix", got.Source)
	assert.Equal(t, "CPU usage > 90% on web-server-01", got.Title)
	assert.Equal(t, "CPU usage is too high", got.Description)
	assert.Equal(t, SeverityCritical, got.Severity, "severity 4 = High -> Critical")
	assert.Equal(t, StatusFiring, got.Status)
	assert.Equal(t, "12345", got.ID, "event_id used as ID")
	assert.Equal(t, "web-server-01", got.Labels["host"])
	assert.Equal(t, "192.168.1.10", got.Labels["host_ip"])
	assert.Equal(t, "98765", got.Labels["trigger_id"])
	assert.NotEmpty(t, got.Fingerprint)
}

// TestZabbixAdapterParseArray covers an array payload.
func TestZabbixAdapterParseArray(t *testing.T) {
	payload := `[
		{
			"event_id": "1",
			"trigger_name": "CPU high",
			"trigger_severity": "4",
			"trigger_status": "PROBLEM",
			"datetime": "2026-08-16T12:00:00Z"
		},
		{
			"event_id": "2",
			"trigger_name": "Disk full",
			"trigger_severity": "5",
			"trigger_status": "PROBLEM",
			"datetime": "2026-08-16T12:01:00Z"
		}
	]`
	alerts, err := NewZabbixAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	require.Len(t, alerts, 2)
	assert.Equal(t, "CPU high", alerts[0].Title)
	assert.Equal(t, "Disk full", alerts[1].Title)
	assert.Equal(t, SeverityCritical, alerts[0].Severity)
	assert.Equal(t, SeverityCritical, alerts[1].Severity)
}

// TestZabbixAdapterSeverityMapping covers all 6 Zabbix severity levels.
func TestZabbixAdapterSeverityMapping(t *testing.T) {
	tests := []struct {
		zabbixSev string
		want      Severity
	}{
		{"0", SeverityInfo},      // Not classified
		{"1", SeverityInfo},      // Information
		{"2", SeverityWarning},   // Warning
		{"3", SeverityWarning},   // Average
		{"4", SeverityCritical},  // High
		{"5", SeverityCritical},  // Disaster
		{"", SeverityWarning},    // empty -> default
		{"99", SeverityWarning},  // out of range -> default
		{"High", SeverityCritical},
		{"Disaster", SeverityCritical},
		{"Warning", SeverityWarning},
		{"Average", SeverityWarning},
		{"Information", SeverityInfo},
		{"Not classified", SeverityInfo},
		{"bogus", SeverityWarning},
	}
	for _, tc := range tests {
		t.Run(tc.zabbixSev, func(t *testing.T) {
			payload := `{"trigger_name":"X","trigger_severity":"` + tc.zabbixSev + `","datetime":"2026-08-16T12:00:00Z"}`
			alerts, err := NewZabbixAdapter().Parse([]byte(payload))
			require.NoError(t, err)
			assert.Equal(t, tc.want, alerts[0].Severity)
		})
	}
}

// TestZabbixAdapterParseResolved covers action=RECOVERY.
func TestZabbixAdapterParseResolved(t *testing.T) {
	payload := `{
		"trigger_name": "CPU high",
		"trigger_severity": "4",
		"trigger_status": "OK",
		"action": "RECOVERY",
		"datetime": "2026-08-16T12:00:00Z"
	}`
	alerts, err := NewZabbixAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, alerts[0].Status)
}

// TestZabbixAdapterParseStatusFromTriggerStatus covers status derived from
// trigger_status when action is absent.
func TestZabbixAdapterParseStatusFromTriggerStatus(t *testing.T) {
	payload := `{
		"trigger_name": "CPU high",
		"trigger_status": "OK",
		"datetime": "2026-08-16T12:00:00Z"
	}`
	alerts, err := NewZabbixAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, alerts[0].Status)
}

// TestZabbixAdapterParseMissingTriggerName errors.
func TestZabbixAdapterParseMissingTriggerName(t *testing.T) {
	payload := `{"event_id":"1","trigger_severity":"4","datetime":"2026-08-16T12:00:00Z"}`
	_, err := NewZabbixAdapter().Parse([]byte(payload))
	require.Error(t, err)
}

// TestZabbixAdapterParseInvalidJSON errors.
func TestZabbixAdapterParseInvalidJSON(t *testing.T) {
	a := NewZabbixAdapter()
	_, err := a.Parse([]byte(`{not json`))
	require.Error(t, err)
	require.Error(t, a.Validate([]byte(`{not json`)))
}

// TestZabbixAdapterParseEmptyArray returns an empty slice.
func TestZabbixAdapterParseEmptyArray(t *testing.T) {
	alerts, err := NewZabbixAdapter().Parse([]byte(`[]`))
	require.NoError(t, err)
	assert.Empty(t, alerts)
}

// TestZabbixAdapterParseDefaults fills missing fields.
func TestZabbixAdapterParseDefaults(t *testing.T) {
	payload := `{"trigger_name":"X"}`
	alerts, err := NewZabbixAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, "zabbix", alerts[0].Source)
	assert.Equal(t, SeverityWarning, alerts[0].Severity, "empty severity defaults to warning")
	assert.Equal(t, StatusFiring, alerts[0].Status, "empty status defaults to firing")
	assert.False(t, alerts[0].StartsAt.IsZero(), "empty datetime defaults to now")
	assert.Equal(t, alerts[0].Fingerprint, alerts[0].ID, "id defaults to fingerprint when event_id absent")
}

// TestZabbixAdapterParseDescriptionFallback uses item_lastvalue when
// trigger_description is absent.
func TestZabbixAdapterParseDescriptionFallback(t *testing.T) {
	payload := `{
		"trigger_name": "CPU high",
		"trigger_description": "",
		"item_lastvalue": "92.5",
		"datetime": "2026-08-16T12:00:00Z"
	}`
	alerts, err := NewZabbixAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, "92.5", alerts[0].Description)
}

// TestZabbixAdapterParseDatetimeFormats covers multiple datetime formats.
func TestZabbixAdapterParseDatetimeFormats(t *testing.T) {
	tests := []struct {
		name     string
		datetime string
		want     time.Time
	}{
		{
			name:     "RFC3339",
			datetime: "2026-08-16T12:00:00Z",
			want:     time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		},
		{
			name:     "Zabbix default",
			datetime: "2026-08-16 12:00:00",
			want:     time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"trigger_name":"X","datetime":"` + tc.datetime + `"}`
			alerts, err := NewZabbixAdapter().Parse([]byte(payload))
			require.NoError(t, err)
			assert.True(t, alerts[0].StartsAt.Equal(tc.want))
		})
	}
}

// TestZabbixAdapterParseUnixTimestamp covers numeric datetime.
func TestZabbixAdapterParseUnixTimestamp(t *testing.T) {
	payload := `{"trigger_name":"X","datetime":"1723809600"}`
	alerts, err := NewZabbixAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	want := time.Unix(1723809600, 0)
	assert.True(t, alerts[0].StartsAt.Equal(want))
}

// TestZabbixAdapterParseInvalidDatetimeDefaultsNow covers unparseable datetime.
func TestZabbixAdapterParseInvalidDatetimeDefaultsNow(t *testing.T) {
	before := time.Now()
	payload := `{"trigger_name":"X","datetime":"not-a-date"}`
	alerts, err := NewZabbixAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	after := time.Now()
	assert.True(t, alerts[0].StartsAt.After(before.Add(-time.Second)))
	assert.True(t, alerts[0].StartsAt.Before(after.Add(time.Second)))
}

// TestZabbixAdapterLabels verifies all expected labels are populated.
func TestZabbixAdapterLabels(t *testing.T) {
	payload := `{
		"event_id": "1",
		"event_source": "trigger",
		"host": "h1",
		"host_ip": "10.0.0.1",
		"trigger_id": "t1",
		"trigger_name": "X",
		"trigger_severity": "3",
		"trigger_url": "http://z/t1",
		"item_lastvalue": "42",
		"datetime": "2026-08-16T12:00:00Z"
	}`
	alerts, err := NewZabbixAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	labels := alerts[0].Labels
	assert.Equal(t, "h1", labels["host"])
	assert.Equal(t, "10.0.0.1", labels["host_ip"])
	assert.Equal(t, "t1", labels["trigger_id"])
	assert.Equal(t, "1", labels["event_id"])
	assert.Equal(t, "3", labels["trigger_severity"])
	assert.Equal(t, "trigger", labels["event_source"])
	assert.Equal(t, "http://z/t1", labels["trigger_url"])
	assert.Equal(t, "42", labels["item_lastvalue"])
}

// TestZabbixAdapterEncodeReverse round-trips an Alert.
func TestZabbixAdapterEncodeReverse(t *testing.T) {
	a := &Alert{
		Source:      "zabbix",
		Title:       "CPU high",
		Severity:    SeverityCritical,
		Description: "CPU > 90%",
		Labels:      map[string]string{"host": "n1", "host_ip": "10.0.0.1", "trigger_id": "t1"},
		StartsAt:    time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		Status:      StatusFiring,
		ID:          "evt-1",
	}
	za, err := NewZabbixAdapter().EncodeReverse(a)
	require.NoError(t, err)
	assert.Equal(t, "CPU high", za.TriggerName)
	assert.Equal(t, "CPU > 90%", za.TriggerDescription)
	assert.Equal(t, "n1", za.Host)
	assert.Equal(t, "10.0.0.1", za.HostIP)
	assert.Equal(t, "t1", za.TriggerID)
	assert.Equal(t, "4", za.TriggerSeverity, "Critical -> High(4)")
	assert.Equal(t, "PROBLEM", za.Action)
	assert.Equal(t, "PROBLEM", za.TriggerStatus)
	assert.Equal(t, "evt-1", za.EventID)
}

// TestZabbixAdapterEncodeReverseResolved covers resolved status.
func TestZabbixAdapterEncodeReverseResolved(t *testing.T) {
	a := &Alert{
		Source:   "zabbix",
		Title:    "CPU high",
		Severity: SeverityWarning,
		Status:   StatusResolved,
	}
	za, err := NewZabbixAdapter().EncodeReverse(a)
	require.NoError(t, err)
	assert.Equal(t, "RECOVERY", za.Action)
	assert.Equal(t, "OK", za.TriggerStatus)
	assert.Equal(t, "2", za.TriggerSeverity, "Warning -> 2")
}

// TestZabbixAdapterEncodeReverseNil errors.
func TestZabbixAdapterEncodeReverseNil(t *testing.T) {
	_, err := NewZabbixAdapter().EncodeReverse(nil)
	assert.Error(t, err)
}

// TestZabbixAlertJSONTags ensures the struct tags are stable.
func TestZabbixAlertJSONTags(t *testing.T) {
	za := ZabbixAlert{EventID: "42", TriggerName: "X"}
	data, err := json.Marshal(za)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"event_id":"42"`)
	assert.Contains(t, string(data), `"trigger_name":"X"`)
}

// TestZabbixAdapterImplementsAdapter verifies the interface at compile time.
func TestZabbixAdapterImplementsAdapter(t *testing.T) {
	var _ Adapter = (*ZabbixAdapter)(nil)
}

// TestZabbixAdapterValidateArray covers Validate with a valid array payload.
func TestZabbixAdapterValidateArray(t *testing.T) {
	payload := `[{"trigger_name":"A","datetime":"2026-08-16T12:00:00Z"},{"trigger_name":"B","datetime":"2026-08-16T12:01:00Z"}]`
	require.NoError(t, NewZabbixAdapter().Validate([]byte(payload)))
}

// TestZabbixAdapterValidateSingle covers Validate with a valid single object.
func TestZabbixAdapterValidateSingle(t *testing.T) {
	payload := `{"trigger_name":"X","datetime":"2026-08-16T12:00:00Z"}`
	require.NoError(t, NewZabbixAdapter().Validate([]byte(payload)))
}

// TestZabbixAdapterParseArrayConvertError covers an array where one element
// is missing trigger_name.
func TestZabbixAdapterParseArrayConvertError(t *testing.T) {
	payload := `[
		{"trigger_name":"A","datetime":"2026-08-16T12:00:00Z"},
		{"event_id":"2","datetime":"2026-08-16T12:01:00Z"}
	]`
	_, err := NewZabbixAdapter().Parse([]byte(payload))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alert[1]")
}

// TestZabbixAdapterValidateMissingTriggerName verifies that Validate rejects
// a payload whose trigger_name is absent, wrapping ErrMissingField.
func TestZabbixAdapterValidateMissingTriggerName(t *testing.T) {
	err := NewZabbixAdapter().Validate([]byte(`{"event_id":"1","host":"h"}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingField), "error should wrap ErrMissingField")
}

// TestZabbixAdapterValidateArrayMissingTriggerName verifies Validate on an
// array element missing trigger_name.
func TestZabbixAdapterValidateArrayMissingTriggerName(t *testing.T) {
	payload := `[{"trigger_name":"ok"},{"event_id":"2"}]`
	err := NewZabbixAdapter().Validate([]byte(payload))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingField))
}

// TestZabbixAdapterParseInvalidJSONSentinel verifies the JSON error wraps
// ErrInvalidPayload.
func TestZabbixAdapterParseInvalidJSONSentinel(t *testing.T) {
	_, err := NewZabbixAdapter().Parse([]byte(`{not json`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidPayload))
}

// TestZabbixAdapterValidateInvalidJSONSentinel verifies Validate JSON error
// wraps ErrInvalidPayload.
func TestZabbixAdapterValidateInvalidJSONSentinel(t *testing.T) {
	err := NewZabbixAdapter().Validate([]byte(`{not json`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidPayload))
}

// TestZabbixAdapterParseMissingTriggerNameSentinel verifies the missing-field
// error from Parse wraps ErrMissingField.
func TestZabbixAdapterParseMissingTriggerNameSentinel(t *testing.T) {
	_, err := NewZabbixAdapter().Parse([]byte(`{"event_id":"1","host":"h"}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingField))
}