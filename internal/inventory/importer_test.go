package inventory

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleYAML = `groups:
  - name: prod
  - name: prod/db
    parent: prod
targets:
  - address: 10.0.0.1:22
    labels:
      env: prod
      app: pay
    group: prod/db
    credential_ref: cred-nginx
  - address: 10.0.0.2
    channel_type: winrm
    port: 5985
`

func mustParse(t *testing.T, data string) *File {
	t.Helper()
	f, err := ParseYAML([]byte(data))
	require.NoError(t, err)
	return f
}

type memStore struct {
	groups  map[string]*state.InventoryGroup
	targets map[string]*state.Target // by address key "host:port"
}

func newMemStore() *memStore {
	return &memStore{groups: map[string]*state.InventoryGroup{}, targets: map[string]*state.Target{}}
}

func (m *memStore) ListInventoryGroups(_ context.Context) ([]*state.InventoryGroup, error) {
	out := make([]*state.InventoryGroup, 0, len(m.groups))
	for _, g := range m.groups {
		out = append(out, g)
	}
	return out, nil
}

func (m *memStore) UpsertInventoryGroup(_ context.Context, g *state.InventoryGroup) error {
	for _, e := range m.groups {
		if e.Name == g.Name && e.ID != g.ID {
			return state.ErrDuplicateTarget // stand-in for unique violation
		}
	}
	m.groups[g.ID] = g
	return nil
}

func (m *memStore) FindTargetByAddress(_ context.Context, host string, port int) (*state.Target, error) {
	return m.targets[hostKey(host, port)], nil
}

func (m *memStore) UpsertTarget(_ context.Context, t *state.Target) error {
	m.targets[hostKey(t.Hostname, t.Port)] = t
	return nil
}

func hostKey(h string, p int) string { return h + ":" + itoa(p) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

func TestParseYAMLRejectsUnknownFields(t *testing.T) {
	_, err := ParseYAML([]byte("targets:\n  - host: x\n"))
	require.Error(t, err, "typo'd field name must be rejected")
}

func TestImportCreatesGroupsAndTargets(t *testing.T) {
	st := newMemStore()
	sum, err := NewImporter(st).Import(context.Background(), mustParse(t, sampleYAML), "")
	require.NoError(t, err)
	assert.Equal(t, 2, sum.Created)
	assert.Equal(t, 0, sum.Failed)
	assert.Empty(t, sum.Errors)

	byAddr, _ := st.FindTargetByAddress(context.Background(), "10.0.0.1", 22)
	require.NotNil(t, byAddr)
	assert.Equal(t, "cred-nginx", byAddr.CredentialRef)
	assert.Equal(t, "prod/db", st.groups[byAddr.GroupID].Name)

	win, _ := st.FindTargetByAddress(context.Background(), "10.0.0.2", 5985)
	require.NotNil(t, win)
	assert.Equal(t, "winrm", win.ChannelType)
}

func TestImportIsIdempotent(t *testing.T) {
	st := newMemStore()
	im := NewImporter(st)
	f := mustParse(t, sampleYAML)

	first, err := im.Import(context.Background(), f, "")
	require.NoError(t, err)
	assert.Equal(t, 2, first.Created)

	second, err := im.Import(context.Background(), f, "")
	require.NoError(t, err)
	assert.Equal(t, 2, second.Updated, "re-import updates instead of duplicating")
	assert.Equal(t, 0, second.Created)
	assert.Len(t, st.targets, 2)
}

func TestImportPartialFailureAccumulates(t *testing.T) {
	bad := `
targets:
  - address: 10.1.1.1
  - address: ""
  - address: 10.1.1.3:99999
  - address: 10.1.1.4
    channel_type: telnet
`
	st := newMemStore()
	sum, err := NewImporter(st).Import(context.Background(), mustParse(t, bad), "")
	require.NoError(t, err, "row-level failures never abort the import")
	assert.Equal(t, 1, sum.Created)
	assert.Equal(t, 3, sum.Failed)
	assert.Len(t, sum.Errors, 3)
}

func TestImportDefaultGroupAndStatusPreserved(t *testing.T) {
	yml := "targets:\n  - address: 10.9.9.9\n"
	st := newMemStore()

	sum, err := NewImporter(st).Import(context.Background(), mustParse(t, yml), "edge")
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Created)
	got, _ := st.FindTargetByAddress(context.Background(), "10.9.9.9", 22)
	assert.Equal(t, "edge", st.groups[got.GroupID].Name)

	// Freeze out-of-band, then re-import without status field: the frozen
	// status must survive (imports never silently un-freeze).
	require.NoError(t, st.UpsertTarget(context.Background(),
		&state.Target{ID: got.ID, Hostname: got.Hostname, Port: got.Port, Status: state.StatusFrozen}))
	_, err = NewImporter(st).Import(context.Background(), mustParse(t, yml), "")
	require.NoError(t, err)
	got2, _ := st.FindTargetByAddress(context.Background(), "10.9.9.9", 22)
	assert.Equal(t, state.StatusFrozen, got2.Status)
}

