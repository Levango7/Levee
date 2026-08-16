package drift

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/nexus/levee/internal/notify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mockStateProber -------------------------------------------------------

// mockStateProber implements StateProber for testing. It returns pre-configured
// state items or an error.
type mockStateProber struct {
	mu    sync.Mutex
	items []StateItem
	err   error
	calls int
}

func (m *mockStateProber) Probe(ctx context.Context, host string, checks []Check) ([]StateItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	// If items are pre-configured, return them; otherwise build items from
	// checks with matching actual values.
	if m.items != nil {
		return m.items, nil
	}
	items := make([]StateItem, len(checks))
	for i, c := range checks {
		items[i] = StateItem{
			CheckName:     c.Name,
			ActualValue:   c.ExpectedValue, // no drift by default
			ExpectedValue: c.ExpectedValue,
		}
	}
	return items, nil
}

// --- NewDetector -----------------------------------------------------------

func TestNewDetector(t *testing.T) {
	prober := &mockStateProber{}
	d := NewDetector(prober)
	assert.NotNil(t, d)
}

// --- Detect: no drift ------------------------------------------------------

func TestDetect_NoDrift(t *testing.T) {
	prober := &mockStateProber{}
	d := NewDetector(prober)

	baseline := &Baseline{
		ID:   "bl-1",
		Host: "web-01",
		Items: []BaselineItem{
			{CheckName: "nginx.conf", Type: CheckTypeFile, Path: "/etc/nginx/nginx.conf", ExpectedValue: "content"},
			{CheckName: "nginx svc", Type: CheckTypeService, Path: "nginx", ExpectedValue: "active"},
		},
	}

	result, err := d.Detect(context.Background(), "web-01", baseline)
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "web-01", result.Host)
	assert.Equal(t, 0, result.DriftCount)
	assert.Equal(t, 2, result.TotalChecks)
	assert.False(t, result.HasDrift())
	assert.False(t, result.Timestamp.IsZero())
}

// --- Detect: full drift ----------------------------------------------------

func TestDetect_FullDrift(t *testing.T) {
	// Prober returns items with different actual values.
	prober := &mockStateProber{
		items: []StateItem{
			{CheckName: "nginx.conf", ActualValue: "changed", ExpectedValue: "original"},
			{CheckName: "nginx svc", ActualValue: "inactive", ExpectedValue: "active"},
		},
	}
	d := NewDetector(prober)

	baseline := &Baseline{
		ID:   "bl-1",
		Host: "web-01",
		Items: []BaselineItem{
			{CheckName: "nginx.conf", Type: CheckTypeFile, Path: "/etc/nginx/nginx.conf", ExpectedValue: "original"},
			{CheckName: "nginx svc", Type: CheckTypeService, Path: "nginx", ExpectedValue: "active"},
		},
	}

	result, err := d.Detect(context.Background(), "web-01", baseline)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDriftDetected))
	assert.NotNil(t, result)
	assert.Equal(t, 2, result.DriftCount)
	assert.Equal(t, 2, result.TotalChecks)
	assert.True(t, result.HasDrift())

	for _, item := range result.Items {
		assert.True(t, item.Drifted)
		assert.NotEmpty(t, item.Diff)
	}
}

// --- Detect: partial drift -------------------------------------------------

func TestDetect_PartialDrift(t *testing.T) {
	prober := &mockStateProber{
		items: []StateItem{
			{CheckName: "nginx.conf", ActualValue: "original", ExpectedValue: "original"},
			{CheckName: "nginx svc", ActualValue: "inactive", ExpectedValue: "active"},
		},
	}
	d := NewDetector(prober)

	baseline := &Baseline{
		ID:   "bl-1",
		Host: "web-01",
		Items: []BaselineItem{
			{CheckName: "nginx.conf", Type: CheckTypeFile, Path: "/etc/nginx/nginx.conf", ExpectedValue: "original"},
			{CheckName: "nginx svc", Type: CheckTypeService, Path: "nginx", ExpectedValue: "active"},
		},
	}

	result, err := d.Detect(context.Background(), "web-01", baseline)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDriftDetected))
	assert.Equal(t, 1, result.DriftCount)
	assert.Equal(t, 2, result.TotalChecks)

	assert.False(t, result.Items[0].Drifted)
	assert.True(t, result.Items[1].Drifted)
}

