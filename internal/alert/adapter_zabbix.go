
// adapter_zabbix.go adapts the Zabbix webhook payload into the unified Alert
// model.
//
// Zabbix 4.0+ supports webhook media types that send JSON payloads. A typical
// payload produced by a Zabbix action webhook looks like:
//
//	{
//	  "event_id": "12345",
//	  "event_source": "trigger",
//	  "event_value": "1",
//	  "host": "web-server-01",
//	  "host_ip": "192.168.1.10",
//	  "trigger_id": "98765",
//	  "trigger_name": "CPU usage > 90% on web-server-01",
//	  "trigger_description": "CPU usage is too high",
//	  "trigger_severity": "4",
//	  "trigger_status": "PROBLEM",
//	  "trigger_url": "http://zabbix/triggers/98765",
//	  "item_lastvalue": "92.5",
//	  "datetime": "2026-08-16T12:00:00Z",
//	  "action": "PROBLEM"
//	}
//
// The adapter maps trigger_name -> Title, trigger_description -> Description,
// trigger_severity (numeric 0-5) -> Severity, trigger_status / action ->
// Status, host -> labels.host, host_ip -> labels.host_ip, and trigger_id ->
// labels.trigger_id. The original payload is preserved in RawPayload.
//
// Zabbix may also send a bare array of such objects (one per problem event).
package alert

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors shared by the Zabbix and Nagios adapters. Callers should
// test with errors.Is.
var (
	// ErrInvalidPayload is returned when a payload cannot be parsed as JSON
	// or does not conform to the expected schema.
	ErrInvalidPayload = errors.New("alert: invalid payload")
	// ErrMissingField is returned when a required field is absent from the
	// payload.
	ErrMissingField = errors.New("alert: missing field")
)

// ZabbixSeverity is the numeric severity level used by Zabbix triggers.
// Zabbix defines 6 levels: 0=Not classified, 1=Information, 2=Warning,
// 3=Average, 4=High, 5=Disaster.
type ZabbixSeverity int

const (
	// ZabbixSeverityNotClassified is Zabbix severity 0.
	ZabbixSeverityNotClassified ZabbixSeverity = 0
	// ZabbixSeverityInformation is Zabbix severity 1.
	ZabbixSeverityInformation ZabbixSeverity = 1
	// ZabbixSeverityWarning is Zabbix severity 2.
	ZabbixSeverityWarning ZabbixSeverity = 2
	// ZabbixSeverityAverage is Zabbix severity 3.
	ZabbixSeverityAverage ZabbixSeverity = 3
	// ZabbixSeverityHigh is Zabbix severity 4.
	ZabbixSeverityHigh ZabbixSeverity = 4
	// ZabbixSeverityDisaster is Zabbix severity 5.
	ZabbixSeverityDisaster ZabbixSeverity = 5
)

// zabbixSeverityToAlert maps a Zabbix numeric severity to the unified Severity.
// Levels 0-1 map to Info, 2-3 map to Warning, 4-5 map to Critical.
func zabbixSeverityToAlert(s ZabbixSeverity) Severity {
	switch s {
	case ZabbixSeverityNotClassified, ZabbixSeverityInformation:
		return SeverityInfo
	case ZabbixSeverityWarning, ZabbixSeverityAverage:
		return SeverityWarning
	case ZabbixSeverityHigh, ZabbixSeverityDisaster:
		return SeverityCritical
	default:
		// Out-of-range values fall back to Warning to avoid silently dropping
		// the alert or over-escalating it.
		return SeverityWarning
	}
}

// parseZabbixSeverity parses a numeric or named Zabbix severity string.
// It accepts "0".."5", "Not classified", "Information", "Warning",
// "Average", "High", "Disaster" (case-insensitive). Unknown values default
// to Warning.
func parseZabbixSeverity(s string) Severity {
	s = strings.TrimSpace(s)
	if s == "" {
		return SeverityWarning
	}
	// Try numeric first.
	if n, err := strconv.Atoi(s); err == nil {
		return zabbixSeverityToAlert(ZabbixSeverity(n))
	}
	// Fall back to named severity.
	switch strings.ToLower(s) {
	case "not classified", "na":
		return SeverityInfo
	case "information", "info":
		return SeverityInfo
	case "warning", "warn":
		return SeverityWarning
	case "average":
		return SeverityWarning
	case "high":
		return SeverityCritical
	case "disaster":
		return SeverityCritical
	default:
		return SeverityWarning
	}
}

