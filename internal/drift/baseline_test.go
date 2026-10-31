package drift

import (
	"errors"
	"strings"
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

	baseline, err := bm.GenerateFromSnapshot("web-01", DefaultBaselineName, "run-123", items)
	require.NoError(t, err)
	assert.NotNil(t, baseline)
	assert.Equal(t, "web-01", baseline.Host)
	assert.Equal(t, DefaultBaselineName, baseline.Name)
	assert.Equal(t, "run-123", baseline.SourceRunID)
	assert.Len(t, baseline.Items, 2)
	assert.NotEmpty(t, baseline.ID)
	assert.False(t, baseline.CreatedAt.IsZero())

	// Verify the baseline is stored.
	got, err := bm.Get("web-01", DefaultBaselineName)
	require.NoError(t, err)
	assert.Equal(t, baseline.ID, got.ID)
}

func TestGenerateFromSnapshot_EmptyHost(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.GenerateFromSnapshot("", DefaultBaselineName, "run-1", []BaselineItem{{CheckName: "x"}})
	assert.ErrorIs(t, err, ErrEmptyHost)
}

func TestGenerateFromSnapshot_EmptyItems(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.GenerateFromSnapshot("web-01", DefaultBaselineName, "run-1", nil)
	assert.ErrorIs(t, err, ErrEmptyBaseline)
}

func TestGenerateFromSnapshot_Overwrite(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}

	first, err := bm.GenerateFromSnapshot("web-01", DefaultBaselineName, "run-1", items)
	require.NoError(t, err)

	second, err := bm.GenerateFromSnapshot("web-01", DefaultBaselineName, "run-2", items)
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID)

	got, err := bm.Get("web-01", DefaultBaselineName)
	require.NoError(t, err)
	assert.Equal(t, second.ID, got.ID)
}

// --- named versions ---------------------------------------------------------

// TestGenerateFromSnapshot_NamedVersionsCoexist is the behaviour this re-keying
// exists for: storing a second version of a host must not take the version
// detection uses with it.
func TestGenerateFromSnapshot_NamedVersionsCoexist(t *testing.T) {
	bm := NewBaselineManager()
	current := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}
	compliance := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "hardened"}}

	stored, err := bm.GenerateFromSnapshot("web-01", "", "run-1", current)
	require.NoError(t, err)
	assert.Equal(t, DefaultBaselineName, stored.Name, "an unchosen name is the default version")

	_, err = bm.GenerateFromSnapshot("web-01", "cis-benchmark", "run-2", compliance)
	require.NoError(t, err)

	got, err := bm.Get("web-01", "")
	require.NoError(t, err)
	assert.Equal(t, "1", got.Items[0].ExpectedValue, "the default version survived the named write")

	named, err := bm.Get("web-01", "cis-benchmark")
	require.NoError(t, err)
	assert.Equal(t, "hardened", named.Items[0].ExpectedValue)

	// Two hosts × two versions all stay addressable.
	_, err = bm.GenerateFromSnapshot("web-02", "cis-benchmark", "run-3", compliance)
	require.NoError(t, err)
	assert.Len(t, bm.List(), 3)
}

