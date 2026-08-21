package alert

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNagiosAdapterName is a smoke test.
func TestNagiosAdapterName(t *testing.T) {
	assert.Equal(t, "nagios", NewNagiosAdapter().Name())
}

// TestNagiosAdapterParseSingle covers a single-object service alert payload.
func TestNagiosAdapterParseSingle(t *testing.T) {
	payload := `{
		"host_name": "web-server-01",
		"host_address": "192.168.1.10",
		"service_description": "CPU Load",
		"state": "CRITICAL",
		"state_type": "HARD",
		"check_output": "CRITICAL - load average: 5.2, 4.8, 4.5",
		"current_attempt": "3",
		"max_attempts": "3",
		"timestamp": "2026-08-16T12:00:00Z",
		"notification_type": "PROBLEM"
	}`
	a := NewNagiosAdapter()
	require.NoError(t, a.Validate([]byte(payload)))
	alerts, err := a.Parse([]byte(payload))
	require.NoError(t, err)
	require.Len(t, alerts, 1)

	got := alerts[0]
	assert.Equal(t, "nagios", got.Source)
	assert.Equal(t, "web-server-01: CPU Load", got.Title)
	assert.Equal(t, "CRITICAL - load average: 5.2, 4.8, 4.5", got.Description)
	assert.Equal(t, SeverityCritical, got.Severity)
	assert.Equal(t, StatusFiring, got.Status)
	assert.Equal(t, "web-server-01", got.Labels["host"])
	assert.Equal(t, "192.168.1.10", got.Labels["host_ip"])
	assert.Equal(t, "CPU Load", got.Labels["service"])
	assert.Equal(t, "CRITICAL", got.Labels["state"])
	assert.NotEmpty(t, got.Fingerprint)
	assert.Equal(t, got.Fingerprint, got.ID)
}

// TestNagiosAdapterParseArray covers an array payload.
func TestNagiosAdapterParseArray(t *testing.T) {
	payload := `[
		{
			"host_name": "h1",
			"service_description": "CPU",
			"state": "WARNING",
			"timestamp": "2026-08-16T12:00:00Z",
			"notification_type": "PROBLEM"
		},
		{
			"host_name": "h2",
			"service_description": "Disk",
			"state": "CRITICAL",
			"timestamp": "2026-08-16T12:01:00Z",
			"notification_type": "PROBLEM"
		}
	]`
	alerts, err := NewNagiosAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	require.Len(t, alerts, 2)
	assert.Equal(t, "h1: CPU", alerts[0].Title)
	assert.Equal(t, "h2: Disk", alerts[1].Title)
	assert.Equal(t, SeverityWarning, alerts[0].Severity)
	assert.Equal(t, SeverityCritical, alerts[1].Severity)
}

// TestNagiosAdapterStateMapping covers all Nagios states.
func TestNagiosAdapterStateMapping(t *testing.T) {
	tests := []struct {
		state string
		sev   Severity
	}{
		{"OK", SeverityInfo},
		{"UP", SeverityInfo},
		{"WARNING", SeverityWarning},
		{"UNKNOWN", SeverityWarning},
		{"CRITICAL", SeverityCritical},
		{"DOWN", SeverityCritical},
		{"UNREACHABLE", SeverityCritical},
		{"bogus", SeverityWarning},
		{"", SeverityWarning},
	}
	for _, tc := range tests {
		t.Run(tc.state, func(t *testing.T) {
			payload := `{"host_name":"h","service_description":"s","state":"` + tc.state + `","timestamp":"2026-08-16T12:00:00Z","notification_type":"PROBLEM"}`
			alerts, err := NewNagiosAdapter().Parse([]byte(payload))
			require.NoError(t, err)
			assert.Equal(t, tc.sev, alerts[0].Severity)
		})
	}
}

// TestNagiosAdapterParseRecovery covers notification_type=RECOVERY.
func TestNagiosAdapterParseRecovery(t *testing.T) {
	payload := `{
		"host_name": "h1",
		"service_description": "CPU",
		"state": "OK",
		"timestamp": "2026-08-16T12:00:00Z",
		"notification_type": "RECOVERY"
	}`
	alerts, err := NewNagiosAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, alerts[0].Status)
	assert.Equal(t, SeverityInfo, alerts[0].Severity)
}

