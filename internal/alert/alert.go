// Package alert implements the LEVEE alert gateway. It receives alerts from
// multiple sources (Prometheus Alertmanager, custom webhooks, ...), normalises
// them into a unified Alert model, and applies deduplication, aggregation and
// silencing before forwarding to a downstream AlertHandler.
//
// The package is concurrency-safe: every shared state structure (Deduper,
// Aggregator, Silencer, AlertGateway) protects its internals with a
// sync.RWMutex. The HTTP server uses the standard net/http multiplexer.
package alert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Severity classifies the urgency of an Alert. Lower values are less urgent.
type Severity int

const (
	// SeverityInfo is the lowest urgency. Used for informational alerts that
	// do not require immediate operator action.
	SeverityInfo Severity = iota
	// SeverityWarning indicates a potentially degraded condition. Operators
	// should investigate but the system is still serving traffic.
	SeverityWarning
	// SeverityCritical indicates a severe condition that requires immediate
	// operator intervention.
	SeverityCritical
)

// String returns the human-readable name of the severity. Unknown values fall
// back to "unknown".
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarning:
		return "warning"
	case SeverityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// ParseSeverity converts a string to a Severity. The comparison is
// case-insensitive. Unknown values return SeverityInfo and a non-nil error so
// callers can decide whether to reject or coerce.
func ParseSeverity(s string) (Severity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info", "informational":
		return SeverityInfo, nil
	case "warn", "warning":
		return SeverityWarning, nil
	case "crit", "critical":
		return SeverityCritical, nil
	default:
		return SeverityInfo, fmt.Errorf("alert: unknown severity %q", s)
	}
}

// AlertStatus indicates whether an Alert is currently firing or has been
// resolved.
type AlertStatus int

const (
	// StatusFiring means the Alert is currently active.
	StatusFiring AlertStatus = iota
	// StatusResolved means the Alert has been resolved.
	StatusResolved
)

// String returns the human-readable name of the status.
func (s AlertStatus) String() string {
	switch s {
	case StatusFiring:
		return "firing"
	case StatusResolved:
		return "resolved"
	default:
		return "unknown"
	}
}

// ParseAlertStatus converts a string to an AlertStatus. Unknown values return
// StatusFiring and a non-nil error.
func ParseAlertStatus(s string) (AlertStatus, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "firing", "active":
		return StatusFiring, nil
	case "resolved", "closed":
		return StatusResolved, nil
	default:
		return StatusFiring, fmt.Errorf("alert: unknown status %q", s)
	}
}

// Sentinel errors returned by the package. Callers should test with errors.Is.
var (
	// ErrAlertNotFound is returned when a lookup misses.
	ErrAlertNotFound = errors.New("alert: not found")
	// ErrDuplicateAlert is returned when an alert is rejected as a duplicate.
	ErrDuplicateAlert = errors.New("alert: duplicate")
	// ErrSilenced is returned when an alert is rejected because it matches a
	// silence rule.
	ErrSilenced = errors.New("alert: silenced")
	// ErrInvalidAlert is returned when an alert fails validation.
	ErrInvalidAlert = errors.New("alert: invalid")
)

// Alert is the unified alert model used internally by the gateway. Every
// adapter normalises its source-specific payload into this struct so that
// downstream processing (dedup, aggregation, silencing, dispatch) operates on
// a single shape.
type Alert struct {
	// ID is the gateway-assigned identifier. It is normally the same as
	// Fingerprint but may be overridden by the handler.
	ID string `json:"id"`
	// Source identifies the adapter that produced the alert, e.g.
	// "prometheus" or "custom".
	Source string `json:"source"`
	// Severity is the urgency level.
	Severity Severity `json:"severity"`
	// Title is a short human-readable summary.
	Title string `json:"title"`
	// Description is a longer human-readable explanation.
	Description string `json:"description"`
	// Labels carry key/value metadata used for grouping, routing and
	// silencing. Labels are sorted by key when computing the fingerprint.
	Labels map[string]string `json:"labels"`
	// Fingerprint is a stable hash of the alert identity. Two alerts with
	// the same Fingerprint are considered duplicates.
	Fingerprint string `json:"fingerprint"`
	// StartsAt is when the alert started firing.
	StartsAt time.Time `json:"starts_at"`
	// EndsAt is when the alert was resolved. Zero value means still firing.
	EndsAt time.Time `json:"ends_at"`
	// Status is the current lifecycle state.
	Status AlertStatus `json:"status"`
	// RawPayload preserves the original bytes received from the source.
	// Useful for debugging and re-processing.
	RawPayload json.RawMessage `json:"raw_payload,omitempty"`
}

