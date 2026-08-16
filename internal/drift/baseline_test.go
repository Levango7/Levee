package drift

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- NewBaselineManager ----------------------------------------------------

func TestNewBaselineManager(t *testing.T) {
	bm := NewBaselineManager()
	assert.NotNil(t, bm)
	assert.Empty(t, bm.List())
}

// --- GenerateFromSnapshot ---------------------------------------------------

func TestGenerateFromSnapshot(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{
		{CheckName: "nginx.conf", Type: CheckTypeFile, Path: "/etc/nginx/nginx.conf", ExpectedValue: "content-hash"},
		{CheckName: "nginx service", Type: CheckTypeService, Path: "nginx", ExpectedValue: "active"},
	}

	baseline, err := bm.GenerateFromSnapshot("web-01", "run-123", items)
	require.NoError(t, err)
	assert.NotNil(t, baseline)
	assert.Equal(t, "web-01", baseline.Host)
	assert.Equal(t, "run-123", baseline.SourceRunID)
	assert.Len(t, baseline.Items, 2)
	assert.NotEmpty(t, baseline.ID)
	assert.False(t, baseline.CreatedAt.IsZero())

	// Verify the baseline is stored.
	got, err := bm.Get("web-01")
	require.NoError(t, err)
	assert.Equal(t, baseline.ID, got.ID)
}

func TestGenerateFromSnapshot_EmptyHost(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.GenerateFromSnapshot("", "run-1", []BaselineItem{{CheckName: "x"}})
	assert.ErrorIs(t, err, ErrEmptyHost)
}

func TestGenerateFromSnapshot_EmptyItems(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.GenerateFromSnapshot("web-01", "run-1", nil)
	assert.ErrorIs(t, err, ErrEmptyBaseline)
}

func TestGenerateFromSnapshot_Overwrite(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}

	first, err := bm.GenerateFromSnapshot("web-01", "run-1", items)
	require.NoError(t, err)

	second, err := bm.GenerateFromSnapshot("web-01", "run-2", items)
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID)

	got, err := bm.Get("web-01")
	require.NoError(t, err)
	assert.Equal(t, second.ID, got.ID)
}

// --- Get -------------------------------------------------------------------

func TestGet_NotFound(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.Get("unknown")
	assert.ErrorIs(t, err, ErrBaselineNotFound)
}

func TestGet_EmptyHost(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.Get("")
	assert.ErrorIs(t, err, ErrEmptyHost)
}

func TestGet_DefensiveCopy(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}
	_, err := bm.GenerateFromSnapshot("web-01", "run-1", items)
	require.NoError(t, err)

	got, err := bm.Get("web-01")
	require.NoError(t, err)
	got.Items[0].CheckName = "modified"

	// The stored baseline should be unchanged.
	got2, err := bm.Get("web-01")
	require.NoError(t, err)
	assert.Equal(t, "a", got2.Items[0].CheckName)
}

// --- Set -------------------------------------------------------------------

func TestSet(t *testing.T) {
	bm := NewBaselineManager()
	baseline := &Baseline{
		Host: "web-01",
		Items: []BaselineItem{
			{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"},
		},
	}

	err := bm.Set("web-01", baseline)
	require.NoError(t, err)

	got, err := bm.Get("web-01")
	require.NoError(t, err)
	assert.Equal(t, "web-01", got.Host)
	assert.NotEmpty(t, got.ID)
	assert.False(t, got.CreatedAt.IsZero())
}

func TestSet_EmptyHost(t *testing.T) {
	bm := NewBaselineManager()
	err := bm.Set("", &Baseline{Items: []BaselineItem{{CheckName: "a"}}})
	assert.ErrorIs(t, err, ErrEmptyHost)
}

func TestSet_NilBaseline(t *testing.T) {
	bm := NewBaselineManager()
	err := bm.Set("web-01", nil)
	assert.Error(t, err)
}

func TestSet_EmptyItems(t *testing.T) {
	bm := NewBaselineManager()
	err := bm.Set("web-01", &Baseline{Host: "web-01"})
	assert.ErrorIs(t, err, ErrEmptyBaseline)
}

func TestSet_PreservesID(t *testing.T) {
	bm := NewBaselineManager()
	baseline := &Baseline{
		ID:   "custom-id",
		Host: "web-01",
		Items: []BaselineItem{
			{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"},
		},
	}

	err := bm.Set("web-01", baseline)
	require.NoError(t, err)

	got, err := bm.Get("web-01")
	require.NoError(t, err)
	assert.Equal(t, "custom-id", got.ID)
}

// --- List ------------------------------------------------------------------

func TestList(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}

	_, err := bm.GenerateFromSnapshot("web-03", "run-1", items)
	require.NoError(t, err)
	_, err = bm.GenerateFromSnapshot("web-01", "run-1", items)
	require.NoError(t, err)
	_, err = bm.GenerateFromSnapshot("web-02", "run-1", items)
	require.NoError(t, err)

	list := bm.List()
	assert.Len(t, list, 3)
	// Should be sorted by host.
	assert.Equal(t, "web-01", list[0].Host)
	assert.Equal(t, "web-02", list[1].Host)
	assert.Equal(t, "web-03", list[2].Host)
}

func TestList_Empty(t *testing.T) {
	bm := NewBaselineManager()
	list := bm.List()
	assert.Empty(t, list)
}

// --- Delete ----------------------------------------------------------------

func TestDelete(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}
	_, err := bm.GenerateFromSnapshot("web-01", "run-1", items)
	require.NoError(t, err)

	err = bm.Delete("web-01")
	require.NoError(t, err)

	_, err = bm.Get("web-01")
	assert.ErrorIs(t, err, ErrBaselineNotFound)
}

