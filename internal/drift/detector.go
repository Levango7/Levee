
// Detector implementation for the LEVEE drift package.
//
// This file defines the StateProber interface (the abstraction used to obtain
// the actual state of a target host), the Check / StateItem / DriftResult
// value types, and the DriftDetector that ties them together. The detector
// compares a probed state against a baseline and produces a DriftResult. When
// drift is detected and a notifier is configured, an alert is sent through the
// notify package.

package drift

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/notify"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrDriftDetected is returned by Detect when the probed state differs
	// from the baseline. The accompanying DriftResult is still returned so
	// callers can inspect the drift items.
	ErrDriftDetected = errors.New("drift: drift detected")
	// ErrNoProber is returned when a detector is used without a prober.
	ErrNoProber = errors.New("drift: no prober configured")
	// ErrEmptyHosts is returned when a batch detection is requested with an
	// empty host list.
	ErrEmptyHosts = errors.New("drift: empty host list")
)

// --- Check ------------------------------------------------------------------

// Check declares a single configuration item to verify on a target host. A
// set of Checks is derived from a Baseline by the detector and passed to the
// StateProber.
type Check struct {
	// Name is the human-readable name of the check, e.g. "nginx.conf".
	Name string `json:"name"`
	// Type is the kind of configuration item (file / service / package / user).
	Type CheckType `json:"type"`
	// Path is the file path, service name, package name or user name.
	Path string `json:"path"`
	// ExpectedValue is the expected content / state / version.
	ExpectedValue string `json:"expected_value"`
}

// --- StateItem --------------------------------------------------------------

// StateItem is the result of probing a single Check on a target host. It
// captures the actual value observed by the prober and whether it differs
// from the expected value.
type StateItem struct {
	// CheckName mirrors Check.Name for correlation.
	CheckName string `json:"check_name"`
	// ActualValue is the value observed by the prober.
	ActualValue string `json:"actual_value"`
	// ExpectedValue is the value the baseline declared.
	ExpectedValue string `json:"expected_value"`
	// Drifted is true when ActualValue differs from ExpectedValue.
	Drifted bool `json:"drifted"`
	// Diff is a human-readable description of the difference when Drifted is
	// true; empty otherwise.
	Diff string `json:"diff,omitempty"`
}

// --- DriftResult ------------------------------------------------------------

// DriftResult is the outcome of a single Detect call. It aggregates the
// StateItems for one host and counts how many drifted.
type DriftResult struct {
	// Host is the target host that was probed.
	Host string `json:"host"`
	// Timestamp is when the detection completed (UTC).
	Timestamp time.Time `json:"timestamp"`
	// Items is the per-check state.
	Items []StateItem `json:"items"`
	// DriftCount is the number of Items with Drifted == true.
	DriftCount int `json:"drift_count"`
	// TotalChecks is the total number of Items.
	TotalChecks int `json:"total_checks"`
}

// HasDrift reports whether the result contains any drifted items.
func (r *DriftResult) HasDrift() bool {
	return r != nil && r.DriftCount > 0
}

// --- StateProber ------------------------------------------------------------

// StateProber is the abstraction implemented by every concrete state probing
// strategy (SSH, WinRM, agent-based, ...). The detector calls Probe with the
// host and the list of checks derived from the baseline; the implementation
// returns the observed state for each check in the same order.
//
// Implementations must honour ctx cancellation and timeouts. They must be
// safe for concurrent use: the detector may call Probe on the same instance
// from multiple goroutines when running batch detection.
type StateProber interface {
	// Probe returns the observed StateItems for the given checks on the
	// given host. The returned slice must have the same length and order as
	// checks; if a check cannot be probed the corresponding StateItem should
	// have Drifted set to true and Diff describing the error.
	Probe(ctx context.Context, host string, checks []Check) ([]StateItem, error)
}

// --- DriftDetector ----------------------------------------------------------