// TestNormalizeBaselineName pins the accepted shape of a version label. The
// label becomes part of a file name and is compared on case-insensitive
// filesystems, so both of those hazards have to be refusals rather than
// surprises discovered after the write.
func TestNormalizeBaselineName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty means default", "", DefaultBaselineName, false},
		{"whitespace only means default", "   ", DefaultBaselineName, false},
		{"trimmed", "  prod  ", "prod", false},
		{"digits and punctuation", "9-a.b_c", "9-a.b_c", false},
		{"rejected: uppercase", "Prod", "", true},
		{"rejected: separator", "a/b", "", true},
		{"rejected: backslash", `a\b`, "", true},
		{"rejected: space inside", "prod 2026", "", true},
		{"rejected: leading punctuation", "_prod", "", true},
		{"rejected: dot name", "..", "", true},
		{"rejected: too long", strings.Repeat("a", 65), "", true},
		{"accepted: 64 chars", strings.Repeat("a", 64), strings.Repeat("a", 64), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeBaselineName(tc.in)
			if tc.wantErr {
				assert.ErrorIs(t, err, ErrInvalidBaselineName)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNormalizeBaselineHost(t *testing.T) {
	// A host is a file stem too: separators would escape the baselines
	// directory, and that directory is reconciled by deletion.
	for _, in := range []string{"a/b", `a\b`, ".", ".."} {
		_, err := normalizeBaselineHost(in)
		assert.ErrorIs(t, err, ErrInvalidHost, in)
	}
	_, err := normalizeBaselineHost("  ")
	assert.ErrorIs(t, err, ErrEmptyHost)

	got, err := normalizeBaselineHost("  web-01.example.com  ")
	require.NoError(t, err)
	assert.Equal(t, "web-01.example.com", got)
}

func TestGenerateFromSnapshot_InvalidName(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.GenerateFromSnapshot("web-01", "Prod/2026", "run-1", []BaselineItem{{CheckName: "x"}})
	assert.ErrorIs(t, err, ErrInvalidBaselineName)
	assert.Empty(t, bm.List(), "a refused write stores nothing")
}

// --- Get -------------------------------------------------------------------

func TestGet_NotFound(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.Get("unknown", DefaultBaselineName)
	assert.ErrorIs(t, err, ErrBaselineNotFound)
}

// TestGet_NotFoundNamesOtherVersions is the operator-facing half of a wrong
// --baseline-name: the error has to say what the host actually has.
func TestGet_NotFoundNamesOtherVersions(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}
	_, err := bm.GenerateFromSnapshot("web-01", "staging", "run-1", items)
	require.NoError(t, err)
	_, err = bm.GenerateFromSnapshot("web-01", "prod", "run-1", items)
	require.NoError(t, err)

	_, err = bm.Get("web-01", "prod-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBaselineNotFound)
	assert.Contains(t, err.Error(), "prod, staging", "versions are listed in a stable order")

	_, err = bm.Get("web-09", DefaultBaselineName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "this host has no stored baseline")
}

func TestGet_EmptyHost(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.Get("", DefaultBaselineName)
	assert.ErrorIs(t, err, ErrEmptyHost)
}

func TestGet_DefensiveCopy(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}
	_, err := bm.GenerateFromSnapshot("web-01", DefaultBaselineName, "run-1", items)
	require.NoError(t, err)

	got, err := bm.Get("web-01", DefaultBaselineName)
	require.NoError(t, err)
	got.Items[0].CheckName = "modified"

	// The stored baseline should be unchanged.
	got2, err := bm.Get("web-01", DefaultBaselineName)
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

	err := bm.Set(baseline)
	require.NoError(t, err)

	got, err := bm.Get("web-01", "")
	require.NoError(t, err)
	assert.Equal(t, "web-01", got.Host)
	assert.Equal(t, DefaultBaselineName, got.Name)
	assert.NotEmpty(t, got.ID)
	assert.False(t, got.CreatedAt.IsZero())

	// The caller's struct is not the stored record.
	assert.Empty(t, baseline.Name, "Set normalises the stored copy, not the argument")
}

func TestSet_EmptyHost(t *testing.T) {
	bm := NewBaselineManager()
	err := bm.Set(&Baseline{Items: []BaselineItem{{CheckName: "a"}}})
	assert.ErrorIs(t, err, ErrEmptyHost)
}

func TestSet_NilBaseline(t *testing.T) {
	bm := NewBaselineManager()
	err := bm.Set(nil)
	assert.Error(t, err)
}

func TestSet_EmptyItems(t *testing.T) {
	bm := NewBaselineManager()
	err := bm.Set(&Baseline{Host: "web-01"})
	assert.ErrorIs(t, err, ErrEmptyBaseline)
}

func TestSet_PreservesID(t *testing.T) {
	bm := NewBaselineManager()
	baseline := &Baseline{
		ID:   "custom-id",
		Host: "web-01",
		Name: "prod",
		Items: []BaselineItem{
			{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"},
		},
	}

	err := bm.Set(baseline)
	require.NoError(t, err)

	got, err := bm.Get("web-01", "prod")
	require.NoError(t, err)
	assert.Equal(t, "custom-id", got.ID)
}

// TestSet_KeyComesFromTheRecord guards the reason Set dropped its host
// argument: a key passed beside the record could name a different host than
// the record carries, and then the two would disagree about which version is
// stored where.
func TestSet_KeyComesFromTheRecord(t *testing.T) {
	bm := NewBaselineManager()
	require.NoError(t, bm.Set(&Baseline{
		Host:  "web-01",
		Name:  "prod",
		Items: []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}},
	}))

	_, err := bm.Get("web-02", "prod")
	assert.ErrorIs(t, err, ErrBaselineNotFound)

	_, err = bm.Get("web-01", DefaultBaselineName)
	assert.ErrorIs(t, err, ErrBaselineNotFound)
}

