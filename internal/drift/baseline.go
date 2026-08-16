
// Package drift implements configuration drift detection for LEVEE.
//
// It compares the declared state (captured from apply snapshots and stored as
// baselines) against the actual state obtained by probing target hosts, and
// reports any differences. Detected drift can be raised through the
// notification framework and aggregated into trend reports.
//
// The package is organised around four collaborating components:
//
//   - BaselineManager: stores and serves per-host baselines. A baseline is the
//     expected state for a host, typically derived from the most recent apply
//     snapshot.
//   - DriftDetector: probes a host, compares the probe result against the
//     baseline and produces a DriftResult. It optionally notifies through the
//     notify package when drift is detected.
//   - DriftScheduler: runs DriftDetector on a cron schedule for a set of hosts.
//   - DriftReport / DriftTrend: aggregate one or more DriftResults into a
//     human- or machine-readable report and analyse historical drift trends.
package drift

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrBaselineNotFound is returned when no baseline exists for the given
	// host.
	ErrBaselineNotFound = errors.New("drift: baseline not found")
	// ErrBaselineExists is returned when attempting to overwrite an existing
	// baseline without explicit intent.
	ErrBaselineExists = errors.New("drift: baseline already exists")
	// ErrEmptyHost is returned when a host identifier is empty.
	ErrEmptyHost = errors.New("drift: empty host")
	// ErrEmptyBaseline is returned when a baseline contains no items.
	ErrEmptyBaseline = errors.New("drift: empty baseline")
)

// --- CheckType --------------------------------------------------------------

// CheckType classifies the kind of configuration item being checked. It is
// shared by Check (what to verify) and BaselineItem (what is expected).
type CheckType string

const (
	// CheckTypeFile verifies the content of a file at a given path.
	CheckTypeFile CheckType = "file"
	// CheckTypeService verifies the state of a systemd / init service.
	CheckTypeService CheckType = "service"
	// CheckTypePackage verifies the installed version of a package.
	CheckTypePackage CheckType = "package"
	// CheckTypeUser verifies the existence / properties of a system user.
	CheckTypeUser CheckType = "user"
)

// --- BaselineItem -----------------------------------------------------------

// BaselineItem is a single expected-state entry within a Baseline. It mirrors
// the fields of a Check so that a baseline can be directly translated into the
// set of checks performed during a detection run.
type BaselineItem struct {
	// CheckName is the human-readable name of the check, e.g. "nginx.conf".
	CheckName string `json:"check_name"`
	// Type is the kind of configuration item (file / service / package / user).
	Type CheckType `json:"type"`
	// Path is the file path, service name, package name or user name being
	// checked.
	Path string `json:"path"`
	// ExpectedValue is the expected content / state / version. The comparison
	// semantics depend on Type: for files it is typically a content hash or
	// verbatim text; for services it is "active" / "inactive"; for packages
	// it is the version string; for users it is "present" / "absent".
	ExpectedValue string `json:"expected_value"`
}

// --- Baseline ---------------------------------------------------------------

// Baseline is the expected-state snapshot for a single host. It is the
// reference against which DriftDetector compares probed state. A baseline is
// usually generated from the most recent apply snapshot (AutoGenerate) but can
// also be set manually (Set / CLI).
type Baseline struct {
	// ID is the unique baseline identifier (a hex string).
	ID string `json:"id"`
	// Host is the target host this baseline applies to.
	Host string `json:"host"`
	// SourceRunID is the run that produced the snapshot this baseline was
	// generated from. It is empty for manually-set baselines.
	SourceRunID string `json:"source_run_id"`
	// CreatedAt is the wall-clock time the baseline was created (UTC).
	CreatedAt time.Time `json:"created_at"`
	// Items is the list of expected-state entries.
	Items []BaselineItem `json:"items"`
}

// --- BaselineManager --------------------------------------------------------

// BaselineManager owns the set of per-host baselines. It is safe for
// concurrent use. Baselines are keyed by host name; each host has at most one
// active baseline at a time.
type BaselineManager struct {
	mu        sync.RWMutex
	baselines map[string]*Baseline
}

