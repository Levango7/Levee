package inventory

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/capability"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingStore counts writes so a refusal can be checked to have written
// nothing, not merely to have returned an error.
type recordingStore struct {
	*memStore
	upserts int
}

func newRecordingStore() *recordingStore {
	return &recordingStore{memStore: newMemStore()}
}

func (r *recordingStore) UpsertTarget(ctx context.Context, t *state.Target) error {
	r.upserts++
	return r.memStore.UpsertTarget(ctx, t)
}

func seedRow(t *testing.T, st *memStore, host string, port int, labels map[string]string) *state.Target {
	t.Helper()
	row := &state.Target{
		ID: "tgt-" + host, Hostname: host, Port: port, ChannelType: "ssh",
		Labels: labels, Status: state.StatusActive,
	}
	require.NoError(t, st.UpsertTarget(context.Background(), row))
	return row
}

// --- import side ---------------------------------------------------------

// TestImportRejectsUnusableRegionLabel: a region value that is not a region name
// is refused at import, while the operator still has the file in front of them.
// Stored verbatim it would never match a node declaration, so the run it belongs
// to would be refused at dispatch with a message about the node set.
func TestImportRejectsUnusableRegionLabel(t *testing.T) {
	for _, bad := range []string{`""`, `"   "`, `"cn north"`, `"cn,a"`} {
		st := newRecordingStore()
		f := mustParse(t, "targets:\n  - address: 10.0.0.1\n    labels:\n      region: "+bad+"\n")
		sum, err := NewImporter(st).Import(context.Background(), f, "")
		require.NoError(t, err)
		assert.Equal(t, 1, sum.Failed, "region %s must fail the row", bad)
		assert.Zero(t, sum.Created, "a refused row is not created")
		assert.Zero(t, st.upserts, "a refused row reaches no write")
		require.Len(t, sum.Errors, 1)
		assert.Contains(t, sum.Errors[0], `label "region"`,
			"the error must name the label the operator has to fix: %s", sum.Errors[0])

		got, _ := st.FindTargetByAddress(context.Background(), "10.0.0.1", 22)
		assert.Nil(t, got)
	}
}

// TestImportCanonicalisesRegionLabel: surrounding space is trimmed rather than
// refused, and the stored value is the one a node declaration produces. The two
// sides are compared by exact equality, so both spellings must collapse to one.
func TestImportCanonicalisesRegionLabel(t *testing.T) {
	st := newRecordingStore()
	f := mustParse(t, "targets:\n  - address: 10.0.0.1\n    labels:\n      region: \"  dc-b \"\n      role: db\n")
	sum, err := NewImporter(st).Import(context.Background(), f, "")
	require.NoError(t, err)
	require.Equal(t, 1, sum.Created)

	got, err := st.FindTargetByAddress(context.Background(), "10.0.0.1", 22)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "dc-b", got.Labels[capability.RegionLabel],
		"the stored name must be the canonical one or no node can match it")
	assert.Equal(t, "db", got.Labels["role"], "other labels pass through untouched")
}

// TestImportIgnoresLabelsThatAreNotTheRegionsVocabulary is the control: the
// validator must fire on the region key only, or every deployment's own label
// values (which are free-form) would start failing the import.
func TestImportIgnoresLabelsThatAreNotTheRegionsVocabulary(t *testing.T) {
	st := newRecordingStore()
	f := mustParse(t, "targets:\n  - address: 10.0.0.1\n    labels:\n      owner: \"cn north operations\"\n")
	sum, err := NewImporter(st).Import(context.Background(), f, "")
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Created)
	assert.Zero(t, sum.Failed)
}

// --- edit side -----------------------------------------------------------

func TestSetTargetLabelsKeepsKeysTheCallerDidNotName(t *testing.T) {
	st := newRecordingStore()
	seedRow(t, st.memStore, "10.0.0.1", 22, map[string]string{"env": "prod", "role": "web"})

	row, changed, err := SetTargetLabels(context.Background(), st, "10.0.0.1", 22,
		map[string]string{capability.RegionLabel: "dc-a"}, nil)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, map[string]string{"env": "prod", "role": "web", "region": "dc-a"}, row.Labels,
		"an edit must not drop labels it was not asked about — that is what an import does")
	assert.Equal(t, 1, st.upserts)
}