func TestSet_InvalidName(t *testing.T) {
	bm := NewBaselineManager()
	err := bm.Set(&Baseline{Host: "web-01", Name: "PROD", Items: []BaselineItem{{CheckName: "a"}}})
	assert.ErrorIs(t, err, ErrInvalidBaselineName)
}

// --- List ------------------------------------------------------------------

func TestList(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}

	_, err := bm.GenerateFromSnapshot("web-03", DefaultBaselineName, "run-1", items)
	require.NoError(t, err)
	_, err = bm.GenerateFromSnapshot("web-01", DefaultBaselineName, "run-1", items)
	require.NoError(t, err)
	_, err = bm.GenerateFromSnapshot("web-02", DefaultBaselineName, "run-1", items)
	require.NoError(t, err)

	list := bm.List()
	assert.Len(t, list, 3)
	// Should be sorted by host.
	assert.Equal(t, "web-01", list[0].Host)
	assert.Equal(t, "web-02", list[1].Host)
	assert.Equal(t, "web-03", list[2].Host)
}

// TestList_SortedByHostThenName: host alone is no longer a total order, so the
// tie-break has to be the version label — otherwise `baseline list` shuffles
// between runs and the output cannot be diffed.
func TestList_SortedByHostThenName(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}
	for _, host := range []string{"web-02", "web-01"} {
		for _, name := range []string{"staging", "cis", DefaultBaselineName} {
			_, err := bm.GenerateFromSnapshot(host, name, "run-1", items)
			require.NoError(t, err)
		}
	}

	var got []string
	for _, b := range bm.List() {
		got = append(got, b.Host+"/"+b.Name)
	}
	// Plain lexicographic within a host — "default" is a name like any other,
	// so it is not hoisted to the top by a special rule.
	assert.Equal(t, []string{
		"web-01/cis", "web-01/" + DefaultBaselineName, "web-01/staging",
		"web-02/cis", "web-02/" + DefaultBaselineName, "web-02/staging",
	}, got)
}

func TestList_DefensiveCopy(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.GenerateFromSnapshot("web-01", DefaultBaselineName, "run-1",
		[]BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}})
	require.NoError(t, err)

	list := bm.List()
	list[0].Items[0].ExpectedValue = "tampered"

	again, err := bm.Get("web-01", DefaultBaselineName)
	require.NoError(t, err)
	assert.Equal(t, "1", again.Items[0].ExpectedValue)
}

func TestList_Empty(t *testing.T) {
	bm := NewBaselineManager()
	list := bm.List()
	assert.Empty(t, list)
}

// --- ListHost --------------------------------------------------------------

func TestListHost(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}
	_, err := bm.GenerateFromSnapshot("web-01", "prod", "run-1", items)
	require.NoError(t, err)
	_, err = bm.GenerateFromSnapshot("web-01", DefaultBaselineName, "run-1", items)
	require.NoError(t, err)
	_, err = bm.GenerateFromSnapshot("web-02", "prod", "run-1", items)
	require.NoError(t, err)

	got := bm.ListHost("web-01")
	require.Len(t, got, 2)
	assert.Equal(t, DefaultBaselineName, got[0].Name)
	assert.Equal(t, "prod", got[1].Name)

	assert.Empty(t, bm.ListHost("web-09"), "a host with no versions has none, which is a fact not an error")
	assert.Nil(t, bm.ListHost(""), "an unusable host query yields nothing")
}

// --- Delete ----------------------------------------------------------------

func TestDelete(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}
	_, err := bm.GenerateFromSnapshot("web-01", DefaultBaselineName, "run-1", items)
	require.NoError(t, err)

	err = bm.Delete("web-01", DefaultBaselineName)
	require.NoError(t, err)

	_, err = bm.Get("web-01", DefaultBaselineName)
	assert.ErrorIs(t, err, ErrBaselineNotFound)
}

