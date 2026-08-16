package alert

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestAggregator builds an Aggregator with a fake clock.
func newTestAggregator(window time.Duration, handler func(context.Context, *AlertGroup) error) (*Aggregator, *fakeClock) {
	clock := &fakeClock{t: time.Unix(2000, 0)}
	ag := NewAggregator(AggregatorConfig{Window: window}, handler)
	ag.now = clock.now
	return ag, clock
}

// TestAggregatorAddGroups verifies grouping by fingerprint.
func TestAggregatorAddGroups(t *testing.T) {
	ag, _ := newTestAggregator(time.Minute, nil)
	ctx := context.Background()

	a1 := NewAlert("s", "t", SeverityWarning, nil, time.Now())
	a2 := NewAlert("s", "t", SeverityWarning, nil, time.Now())  // same fingerprint
	a3 := NewAlert("s", "u", SeverityCritical, nil, time.Now()) // different

	g1, err := ag.Add(ctx, a1)
	require.NoError(t, err)
	g2, err := ag.Add(ctx, a2)
	require.NoError(t, err)
	assert.Equal(t, g1.Key, g2.Key, "same fingerprint -> same group")
	assert.Len(t, g1.Alerts, 2)

	g3, err := ag.Add(ctx, a3)
	require.NoError(t, err)
	assert.NotEqual(t, g1.Key, g3.Key)
	assert.Equal(t, SeverityCritical, g3.Severity)
	assert.Equal(t, 2, ag.Size())
}

// TestAggregatorGetGroup covers the lookup.
func TestAggregatorGetGroup(t *testing.T) {
	ag, _ := newTestAggregator(time.Minute, nil)
	ctx := context.Background()
	a := NewAlert("s", "t", SeverityWarning, nil, time.Now())
	g, err := ag.Add(ctx, a)
	require.NoError(t, err)

	got, err := ag.GetGroup(g.Key)
	require.NoError(t, err)
	assert.Equal(t, g.Key, got.Key)

	_, err = ag.GetGroup("missing")
	assert.Error(t, err)
}

// TestAggregatorSweep flushes expired groups to the handler.
func TestAggregatorSweep(t *testing.T) {
	var flushed atomic.Int32
	ag, clock := newTestAggregator(time.Minute, func(_ context.Context, _ *AlertGroup) error {
		flushed.Add(1)
		return nil
	})
	ctx := context.Background()

	_, err := ag.Add(ctx, NewAlert("s", "t", SeverityWarning, nil, time.Now()))
	require.NoError(t, err)
	_, err = ag.Add(ctx, NewAlert("s", "u", SeverityCritical, nil, time.Now()))
	require.NoError(t, err)

	// Nothing flushed yet.
	require.NoError(t, ag.Sweep(ctx))
	assert.Equal(t, int32(0), flushed.Load())

	clock.advance(2 * time.Minute)
	require.NoError(t, ag.Sweep(ctx))
	assert.Equal(t, int32(2), flushed.Load())
	assert.Equal(t, 0, ag.Size())
}

// TestAggregatorFlush forces every group out.
func TestAggregatorFlush(t *testing.T) {
	var flushed atomic.Int32
	ag, _ := newTestAggregator(time.Minute, func(_ context.Context, _ *AlertGroup) error {
		flushed.Add(1)
		return nil
	})
	ctx := context.Background()
	_, _ = ag.Add(ctx, NewAlert("s", "t", SeverityWarning, nil, time.Now()))
	_, _ = ag.Add(ctx, NewAlert("s", "u", SeverityCritical, nil, time.Now()))

	require.NoError(t, ag.Flush(ctx))
	assert.Equal(t, int32(2), flushed.Load())
	assert.Equal(t, 0, ag.Size())
}

// TestAggregatorCustomKey uses a custom grouping function.
func TestAggregatorCustomKey(t *testing.T) {
	bySource := func(a *Alert) string { return a.Source }
	ag := NewAggregator(AggregatorConfig{Window: time.Minute, GroupKeyFn: bySource}, nil)
	ctx := context.Background()

	a1 := NewAlert("prom", "t1", SeverityWarning, nil, time.Now())
	a2 := NewAlert("prom", "t2", SeverityWarning, nil, time.Now())
	a3 := NewAlert("custom", "t3", SeverityWarning, nil, time.Now())

	_, _ = ag.Add(ctx, a1)
	_, _ = ag.Add(ctx, a2)
	_, _ = ag.Add(ctx, a3)
	assert.Equal(t, 2, ag.Size(), "grouped by source")
}

// TestAggregatorNilAlert errors.
func TestAggregatorNilAlert(t *testing.T) {
	ag, _ := newTestAggregator(time.Minute, nil)
	_, err := ag.Add(context.Background(), nil)
	assert.Error(t, err)
}

// TestAggregatorGroupsSnapshot returns a copy.
func TestAggregatorGroupsSnapshot(t *testing.T) {
	ag, _ := newTestAggregator(time.Minute, nil)
	_, _ = ag.Add(context.Background(), NewAlert("s", "t", SeverityWarning, nil, time.Now()))
	snap := ag.Groups()
	assert.Len(t, snap, 1)
	ag.Reset()
	assert.Len(t, snap, 1, "snapshot is independent")
}
