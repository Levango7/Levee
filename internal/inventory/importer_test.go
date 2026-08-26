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
