// Package drift implements configuration drift detection for LEVEE.
//
// It compares the declared state (captured from apply snapshots and stored as
// baselines) against the actual state obtained by probing target hosts, and
// reports any differences. Detected drift can be raised through the
// notification framework and aggregated into trend reports.
//
// The package is organised around four collaborating components:
//
//   - BaselineManager: stores and serves baselines keyed by (host, name). A
//     baseline is the expected state for one named version of a host, typically
//     derived from the most recent apply snapshot. A host with no name chosen
//     lives under DefaultBaselineName, which is also the version every
//     consumer that does not name one resolves to.
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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrBaselineNotFound is returned when no baseline exists for the given
	// (host, name) pair.
	ErrBaselineNotFound = errors.New("drift: baseline not found")
	// ErrEmptyHost is returned when a host identifier is empty.
	ErrEmptyHost = errors.New("drift: empty host")
	// ErrInvalidHost is returned when a host identifier cannot be used as a
	// storage key: it is empty after trimming or contains a path separator.
	// The CLI persists one baseline per file whose stem is derived from the
	// host, so a separator would escape the baselines directory — and the same
	// directory is reconciled by deleting every file it no longer expects.
	ErrInvalidHost = errors.New("drift: invalid host")
	// ErrInvalidBaselineName is returned when a baseline name is not one of
	// the accepted version labels.
	ErrInvalidBaselineName = errors.New("drift: invalid baseline name")
	// ErrEmptyBaseline is returned when a baseline contains no items.
	ErrEmptyBaseline = errors.New("drift: empty baseline")
)

// --- Baseline naming --------------------------------------------------------

// DefaultBaselineName is the name a baseline gets when its caller does not
// choose one. It is also the only name consumers that do not name a version
// resolve to — `drift detect --baseline auto`, `show`, `delete` and every
// scheduled job without a pinned name all read it. Baselines stored before
// named versions existed (a file per host, no name in the JSON) load under it,
// so upgrading changes nothing an operator has to do.
const DefaultBaselineName = "default"

// baselineNamePattern is the accepted shape of a chosen name. Lowercase only:
// the name becomes part of an on-disk file stem, and a case-insensitive
// filesystem would silently conflate "Prod" and "prod" into one baseline —
// the last state here wants to be a refusal, not a coin flip.
const baselineNamePattern = `^[a-z0-9][a-z0-9._-]{0,63}$`

var baselineNameRegexp = regexp.MustCompile(baselineNamePattern)

// NormalizeBaselineName trims a name and maps the empty name to
// DefaultBaselineName. A name that is present but malformed is an error.
// Callers that persist a name before a manager sees it use this so an unusable
// label is refused when it is written, not when a 3am scheduled run reads it.
func NormalizeBaselineName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return DefaultBaselineName, nil
	}
	if !baselineNameRegexp.MatchString(trimmed) {
		return "", fmt.Errorf("drift: baseline name %q: %w (must match %s)",
			name, ErrInvalidBaselineName, baselineNamePattern)
	}
	return trimmed, nil
}