// ZabbixAlert is the per-event object in a Zabbix webhook payload.
// Field names follow the Zabbix webhook macro convention. Unknown fields
// are ignored during unmarshalling.
type ZabbixAlert struct {
	EventID           string `json:"event_id"`
	EventSource       string `json:"event_source"`
	EventValue        string `json:"event_value"`
	Host              string `json:"host"`
	HostIP            string `json:"host_ip"`
	TriggerID         string `json:"trigger_id"`
	TriggerName       string `json:"trigger_name"`
	TriggerDescription string `json:"trigger_description"`
	TriggerSeverity   string `json:"trigger_severity"`
	TriggerStatus     string `json:"trigger_status"`
	TriggerURL        string `json:"trigger_url"`
	ItemLastValue     string `json:"item_lastvalue"`
	Datetime          string `json:"datetime"`
	Action            string `json:"action"`
}

// ZabbixAdapter parses Zabbix webhook payloads.
type ZabbixAdapter struct{}

// NewZabbixAdapter constructs a ZabbixAdapter.
func NewZabbixAdapter() *ZabbixAdapter {
	return &ZabbixAdapter{}
}

// Name returns the adapter identifier.
func (a *ZabbixAdapter) Name() string { return "zabbix" }

// Validate checks that raw is a syntactically valid Zabbix payload (single
// object or array of objects) and that every object carries a trigger_name
// (the minimum field required to build an Alert title).
func (a *ZabbixAdapter) Validate(raw []byte) error {
	// Try array first, then single object.
	var arr []ZabbixAlert
	if err := json.Unmarshal(raw, &arr); err == nil {
		for i, za := range arr {
			if za.TriggerName == "" {
				return fmt.Errorf("%w: zabbix adapter: alert[%d]: trigger_name", ErrMissingField, i)
			}
		}
		return nil
	}
	var one ZabbixAlert
	if err := json.Unmarshal(raw, &one); err != nil {
		return fmt.Errorf("%w: zabbix adapter: unmarshal: %v", ErrInvalidPayload, err)
	}
	if one.TriggerName == "" {
		return fmt.Errorf("%w: zabbix adapter: trigger_name", ErrMissingField)
	}
	return nil
}

// Parse converts the Zabbix payload into a slice of unified Alerts.
// The payload may be a single JSON object or a JSON array of objects.
func (a *ZabbixAdapter) Parse(raw []byte) ([]*Alert, error) {
	// Try array first.
	var arr []ZabbixAlert
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]*Alert, 0, len(arr))
		for i, za := range arr {
			alert, err := a.convertOne(za, raw)
			if err != nil {
				return nil, fmt.Errorf("zabbix adapter: alert[%d]: %w", i, err)
			}
			out = append(out, alert)
		}
		return out, nil
	}
	// Fall back to single object.
	var one ZabbixAlert
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("%w: zabbix adapter: unmarshal: %v", ErrInvalidPayload, err)
	}
	alert, err := a.convertOne(one, raw)
	if err != nil {
		return nil, fmt.Errorf("zabbix adapter: %w", err)
	}
	return []*Alert{alert}, nil
}

// convertOne maps a ZabbixAlert to an Alert.
func (a *ZabbixAdapter) convertOne(za ZabbixAlert, raw []byte) (*Alert, error) {
	title := za.TriggerName
	if title == "" {
		return nil, fmt.Errorf("%w: trigger_name", ErrMissingField)
	}

	severity := parseZabbixSeverity(za.TriggerSeverity)

	status := a.parseStatus(za.TriggerStatus, za.Action)

	startsAt := a.parseTime(za.Datetime)

	labels := a.buildLabels(za)

	description := za.TriggerDescription
	if description == "" {
		description = za.ItemLastValue
	}

	alert := &Alert{
		Source:      a.Name(),
		Severity:    severity,
		Title:       title,
		Description: description,
		Labels:      labels,
		StartsAt:    startsAt,
		Status:      status,
		RawPayload:  raw,
	}
	alert.Fingerprint = alert.GenerateFingerprint()
	// Prefer the Zabbix event_id as the alert ID when present.
	if za.EventID != "" {
		alert.ID = za.EventID
	} else {
		alert.ID = alert.Fingerprint
	}
	return alert, nil
}

