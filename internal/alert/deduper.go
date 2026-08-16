// deduper.go implements fingerprint-based alert deduplication. Two alerts
// sharing the same Fingerprint within a configurable time window are
// considered duplicates and rejected by the gateway.
//
// The Deduper is concurrency-safe. Cleanup is amortised: every IsDuplicate
// call has a small probability of triggering a sweep that evicts expired
// entries, so callers do not need to run a separate goroutine.
package alert

import (
	"sync"
	"time"
)

// DeduperConfig controls Deduper behaviour.
type DeduperConfig struct {
	// Window is how long a fingerprint is remembered. Zero defaults to 5m.
	Window time.Duration
	// CleanupInterval governs the amortised sweep. Zero defaults to 1m.
	CleanupInterval time.Duration
}

// Deduper tracks recently seen alert fingerprints so that duplicates within
// the configured window can be rejected. Construct with NewDeduper.
type Deduper struct {
	mu      sync.RWMutex
	seen    map[string]time.Time
	window  time.Duration
	cleanup time.Duration
	// lastSweep is the last time Cleanup ran. Guarded by mu.
	lastSweep time.Time
	// now is the clock function. Override in tests for deterministic
	// behaviour.
	now func() time.Time
}

// NewDeduper constructs a Deduper with the given config. Zero-value fields
// are replaced with sensible defaults.
func NewDeduper(cfg DeduperConfig) *Deduper {
	if cfg.Window <= 0 {
		cfg.Window = 5 * time.Minute
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = time.Minute
	}
	return &Deduper{
		seen:    make(map[string]time.Time),
		window:  cfg.Window,
		cleanup: cfg.CleanupInterval,
		now:     time.Now,
	}
}

// IsDuplicate reports whether an alert with the given fingerprint has been
// seen within the dedup window. It does NOT mark the fingerprint as seen;
// call MarkSeen for that. The method is concurrency-safe and may trigger an
// amortised cleanup sweep.
func (d *Deduper) IsDuplicate(fingerprint string) bool {
	if fingerprint == "" {
		return false
	}
	d.maybeCleanup()
	d.mu.RLock()
	defer d.mu.RUnlock()
	ts, ok := d.seen[fingerprint]
	if !ok {
		return false
	}
	return d.now().Sub(ts) < d.window
}

// MarkSeen records the fingerprint as seen at the current time. If the
// fingerprint was already seen, the timestamp is refreshed. The method is
// concurrency-safe.
func (d *Deduper) MarkSeen(fingerprint string) {
	if fingerprint == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen[fingerprint] = d.now()
}

// CheckAndMark is a convenience helper that returns true (and marks the
// fingerprint) when the alert is NOT a duplicate, and false when it is. It
// performs the check-and-mark atomically under the write lock so that two
// concurrent goroutines cannot both pass.
func (d *Deduper) CheckAndMark(fingerprint string) bool {
	if fingerprint == "" {
		return true
	}
	d.maybeCleanup()
	d.mu.Lock()
	defer d.mu.Unlock()
	ts, ok := d.seen[fingerprint]
	now := d.now()
	if ok && now.Sub(ts) < d.window {
		return false
	}
	d.seen[fingerprint] = now
	return true
}

// Cleanup evicts every entry older than the dedup window. It is called
// automatically by maybeCleanup but is also exposed for tests and explicit
// maintenance.
func (d *Deduper) Cleanup() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cleanupLocked()
}

// cleanupLocked performs the sweep assuming the write lock is held.
func (d *Deduper) cleanupLocked() {
	now := d.now()
	d.lastSweep = now
	for fp, ts := range d.seen {
		if now.Sub(ts) >= d.window {
			delete(d.seen, fp)
		}
	}
}

// maybeCleanup runs Cleanup if the cleanup interval has elapsed since the
// last sweep. It uses a try-lock pattern so that concurrent readers do not
// block on the sweep.
func (d *Deduper) maybeCleanup() {
	d.mu.RLock()
	last := d.lastSweep
	d.mu.RUnlock()
	if d.now().Sub(last) < d.cleanup {
		return
	}
	// Try to acquire the write lock without blocking; if we can't, another
	// goroutine is already cleaning up.
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.now().Sub(d.lastSweep) < d.cleanup {
		return
	}
	d.cleanupLocked()
}

// Size returns the number of fingerprints currently remembered. It is
// primarily intended for diagnostics and tests.
func (d *Deduper) Size() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.seen)
}

// Reset clears all remembered fingerprints. Concurrency-safe.
func (d *Deduper) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = make(map[string]time.Time)
	d.lastSweep = time.Time{}
}