// NewAlert constructs an Alert with sensible defaults and a freshly computed
// fingerprint. The caller may override the fingerprint afterwards if needed.
//
// The function never panics. Missing fields are filled with zero values.
func NewAlert(source, title string, severity Severity, labels map[string]string, startsAt time.Time) *Alert {
	a := &Alert{
		Source:   source,
		Severity: severity,
		Title:    title,
		Labels:   labels,
		StartsAt: startsAt,
		Status:   StatusFiring,
	}
	a.Fingerprint = a.GenerateFingerprint()
	a.ID = a.Fingerprint
	return a
}

// Validate checks that the alert has the minimum required fields. It returns
// ErrInvalidAlert wrapping a descriptive message when validation fails.
func (a *Alert) Validate() error {
	if a == nil {
		return fmt.Errorf("%w: nil alert", ErrInvalidAlert)
	}
	if a.Source == "" {
		return fmt.Errorf("%w: empty source", ErrInvalidAlert)
	}
	if a.Title == "" {
		return fmt.Errorf("%w: empty title", ErrInvalidAlert)
	}
	if a.StartsAt.IsZero() {
		return fmt.Errorf("%w: zero starts_at", ErrInvalidAlert)
	}
	if a.Severity < SeverityInfo || a.Severity > SeverityCritical {
		return fmt.Errorf("%w: severity out of range %d", ErrInvalidAlert, a.Severity)
	}
	return nil
}

// GenerateFingerprint computes a stable SHA-256 hash over the alert identity
// fields (source, title, severity, sorted labels). The hash is hex-encoded
// and truncated to 32 characters for readability. Two alerts that differ only
// in description, timestamps or raw payload produce the same fingerprint.
func (a *Alert) GenerateFingerprint() string {
	if a == nil {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "source=%s|title=%s|severity=%d", a.Source, a.Title, a.Severity)
	// Sort label keys for deterministic output.
	keys := sortedKeys(a.Labels)
	for _, k := range keys {
		fmt.Fprintf(h, "|%s=%s", k, a.Labels[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// sortedKeys returns the keys of m in lexicographic order. Returns nil for a
// nil map.
func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Simple insertion sort: label maps are usually small (< 32 entries) so
	// the overhead of sort.Slice is not worth paying.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

// String returns a one-line human-readable representation suitable for logs.
func (a *Alert) String() string {
	if a == nil {
		return "<nil alert>"
	}
	return fmt.Sprintf("alert{id=%s source=%s sev=%s title=%q status=%s starts=%s}",
		a.ID, a.Source, a.Severity, a.Title, a.Status, a.StartsAt.Format(time.RFC3339))
}

// IsFiring reports whether the alert is currently in the firing state.
func (a *Alert) IsFiring() bool {
	return a != nil && a.Status == StatusFiring
}

// Duration returns the elapsed time between StartsAt and EndsAt. If the alert
// is still firing, the duration is measured from StartsAt to now.
func (a *Alert) Duration() time.Duration {
	if a == nil {
		return 0
	}
	end := a.EndsAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(a.StartsAt)
}

// MarshalJSON implements json.Marshaler. It ensures the fingerprint is
// recomputed when empty so that serialised alerts always carry an identity.
func (a *Alert) MarshalJSON() ([]byte, error) {
	if a == nil {
		return []byte("null"), nil
	}
	type alias Alert
	tmp := alias(*a)
	if tmp.Fingerprint == "" {
		tmp.Fingerprint = a.GenerateFingerprint()
	}
	if tmp.ID == "" {
		tmp.ID = tmp.Fingerprint
	}
	return json.Marshal(tmp)
}
