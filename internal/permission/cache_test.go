package permission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPermissionCacheGetSet(t *testing.T) {
	c := NewPermissionCache(time.Minute)
	c.Set("alice", "apply", "change:1", true, "allowed by p1")

	allowed, reason, ok := c.Get("alice", "apply", "change:1")
	require.True(t, ok)
	assert.True(t, allowed)
	assert.Equal(t, "allowed by p1", reason)
}

func TestPermissionCacheMiss(t *testing.T) {
	c := NewPermissionCache(time.Minute)
	_, _, ok := c.Get("alice", "apply", "change:1")
	assert.False(t, ok)
}

func TestPermissionCacheTTL(t *testing.T) {
	c := NewPermissionCache(50 * time.Millisecond)
	c.Set("alice", "apply", "change:1", true, "ok")

	// Fresh.
	_, _, ok := c.Get("alice", "apply", "change:1")
	assert.True(t, ok)

	// Wait for expiry.
	time.Sleep(80 * time.Millisecond)
	_, _, ok = c.Get("alice", "apply", "change:1")
	assert.False(t, ok)
}

func TestPermissionCacheInvalidate(t *testing.T) {
	c := NewPermissionCache(time.Minute)
	c.Set("alice", "apply", "change:1", true, "ok")
	c.Set("alice", "view", "change:1", true, "ok")
	c.Set("bob", "apply", "change:1", true, "ok")
	assert.Equal(t, 3, c.Len())

	c.Invalidate("alice", "apply", "change:1")
	assert.Equal(t, 2, c.Len())

	_, _, ok := c.Get("alice", "apply", "change:1")
	assert.False(t, ok)
}

func TestPermissionCacheInvalidateSubject(t *testing.T) {
	c := NewPermissionCache(time.Minute)
	c.Set("alice", "apply", "change:1", true, "ok")
	c.Set("alice", "view", "change:2", true, "ok")
	c.Set("bob", "apply", "change:1", true, "ok")
	assert.Equal(t, 3, c.Len())

	c.InvalidateSubject("alice")
	assert.Equal(t, 1, c.Len())

	_, _, ok := c.Get("bob", "apply", "change:1")
	assert.True(t, ok)
}

func TestPermissionCacheInvalidateAll(t *testing.T) {
	c := NewPermissionCache(time.Minute)
	c.Set("alice", "apply", "change:1", true, "ok")
	c.Set("bob", "view", "change:2", true, "ok")
	assert.Equal(t, 2, c.Len())

	c.InvalidateAll()
	assert.Equal(t, 0, c.Len())
}

func TestPermissionCacheStats(t *testing.T) {
	c := NewPermissionCache(time.Minute)
	c.Set("a", "x", "r1", true, "ok")
	c.Set("b", "y", "r2", true, "ok")

	total, fresh := c.Stats()
	assert.Equal(t, 2, total)
	assert.Equal(t, 2, fresh)
}

func TestPermissionCacheSweep(t *testing.T) {
	c := NewPermissionCache(20 * time.Millisecond)
	c.Set("a", "x", "r1", true, "ok")
	c.Set("b", "y", "r2", true, "ok")

	time.Sleep(40 * time.Millisecond)
	// Entries are expired but not yet swept.
	total, fresh := c.Stats()
	assert.Equal(t, 2, total)
	assert.Equal(t, 0, fresh)

	c.sweep(time.Now())
	total, fresh = c.Stats()
	assert.Equal(t, 0, total)
	assert.Equal(t, 0, fresh)
}

func TestPermissionCacheStartStopSweeper(t *testing.T) {
	c := NewPermissionCache(20 * time.Millisecond)
	c.StartSweeper(10 * time.Millisecond)
	defer c.StopSweeper()

	c.Set("a", "x", "r1", true, "ok")
	time.Sleep(60 * time.Millisecond)

	// The sweeper should have evicted the expired entry.
	total, _ := c.Stats()
	assert.Equal(t, 0, total)
}

func TestPermissionCacheStartSweeperIdempotent(t *testing.T) {
	c := NewPermissionCache(time.Minute)
	c.StartSweeper(50 * time.Millisecond)
	c.StartSweeper(50 * time.Millisecond) // no-op
	c.StopSweeper()
}

func TestPermissionCacheStopSweeperNoop(t *testing.T) {
	c := NewPermissionCache(time.Minute)
	// StopSweeper when nothing is running should be a no-op.
	c.StopSweeper()
}

func TestPermissionCacheDefaultTTL(t *testing.T) {
	c := NewPermissionCache(0)
	assert.Equal(t, DefaultCacheTTL, c.ttl)

	c2 := NewPermissionCache(-1)
	assert.Equal(t, DefaultCacheTTL, c2.ttl)
}

func TestPermissionCacheString(t *testing.T) {
	c := NewPermissionCache(time.Minute)
	c.Set("a", "x", "r1", true, "ok")
	s := c.String()
	assert.Contains(t, s, "total=1")
	assert.Contains(t, s, "fresh=1")
}

func TestPermissionCacheKeyCollision(t *testing.T) {
	c := NewPermissionCache(time.Minute)
	// Different triples should not collide.
	c.Set("a", "b", "c", true, "first")
	c.Set("a", "b", "d", false, "second")

	allowed, _, ok := c.Get("a", "b", "c")
	require.True(t, ok)
	assert.True(t, allowed)

	allowed, _, ok = c.Get("a", "b", "d")
	require.True(t, ok)
	assert.False(t, allowed)
}
