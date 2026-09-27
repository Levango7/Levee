package identity

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadReadsPreMoveFile feeds the exact bytes the CLI wrote before this
// package existed. The move must not change the on-disk contract: an
// operator's existing <dataDir>/users.yaml has to keep loading, or promoting
// the registry would silently delete everyone's team assignments.
func TestLoadReadsPreMoveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	legacy := "users:\n- name: alice\n  team: sre\n  role: admin\n- name: bob\n  team: dba\n  role: viewer\n"
	require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))

	reg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, reg.Users, 2)
	assert.Equal(t, User{Name: "alice", Team: "sre", Role: "admin"}, reg.Users[0])
	assert.Equal(t, User{Name: "bob", Team: "dba", Role: "viewer"}, reg.Users[1])
}

// TestSaveLoadRoundTripAlsoMatchesLegacyReader closes the other direction:
// output written by the new package must still parse for anything reading the
// old schema (the CLI's own list command, and operators' grep habits).
func TestSaveLoadRoundTripAlsoMatchesLegacyReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	reg := &Registry{Users: []User{
		{Name: "alice", Team: "sre", Role: "admin"},
		{Name: "carol", Team: "web", Role: "operator"},
	}}
	require.NoError(t, Save(path, reg))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, reg.Users, loaded.Users)

	// Legacy-shape assertion, written against the schema rather than the
	// package, so a tag rename cannot pass by moving both sides together.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "users:")
	assert.Contains(t, string(raw), "- name: alice")
	assert.Contains(t, string(raw), "team: sre")
	assert.Contains(t, string(raw), "role: admin")
}

func TestSaveUsesOwnerOnlyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows has no POSIX mode bits: os.Stat reports 0666 for every
		// regular file regardless of what was requested, so this assertion
		// cannot fail meaningfully there — it cannot succeed either. The
		// ubuntu/macos legs of the test matrix still enforce it.
		t.Skip("file mode bits are not a Windows concept")
	}
	path := filepath.Join(t.TempDir(), FileName)
	require.NoError(t, Save(path, &Registry{Users: []User{{Name: "alice", Team: "sre", Role: "admin"}}}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	// The registry is authorisation data; world-readable would expose every
	// account and its team to anyone who can list the data directory.
	assert.EqualValues(t, 0o600, info.Mode().Perm())
}

func TestLoadMissingFileIsEmptyRegistry(t *testing.T) {
	reg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	require.NoError(t, err, "an absent registry is a normal state, not an error")
	assert.Empty(t, reg.Users)
}

func TestLoadCorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	require.NoError(t, os.WriteFile(path, []byte("users: [this is not: mapping\n"), 0o600))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal user registry")
}

func TestLookupIsExactAndCaseSensitive(t *testing.T) {
	reg := &Registry{Users: []User{{Name: "alice", Team: "sre", Role: "admin"}}}

	got, ok := reg.Lookup("alice")
	require.True(t, ok)
	assert.Equal(t, "sre", got.Team)

	// Case-folding would merge two distinct IdP accounts into one principal
	// with one team, so lookups must be exact.
	_, ok = reg.Lookup("Alice")
	assert.False(t, ok)

	_, ok = reg.Lookup("")
	assert.False(t, ok)

	// A nil registry must not panic: callers reach it on the "no data
	// configured" path.
	var nilReg *Registry
	_, ok = nilReg.Lookup("alice")
	assert.False(t, ok)
	assert.Nil(t, nilReg.Names())
}

func TestNamesPreservesFileOrder(t *testing.T) {
	reg := &Registry{Users: []User{
		{Name: "carol", Team: "web", Role: "viewer"},
		{Name: "alice", Team: "sre", Role: "admin"},
	}}
	// Order is what makes the startup reconciliation diff readable against
	// the operator's own file; sorting here would hide edits.
	assert.Equal(t, []string{"carol", "alice"}, reg.Names())
}

func TestFilePathUsesDataDirConvention(t *testing.T) {
	assert.Equal(t, filepath.Join("/var/lib/levee", FileName), FilePath("/var/lib/levee"))
}
