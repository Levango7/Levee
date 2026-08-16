// adapter_prometheus.go adapts the Prometheus Alertmanager webhook payload
// into the unified Alert model.
//
// Alertmanager sends a JSON array of objects with the following shape:
//
//	[
//	  {
//	    "status": "firing",
//	    "labels": {"alertname": "HighCpu", "severity": "warning"},
//	    "annotations": {"summary": "CPU > 90%", "description": "..."},
//	    "startsAt": "2026-08-16T12:00:00Z",
//	    "endsAt": "0001-01-01T00:00:00Z",
//	    "generatorURL": "http://prom/..."
//	  }
//	]
//
// The adapter maps labels.alertname -> Title, annotations.summary ->
// Description (falling back to annotations.description), labels.severity ->
// Severity (defaulting to warning), and the status / timestamps directly.
package alert

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PrometheusAlert is the per-alert object inside an Alertmanager webhook.
type PrometheusAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// PrometheusAdapter parses Alertmanager webhook payloads.
type PrometheusAdapter struct{}

// NewPrometheusAdapter constructs a PrometheusAdapter.
func NewPrometheusAdapter() *PrometheusAdapter {
	return &PrometheusAdapter{}
}

// Name returns the adapter identifier.
func (a *PrometheusAdapter) Name() string { return "prometheus" }

// Validate checks that raw is a syntactically valid Alertmanager payload.
// It does not validate individual alert fields; use Parse for that.
func (a *PrometheusAdapter) Validate(raw []byte) error {
	var arr []PrometheusAlert
	if err := json.Unmarshal(raw, &arr); err != nil {
		return fmt.Errorf("prometheus adapter: unmarshal: %w", err)
	}
	return nil
}

// Parse converts the Alertmanager payload into a slice of unified Alerts.
// Each input alert is mapped as documented at the top of the file. The
// original payload is preserved in RawPayload.
func (a *PrometheusAdapter) Parse(raw []byte) ([]*Alert, error) {
	var arr []PrometheusAlert
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("prometheus adapter: unmarshal: %w", err)
	}
	out := make([]*Alert, 0, len(arr))
	for i, pa := range arr {
		alert, err := a.convertOne(pa, raw)
		if err != nil {
			return nil, fmt.Errorf("prometheus adapter: alert[%d]: %w", i, err)
		}
		out = append(out, alert)
	}
	return out, nil
}

// convertOne maps a single PrometheusAlert to an Alert.
func (a *PrometheusAdapter) convertOne(pa PrometheusAlert, raw []byte) (*Alert, error) {
	title := pa.Labels["alertname"]
	if title == "" {
		return nil, fmt.Errorf("missing labels.alertname")
	}

	description := pa.Annotations["summary"]
	if description == "" {
		description = pa.Annotations["description"]
	}

	severity := SeverityWarning
	if sevStr, ok := pa.Labels["severity"]; ok {
		if parsed, err := ParseSeverity(sevStr); err == nil {
			severity = parsed
		}
	}

	status := StatusFiring
	if st, err := ParseAlertStatus(pa.Status); err == nil {
		status = st
	}
	// Alertmanager sometimes sends status="resolved" but endsAt in the far
	// future; trust the explicit status field.

	startsAt := pa.StartsAt
	if startsAt.IsZero() {
		startsAt = time.Now()
	}

	alert := &Alert{
		Source:      a.Name(),
		Severity:    severity,
		Title:       title,
		Description: description,
		Labels:      pa.Labels,
		StartsAt:    startsAt,
		EndsAt:      pa.EndsAt,
		Status:      status,
		RawPayload:  raw,
	}
	// Prefer the Alertmanager-provided fingerprint when present (it is
	// computed by Alertmanager itself); otherwise compute our own.
	if pa.Fingerprint != "" {
		alert.Fingerprint = pa.Fingerprint
	} else {
		alert.Fingerprint = alert.GenerateFingerprint()
	}
	alert.ID = alert.Fingerprint
	return alert, nil
}

// EncodeReverse is a small helper that converts a unified Alert back into the
// Prometheus shape. It is primarily useful for tests and for forwarding
// alerts to downstream Alertmanager instances.
func (a *PrometheusAdapter) EncodeReverse(alert *Alert) (PrometheusAlert, error) {
	if alert == nil {
		return PrometheusAlert{}, fmt.Errorf("prometheus adapter: nil alert")
	}
	pa := PrometheusAlert{
		Status:      alert.Status.String(),
		Labels:      alert.Labels,
		StartsAt:    alert.StartsAt,
		EndsAt:      alert.EndsAt,
		Fingerprint: alert.Fingerprint,
	}
	if pa.Labels == nil {
		pa.Labels = make(map[string]string)
	}
	pa.Labels["alertname"] = alert.Title
	pa.Labels["severity"] = strings.ToLower(alert.Severity.String())
	if alert.Description != "" {
		if pa.Annotations == nil {
			pa.Annotations = make(map[string]string)
		}
		pa.Annotations["summary"] = alert.Description
	}
	return pa, nil
}