// --- Detect: prober error --------------------------------------------------

func TestDetect_ProberError(t *testing.T) {
	prober := &mockStateProber{err: errors.New("connection refused")}
	d := NewDetector(prober)

	baseline := &Baseline{
		ID:   "bl-1",
		Host: "web-01",
		Items: []BaselineItem{
			{CheckName: "nginx.conf", Type: CheckTypeFile, Path: "/etc/nginx/nginx.conf", ExpectedValue: "original"},
		},
	}

	_, err := d.Detect(context.Background(), "web-01", baseline)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrDriftDetected))
}

// --- Detect: edge cases ----------------------------------------------------

func TestDetect_EmptyHost(t *testing.T) {
	prober := &mockStateProber{}
	d := NewDetector(prober)

	baseline := &Baseline{ID: "bl-1", Host: "web-01", Items: []BaselineItem{{CheckName: "a"}}}
	_, err := d.Detect(context.Background(), "", baseline)
	assert.ErrorIs(t, err, ErrEmptyHost)
}

func TestDetect_NilBaseline(t *testing.T) {
	prober := &mockStateProber{}
	d := NewDetector(prober)

	_, err := d.Detect(context.Background(), "web-01", nil)
	assert.Error(t, err)
}

func TestDetect_NoProber(t *testing.T) {
	d := NewDetector(nil)
	d.SetProber(nil)

	baseline := &Baseline{ID: "bl-1", Host: "web-01", Items: []BaselineItem{{CheckName: "a"}}}
	_, err := d.Detect(context.Background(), "web-01", baseline)
	assert.ErrorIs(t, err, ErrNoProber)
}

// --- DetectBatch ------------------------------------------------------------

func TestDetectBatch_NoDrift(t *testing.T) {
	prober := &mockStateProber{}
	d := NewDetector(prober)

	baseline := &Baseline{
		ID:   "bl-1",
		Host: "",
		Items: []BaselineItem{
			{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"},
		},
	}

	results, err := d.DetectBatch(context.Background(), []string{"web-01", "web-02", "web-03"}, baseline)
	require.NoError(t, err)
	assert.Len(t, results, 3)
	for _, r := range results {
		assert.Equal(t, 0, r.DriftCount)
	}
}

func TestDetectBatch_WithDrift(t *testing.T) {
	prober := &mockStateProber{
		items: []StateItem{
			{CheckName: "a", ActualValue: "changed", ExpectedValue: "1"},
		},
	}
	d := NewDetector(prober)

	baseline := &Baseline{
		ID:   "bl-1",
		Host: "",
		Items: []BaselineItem{
			{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"},
		},
	}

	results, err := d.DetectBatch(context.Background(), []string{"web-01", "web-02"}, baseline)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDriftDetected))
	assert.Len(t, results, 2)
	for _, r := range results {
		assert.Equal(t, 1, r.DriftCount)
	}
}

func TestDetectBatch_EmptyHosts(t *testing.T) {
	prober := &mockStateProber{}
	d := NewDetector(prober)

	baseline := &Baseline{ID: "bl-1", Host: "", Items: []BaselineItem{{CheckName: "a"}}}
	_, err := d.DetectBatch(context.Background(), nil, baseline)
	assert.ErrorIs(t, err, ErrEmptyHosts)
}

func TestDetectBatch_NilBaseline(t *testing.T) {
	prober := &mockStateProber{}
	d := NewDetector(prober)

	_, err := d.DetectBatch(context.Background(), []string{"web-01"}, nil)
	assert.Error(t, err)
}

