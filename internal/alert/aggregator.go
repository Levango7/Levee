// aggregator.go groups alerts that share a common key (by default the
// Fingerprint) within a configurable time window. Grouped alerts are flushed
// to a caller-supplied handler when the window elapses or when Flush is
// called explicitly.
//
// The Aggregator is concurrency-safe.
package alert

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// AggregatorConfig controls Aggregator behaviour.
type AggregatorConfig struct {
	// Window is how long alerts are accumulated before being flushed. Zero
	// defaults to 30s.
	Window time.Duration
	// GroupKeyFn extracts the grouping key from an alert. When nil, the
	// alert Fingerprint is used.
	GroupKeyFn func(*Alert) string
}

// AlertGroup is a collection of alerts sharing a group key.
type AlertGroup struct {
	// Key is the grouping key.
	Key string `json:"key"`
	// Alerts is the slice of alerts in the group, in insertion order.
	Alerts []*Alert `json:"alerts"`
	// FirstSeen is when the group was created.
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen is when the most recent alert was added.
	LastSeen time.Time `json:"last_seen"`
	// Severity is the maximum severity across all alerts in the group.
	Severity Severity `json:"severity"`
}

// Add appends an alert to the group and updates aggregate metadata.
func (g *AlertGroup) Add(a *Alert, now time.Time) {
	g.Alerts = append(g.Alerts, a)
	g.LastSeen = now
	if g.FirstSeen.IsZero() {
		g.FirstSeen = now
	}
	if a.Severity > g.Severity {
		g.Severity = a.Severity
	}
}

// Aggregator accumulates alerts into groups keyed by a configurable function.
// Construct with NewAggregator.
type Aggregator struct {
	mu      sync.RWMutex
	groups  map[string]*AlertGroup
	window  time.Duration
	keyFn   func(*Alert) string
	handler func(context.Context, *AlertGroup) error
	now     func() time.Time
}

// NewAggregator constructs an Aggregator. The flush handler is invoked when
// a group's window elapses (during a periodic sweep) or when Flush is called
// explicitly. The handler may be nil; in that case flushed groups are simply
// discarded.
func NewAggregator(cfg AggregatorConfig, handler func(context.Context, *AlertGroup) error) *Aggregator {
	if cfg.Window <= 0 {
		cfg.Window = 30 * time.Second
	}
	if cfg.GroupKeyFn == nil {
		cfg.GroupKeyFn = func(a *Alert) string { return a.Fingerprint }
	}
	return &Aggregator{
		groups:  make(map[string]*AlertGroup),
		window:  cfg.Window,
		keyFn:   cfg.GroupKeyFn,
		handler: handler,
		now:     time.Now,
	}
}

// Add places the alert into its group. If the group did not exist it is
// created. The method is concurrency-safe.
func (ag *Aggregator) Add(ctx context.Context, a *Alert) (*AlertGroup, error) {
	if a == nil {
		return nil, fmt.Errorf("aggregator: nil alert")
	}
	key := ag.keyFn(a)
	if key == "" {
		key = a.Fingerprint
	}
	now := ag.now()
	ag.mu.Lock()
	defer ag.mu.Unlock()
	g, ok := ag.groups[key]
	if !ok {
		g = &AlertGroup{Key: key}
		ag.groups[key] = g
	}
	g.Add(a, now)
	return g, nil
}

// GetGroup returns the group for the given key, or ErrAlertNotFound.
func (ag *Aggregator) GetGroup(key string) (*AlertGroup, error) {
	ag.mu.RLock()
	defer ag.mu.RUnlock()
	g, ok := ag.groups[key]
	if !ok {
		return nil, fmt.Errorf("%w: group %q", ErrAlertNotFound, key)
	}
	return g, nil
}

// Flush forces every group to be flushed to the handler immediately. The
// aggregator is left empty. Returns the first handler error encountered; the
// remaining groups are still flushed.
func (ag *Aggregator) Flush(ctx context.Context) error {
	ag.mu.Lock()
	groups := ag.groups
	ag.groups = make(map[string]*AlertGroup)
	ag.mu.Unlock()

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var firstErr error
	for _, k := range keys {
		g := groups[k]
		if ag.handler == nil {
			continue
		}
		if err := ag.handler(ctx, g); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Sweep flushes every group whose window has elapsed. Groups that are still
// within the window are kept. The method is concurrency-safe and is called
// automatically by Add when the sweep interval has elapsed; callers may also
// invoke it explicitly.
func (ag *Aggregator) Sweep(ctx context.Context) error {
	now := ag.now()
	ag.mu.Lock()
	var expired []*AlertGroup
	for k, g := range ag.groups {
		if now.Sub(g.LastSeen) >= ag.window {
			expired = append(expired, g)
			delete(ag.groups, k)
		}
	}
	ag.mu.Unlock()

	if len(expired) == 0 {
		return nil
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].Key < expired[j].Key })

	var firstErr error
	if ag.handler != nil {
		for _, g := range expired {
			if err := ag.handler(ctx, g); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Groups returns a snapshot of all current groups keyed by their group key.
// The returned map is a copy and may be mutated safely by the caller.
func (ag *Aggregator) Groups() map[string]*AlertGroup {
	ag.mu.RLock()
	defer ag.mu.RUnlock()
	out := make(map[string]*AlertGroup, len(ag.groups))
	for k, g := range ag.groups {
		out[k] = g
	}
	return out
}

// Size returns the number of groups currently held.
func (ag *Aggregator) Size() int {
	ag.mu.RLock()
	defer ag.mu.RUnlock()
	return len(ag.groups)
}

// Reset drops every group without invoking the handler.
func (ag *Aggregator) Reset() {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	ag.groups = make(map[string]*AlertGroup)
}
