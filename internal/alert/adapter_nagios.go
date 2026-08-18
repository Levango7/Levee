
// adapter_nagios.go adapts the Nagios webhook payload into the unified Alert
// model.
//
// Nagios can forward notifications via HTTP webhooks (e.g. through a custom
// notification script or the nagios-http plugin). A typical payload looks like:
//
//	{
//	  "host_name": "web-server-01",
//	  "host_address": "192.168.1.10",
//	  "service_description": "CPU Load",
//	  "state": "CRITICAL",
//	  "state_type": "HARD",
//	  "check_output": "CRITICAL - load average: 5.2, 4.8, 4.5",
//	  "current_attempt": "3",
//	  "max_attempts": "3",
//	  "timestamp": "2026-08-16T12:00:00Z",
//	  "notification_type": "PROBLEM",
//	  "author": "",
//	  "comment": ""
//	}
//
// The adapter maps service_description (or host_name for host checks) -> Title,
// check_output -> Description, state -> Severity, notification_type -> Status,
// host_name -> labels.host, host_address -> labels.host_ip. The original
// payload is preserved in RawPayload.
//
// Nagios may also send a bare array of such objects.
package alert

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NagiosState represents the Nagios check state. Nagios uses 4 states for
// services (OK, WARNING, CRITICAL, UNKNOWN) and additional host states
// (UP, DOWN, UNREACHABLE).
type NagiosState string

const (
	// NagiosStateOK is the Nagios OK state.
	NagiosStateOK NagiosState = "OK"
	// NagiosStateWarning is the Nagios WARNING state.
	NagiosStateWarning NagiosState = "WARNING"
	// NagiosStateCritical is the Nagios CRITICAL state.
	NagiosStateCritical NagiosState = "CRITICAL"
	// NagiosStateUnknown is the Nagios UNKNOWN state.
	NagiosStateUnknown NagiosState = "UNKNOWN"
	// NagiosStateUp is the Nagios host UP state.
	NagiosStateUp NagiosState = "UP"
	// NagiosStateDown is the Nagios host DOWN state.
	NagiosStateDown NagiosState = "DOWN"
	// NagiosStateUnreachable is the Nagios host UNREACHABLE state.
	NagiosStateUnreachable NagiosState = "UNREACHABLE"
)

// nagiosStateToSeverity maps a Nagios state string to the unified Severity.
// OK/UP -> Info, WARNING/UNKNOWN -> Warning, CRITICAL/DOWN/UNREACHABLE ->
// Critical. Unknown values default to Warning.
func nagiosStateToSeverity(state string) Severity {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case string(NagiosStateOK), string(NagiosStateUp):
		return SeverityInfo
	case string(NagiosStateWarning), string(NagiosStateUnknown):
		return SeverityWarning
	case string(NagiosStateCritical), string(NagiosStateDown), string(NagiosStateUnreachable):
		return SeverityCritical
	default:
		return SeverityWarning
	}
}

// nagiosStateToStatus maps a Nagios state to an AlertStatus. OK/UP states
// indicate the check is healthy, which we treat as resolved. All other states
// are firing.
func nagiosStateToStatus(state string) AlertStatus {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case string(NagiosStateOK), string(NagiosStateUp):
		return StatusResolved
	default:
		return StatusFiring
	}
}

// NagiosAlert is the per-event object in a Nagios webhook payload.
// Field names follow common Nagios notification conventions. Unknown fields
// are ignored during unmarshalling.
type NagiosAlert struct {
	HostName           string `json:"host_name"`
	HostAddress        string `json:"host_address"`
	ServiceDescription string `json:"service_description"`
	State              string `json:"state"`
	StateType          string `json:"state_type"`
	CheckOutput        string `json:"check_output"`
	CurrentAttempt     string `json:"current_attempt"`
	MaxAttempts        string `json:"max_attempts"`
	Timestamp          string `json:"timestamp"`
	NotificationType   string `json:"notification_type"`
	Author             string `json:"author"`
	Comment            string `json:"comment"`
}

// NagiosAdapter parses Nagios webhook payloads.
type NagiosAdapter struct{}

// NewNagiosAdapter constructs a NagiosAdapter.
func NewNagiosAdapter() *NagiosAdapter {
	return &NagiosAdapter{}
}

// Name returns the adapter identifier.
func (a *NagiosAdapter) Name() string { return "nagios" }

// Validate checks that raw is a syntactically valid Nagios payload (single
// object or array of objects) and that every object carries at least a
// host_name or service_description (the minimum fields required to build an
// Alert title).
func (a *NagiosAdapter) Validate(raw []byte) error {
	// Try array first, then single object.
	var arr []NagiosAlert
	if err := json.Unmarshal(raw, &arr); err == nil {
		for i, na := range arr {
			if a.buildTitle(na) == "" {
				return fmt.Errorf("%w: nagios adapter: alert[%d]: host_name or service_description", ErrMissingField, i)
			}
		}
		return nil
	}
	var one NagiosAlert
	if err := json.Unmarshal(raw, &one); err != nil {
		return fmt.Errorf("%w: nagios adapter: unmarshal: %v", ErrInvalidPayload, err)
	}
	if a.buildTitle(one) == "" {
		return fmt.Errorf("%w: nagios adapter: host_name or service_description", ErrMissingField)
	}
	return nil
}

// Parse converts the Nagios payload into a slice of unified Alerts.
// The payload may be a single JSON object or a JSON array of objects.
func (a *NagiosAdapter) Parse(raw []byte) ([]*Alert, error) {
	// Try array first.
	var arr []NagiosAlert
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]*Alert, 0, len(arr))
		for i, na := range arr {
			alert, err := a.convertOne(na, raw)
			if err != nil {
				return nil, fmt.Errorf("nagios adapter: alert[%d]: %w", i, err)
			}
			out = append(out, alert)
		}
		return out, nil
	}
	// Fall back to single object.
	var one NagiosAlert
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("%w: nagios adapter: unmarshal: %v", ErrInvalidPayload, err)
	}
	alert, err := a.convertOne(one, raw)
	if err != nil {
		return nil, fmt.Errorf("nagios adapter: %w", err)
	}
	return []*Alert{alert}, nil
}

