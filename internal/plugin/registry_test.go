package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestRegistry returns an in-memory registry suitable for tests. It
// is closed automatically by the test cleanup.
func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	ctx := context.Background()
	r, err := NewRegistry(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// sampleMeta returns a PluginMeta suitable for tests.
func sampleMeta() PluginMeta {
	return PluginMeta{
		Name:        "http-probe",
		Version:     "1.0.0",
		Type:        TypeGate,
		Author:      "levee-team",
		Description: "HTTP probe gate",
		EntryPoint:  "plugin",
	}
}

// writeTempBinary writes a small file to a temp dir and returns its path.
// It is used to simulate a plugin binary for signature verification.
func writeTempBinary(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "plugin")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o755))
	return p
}

func TestRegistryInstallAndGet(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	meta := sampleMeta()
	bin := writeTempBinary(t, "binary")

	rec, err := r.Install(ctx, meta, bin, "", false)
	require.NoError(t, err)
	assert.Equal(t, "http-probe", rec.Name)
	assert.Equal(t, "1.0.0", rec.Version)
	assert.Equal(t, TypeGate, rec.Type)
	assert.Equal(t, StateInstalled, rec.State)
	assert.Equal(t, bin, rec.BinaryPath)
	assert.False(t, rec.InstalledAt.IsZero())

	got, err := r.Get(ctx, "http-probe")
	require.NoError(t, err)
	assert.Equal(t, rec, got)
}

func TestRegistryInstallDuplicate(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	meta := sampleMeta()
	bin := writeTempBinary(t, "binary")

	_, err := r.Install(ctx, meta, bin, "", false)
	require.NoError(t, err)

	_, err = r.Install(ctx, meta, bin, "", false)
	assert.ErrorIs(t, err, ErrPluginExists)
}

func TestRegistryInstallInvalidType(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	meta := sampleMeta()
	meta.Type = PluginType("bogus")

	_, err := r.Install(ctx, meta, "", "", false)
	assert.ErrorIs(t, err, ErrInvalidPluginType)
}

func TestRegistryGetNotFound(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	_, err := r.Get(ctx, "nope")
	assert.ErrorIs(t, err, ErrPluginNotFound)
}

func TestRegistryList(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	metas := []PluginMeta{
		{Name: "alpha", Version: "1.0.0", Type: TypeGate},
		{Name: "beta", Version: "2.0.0", Type: TypeChannel},
		{Name: "gamma", Version: "3.0.0", Type: TypeNotifier},
	}
	for _, m := range metas {
		_, err := r.Install(ctx, m, "", "", false)
		require.NoError(t, err)
	}

	all, err := r.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, all, 3)
	// Sorted by name.
	assert.Equal(t, "alpha", all[0].Name)
	assert.Equal(t, "beta", all[1].Name)
	assert.Equal(t, "gamma", all[2].Name)
}

func TestRegistryListByState(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	_, err := r.Install(ctx, PluginMeta{Name: "a", Version: "1", Type: TypeGate}, "", "", false)
	require.NoError(t, err)
	_, err = r.Install(ctx, PluginMeta{Name: "b", Version: "1", Type: TypeGate}, "", "", false)
	require.NoError(t, err)
	require.NoError(t, r.SetState(ctx, "b", StateEnabled, ""))

	installed, err := r.List(ctx, StateInstalled)
	require.NoError(t, err)
	require.Len(t, installed, 1)
	assert.Equal(t, "a", installed[0].Name)

	enabled, err := r.List(ctx, StateEnabled)
	require.NoError(t, err)
	require.Len(t, enabled, 1)
	assert.Equal(t, "b", enabled[0].Name)
}

func TestRegistrySetState(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	_, err := r.Install(ctx, sampleMeta(), "", "", false)
	require.NoError(t, err)

	require.NoError(t, r.SetState(ctx, "http-probe", StateEnabled, ""))
	rec, err := r.Get(ctx, "http-probe")
	require.NoError(t, err)
	assert.Equal(t, StateEnabled, rec.State)

	require.NoError(t, r.SetState(ctx, "http-probe", StateError, "boom"))
	rec, err = r.Get(ctx, "http-probe")
	require.NoError(t, err)
	assert.Equal(t, StateError, rec.State)
	assert.Equal(t, "boom", rec.ErrorMsg)
}

func TestRegistrySetStateNotFound(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	err := r.SetState(ctx, "nope", StateEnabled, "")
	assert.ErrorIs(t, err, ErrPluginNotFound)
}

func TestRegistryUpdateConfig(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	_, err := r.Install(ctx, sampleMeta(), "", "", false)
	require.NoError(t, err)

	require.NoError(t, r.UpdateConfig(ctx, "http-probe", "timeout: 10s"))
	rec, err := r.Get(ctx, "http-probe")
	require.NoError(t, err)
	assert.Equal(t, "timeout: 10s", rec.ConfigYAML)
}

