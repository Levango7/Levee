package push

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFileDeviceStore_RoundTrip is the basic durability guarantee: what Save
// writes, Load reads back.
func TestFileDeviceStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push_devices.json")
	store := NewFileDeviceStore(path)

	devices := map[string][]DeviceToken{
		"alice": {
			{Token: "tok-a", Platform: PlatformIOS, UserID: "alice"},
			{Token: "tok-b", Platform: PlatformAndroid, UserID: "alice"},
		},
		"bob": {{Token: "tok-c", Platform: PlatformIOS, UserID: "bob"}},
	}
	require.NoError(t, store.Save(devices))

	got, err := store.Load()
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "tok-a", got["alice"][0].Token)
	assert.Equal(t, PlatformAndroid, got["alice"][1].Platform)
	assert.Equal(t, "tok-c", got["bob"][0].Token)
}

// A registry that was never written is an empty registry, not an error —
// the first `levee push register` on a fresh deployment must work.
func TestFileDeviceStore_LoadMissingFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "push_devices.json")
	store := NewFileDeviceStore(path)

	got, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, got)
}

// A corrupt file must be reported, not silently read as "no devices". Reading
// garbage as an empty registry would look exactly like the bug the store
// exists to fix: every registered device would vanish.
func TestFileDeviceStore_LoadCorruptIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push_devices.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	store := NewFileDeviceStore(path)
	_, err := store.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse device registry")
}

// An empty file is treated as an empty registry: it is what a crashed write
// used to leave behind, and it must not brick the push commands.
func TestFileDeviceStore_LoadEmptyFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push_devices.json")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	store := NewFileDeviceStore(path)
	got, err := store.Load()
	require.NoError(t, err)
	assert.Empty(t, got)
}

// The write is atomic and private: no half-written file is ever visible, no
// temp file is left behind, and the registry (which holds device tokens) is
// not world-readable.
func TestFileDeviceStore_SaveIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "push_devices.json")
	store := NewFileDeviceStore(path)

	require.NoError(t, store.Save(map[string][]DeviceToken{
		"alice": {{Token: "tok-a", Platform: PlatformIOS, UserID: "alice"}},
	}))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the temp file used for the atomic write must be renamed, not left behind")
	assert.Equal(t, "push_devices.json", entries[0].Name())

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
			"the registry holds device tokens and must not be group/world readable")
	}
}

// A user with an empty device list is not a registration; it is dropped so
// the file and the in-memory map have one shape.
func TestFileDeviceStore_SaveDropsEmptyUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push_devices.json")
	store := NewFileDeviceStore(path)

	require.NoError(t, store.Save(map[string][]DeviceToken{
		"alice": {{Token: "tok-a", Platform: PlatformIOS, UserID: "alice"}},
		"ghost": {},
	}))

	got, err := store.Load()
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.NotContains(t, got, "ghost")
}

// Saving replaces the whole registry: a user removed in the new snapshot
// must not linger from the previous one.
func TestFileDeviceStore_SaveReplacesPrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push_devices.json")
	store := NewFileDeviceStore(path)

	require.NoError(t, store.Save(map[string][]DeviceToken{
		"alice": {{Token: "tok-a", Platform: PlatformIOS, UserID: "alice"}},
		"bob":   {{Token: "tok-b", Platform: PlatformIOS, UserID: "bob"}},
	}))
	require.NoError(t, store.Save(map[string][]DeviceToken{
		"alice": {{Token: "tok-a", Platform: PlatformIOS, UserID: "alice"}},
	}))

	got, err := store.Load()
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.NotContains(t, got, "bob")
}