func TestSetTargetLabelsRemovesKeysAndIsIdempotent(t *testing.T) {
	st := newRecordingStore()
	seedRow(t, st.memStore, "10.0.0.1", 22, map[string]string{"env": "prod", "role": "web", "tier": "1"})

	row, changed, err := SetTargetLabels(context.Background(), st, "10.0.0.1", 22, nil, []string{"role"})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, map[string]string{"env": "prod", "tier": "1"}, row.Labels)

	// Removing a key that is not there is a no-op, not an error: re-running an
	// edit script is ordinary, and a write here would report a change that did
	// not happen.
	_, changed, err = SetTargetLabels(context.Background(), st, "10.0.0.1", 22, nil, []string{"role"})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 1, st.upserts, "unchanged rows are not rewritten")

	// Setting a key to the value it already has is the same case.
	_, changed, err = SetTargetLabels(context.Background(), st, "10.0.0.1", 22,
		map[string]string{"tier": "1"}, nil)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 1, st.upserts)
}

// TestSetTargetLabelsRefusesEnvKey: env is what the policy scopes reads and
// remote execution to, and target-ownership.yaml governs it on the import path.
// A CLI that could rewrite it directly would be the bypass of that check.
func TestSetTargetLabelsRefusesEnvKey(t *testing.T) {
	st := newRecordingStore()
	seedRow(t, st.memStore, "10.0.0.1", 22, map[string]string{"env": "prod"})

	_, _, err := SetTargetLabels(context.Background(), st, "10.0.0.1", 22,
		map[string]string{"env": "dev"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), targetEnvLabelKey)
	assert.Contains(t, err.Error(), OwnershipFileName, "the refusal must name the file that owns the fact")

	_, _, err = SetTargetLabels(context.Background(), st, "10.0.0.1", 22, nil, []string{"env"})
	require.Error(t, err, "removing env is the same bypass as overwriting it")

	assert.Zero(t, st.upserts, "a refused edit reaches no write")
	row, _ := st.FindTargetByAddress(context.Background(), "10.0.0.1", 22)
	assert.Equal(t, "prod", row.Labels["env"])
}

func TestSetTargetLabelsValidatesRegion(t *testing.T) {
	st := newRecordingStore()
	seedRow(t, st.memStore, "10.0.0.1", 22, nil)

	_, _, err := SetTargetLabels(context.Background(), st, "10.0.0.1", 22,
		map[string]string{capability.RegionLabel: "cn north"}, nil)
	require.Error(t, err)
	assert.Zero(t, st.upserts)

	row, changed, err := SetTargetLabels(context.Background(), st, "10.0.0.1", 22,
		map[string]string{capability.RegionLabel: " dc-b "}, nil)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "dc-b", row.Labels[capability.RegionLabel],
		"the edited value must be canonical, like the imported one")
}

func TestSetTargetLabelsReportsAnUnknownTarget(t *testing.T) {
	st := newRecordingStore()
	_, _, err := SetTargetLabels(context.Background(), st, "10.9.9.9", 22,
		map[string]string{"role": "web"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "10.9.9.9")
	assert.Zero(t, st.upserts)
}

// --- address and rendering ------------------------------------------------

// TestParseAddressSharesTheImportPrecedence: a shell edit must name the row the
// importer would have written. Same grammar, same override, same default — one
// function, because two interpretations of "host" is how an edit lands on the
// wrong row.
func TestParseAddressSharesTheImportPrecedence(t *testing.T) {
	cases := []struct {
		addr     string
		port     int
		wantHost string
		wantPort int
	}{
		{"10.0.0.1", 0, "10.0.0.1", 22},
		{"10.0.0.1:2222", 0, "10.0.0.1", 2222},
		{"10.0.0.1:2222", 5985, "10.0.0.1", 5985}, // an explicit port overrides the address
	}
	for _, tc := range cases {
		host, port, err := ParseAddress(tc.addr, tc.port)
		require.NoError(t, err, tc.addr)
		assert.Equal(t, tc.wantHost, host, tc.addr)
		assert.Equal(t, tc.wantPort, port, tc.addr)
	}
	_, _, err := ParseAddress("", 0)
	assert.Error(t, err, "an empty address names no row")
}

func TestFormatLabelsIsSortedAndEmptyWhenThereAreNone(t *testing.T) {
	assert.Equal(t, "", FormatLabels(nil))
	assert.Equal(t, "env=prod,region=dc-a", FormatLabels(map[string]string{
		capability.RegionLabel: "dc-a", "env": "prod",
	}))
}