func TestRegistryRemove(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	_, err := r.Install(ctx, sampleMeta(), "", "", false)
	require.NoError(t, err)

	require.NoError(t, r.Remove(ctx, "http-probe"))
	_, err = r.Get(ctx, "http-probe")
	assert.ErrorIs(t, err, ErrPluginNotFound)
}

func TestRegistryRemoveNotFound(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	err := r.Remove(ctx, "nope")
	assert.ErrorIs(t, err, ErrPluginNotFound)
}

func TestRegistrySignatureVerification(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	meta := sampleMeta()
	bin := writeTempBinary(t, "binary-content")

	rec, err := r.Install(ctx, meta, bin, "", true)
	require.NoError(t, err)
	assert.NotEmpty(t, rec.Signature, "signature should be recorded")

	// Verify should pass.
	require.NoError(t, r.VerifySignature(ctx, "http-probe"))

	// Tamper with the binary.
	require.NoError(t, os.WriteFile(bin, []byte("tampered"), 0o755))
	err = r.VerifySignature(ctx, "http-probe")
	assert.ErrorIs(t, err, ErrSignatureMismatch)
}

func TestRegistrySignatureSkippedWhenEmpty(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	_, err := r.Install(ctx, sampleMeta(), "", "", false)
	require.NoError(t, err)

	// No signature recorded → verification is a no-op.
	require.NoError(t, r.VerifySignature(ctx, "http-probe"))
}

func TestRegistryIsCompatible(t *testing.T) {
	r := newTestRegistry(t)


	cases := []struct {
		name   string
		min    string
		max    string
		host   string
		expect bool
	}{
		{"no bounds", "", "", "1.0.0", true},
		{"within range", "1.0.0", "2.0.0", "1.5.0", true},
		{"at min", "1.0.0", "2.0.0", "1.0.0", true},
		{"at max", "1.0.0", "2.0.0", "2.0.0", true},
		{"below min", "1.0.0", "2.0.0", "0.9.0", false},
		{"above max", "1.0.0", "2.0.0", "2.1.0", false},
		{"only min", "1.0.0", "", "0.9.0", false},
		{"only max", "", "2.0.0", "2.1.0", false},
		{"unparseable host", "1.0.0", "2.0.0", "garbage", true}, // fail open
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &RegistryRecord{
				Name:           "test",
				MinHostVersion: c.min,
				MaxHostVersion: c.max,
			}
			assert.Equal(t, c.expect, r.IsCompatible(rec, c.host))
		})
	}
}

func TestParseSemver(t *testing.T) {
	cases := []struct {
		input string
		ok    bool
		maj   int
		min   int
		pat   int
		pre   string
	}{
		{"1.2.3", true, 1, 2, 3, ""},
		{"0.0.0", true, 0, 0, 0, ""},
		{"1.2.3-rc1", true, 1, 2, 3, "rc1"},
		{"1.2", false, 0, 0, 0, ""},
		{"1.2.3.4", false, 0, 0, 0, ""},
		{"", false, 0, 0, 0, ""},
		{"a.b.c", false, 0, 0, 0, ""},
	}
	for _, c := range cases {
		v, ok := parseSemver(c.input)
		assert.Equal(t, c.ok, ok, "input=%q", c.input)
		if ok {
			assert.Equal(t, c.maj, v.major, "input=%q", c.input)
			assert.Equal(t, c.min, v.minor, "input=%q", c.input)
			assert.Equal(t, c.pat, v.patch, "input=%q", c.input)
			assert.Equal(t, c.pre, v.pre, "input=%q", c.input)
		}
	}
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "2.0.0", -1},
		{"2.0.0", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.1.0", -1},
		{"1.1.0", "1.0.0", 1},
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc1", "1.0.0-rc2", -1},
	}
	for _, c := range cases {
		a, _ := parseSemver(c.a)
		b, _ := parseSemver(c.b)
		assert.Equal(t, c.want, compareSemver(a, b), "a=%q b=%q", c.a, c.b)
	}
}

func TestRegistryInstallWithConfig(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	meta := sampleMeta()

	rec, err := r.Install(ctx, meta, "", "timeout: 10s\nretry: 3", false)
	require.NoError(t, err)
	assert.Equal(t, "timeout: 10s\nretry: 3", rec.ConfigYAML)
}

func TestRegistryInstallUpdatesTimestamps(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	meta := sampleMeta()

	rec, err := r.Install(ctx, meta, "", "", false)
	require.NoError(t, err)
	assert.False(t, rec.InstalledAt.IsZero())
	assert.False(t, rec.UpdatedAt.IsZero())

	// SetState should bump updated_at.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, r.SetState(ctx, meta.Name, StateEnabled, ""))
	rec2, err := r.Get(ctx, meta.Name)
	require.NoError(t, err)
	assert.True(t, rec2.UpdatedAt.After(rec.UpdatedAt) || rec2.UpdatedAt.Equal(rec.UpdatedAt))
}