// TestDelete_LeavesSiblingVersionsAlone is the blast-radius half of the
// feature: one call drops one version. A typo in --baseline-name must not take
// the host's current expected state with it.
func TestDelete_LeavesSiblingVersionsAlone(t *testing.T) {
	bm := NewBaselineManager()
	items := []BaselineItem{{CheckName: "a", Type: CheckTypeFile, Path: "/a", ExpectedValue: "1"}}
	_, err := bm.GenerateFromSnapshot("web-01", DefaultBaselineName, "run-1", items)
	require.NoError(t, err)
	_, err = bm.GenerateFromSnapshot("web-01", "prod", "run-1", items)
	require.NoError(t, err)

	require.NoError(t, bm.Delete("web-01", "prod"))

	_, err = bm.Get("web-01", DefaultBaselineName)
	assert.NoError(t, err, "the default version survived")
	assert.Len(t, bm.List(), 1)

	require.NoError(t, bm.Delete("web-01", ""))
	assert.Empty(t, bm.List())
}

func TestDelete_NotFound(t *testing.T) {
	bm := NewBaselineManager()
	err := bm.Delete("unknown", DefaultBaselineName)
	assert.ErrorIs(t, err, ErrBaselineNotFound)
}

func TestDelete_EmptyHost(t *testing.T) {
	bm := NewBaselineManager()
	err := bm.Delete("", DefaultBaselineName)
	assert.ErrorIs(t, err, ErrEmptyHost)
}

// --- AutoGenerate ----------------------------------------------------------

func TestAutoGenerate_NoSource(t *testing.T) {
	// Ensure no snapshot source is configured.
	SetSnapshotSource(nil)

	bm := NewBaselineManager()
	_, err := bm.AutoGenerate("web-01", DefaultBaselineName, "run-1")
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
	baseline, err := bm.AutoGenerate("web-01", "pre-migration", "run-1")
	require.NoError(t, err)
	assert.NotNil(t, baseline)
	assert.Equal(t, "web-01", baseline.Host)
	assert.Equal(t, "pre-migration", baseline.Name)
	assert.Equal(t, "run-1", baseline.SourceRunID)
	assert.Len(t, baseline.Items, 1)
}

func TestAutoGenerate_EmptyHost(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.AutoGenerate("", DefaultBaselineName, "run-1")
	assert.ErrorIs(t, err, ErrEmptyHost)
}

func TestAutoGenerate_InvalidName(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.AutoGenerate("web-01", "Pre Migration", "run-1")
	assert.ErrorIs(t, err, ErrInvalidBaselineName)
}

func TestAutoGenerate_EmptyRunID(t *testing.T) {
	bm := NewBaselineManager()
	_, err := bm.AutoGenerate("web-01", DefaultBaselineName, "")
	assert.Error(t, err)
}

func TestAutoGenerate_EmptyItemsFromSource(t *testing.T) {
	src := &mockSnapshotSource{items: nil}
	SetSnapshotSource(src)
	defer SetSnapshotSource(nil)

	bm := NewBaselineManager()
	_, err := bm.AutoGenerate("web-01", DefaultBaselineName, "run-1")
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

	// Run concurrent operations to verify thread safety. The two writers use
	// different versions so a race cannot be masked by last-writer-wins.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_, _ = bm.GenerateFromSnapshot("web-01", "prod", "run-1", items)
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		_, _ = bm.Get("web-01", DefaultBaselineName)
		_, _ = bm.Get("web-01", "prod")
		_ = bm.ListHost("web-01")
	}
	<-done

	// Should not panic or race.
	got, err := bm.Get("web-01", "prod")
	require.NoError(t, err)
	assert.NotNil(t, got)
}

// --- Error wrapping --------------------------------------------------------

func TestErrors_Wrapping(t *testing.T) {
	bm := NewBaselineManager()

	_, err := bm.Get("unknown", DefaultBaselineName)
	assert.True(t, errors.Is(err, ErrBaselineNotFound))

	err = bm.Delete("unknown", DefaultBaselineName)
	assert.True(t, errors.Is(err, ErrBaselineNotFound))
}