// TestNagiosAdapterParseStatusFromState covers status derived from state
// when notification_type is absent.
func TestNagiosAdapterParseStatusFromState(t *testing.T) {
	payload := `{
		"host_name": "h1",
		"service_description": "CPU",
		"state": "OK",
		"timestamp": "2026-08-16T12:00:00Z"
	}`
	alerts, err := NewNagiosAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, alerts[0].Status, "OK state without notification_type -> resolved")
}

// TestNagiosAdapterParseStatusAcknowledgement covers ACKNOWLEDGEMENT which
// derives status from state.
func TestNagiosAdapterParseStatusAcknowledgement(t *testing.T) {
	payload := `{
		"host_name": "h1",
		"service_description": "CPU",
		"state": "CRITICAL",
		"timestamp": "2026-08-16T12:00:00Z",
		"notification_type": "ACKNOWLEDGEMENT"
	}`
	alerts, err := NewNagiosAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, StatusFiring, alerts[0].Status, "ACK with CRITICAL state -> firing")
}

// TestNagiosAdapterParseHostCheck covers a host check (no service_description).
func TestNagiosAdapterParseHostCheck(t *testing.T) {
	payload := `{
		"host_name": "web-server-01",
		"host_address": "192.168.1.10",
		"state": "DOWN",
		"check_output": "HOST DOWN - rta: nan",
		"timestamp": "2026-08-16T12:00:00Z",
		"notification_type": "PROBLEM"
	}`
	alerts, err := NewNagiosAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, "web-server-01", alerts[0].Title, "host check title is just host_name")
	assert.Equal(t, SeverityCritical, alerts[0].Severity)
	assert.Equal(t, StatusFiring, alerts[0].Status)
}

// TestNagiosAdapterParseServiceNoHost covers service without host_name.
func TestNagiosAdapterParseServiceNoHost(t *testing.T) {
	payload := `{
		"service_description": "CPU",
		"state": "WARNING",
		"timestamp": "2026-08-16T12:00:00Z",
		"notification_type": "PROBLEM"
	}`
	alerts, err := NewNagiosAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, "CPU", alerts[0].Title)
}

// TestNagiosAdapterParseMissingTitle errors when both host_name and
// service_description are absent.
func TestNagiosAdapterParseMissingTitle(t *testing.T) {
	payload := `{"state":"CRITICAL","timestamp":"2026-08-16T12:00:00Z"}`
	_, err := NewNagiosAdapter().Parse([]byte(payload))
	require.Error(t, err)
}

// TestNagiosAdapterParseInvalidJSON errors.
func TestNagiosAdapterParseInvalidJSON(t *testing.T) {
	a := NewNagiosAdapter()
	_, err := a.Parse([]byte(`{not json`))
	require.Error(t, err)
	require.Error(t, a.Validate([]byte(`{not json`)))
}

// TestNagiosAdapterParseEmptyArray returns an empty slice.
func TestNagiosAdapterParseEmptyArray(t *testing.T) {
	alerts, err := NewNagiosAdapter().Parse([]byte(`[]`))
	require.NoError(t, err)
	assert.Empty(t, alerts)
}

// TestNagiosAdapterParseDefaults fills missing fields.
func TestNagiosAdapterParseDefaults(t *testing.T) {
	payload := `{"host_name":"h1","service_description":"s1"}`
	alerts, err := NewNagiosAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	assert.Equal(t, "nagios", alerts[0].Source)
	assert.Equal(t, SeverityWarning, alerts[0].Severity, "empty state defaults to warning")
	assert.Equal(t, StatusFiring, alerts[0].Status, "empty state defaults to firing")
	assert.False(t, alerts[0].StartsAt.IsZero(), "empty timestamp defaults to now")
}

