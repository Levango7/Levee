// adapter_custom.go adapts the LEVEE self-developed webhook payload into the
// unified Alert model.
//
// The custom payload is a JSON object (or array of objects) with the shape:
//
//	{
//	  "source": "custom",
//	  "severity": "critical",
//	  "title": "DiskFull",
//	  "description": "/ is 95% full",
//	  "labels": {"host": "node-1"},
//	  "starts_at": "2026-08-16T12:00:00Z",
//	  "ends_at": "0001-01-01T00:00:00Z",
//	  "status": "firing",
//	  "id": "optional-external-id"
//	}
//
// When the payload is an array, every element is parsed independently.
package alert

import (
	"encoding/json"
	"fmt"
	"time"
)

// CustomAlert is the per-alert object in a custom webhook payload.
type CustomAlert struct {
	Source      string            `json:"source"`
	Severity    string            `json:"severity"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Labels      map[string]string `json:"labels"`
	StartsAt    time.Time         `json:"starts_at"`
	EndsAt      time.Time         `json:"ends_at"`
	Status      string            `json:"status"`
	ID          string            `json:"id"`
}

// CustomAdapter parses custom webhook payloads.
type CustomAdapter struct{}

// NewCustomAdapter constructs a CustomAdapter.
func NewCustomAdapter() *CustomAdapter {
	return &CustomAdapter{}
}

// Name returns the adapter identifier.
func (a *CustomAdapter) Name() string { return "custom" }

// Validate checks that raw is a syntactically valid custom payload (single
// object or array of objects).
func (a *CustomAdapter) Validate(raw []byte) error {
	// Try array first, then single object.
	var arr []CustomAlert
	if err := json.Unmarshal(raw, &arr); err == nil {
		return nil
	}
	var one CustomAlert
	if err := json.Unmarshal(raw, &one); err != nil {
		return fmt.Errorf("custom adapter: unmarshal: %w", err)
	}
	return nil
}

// Parse converts the custom payload into a slice of unified Alerts.
func (a *CustomAdapter) Parse(raw []byte) ([]*Alert, error) {
	// Try array first.
	var arr []CustomAlert
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]*Alert, 0, len(arr))
		for i, ca := range arr {
			alert, err := a.convertOne(ca, raw)
			if err != nil {
				return nil, fmt.Errorf("custom adapter: alert[%d]: %w", i, err)
			}
			out = append(out, alert)
		}
		return out, nil
	}
	// Fall back to single object.
	var one CustomAlert
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("custom adapter: unmarshal: %w", err)
	}
	alert, err := a.convertOne(one, raw)
	if err != nil {
		return nil, fmt.Errorf("custom adapter: %w", err)
	}
	return []*Alert{alert}, nil
}

// convertOne maps a CustomAlert to an Alert.
func (a *CustomAdapter) convertOne(ca CustomAlert, raw []byte) (*Alert, error) {
	if ca.Title == "" {
		return nil, fmt.Errorf("missing title")
	}

	severity := SeverityWarning
	if ca.Severity != "" {
		if parsed, err := ParseSeverity(ca.Severity); err == nil {
			severity = parsed
		}
	}

	status := StatusFiring
	if ca.Status != "" {
		if st, err := ParseAlertStatus(ca.Status); err == nil {
			status = st
		}
	}

	startsAt := ca.StartsAt
	if startsAt.IsZero() {
		startsAt = time.Now()
	}

	source := ca.Source
	if source == "" {
		source = a.Name()
	}

	alert := &Alert{
		Source:      source,
		Severity:    severity,
		Title:       ca.Title,
		Description: ca.Description,
		Labels:      ca.Labels,
		StartsAt:    startsAt,
		EndsAt:      ca.EndsAt,
		Status:      status,
		RawPayload:  raw,
	}
	alert.Fingerprint = alert.GenerateFingerprint()
	if ca.ID != "" {
		alert.ID = ca.ID
	} else {
		alert.ID = alert.Fingerprint
	}
	return alert, nil
}