// --- LabelChanges: the preview ImportTargets refuses against -----------------
//
// The environment label is what the policy scopes reads and remote execution
// to, so a re-import that moves a host between environments is a scope change,
// not a cosmetic edit. These cases pin what the preview reports — and, just as
// importantly, what it stays quiet about, because a preview that reported every
// re-import would make the caller judge rows it has nothing to decide about.

func TestLabelChangesReportsAnEnvironmentMove(t *testing.T) {
	st := newMemStore()
	st.targets[hostKey("10.0.0.1", 22)] = &state.Target{
		ID: "tgt-1", Hostname: "10.0.0.1", Port: 22, Status: state.StatusActive,
		Labels: map[string]string{"env": "prod"},
	}
	f := mustParse(t, `
targets:
  - address: 10.0.0.1
    labels:
      env: dev
`)

	changes, err := NewImporter(st).LabelChanges(context.Background(), f)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "prod", changes[0].Stored["env"])
	assert.Equal(t, "dev", changes[0].Resulting["env"])
	assert.Equal(t, "10.0.0.1", changes[0].Host)
	assert.Equal(t, 22, changes[0].Port)
	assert.Equal(t, "targets[0]", changes[0].Row)
}

func TestLabelChangesIgnoresPureRedeclaration(t *testing.T) {
	st := newMemStore()
	st.targets[hostKey("10.0.0.1", 22)] = &state.Target{
		ID: "tgt-1", Hostname: "10.0.0.1", Port: 22, Status: state.StatusActive,
		Labels: map[string]string{"env": "prod"},
	}
	// No labels: the importer keeps the stored ones, so nothing is taken away.
	f := mustParse(t, "targets:\n  - address: 10.0.0.1\n")

	changes, err := NewImporter(st).LabelChanges(context.Background(), f)
	require.NoError(t, err)
	assert.Empty(t, changes, "a silent re-declaration does not change the scope")
}

func TestLabelChangesIgnoresNewHosts(t *testing.T) {
	st := newMemStore()
	f := mustParse(t, "targets:\n  - address: 10.0.0.9\n    labels:\n      env: dev\n")

	changes, err := NewImporter(st).LabelChanges(context.Background(), f)
	require.NoError(t, err)
	assert.Empty(t, changes, "a new row has no stored scope to take away")
}

func TestLabelChangesReportsAClearedEnvironment(t *testing.T) {
	st := newMemStore()
	st.targets[hostKey("10.0.0.1", 22)] = &state.Target{
		ID: "tgt-1", Hostname: "10.0.0.1", Port: 22, Status: state.StatusActive,
		Labels: map[string]string{"env": "prod"},
	}
	// A silent declaration that ALSO changes the status is a redefinition, not a
	// re-address: the merge drops the stored labels, which clears the env.
	f := mustParse(t, "targets:\n  - address: 10.0.0.1\n    status: frozen\n")

	changes, err := NewImporter(st).LabelChanges(context.Background(), f)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "prod", changes[0].Stored["env"])
	assert.Empty(t, changes[0].Resulting["env"], "a cleared env is a change too")
}

func TestLabelChangesIgnoresIdenticalLabels(t *testing.T) {
	st := newMemStore()
	st.targets[hostKey("10.0.0.1", 22)] = &state.Target{
		ID: "tgt-1", Hostname: "10.0.0.1", Port: 22, Status: state.StatusActive,
		Labels: map[string]string{"env": "prod", "app": "pay"},
	}
	f := mustParse(t, `
targets:
  - address: 10.0.0.1
    labels:
      env: prod
      app: pay
`)

	changes, err := NewImporter(st).LabelChanges(context.Background(), f)
	require.NoError(t, err)
	assert.Empty(t, changes, "restating the same labels changes nothing")
}

// The preview must agree with what the write actually does: if these two ever
// disagree, the caller is judging a transition that does not happen (or missing
// one that does).
func TestLabelChangesAgreesWithImport(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		moved bool
	}{
		{"moved", "targets:\n  - address: 10.0.0.1\n    labels:\n      env: dev\n", true},
		{"silent", "targets:\n  - address: 10.0.0.1\n", false},
		{"restated", "targets:\n  - address: 10.0.0.1\n    labels:\n      env: prod\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newMemStore()
			st.targets[hostKey("10.0.0.1", 22)] = &state.Target{
				ID: "tgt-1", Hostname: "10.0.0.1", Port: 22, Status: state.StatusActive,
				Labels: map[string]string{"env": "prod"},
			}
			im := NewImporter(st)
			changes, err := im.LabelChanges(context.Background(), mustParse(t, tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.moved, len(changes) > 0, "preview says moved=%v", len(changes) > 0)

			_, err = im.Import(context.Background(), mustParse(t, tc.body), "")
			require.NoError(t, err)
			got := st.targets[hostKey("10.0.0.1", 22)].Labels["env"]
			assert.Equal(t, tc.moved, got == "dev", "the write landed env=%q", got)
		})
	}
}