// TestNagiosAdapterParseTimestampFormats covers multiple timestamp formats.
func TestNagiosAdapterParseTimestampFormats(t *testing.T) {
	tests := []struct {
		name      string
		timestamp string
		want      time.Time
	}{
		{
			name:      "RFC3339",
			timestamp: "2026-08-16T12:00:00Z",
			want:      time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		},
		{
			name:      "Nagios default",
			timestamp: "2026-08-16 12:00:00",
			want:      time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"host_name":"h","service_description":"s","timestamp":"` + tc.timestamp + `"}`
			alerts, err := NewNagiosAdapter().Parse([]byte(payload))
			require.NoError(t, err)
			assert.True(t, alerts[0].StartsAt.Equal(tc.want))
		})
	}
}

// TestNagiosAdapterParseUnixTimestamp covers numeric timestamp.
func TestNagiosAdapterParseUnixTimestamp(t *testing.T) {
	payload := `{"host_name":"h","service_description":"s","timestamp":"1723809600"}`
	alerts, err := NewNagiosAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	want := time.Unix(1723809600, 0)
	assert.True(t, alerts[0].StartsAt.Equal(want))
}

// TestNagiosAdapterParseInvalidTimestampDefaultsNow covers unparseable
// timestamp.
func TestNagiosAdapterParseInvalidTimestampDefaultsNow(t *testing.T) {
	before := time.Now()
	payload := `{"host_name":"h","service_description":"s","timestamp":"not-a-date"}`
	alerts, err := NewNagiosAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	after := time.Now()
	assert.True(t, alerts[0].StartsAt.After(before.Add(-time.Second)))
	assert.True(t, alerts[0].StartsAt.Before(after.Add(time.Second)))
}

// TestNagiosAdapterLabels verifies all expected labels are populated.
func TestNagiosAdapterLabels(t *testing.T) {
	payload := `{
		"host_name": "h1",
		"host_address": "10.0.0.1",
		"service_description": "CPU",
		"state": "WARNING",
		"state_type": "SOFT",
		"current_attempt": "2",
		"max_attempts": "3",
		"timestamp": "2026-08-16T12:00:00Z",
		"notification_type": "PROBLEM"
	}`
	alerts, err := NewNagiosAdapter().Parse([]byte(payload))
	require.NoError(t, err)
	labels := alerts[0].Labels
	assert.Equal(t, "h1", labels["host"])
	assert.Equal(t, "10.0.0.1", labels["host_ip"])
	assert.Equal(t, "CPU", labels["service"])
	assert.Equal(t, "WARNING", labels["state"])
	assert.Equal(t, "SOFT", labels["state_type"])
	assert.Equal(t, "2", labels["current_attempt"])
	assert.Equal(t, "3", labels["max_attempts"])
	assert.Equal(t, "PROBLEM", labels["notification_type"])
}

// TestNagiosAdapterEncodeReverse round-trips an Alert.
func TestNagiosAdapterEncodeReverse(t *testing.T) {
	a := &Alert{
		Source:      "nagios",
		Title:       "h1: CPU",
		Severity:    SeverityCritical,
		Description: "CRITICAL - load high",
		Labels: map[string]string{
			"host":            "h1",
			"host_ip":         "10.0.0.1",
			"service":         "CPU",
			"state_type":      "HARD",
			"current_attempt": "3",
			"max_attempts":    "3",
		},
		StartsAt: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		Status:   StatusFiring,
	}
	na, err := NewNagiosAdapter().EncodeReverse(a)
	require.NoError(t, err)
	assert.Equal(t, "CRITICAL - load high", na.CheckOutput)
	assert.Equal(t, "h1", na.HostName)
	assert.Equal(t, "10.0.0.1", na.HostAddress)
	assert.Equal(t, "CPU", na.ServiceDescription)
	assert.Equal(t, "CRITICAL", na.State, "Critical severity -> CRITICAL state")
	assert.Equal(t, "PROBLEM", na.NotificationType)
	assert.Equal(t, "HARD", na.StateType)
}

// TestNagiosAdapterEncodeReverseResolved covers resolved status.
func TestNagiosAdapterEncodeReverseResolved(t *testing.T) {
	a := &Alert{
		Source:   "nagios",
		Title:    "h1: CPU",
		Severity: SeverityInfo,
		Status:   StatusResolved,
	}
	na, err := NewNagiosAdapter().EncodeReverse(a)
	require.NoError(t, err)
	assert.Equal(t, "RECOVERY", na.NotificationType)
	assert.Equal(t, "OK", na.State, "Info severity -> OK state")
}

// TestNagiosAdapterEncodeReverseNil errors.
func TestNagiosAdapterEncodeReverseNil(t *testing.T) {
	_, err := NewNagiosAdapter().EncodeReverse(nil)
	assert.Error(t, err)
}

// TestNagiosAlertJSONTags ensures the struct tags are stable.
func TestNagiosAlertJSONTags(t *testing.T) {
	na := NagiosAlert{HostName: "h1", State: "CRITICAL"}
	data, err := json.Marshal(na)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"host_name":"h1"`)
	assert.Contains(t, string(data), `"state":"CRITICAL"`)
}