// convertOne maps a NagiosAlert to an Alert.
func (a *NagiosAdapter) convertOne(na NagiosAlert, raw []byte) (*Alert, error) {
	title := a.buildTitle(na)
	if title == "" {
		return nil, fmt.Errorf("%w: host_name or service_description", ErrMissingField)
	}

	severity := nagiosStateToSeverity(na.State)
	status := a.parseStatus(na.NotificationType, na.State)
	startsAt := a.parseTime(na.Timestamp)
	labels := a.buildLabels(na)

	alert := &Alert{
		Source:      a.Name(),
		Severity:    severity,
		Title:       title,
		Description: na.CheckOutput,
		Labels:      labels,
		StartsAt:    startsAt,
		Status:      status,
		RawPayload:  raw,
	}
	alert.Fingerprint = alert.GenerateFingerprint()
	alert.ID = alert.Fingerprint
	return alert, nil
}

// buildTitle constructs the alert title. For service checks, the title is
// "host: service". For host checks (no service_description), the title is
// just the host name.
func (a *NagiosAdapter) buildTitle(na NagiosAlert) string {
	if na.ServiceDescription != "" {
		if na.HostName != "" {
			return na.HostName + ": " + na.ServiceDescription
		}
		return na.ServiceDescription
	}
	return na.HostName
}

// parseStatus determines the AlertStatus from notification_type and state.
// Nagios notification_type values: "PROBLEM", "RECOVERY", "ACKNOWLEDGEMENT",
// "FLAPPINGSTART", "FLAPPINGSTOP", "FLAPPINGDISABLE", "DOWNTIMESTART",
// "DOWNTIMEEND", "DOWNTIMECANCEL". Only "PROBLEM" is firing; "RECOVERY" is
// resolved; others default to firing unless the state is OK/UP.
func (a *NagiosAdapter) parseStatus(notificationType, state string) AlertStatus {
	nt := strings.ToUpper(strings.TrimSpace(notificationType))
	switch nt {
	case "PROBLEM":
		return StatusFiring
	case "RECOVERY":
		return StatusResolved
	case "":
		// No notification_type: derive from state.
		return nagiosStateToStatus(state)
	default:
		// ACKNOWLEDGEMENT, DOWNTIME*, FLAPPING*: derive from state so we
		// don't mislabel a recovery-time acknowledgement as firing.
		return nagiosStateToStatus(state)
	}
}

// parseTime parses the Nagios timestamp field. Supports RFC3339, the common
// Nagios format "2006-01-02 15:04:05", and Unix epoch seconds. Empty or
// unparseable values default to now.
func (a *NagiosAdapter) parseTime(timestamp string) time.Time {
	timestamp = strings.TrimSpace(timestamp)
	if timestamp == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339, timestamp); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", timestamp); err == nil {
		return t
	}
	// Try Unix timestamp (seconds).
	if n, err := strconv.ParseInt(timestamp, 10, 64); err == nil {
		return time.Unix(n, 0)
	}
	return time.Now()
}

// buildLabels constructs the label map from Nagios fields.
func (a *NagiosAdapter) buildLabels(na NagiosAlert) map[string]string {
	labels := make(map[string]string, 8)
	if na.HostName != "" {
		labels["host"] = na.HostName
	}
	if na.HostAddress != "" {
		labels["host_ip"] = na.HostAddress
	}
	if na.ServiceDescription != "" {
		labels["service"] = na.ServiceDescription
	}
	if na.State != "" {
		labels["state"] = na.State
	}
	if na.StateType != "" {
		labels["state_type"] = na.StateType
	}
	if na.CurrentAttempt != "" {
		labels["current_attempt"] = na.CurrentAttempt
	}
	if na.MaxAttempts != "" {
		labels["max_attempts"] = na.MaxAttempts
	}
	if na.NotificationType != "" {
		labels["notification_type"] = na.NotificationType
	}
	return labels
}

// EncodeReverse converts a unified Alert back into the Nagios shape. It is
// primarily useful for tests and for forwarding alerts to downstream Nagios
// instances.
func (a *NagiosAdapter) EncodeReverse(alert *Alert) (NagiosAlert, error) {
	if alert == nil {
		return NagiosAlert{}, fmt.Errorf("nagios adapter: nil alert")
	}
	na := NagiosAlert{
		CheckOutput: alert.Description,
		Timestamp:   alert.StartsAt.Format(time.RFC3339),
	}
	if alert.Labels != nil {
		na.HostName = alert.Labels["host"]
		na.HostAddress = alert.Labels["host_ip"]
		na.ServiceDescription = alert.Labels["service"]
		na.StateType = alert.Labels["state_type"]
		na.CurrentAttempt = alert.Labels["current_attempt"]
		na.MaxAttempts = alert.Labels["max_attempts"]
	}
	// Map unified severity back to Nagios state.
	switch alert.Severity {
	case SeverityInfo:
		na.State = string(NagiosStateOK)
	case SeverityWarning:
		na.State = string(NagiosStateWarning)
	case SeverityCritical:
		na.State = string(NagiosStateCritical)
	default:
		na.State = string(NagiosStateWarning)
	}
	// Map status to notification type.
	if alert.Status == StatusResolved {
		na.NotificationType = "RECOVERY"
	} else {
		na.NotificationType = "PROBLEM"
	}
	return na, nil
}