// parseStatus determines the AlertStatus from trigger_status and action.
// Zabbix uses "PROBLEM" for firing and "OK"/"RESOLVED" for resolved.
func (a *ZabbixAdapter) parseStatus(triggerStatus, action string) AlertStatus {
	// The action field is more reliable: "PROBLEM" vs "RECOVERY".
	for _, s := range []string{action, triggerStatus} {
		switch strings.ToUpper(strings.TrimSpace(s)) {
		case "PROBLEM":
			return StatusFiring
		case "OK", "RESOLVED", "RECOVERY":
			return StatusResolved
		}
	}
	return StatusFiring
}

// parseTime parses the Zabbix datetime field. Zabbix sends timestamps in
// various formats depending on configuration; we try RFC3339 first, then a
// couple of common Zabbix date formats. A zero/empty value defaults to now.
func (a *ZabbixAdapter) parseTime(datetime string) time.Time {
	datetime = strings.TrimSpace(datetime)
	if datetime == "" {
		return time.Now()
	}
	// Try RFC3339 (most common from webhook media types).
	if t, err := time.Parse(time.RFC3339, datetime); err == nil {
		return t
	}
	// Try Zabbix default: "2026-08-16 12:00:00".
	if t, err := time.Parse("2006-01-02 15:04:05", datetime); err == nil {
		return t
	}
	// Try Unix timestamp (seconds).
	if n, err := strconv.ParseInt(datetime, 10, 64); err == nil {
		return time.Unix(n, 0)
	}
	// Fall back to now; better to accept the alert than to drop it.
	return time.Now()
}

// buildLabels constructs the label map from Zabbix fields that are useful for
// grouping, routing and silencing downstream.
func (a *ZabbixAdapter) buildLabels(za ZabbixAlert) map[string]string {
	labels := make(map[string]string, 8)
	if za.Host != "" {
		labels["host"] = za.Host
	}
	if za.HostIP != "" {
		labels["host_ip"] = za.HostIP
	}
	if za.TriggerID != "" {
		labels["trigger_id"] = za.TriggerID
	}
	if za.EventID != "" {
		labels["event_id"] = za.EventID
	}
	if za.TriggerSeverity != "" {
		labels["trigger_severity"] = za.TriggerSeverity
	}
	if za.EventSource != "" {
		labels["event_source"] = za.EventSource
	}
	if za.TriggerURL != "" {
		labels["trigger_url"] = za.TriggerURL
	}
	if za.ItemLastValue != "" {
		labels["item_lastvalue"] = za.ItemLastValue
	}
	return labels
}

// EncodeReverse converts a unified Alert back into the Zabbix shape. It is
// primarily useful for tests and for forwarding alerts to downstream Zabbix
// instances.
func (a *ZabbixAdapter) EncodeReverse(alert *Alert) (ZabbixAlert, error) {
	if alert == nil {
		return ZabbixAlert{}, fmt.Errorf("zabbix adapter: nil alert")
	}
	za := ZabbixAlert{
		TriggerName: alert.Title,
		TriggerDescription: alert.Description,
		Datetime:    alert.StartsAt.Format(time.RFC3339),
	}
	if alert.Labels != nil {
		za.Host = alert.Labels["host"]
		za.HostIP = alert.Labels["host_ip"]
		za.TriggerID = alert.Labels["trigger_id"]
		za.EventID = alert.Labels["event_id"]
		za.TriggerURL = alert.Labels["trigger_url"]
		za.ItemLastValue = alert.Labels["item_lastvalue"]
	}
	// Map unified severity back to Zabbix numeric severity.
	switch alert.Severity {
	case SeverityInfo:
		za.TriggerSeverity = strconv.Itoa(int(ZabbixSeverityInformation))
	case SeverityWarning:
		za.TriggerSeverity = strconv.Itoa(int(ZabbixSeverityWarning))
	case SeverityCritical:
		za.TriggerSeverity = strconv.Itoa(int(ZabbixSeverityHigh))
	default:
		za.TriggerSeverity = strconv.Itoa(int(ZabbixSeverityWarning))
	}
	// Map status to Zabbix action.
	if alert.Status == StatusResolved {
		za.Action = "RECOVERY"
		za.TriggerStatus = "OK"
	} else {
		za.Action = "PROBLEM"
		za.TriggerStatus = "PROBLEM"
	}
	if alert.ID != "" {
		za.EventID = alert.ID
	}
	return za, nil
}