// DriftDetector compares the actual state of a target host (obtained through a
// StateProber) against a Baseline and produces a DriftResult. It optionally
// notifies through the notify package when drift is detected.
//
// A single DriftDetector is safe for concurrent use. The notifier is optional;
// when nil, detection runs silently.
type DriftDetector struct {
	prober   StateProber
	notifier notify.Notifier
	mu       sync.RWMutex
}

// NewDetector returns a DriftDetector that uses the given prober to obtain
// actual state. The prober must be non-nil; pass a mock implementation in
// tests.
func NewDetector(prober StateProber) *DriftDetector {
	return &DriftDetector{
		prober: prober,
	}
}

// SetNotifier configures the notifier used to alert on drift. Pass nil to
// disable notifications. The change takes effect for subsequent Detect calls.
func (d *DriftDetector) SetNotifier(n notify.Notifier) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.notifier = n
}

// getNotifier returns the currently configured notifier under the read lock.
func (d *DriftDetector) getNotifier() notify.Notifier {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.notifier
}

// getProber returns the configured prober under the read lock.
func (d *DriftDetector) getProber() StateProber {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.prober
}

// SetProber replaces the prober. This is primarily intended for tests that
// want to swap in a mock after construction. Pass nil to clear the prober;
// subsequent Detect calls will return ErrNoProber.
func (d *DriftDetector) SetProber(p StateProber) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prober = p
}

// Detect probes a single host and compares the result against the given
// baseline. It returns a DriftResult regardless of whether drift was
// detected; the error wraps ErrDriftDetected when drift is present so callers
// can use errors.Is to distinguish the two cases.
func (d *DriftDetector) Detect(ctx context.Context, host string, baseline *Baseline) (*DriftResult, error) {
	if host == "" {
		return nil, fmt.Errorf("drift: detect: %w", ErrEmptyHost)
	}
	if baseline == nil {
		return nil, fmt.Errorf("drift: detect: nil baseline")
	}

	prober := d.getProber()
	if prober == nil {
		return nil, fmt.Errorf("drift: detect: %w", ErrNoProber)
	}

	checks := baselineToChecks(baseline)
	items, err := prober.Probe(ctx, host, checks)
	if err != nil {
		return nil, fmt.Errorf("drift: detect: probe: %w", err)
	}

	result := buildResult(host, items)
	if result.HasDrift() {
		d.sendDriftAlert(ctx, host, result)
		log.Warn("drift: drift detected",
			"host", host,
			"drift_count", result.DriftCount,
			"total_checks", result.TotalChecks)
		return result, fmt.Errorf("drift: detect: %w", ErrDriftDetected)
	}

	log.Info("drift: no drift detected",
		"host", host,
		"total_checks", result.TotalChecks)
	return result, nil
}

// DetectBatch runs Detect for each host in hosts against the same baseline.
// Detection runs concurrently across hosts; the returned slice preserves the
// order of hosts. A per-host error does not abort the batch: the corresponding
// entry in the returned results slice is nil and the error is recorded in the
// returned aggregated error (which wraps ErrDriftDetected when at least one
// host drifted).
//
// When a host's baseline differs (e.g. per-host baselines), callers should
// invoke Detect directly in a loop.
func (d *DriftDetector) DetectBatch(ctx context.Context, hosts []string, baseline *Baseline) ([]*DriftResult, error) {
	if len(hosts) == 0 {
		return nil, fmt.Errorf("drift: detect batch: %w", ErrEmptyHosts)
	}
	if baseline == nil {
		return nil, fmt.Errorf("drift: detect batch: nil baseline")
	}

	results := make([]*DriftResult, len(hosts))
	errs := make([]error, len(hosts))
	var wg sync.WaitGroup

	for i, host := range hosts {
		wg.Add(1)
		go func(idx int, h string) {
			defer wg.Done()
			r, err := d.Detect(ctx, h, baseline)
			results[idx] = r
			errs[idx] = err
		}(i, host)
	}
	wg.Wait()

	// Aggregate errors. We keep the first non-nil error as the wrapped cause
	// and count how many hosts drifted or failed.
	var firstErr error
	driftCount := 0
	failCount := 0
	for i := range hosts {
		if errs[i] == nil {
			continue
		}
		if firstErr == nil {
			firstErr = errs[i]
		}
		if errors.Is(errs[i], ErrDriftDetected) {
			driftCount++
		} else {
			failCount++
		}
	}

	if firstErr == nil {
		return results, nil
	}
	if failCount > 0 {
		return results, fmt.Errorf("drift: detect batch: %d host(s) drifted, %d failed: %w",
			driftCount, failCount, firstErr)
	}
	return results, fmt.Errorf("drift: detect batch: %d host(s) drifted: %w",
		driftCount, firstErr)
}