// NewBaselineManager returns an empty BaselineManager ready to use.
func NewBaselineManager() *BaselineManager {
	return &BaselineManager{
		baselines: make(map[string]*Baseline),
	}
}

// GenerateFromSnapshot creates a new Baseline from the given snapshot items
// and stores it as the active baseline for host. It overwrites any previously
// stored baseline for the same host. The runID is recorded as the source of
// the baseline; pass an empty string for manually-constructed baselines.
func (bm *BaselineManager) GenerateFromSnapshot(host string, runID string, snapshotItems []BaselineItem) (*Baseline, error) {
	if host == "" {
		return nil, fmt.Errorf("drift: generate baseline: %w", ErrEmptyHost)
	}
	if len(snapshotItems) == 0 {
		return nil, fmt.Errorf("drift: generate baseline: %w", ErrEmptyBaseline)
	}

	id, err := generateBaselineID()
	if err != nil {
		return nil, fmt.Errorf("drift: generate baseline id: %w", err)
	}

	baseline := &Baseline{
		ID:          id,
		Host:        host,
		SourceRunID: runID,
		CreatedAt:   time.Now().UTC(),
		Items:       append([]BaselineItem(nil), snapshotItems...),
	}

	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.baselines[host] = baseline

	log.Info("drift: baseline generated from snapshot",
		"host", host,
		"run_id", runID,
		"baseline_id", id,
		"items", len(snapshotItems))
	return baseline, nil
}

// Get returns the active baseline for the given host. It returns
// ErrBaselineNotFound when no baseline has been set for the host.
func (bm *BaselineManager) Get(host string) (*Baseline, error) {
	if host == "" {
		return nil, fmt.Errorf("drift: get baseline: %w", ErrEmptyHost)
	}

	bm.mu.RLock()
	defer bm.mu.RUnlock()

	b, ok := bm.baselines[host]
	if !ok {
		return nil, fmt.Errorf("drift: get baseline for %q: %w", host, ErrBaselineNotFound)
	}
	// Return a defensive copy so callers cannot mutate the stored baseline.
	out := *b
	out.Items = append([]BaselineItem(nil), b.Items...)
	return &out, nil
}

// Set stores baseline as the active baseline for baseline.Host. It overwrites
// any previously stored baseline for the same host. The baseline must have a
// non-empty Host and at least one item.
func (bm *BaselineManager) Set(host string, baseline *Baseline) error {
	if host == "" {
		return fmt.Errorf("drift: set baseline: %w", ErrEmptyHost)
	}
	if baseline == nil {
		return fmt.Errorf("drift: set baseline: nil baseline")
	}
	if len(baseline.Items) == 0 {
		return fmt.Errorf("drift: set baseline: %w", ErrEmptyBaseline)
	}

	// Normalise the host field on the baseline so it matches the key.
	stored := *baseline
	stored.Host = host
	if stored.ID == "" {
		id, err := generateBaselineID()
		if err != nil {
			return fmt.Errorf("drift: set baseline id: %w", err)
		}
		stored.ID = id
	}
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	}
	stored.Items = append([]BaselineItem(nil), baseline.Items...)

	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.baselines[host] = &stored

	log.Info("drift: baseline set",
		"host", host,
		"baseline_id", stored.ID,
		"items", len(stored.Items))
	return nil
}