func TestDetectBatch_PreservesOrder(t *testing.T) {
	prober := &mockStateProber{}
	d := NewDetector(prober)

	baseline := &Baseline{
		ID:   "bl-1",
		Host: "",
		Items: []BaselineItem{
			{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"},
		},
	}

	hosts := []string{"web-03", "web-01", "web-02"}
	results, err := d.DetectBatch(context.Background(), hosts, baseline)
	require.NoError(t, err)
	assert.Len(t, results, 3)
	// Order should match input.
	assert.Equal(t, "web-03", results[0].Host)
	assert.Equal(t, "web-01", results[1].Host)
	assert.Equal(t, "web-02", results[2].Host)
}

// --- SetProber / SetNotifier -----------------------------------------------

func TestSetProber(t *testing.T) {
	d := NewDetector(nil)
	d.SetProber(&mockStateProber{})
	// After setting a prober, detection should work.
	baseline := &Baseline{ID: "bl-1", Host: "web-01", Items: []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}}
	_, err := d.Detect(context.Background(), "web-01", baseline)
	require.NoError(t, err)
}

func TestSetNotifier(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	n := &mockNotifier{}
	d.SetNotifier(n)

	baseline := &Baseline{
		ID:   "bl-1",
		Host: "web-01",
		Items: []BaselineItem{
			{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"},
		},
	}

	// Trigger drift to invoke notifier.
	d.SetProber(&mockStateProber{
		items: []StateItem{
			{CheckName: "a", ActualValue: "changed", ExpectedValue: "1"},
		},
	})
	_, err := d.Detect(context.Background(), "web-01", baseline)
	require.Error(t, err)

	// Notifier should have been called.
	assert.True(t, n.called)
}

func TestSetNotifier_Nil(t *testing.T) {
	d := NewDetector(&mockStateProber{})
	d.SetNotifier(nil)

	baseline := &Baseline{
		ID:   "bl-1",
		Host: "web-01",
		Items: []BaselineItem{
			{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"},
		},
	}

	// Detection with drift should not panic without a notifier.
	d.SetProber(&mockStateProber{
		items: []StateItem{
			{CheckName: "a", ActualValue: "changed", ExpectedValue: "1"},
		},
	})
	_, err := d.Detect(context.Background(), "web-01", baseline)
	require.Error(t, err) // ErrDriftDetected
}

// --- DriftResult.HasDrift --------------------------------------------------

func TestDriftResult_HasDrift(t *testing.T) {
	r := &DriftResult{DriftCount: 0}
	assert.False(t, r.HasDrift())

	r2 := &DriftResult{DriftCount: 1}
	assert.True(t, r2.HasDrift())

	assert.False(t, (*DriftResult)(nil).HasDrift())
}

// --- mockNotifier ----------------------------------------------------------

type mockNotifier struct {
	mu     sync.Mutex
	called bool
	msg    notify.Message
	err    error
}

func (m *mockNotifier) Name() string { return "mock" }

func (m *mockNotifier) Send(ctx context.Context, msg notify.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called = true
	m.msg = msg
	if m.err != nil {
		return m.err
	}
	return nil
}

// --- buildResult tests -----------------------------------------------------

func TestBuildResult(t *testing.T) {
	items := []StateItem{
		{CheckName: "a", ActualValue: "1", ExpectedValue: "1"},
		{CheckName: "b", ActualValue: "2", ExpectedValue: "3"},
		{CheckName: "c", ActualValue: "x", ExpectedValue: "x", Drifted: true, Diff: "pre-set"},
	}

	result := buildResult("web-01", items)
	assert.Equal(t, "web-01", result.Host)
	assert.Equal(t, 3, result.TotalChecks)
	assert.Equal(t, 2, result.DriftCount) // b drifted, c pre-set drifted
	assert.False(t, result.Items[0].Drifted)
	assert.True(t, result.Items[1].Drifted)
	assert.True(t, result.Items[2].Drifted)
	assert.Equal(t, "pre-set", result.Items[2].Diff) // preserved
}

func TestBaselineToChecks(t *testing.T) {
	b := &Baseline{
		Items: []BaselineItem{
			{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"},
			{CheckName: "b", Type: CheckTypeService, Path: "nginx", ExpectedValue: "active"},
		},
	}
	checks := baselineToChecks(b)
	assert.Len(t, checks, 2)
	assert.Equal(t, "a", checks[0].Name)
	assert.Equal(t, CheckTypeFile, checks[0].Type)
	assert.Equal(t, "/a", checks[0].Path)
	assert.Equal(t, "1", checks[0].ExpectedValue)
}