// --- Helpers ----------------------------------------------------------------

// baselineToChecks translates a Baseline into the slice of Checks performed by
// the prober. The order of items is preserved.
func baselineToChecks(b *Baseline) []Check {
	checks := make([]Check, len(b.Items))
	for i, item := range b.Items {
		checks[i] = Check{
			Name:         item.CheckName,
			Type:         item.Type,
			Path:         item.Path,
			ExpectedValue: item.ExpectedValue,
		}
	}
	return checks
}

// buildResult assembles a DriftResult from the probed items, marking each
// item as drifted when its actual value differs from the expected value.
func buildResult(host string, items []StateItem) *DriftResult {
	result := &DriftResult{
		Host:        host,
		Timestamp:   time.Now().UTC(),
		Items:       make([]StateItem, len(items)),
		TotalChecks: len(items),
	}
	for i, item := range items {
		// The prober may already set Drifted; if not, derive it from the
		// expected/actual values so simple string comparison works out of
		// the box.
		copied := item
		if !copied.Drifted && copied.ActualValue != copied.ExpectedValue {
			copied.Drifted = true
			copied.Diff = diffDescription(copied.CheckName, copied.ExpectedValue, copied.ActualValue)
		}
		if copied.Drifted && copied.Diff == "" {
			copied.Diff = diffDescription(copied.CheckName, copied.ExpectedValue, copied.ActualValue)
		}
		result.Items[i] = copied
		if copied.Drifted {
			result.DriftCount++
		}
	}
	return result
}

// diffDescription returns a human-readable description of the difference
// between the expected and actual values for a check.
func diffDescription(name, expected, actual string) string {
	return fmt.Sprintf("%s: expected %q, got %q", name, expected, actual)
}

// --- Drift alerting ---------------------------------------------------------

// sendDriftAlert dispatches a notification when drift is detected. It is best
// effort: a notification failure is logged but does not affect the detection
// result. The alert is sent on a background-detached context so that
// cancellation of the caller's context does not abort delivery.
func (d *DriftDetector) sendDriftAlert(ctx context.Context, host string, result *DriftResult) {
	notifier := d.getNotifier()
	if notifier == nil {
		return
	}

	msg := notify.NewMessage(
		"drift_detected",
		result.Host,
		notify.LevelWarning,
		fmt.Sprintf("Configuration drift detected on %s", host),
		buildAlertBody(result),
	)
	msg.Metadata = map[string]string{
		"host":        host,
		"drift_count": fmt.Sprintf("%d", result.DriftCount),
		"total_checks": fmt.Sprintf("%d", result.TotalChecks),
	}

	// Detach from the caller's context so cancellation does not abort
	// delivery. We use context.Background() for the synchronous send; the
	// notifier is expected to honour its own timeouts.
	bg := context.Background()
	if err := notifier.Send(bg, msg); err != nil {
		log.Error("drift: failed to send drift alert",
			"host", host,
			"notifier", notifier.Name(),
			"err", err)
	}
	_ = ctx // ctx retained for future tracing / cancellation hooks
}

// buildAlertBody renders a human-readable body for the drift alert message.
func buildAlertBody(result *DriftResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Drift detected on host %s.\n", result.Host)
	fmt.Fprintf(&b, "Drifted checks: %d / %d\n\n", result.DriftCount, result.TotalChecks)
	for _, item := range result.Items {
		if !item.Drifted {
			continue
		}
		fmt.Fprintf(&b, "  - %s: %s\n", item.CheckName, item.Diff)
	}
	return b.String()
}