// List returns all stored baselines ordered by host name. The returned slice
// is a defensive copy; callers cannot mutate the stored baselines through it.
func (bm *BaselineManager) List() []*Baseline {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	out := make([]*Baseline, 0, len(bm.baselines))
	for _, b := range bm.baselines {
		copy := *b
		copy.Items = append([]BaselineItem(nil), b.Items...)
		out = append(out, &copy)
	}
	// Sort by host for stable output.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Host > out[j].Host; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Delete removes the baseline for the given host. It returns
// ErrBaselineNotFound when no baseline exists for the host.
func (bm *BaselineManager) Delete(host string) error {
	if host == "" {
		return fmt.Errorf("drift: delete baseline: %w", ErrEmptyHost)
	}

	bm.mu.Lock()
	defer bm.mu.Unlock()

	if _, ok := bm.baselines[host]; !ok {
		return fmt.Errorf("drift: delete baseline for %q: %w", host, ErrBaselineNotFound)
	}
	delete(bm.baselines, host)
	log.Info("drift: baseline deleted", "host", host)
	return nil
}

// AutoGenerate generates a baseline for host from the most recent apply
// snapshot. The runID identifies the source run; the snapshot items are
// extracted by the caller (typically from internal/rollback/snapshot) and
// passed in. This is a convenience wrapper around GenerateFromSnapshot that
// logs the auto-generation intent.
//
// In a full implementation the snapshot items would be fetched from the
// rollback snapshot store; here we accept them as a parameter so the drift
// package does not depend on the rollback package's on-disk layout.
func (bm *BaselineManager) AutoGenerate(host string, runID string) (*Baseline, error) {
	if host == "" {
		return nil, fmt.Errorf("drift: auto generate baseline: %w", ErrEmptyHost)
	}
	if runID == "" {
		return nil, fmt.Errorf("drift: auto generate baseline: empty run id")
	}

	items, err := extractSnapshotItems(host, runID)
	if err != nil {
		return nil, fmt.Errorf("drift: auto generate baseline: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("drift: auto generate baseline: %w", ErrEmptyBaseline)
	}

	baseline, err := bm.GenerateFromSnapshot(host, runID, items)
	if err != nil {
		return nil, err
	}
	log.Info("drift: baseline auto-generated",
		"host", host,
		"run_id", runID,
		"baseline_id", baseline.ID)
	return baseline, nil
}

// --- SnapshotSource ---------------------------------------------------------

// SnapshotSource is the abstraction used by AutoGenerate to extract baseline
// items from an apply snapshot. The default implementation returns an empty
// list; callers can inject a real implementation via SetSnapshotSource so
// AutoGenerate works without the drift package depending on internal/rollback.
type SnapshotSource interface {
	// ExtractItems returns the baseline items derived from the snapshot of
	// the given run on the given host. An empty slice with a nil error
	// indicates the snapshot exists but contains no checkable items.
	ExtractItems(host string, runID string) ([]BaselineItem, error)
}

// snapshotSource is the package-level SnapshotSource used by AutoGenerate.
// It defaults to a no-op source that returns ErrNoSnapshotSource.
var (
	snapshotSourceMu sync.RWMutex
	snapshotSource   SnapshotSource
)

// ErrNoSnapshotSource is returned by AutoGenerate when no SnapshotSource has
// been configured.
var ErrNoSnapshotSource = errors.New("drift: no snapshot source configured")

// SetSnapshotSource configures the SnapshotSource used by AutoGenerate. Pass
// nil to clear a previously-configured source.
func SetSnapshotSource(src SnapshotSource) {
	snapshotSourceMu.Lock()
	defer snapshotSourceMu.Unlock()
	snapshotSource = src
}

// extractSnapshotItems delegates to the configured SnapshotSource. It returns
// ErrNoSnapshotSource when no source has been set.
func extractSnapshotItems(host string, runID string) ([]BaselineItem, error) {
	snapshotSourceMu.RLock()
	src := snapshotSource
	snapshotSourceMu.RUnlock()

	if src == nil {
		return nil, fmt.Errorf("drift: extract snapshot items: %w", ErrNoSnapshotSource)
	}
	return src.ExtractItems(host, runID)
}

// --- ID generation ----------------------------------------------------------

// generateBaselineID returns a random 16-byte hex string suitable for use as a
// Baseline.ID. If the crypto RNG fails it falls back to a timestamp-based id
// so that construction never fails.
func generateBaselineID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("bl-t%d", time.Now().UnixNano()), nil
	}
	return "bl-" + hex.EncodeToString(b), nil
}