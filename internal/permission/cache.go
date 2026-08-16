// Package permission cache.go implements an in-memory TTL cache for
// permission decisions. The cache is keyed by the (subject, action,
// resource) triple and stores a boolean decision plus the reason string
// returned by the ABAC engine. Entries expire after a configurable TTL
// (default 5 minutes) and a background goroutine sweeps expired entries
// periodically.
//
// The cache is opt-in: callers that do not need caching can use the
// engine directly. When the permission configuration changes the
// Invalidate / InvalidateAll methods must be called so stale entries do
// not produce wrong decisions.
package permission

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// DefaultCacheTTL is the TTL applied to cache entries when no explicit
// TTL is configured. Five minutes balances freshness against the cost
// of re-evaluating policies on every request.
const DefaultCacheTTL = 5 * time.Minute

// DefaultCacheSweepInterval is the interval between background sweeps
// that evict expired entries. It defaults to one minute.
const DefaultCacheSweepInterval = 1 * time.Minute

// cacheEntry is a single cached decision.
type cacheEntry struct {
	allowed   bool
	reason    string
	expiresAt time.Time
}

// isFresh reports whether the entry is still within its TTL.
func (e *cacheEntry) isFresh(now time.Time) bool {
	return now.Before(e.expiresAt)
}

// PermissionCache is a TTL cache for permission decisions. It is safe
// for concurrent use. A background goroutine started by StartSweeper
// periodically evicts expired entries; StopSweeper shuts it down.
type PermissionCache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
	ttl     time.Duration

	// sweeper control.
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewPermissionCache returns a cache with the given TTL. A zero or
// negative TTL is replaced by DefaultCacheTTL.
func NewPermissionCache(ttl time.Duration) *PermissionCache {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &PermissionCache{
		entries: make(map[string]cacheEntry),
		ttl:     ttl,
	}
}

// cacheKey builds the cache key from the (subject, action, resource)
// triple. The components are joined by a NUL byte so that no legal
// input can produce a key collision.
func cacheKey(subject, action, resource string) string {
	return strings.Join([]string{subject, action, resource}, "\x00")
}

// Get returns the cached decision for the triple. The ok result is true
// when an entry exists and is still fresh. Expired entries are evicted
// lazily on read.
func (c *PermissionCache) Get(subject, action, resource string) (allowed bool, reason string, ok bool) {
	key := cacheKey(subject, action, resource)
	now := time.Now()

	c.mu.RLock()
	entry, found := c.entries[key]
	c.mu.RUnlock()
	if !found {
		return false, "", false
	}
	if !entry.isFresh(now) {
		// Lazy eviction: take the write lock and delete. Another
		// goroutine may have refreshed the entry in the meantime, so
		// re-check under the write lock.
		c.mu.Lock()
		if cur, still := c.entries[key]; still && !cur.isFresh(now) {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		return false, "", false
	}
	return entry.allowed, entry.reason, true
}

// Set stores a decision for the triple with the configured TTL.
func (c *PermissionCache) Set(subject, action, resource string, allowed bool, reason string) {
	key := cacheKey(subject, action, resource)
	expiresAt := time.Now().Add(c.ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{
		allowed:   allowed,
		reason:    reason,
		expiresAt: expiresAt,
	}
}

// Invalidate removes the cached decision for a single triple. It is
// safe to call for triples that are not cached.
func (c *PermissionCache) Invalidate(subject, action, resource string) {
	key := cacheKey(subject, action, resource)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// InvalidateSubject removes every cached decision for the given subject.
// Use it when a subject's roles or group memberships change.
func (c *PermissionCache) InvalidateSubject(subject string) {
	prefix := subject + "\x00"
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) {
			delete(c.entries, k)
		}
	}
}

// InvalidateAll empties the cache. Use it when the permission
// configuration changes in a way that could affect any decision.
func (c *PermissionCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cacheEntry)
}

// Len returns the number of entries currently in the cache, including
// expired ones that have not yet been swept.
func (c *PermissionCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Stats returns the total number of entries and the number of fresh
// (non-expired) entries. Useful for diagnostics and tests.
func (c *PermissionCache) Stats() (total, fresh int) {
	now := time.Now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.entries {
		total++
		if e.isFresh(now) {
			fresh++
		}
	}
	return total, fresh
}

// StartSweeper launches a background goroutine that evicts expired
// entries every interval. Calling StartSweeper when a sweeper is already
// running is a no-op. The sweeper runs until StopSweeper is called.
func (c *PermissionCache) StartSweeper(interval time.Duration) {
	if interval <= 0 {
		interval = DefaultCacheSweepInterval
	}
	c.mu.Lock()
	if c.stopCh != nil {
		c.mu.Unlock()
		return
	}
	c.stopCh = make(chan struct{})
	stopCh := c.stopCh
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.sweep(time.Now())
			case <-stopCh:
				return
			}
		}
	}()
}

// StopSweeper shuts down the background sweeper started by
// StartSweeper. It blocks until the sweeper has exited. Calling
// StopSweeper when no sweeper is running is a no-op.
func (c *PermissionCache) StopSweeper() {
	c.mu.Lock()
	if c.stopCh == nil {
		c.mu.Unlock()
		return
	}
	stopCh := c.stopCh
	c.stopCh = nil
	c.mu.Unlock()

	close(stopCh)
	c.wg.Wait()
}

// sweep evicts expired entries. It is called by the background sweeper
// and may also be called directly (mainly by tests).
func (c *PermissionCache) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if !e.isFresh(now) {
			delete(c.entries, k)
		}
	}
}

// String returns a human-readable summary of the cache state for
// diagnostics.
func (c *PermissionCache) String() string {
	total, fresh := c.Stats()
	return fmt.Sprintf("PermissionCache{total=%d, fresh=%d, ttl=%s}", total, fresh, c.ttl)
}
