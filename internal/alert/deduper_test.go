package alert

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fakeClock is a controllable clock for deterministic tests.
type fakeClock struct {
	t time.Time
}

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

// newTestDeduper builds a Deduper with a fake clock wired in.
func newTestDeduper(window time.Duration) (*Deduper, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	d := NewDeduper(DeduperConfig{Window: window, CleanupInterval: time.Hour})
	d.now = clock.now
	return d, clock
}

// TestDeduperBasic covers IsDuplicate / MarkSeen / CheckAndMark.
func TestDeduperBasic(t *testing.T) {
	d, _ := newTestDeduper(time.Minute)

	assert.False(t, d.IsDuplicate("fp1"), "fresh fingerprint is not a duplicate")
	d.MarkSeen("fp1")
	assert.True(t, d.IsDuplicate("fp1"), "after MarkSeen it is a duplicate")
	assert.False(t, d.IsDuplicate("fp2"), "different fingerprint is not a duplicate")
	assert.Equal(t, 1, d.Size())
}

// TestDeduperCheckAndMark is atomic.
func TestDeduperCheckAndMark(t *testing.T) {
	d, _ := newTestDeduper(time.Minute)
	assert.True(t, d.CheckAndMark("fp"), "first call should pass")
	assert.False(t, d.CheckAndMark("fp"), "second call should fail")
	assert.True(t, d.CheckAndMark("other"), "different fp should pass")
}

// TestDeduperExpiry verifies that fingerprints expire after the window.
func TestDeduperExpiry(t *testing.T) {
	d, clock := newTestDeduper(time.Minute)
	d.MarkSeen("fp")
	assert.True(t, d.IsDuplicate("fp"))
	clock.advance(time.Minute + time.Second)
	assert.False(t, d.IsDuplicate("fp"), "after window elapses it is no longer a duplicate")
}

// TestDeduperCleanup evicts expired entries.
func TestDeduperCleanup(t *testing.T) {
	d, clock := newTestDeduper(time.Minute)
	d.MarkSeen("a")
	d.MarkSeen("b")
	clock.advance(2 * time.Minute)
	d.Cleanup()
	assert.Equal(t, 0, d.Size())
}

// TestDeduperReset clears state.
func TestDeduperReset(t *testing.T) {
	d, _ := newTestDeduper(time.Minute)
	d.MarkSeen("a")
	d.MarkSeen("b")
	d.Reset()
	assert.Equal(t, 0, d.Size())
}

// TestDeduperEmptyFingerprint is a no-op.
func TestDeduperEmptyFingerprint(t *testing.T) {
	d, _ := newTestDeduper(time.Minute)
	assert.False(t, d.IsDuplicate(""))
	d.MarkSeen("") // no-op
	assert.True(t, d.CheckAndMark(""), "empty fingerprint always passes")
	assert.Equal(t, 0, d.Size())
}

// TestDeduperDefaults ensures zero config falls back to defaults.
func TestDeduperDefaults(t *testing.T) {
	d := NewDeduper(DeduperConfig{})
	assert.NotNil(t, d)
	d.MarkSeen("x")
	assert.True(t, d.IsDuplicate("x"))
}