// TestNagiosAdapterImplementsAdapter verifies the interface at compile time.
func TestNagiosAdapterImplementsAdapter(t *testing.T) {
	var _ Adapter = (*NagiosAdapter)(nil)
}

// TestNagiosAdapterValidateArray covers Validate with a valid array payload.
func TestNagiosAdapterValidateArray(t *testing.T) {
	payload := `[{"host_name":"h1","service_description":"s","timestamp":"2026-08-16T12:00:00Z"},{"host_name":"h2","service_description":"s","timestamp":"2026-08-16T12:01:00Z"}]`
	require.NoError(t, NewNagiosAdapter().Validate([]byte(payload)))
}

// TestNagiosAdapterValidateSingle covers Validate with a valid single object.
func TestNagiosAdapterValidateSingle(t *testing.T) {
	payload := `{"host_name":"h","service_description":"s","timestamp":"2026-08-16T12:00:00Z"}`
	require.NoError(t, NewNagiosAdapter().Validate([]byte(payload)))
}

// TestNagiosAdapterParseArrayConvertError covers an array where one element
// is missing both host_name and service_description.
func TestNagiosAdapterParseArrayConvertError(t *testing.T) {
	payload := `[
		{"host_name":"h1","service_description":"s","timestamp":"2026-08-16T12:00:00Z"},
		{"state":"CRITICAL","timestamp":"2026-08-16T12:01:00Z"}
	]`
	_, err := NewNagiosAdapter().Parse([]byte(payload))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alert[1]")
}

// TestNagiosAdapterParseInvalidJSONSentinel verifies the JSON error wraps
// ErrInvalidPayload.
func TestNagiosAdapterParseInvalidJSONSentinel(t *testing.T) {
	_, err := NewNagiosAdapter().Parse([]byte(`{not json`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidPayload))
}

// TestNagiosAdapterValidateInvalidJSONSentinel verifies the Validate JSON
// error wraps ErrInvalidPayload.
func TestNagiosAdapterValidateInvalidJSONSentinel(t *testing.T) {
	err := NewNagiosAdapter().Validate([]byte(`{not json`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidPayload))
}

// TestNagiosAdapterValidateMissingTitleSentinel verifies Validate rejects a
// payload missing both host_name and service_description, wrapping
// ErrMissingField.
func TestNagiosAdapterValidateMissingTitleSentinel(t *testing.T) {
	err := NewNagiosAdapter().Validate([]byte(`{"state":"CRITICAL"}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingField))
}

// TestNagiosAdapterValidateArrayMissingTitleSentinel verifies Validate on an
// array element missing both host_name and service_description.
func TestNagiosAdapterValidateArrayMissingTitleSentinel(t *testing.T) {
	payload := `[{"host_name":"ok"},{"state":"CRITICAL"}]`
	err := NewNagiosAdapter().Validate([]byte(payload))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingField))
}

// TestNagiosAdapterParseMissingTitleSentinel verifies the missing-field error
// from Parse wraps ErrMissingField.
func TestNagiosAdapterParseMissingTitleSentinel(t *testing.T) {
	_, err := NewNagiosAdapter().Parse([]byte(`{"state":"CRITICAL"}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingField))
}

// TestNagiosAdapterStateToStatusDirect directly exercises nagiosStateToStatus
// to cover the OK/UP and default branches.
func TestNagiosStateToStatusDirect(t *testing.T) {
	assert.Equal(t, StatusResolved, nagiosStateToStatus("OK"))
	assert.Equal(t, StatusResolved, nagiosStateToStatus("UP"))
	assert.Equal(t, StatusFiring, nagiosStateToStatus("CRITICAL"))
	assert.Equal(t, StatusFiring, nagiosStateToStatus("DOWN"))
	assert.Equal(t, StatusFiring, nagiosStateToStatus("bogus"))
}

// TestNagiosAdapterEncodeReverseWarning covers warning severity mapping in
// EncodeReverse.
func TestNagiosAdapterEncodeReverseWarning(t *testing.T) {
	a := &Alert{
		Source:   "nagios",
		Title:    "warn",
		Severity: SeverityWarning,
		Status:   StatusFiring,
	}
	na, err := NewNagiosAdapter().EncodeReverse(a)
	require.NoError(t, err)
	assert.Equal(t, "WARNING", na.State)
}