// normalizeBaselineHost trims a host identifier and rejects values that cannot
// be a storage key.
func normalizeBaselineHost(host string) (string, error) {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return "", fmt.Errorf("drift: host: %w", ErrEmptyHost)
	}
	if strings.ContainsAny(trimmed, `/\`) || trimmed == "." || trimmed == ".." {
		return "", fmt.Errorf("drift: host %q: %w (path separators and dot-names are not allowed)",
			host, ErrInvalidHost)
	}
	return trimmed, nil
}

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

// Baseline is the expected-state snapshot for one named version of a single
// host. It is the reference against which DriftDetector compares probed state.
// A baseline is usually generated from the most recent apply snapshot
// (AutoGenerate) but can also be set manually (Set / CLI).
type Baseline struct {
	// ID is the unique baseline identifier (a hex string).
	ID string `json:"id"`
	// Host is the target host this baseline applies to.
	Host string `json:"host"`
	// Name is the version label of this baseline within its host. Two
	// baselines for the same host differ by name; the empty name on input
	// means DefaultBaselineName. It never collides with its host's other
	// versions, so keeping a second expected state (a compliance profile, the
	// snapshot before a migration) does not overwrite the one detection uses.
	Name string `json:"name"`
	// SourceRunID is the run that produced the snapshot this baseline was
	// generated from. It is empty for manually-set baselines.
	SourceRunID string `json:"source_run_id"`
	// CreatedAt is the wall-clock time the baseline was created (UTC).
	CreatedAt time.Time `json:"created_at"`
	// Items is the list of expected-state entries.
	Items []BaselineItem `json:"items"`
}

// --- BaselineManager --------------------------------------------------------

// baselineKey is the storage key of a baseline: a host plus the version label
// chosen for it. Both parts are already normalized, so one key shape exists.
type baselineKey struct {
	host string
	name string
}

// BaselineManager owns the set of stored baselines. It is safe for concurrent
// use. Baselines are keyed by (host, name): a host may keep several named
// versions at once, and the name that unaddressed reads resolve to is
// DefaultBaselineName.
type BaselineManager struct {
	mu        sync.RWMutex
	baselines map[baselineKey]*Baseline
}

// NewBaselineManager returns an empty BaselineManager ready to use.
func NewBaselineManager() *BaselineManager {
	return &BaselineManager{
		baselines: make(map[baselineKey]*Baseline),
	}
}

// GenerateFromSnapshot creates a Baseline named name for host from the given
// snapshot items and stores it. Re-generating the same (host, name) overwrites
// that version — that is how a host's current expected state is refreshed —
// while the host's other names are left alone. An empty name means
// DefaultBaselineName. The runID is recorded as the source of the baseline;
// pass an empty string for manually-constructed baselines.
func (bm *BaselineManager) GenerateFromSnapshot(host string, name string, runID string, snapshotItems []BaselineItem) (*Baseline, error) {
	key, err := newBaselineKey(host, name)
	if err != nil {
		return nil, fmt.Errorf("drift: generate baseline: %w", err)
	}
	if len(snapshotItems) == 0 {
		return nil, fmt.Errorf("drift: generate baseline: %w", ErrEmptyBaseline)
	}

	baselineID, err := generateBaselineID()
	if err != nil {
		return nil, err
	}
	baseline := &Baseline{
		ID:          baselineID,
		Host:        key.host,
		Name:        key.name,
		SourceRunID: runID,
		CreatedAt:   time.Now().UTC(),
		Items:       append([]BaselineItem(nil), snapshotItems...),
	}

	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.baselines[key] = baseline

	log.Info("drift: baseline generated from snapshot",
		"host", key.host,
		"name", key.name,
		"run_id", runID,
		"baseline_id", baseline.ID,
		"items", len(snapshotItems))
	return baseline, nil
}

// Get returns the baseline stored under (host, name), or ErrBaselineNotFound
// when that version does not exist. An empty name means DefaultBaselineName.
// When the host has other versions the error names them: "no baseline for
// web-01" is a dead end, while "…this host has: prod, staging" is the
// correction the operator was about to guess at anyway.
func (bm *BaselineManager) Get(host string, name string) (*Baseline, error) {
	key, err := newBaselineKey(host, name)
	if err != nil {
		return nil, fmt.Errorf("drift: get baseline: %w", err)
	}

	bm.mu.RLock()
	defer bm.mu.RUnlock()

	b, ok := bm.baselines[key]
	if !ok {
		return nil, fmt.Errorf("drift: get baseline for %q name %q: %w%s",
			key.host, key.name, ErrBaselineNotFound, bm.otherNamesHintLocked(key))
	}
	// Return a defensive copy so callers cannot mutate the stored baseline.
	out := *b
	out.Items = append([]BaselineItem(nil), b.Items...)
	return &out, nil
}

// Set stores baseline under its own (Host, Name). The struct is the single
// source for both key parts — a separate key argument could only disagree with
// the record it was supposed to index. It overwrites any baseline already
// stored under that pair and leaves the host's other versions untouched;
// re-generating the current expected state is how it gets refreshed. The
// baseline must have at least one item, an empty Name means
// DefaultBaselineName, and the stored copy carries a generated ID and
// CreatedAt when it had none.
func (bm *BaselineManager) Set(baseline *Baseline) error {
	if baseline == nil {
		return fmt.Errorf("drift: set baseline: nil baseline")
	}
	key, err := newBaselineKey(baseline.Host, baseline.Name)
	if err != nil {
		return fmt.Errorf("drift: set baseline: %w", err)
	}
	if len(baseline.Items) == 0 {
		return fmt.Errorf("drift: set baseline: %w", ErrEmptyBaseline)
	}

	// Normalise the key fields on the baseline so they match the key.
	stored := *baseline
	stored.Host = key.host
	stored.Name = key.name
	if stored.ID == "" {
		id, err := generateBaselineID()
		if err != nil {
			return err
		}
		stored.ID = id
	}
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	}
	stored.Items = append([]BaselineItem(nil), baseline.Items...)

	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.baselines[key] = &stored

	log.Info("drift: baseline set",
		"host", key.host,
		"name", key.name,
		"baseline_id", stored.ID,
		"items", len(stored.Items))
	return nil
}

// List returns every stored baseline ordered by host then name. The returned
// slice is a defensive copy; callers cannot mutate the stored baselines
// through it.
func (bm *BaselineManager) List() []*Baseline {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	return bm.listLocked(func(baselineKey) bool { return true })
}

// ListHost returns the stored baselines of one host, ordered by name. An
// unknown host yields an empty slice: the caller asked what versions a host
// has, and "none" is a fact rather than an error.
func (bm *BaselineManager) ListHost(host string) []*Baseline {
	key, err := newBaselineKey(host, "")
	if err != nil {
		return nil
	}

	bm.mu.RLock()
	defer bm.mu.RUnlock()

	return bm.listLocked(func(k baselineKey) bool { return k.host == key.host })
}

// Delete removes the baseline stored under (host, name) and returns
// ErrBaselineNotFound when it does not exist. It never touches the host's
// other versions: dropping a named version is one call per name, so a typo in
// --name cannot take the host's current expected state with it.
func (bm *BaselineManager) Delete(host string, name string) error {
	key, err := newBaselineKey(host, name)
	if err != nil {
		return fmt.Errorf("drift: delete baseline: %w", err)
	}

	bm.mu.Lock()
	defer bm.mu.Unlock()

	if _, ok := bm.baselines[key]; !ok {
		return fmt.Errorf("drift: delete baseline for %q name %q: %w%s",
			key.host, key.name, ErrBaselineNotFound, bm.otherNamesHintLocked(key))
	}
	delete(bm.baselines, key)
	log.Info("drift: baseline deleted", "host", key.host, "name", key.name)
	return nil
}

// --- key, listing and error-hint helpers ------------------------------------

// newBaselineKey normalizes a (host, name) pair into its storage key, mapping
// an empty name to DefaultBaselineName.
func newBaselineKey(host string, name string) (baselineKey, error) {
	h, err := normalizeBaselineHost(host)
	if err != nil {
		return baselineKey{}, err
	}
	n, err := NormalizeBaselineName(name)
	if err != nil {
		return baselineKey{}, err
	}
	return baselineKey{host: h, name: n}, nil
}

// listLocked collects defensive copies of the stored baselines whose key
// passes keep, ordered by host then name. The caller must hold the lock.
func (bm *BaselineManager) listLocked(keep func(baselineKey) bool) []*Baseline {
	out := make([]*Baseline, 0, len(bm.baselines))
	for key, b := range bm.baselines {
		if !keep(key) {
			continue
		}
		copied := *b
		copied.Items = append([]BaselineItem(nil), b.Items...)
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// otherNamesHintLocked renders the same host's stored versions for a
// not-found error. The caller must hold the lock.
func (bm *BaselineManager) otherNamesHintLocked(key baselineKey) string {
	names := make([]string, 0, len(bm.baselines))
	for k, b := range bm.baselines {
		if k.host != key.host {
			continue
		}
		names = append(names, b.Name)
	}
	if len(names) == 0 {
		return "; this host has no stored baseline"
	}
	sort.Strings(names)
	return fmt.Sprintf("; this host has other baselines: %s", strings.Join(names, ", "))
}

// AutoGenerate generates the baseline named name for host from the most recent
// apply snapshot. The runID identifies the source run; the snapshot items are
// extracted by the caller (typically from internal/rollback/snapshot) and
// passed in. This is a convenience wrapper around GenerateFromSnapshot that
// logs the auto-generation intent.
//
// In a full implementation the snapshot items would be fetched from the
// rollback snapshot store; here we accept them as a parameter so the drift
// package does not depend on the rollback package's on-disk layout.
func (bm *BaselineManager) AutoGenerate(host string, name string, runID string) (*Baseline, error) {
	key, err := newBaselineKey(host, name)
	if err != nil {
		return nil, fmt.Errorf("drift: auto generate baseline: %w", err)
	}
	if runID == "" {
		return nil, fmt.Errorf("drift: auto generate baseline: empty run id")
	}

	items, err := extractSnapshotItems(key.host, runID)
	if err != nil {
		return nil, fmt.Errorf("drift: auto generate baseline: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("drift: auto generate baseline: %w", ErrEmptyBaseline)
	}

	baseline, err := bm.GenerateFromSnapshot(key.host, key.name, runID, items)
	if err != nil {
		return nil, err
	}
	log.Info("drift: baseline auto-generated",
		"host", key.host,
		"name", key.name,
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

// generateBaselineID returns a random 16-byte hex string suitable for use as
// a Baseline.ID. Baseline IDs are uniqueness-critical primary keys, so a
// crypto/rand failure is an error — no timestamp fallback (SA-012).
func generateBaselineID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("drift: generate baseline id: %w", err)
	}
	return "bl-" + hex.EncodeToString(b), nil
}