func TestDelete_NotFound(t *testing.T) {
	bm := NewBaselineManager()
	err := bm.Delete("unknown")
	assert.ErrorIs(t, err, ErrBaselineNotFound)
}

func TestDelete_EmptyHost(t *testing.T) {
	bm := NewBaselineManager()
	err := bm.Delete("")
	assert.ErrorIs(t, err, ErrEmptyHost)
}

// --- AutoGenerate ----------------------------------------------------------

func TestAutoGenerate_NoSource(t *testing.T) {
	// Ensure no snapshot source is configured.
	SetSnapshotSource(nil)

	bm := NewBaselineManager()
	_, err := bm.AutoGenerate("web-01", "run-1")
	assert.ErrorIs(t, err, ErrNoSnapshotSource)
}

func TestAutoGenerate_WithSource(t *testing.T) {
	// Use a mock snapshot source.
	src := &mockSnapshotSource{
		items: []BaselineItem{
			{CheckName: "nginx.conf", Type: CheckTypeFile, Path: "/etc/nginx/nginx.conf", ExpectedValue: "hash"},
		},
	}
	SetSnapshotSource(src)
	defer SetSnapshotSource(nil)

	bm := NewBaselineManager()
	baseline, err := bm.AutoGenerate("web-01", "run-1")
	require.NoError(t, err)
	assert.NotNil(t, baseline)
	assert.Equal(t, "web-01", baseline.Host)
	assert.Equal(t, "run-1", baseline.SourceRunID)
	assert.Len(t, baseline.Items, 1)
}

func TestAutoGenerate_EmptyHost(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.AutoGenerate("", "run-1")
	assert.ErrorIs(t, err, ErrEmptyHost)
}

func TestAutoGenerate_EmptyRunID(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.AutoGenerate("web-01", "")
	assert.Error(t, err)
}

func TestAutoGenerate_EmptyItemsFromSource(t *testing.T) {
	src := &mockSnapshotSource{items: nil}
	SetSnapshotSource(src)
	defer SetSnapshotSource(nil)

	bm := NewBaselineManager()
	_, err := bm.AutoGenerate("web-01", "run-1")
	assert.ErrorIs(t, err, ErrEmptyBaseline)
}

// --- mockSnapshotSource -----------------------------------------------------

type mockSnapshotSource struct {
	items []BaselineItem
	err   error
}

func (m *mockSnapshotSource) ExtractItems(host string, runID string) ([]BaselineItem, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.items, nil
}

// --- Concurrent access -----------------------------------------------------

func TestBaselineManager_Concurrent(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}

	// Run concurrent operations to verify thread safety.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_, _ = bm.GenerateFromSnapshot("web-01", "run-1", items)
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		_, _ = bm.Get("web-01")
	}
	<-done

	// Should not panic or race.
	got, err := bm.Get("web-01")
	require.NoError(t, err)
	assert.NotNil(t, got)
}

// --- Error wrapping --------------------------------------------------------

func TestErrors_Wrapping(t *testing.T) {
	bm := NewBaselineManager()

	_, err := bm.Get("unknown")
	assert.True(t, errors.Is(err, ErrBaselineNotFound))

	err = bm.Delete("unknown")
	assert.True(t, errors.Is(err, ErrBaselineNotFound))
}
