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

// setupManager returns a PluginManager backed by an in-memory registry,
// suitable for tests. The registry is closed automatically.
func setupManager(t *testing.T) *PluginManager {
	t.Helper()
	r := newTestRegistry(t)
	cfg := DefaultManagerConfig()
	cfg.HostVersion = "1.0.0"
	cfg.Sandbox.MaxRestarts = -1 // never restart in tests
	cfg.StopGrace = 2 * time.Second
	return NewPluginManager(r, cfg)
}

// writePluginDir creates a plugin directory with a manifest (plugin.yaml)
// and a binary. It returns the directory path.
func writePluginDir(t *testing.T, meta PluginMeta, configYAML string) string {
	t.Helper()
	dir := t.TempDir()

	// Write the manifest.
	manifest := "name: " + meta.Name + "\n"
	manifest += "version: " + meta.Version + "\n"
	manifest += "type: " + string(meta.Type) + "\n"
	manifest += "author: " + meta.Author + "\n"
	manifest += "description: " + meta.Description + "\n"
	if meta.EntryPoint != "" {
		manifest += "entry_point: " + meta.EntryPoint + "\n"
	}
	if meta.MinHostVersion != "" {
		manifest += "min_host_version: " + meta.MinHostVersion + "\n"
	}
	if meta.MaxHostVersion != "" {
		manifest += "max_host_version: " + meta.MaxHostVersion + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644))

	// Write the binary.
	entry := meta.EntryPoint
	if entry == "" {
		entry = "plugin"
	}
	binPath := filepath.Join(dir, entry)
	if testing.Short() {
		// In short mode, write a script that exits 0 immediately.
		require.NoError(t, os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	} else {
		// In full mode, write a script that sleeps so the sandbox stays up.
		require.NoError(t, os.WriteFile(binPath, []byte("#!/bin/sh\nsleep 30\n"), 0o755))
	}

	// Write the config.
	if configYAML != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o644))
	}

	return dir
}

func TestManagerInstall(t *testing.T) {
	m := setupManager(t)
	ctx := context.Background()
	dir := writePluginDir(t, sampleMeta(), "timeout: 10s")

	rec, err := m.Install(ctx, dir)
	require.NoError(t, err)
	assert.Equal(t, "http-probe", rec.Name)
	assert.Equal(t, StateInstalled, rec.State)
	assert.Equal(t, "timeout: 10s", rec.ConfigYAML)
}

func TestManagerInstallDuplicate(t *testing.T) {
	m := setupManager(t)
	ctx := context.Background()
	dir := writePluginDir(t, sampleMeta(), "")

	_, err := m.Install(ctx, dir)
	require.NoError(t, err)

	_, err = m.Install(ctx, dir)
	assert.ErrorIs(t, err, ErrPluginExists)
}

func TestManagerLoad(t *testing.T) {
	m := setupManager(t)
	ctx := context.Background()
	dir := writePluginDir(t, sampleMeta(), "")

	require.NoError(t, m.Load(ctx, dir))

	metas, err := m.ListPlugins(ctx)
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, "http-probe", metas[0].Name)
}

func TestManagerListPlugins(t *testing.T) {
	m := setupManager(t)
	ctx := context.Background()

	plugins := []PluginMeta{
		{Name: "alpha", Version: "1.0.0", Type: TypeGate},
		{Name: "beta", Version: "2.0.0", Type: TypeChannel},
	}
	for _, p := range plugins {
		dir := writePluginDir(t, p, "")
		_, err := m.Install(ctx, dir)
		require.NoError(t, err)
	}

	metas, err := m.ListPlugins(ctx)
	require.NoError(t, err)
	require.Len(t, metas, 2)
}

func TestManagerEnableDisable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	m := setupManager(t)
	ctx := context.Background()
	dir := writePluginDir(t, sampleMeta(), "timeout: 10s")

	_, err := m.Install(ctx, dir)
	require.NoError(t, err)

	// Enable.
	require.NoError(t, m.Enable(ctx, "http-probe"))
	rec, err := m.Info(ctx, "http-probe")
	require.NoError(t, err)
	assert.Equal(t, StateEnabled, rec.State)

	// GetPlugin should work.
	meta, err := m.GetPlugin("http-probe")
	require.NoError(t, err)
	assert.Equal(t, "http-probe", meta.Name)

	// Disable.
	require.NoError(t, m.Disable(ctx, "http-probe"))
	rec, err = m.Info(ctx, "http-probe")
	require.NoError(t, err)
	assert.Equal(t, StateDisabled, rec.State)

	// GetPlugin should fail after disable.
	_, err = m.GetPlugin("http-probe")
	assert.ErrorIs(t, err, ErrPluginNotEnabled)
}

func TestManagerEnableAlreadyEnabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	m := setupManager(t)
	ctx := context.Background()
	dir := writePluginDir(t, sampleMeta(), "")

	_, err := m.Install(ctx, dir)
	require.NoError(t, err)

	require.NoError(t, m.Enable(ctx, "http-probe"))
	err = m.Enable(ctx, "http-probe")
	assert.ErrorIs(t, err, ErrPluginAlreadyEnabled)

	_ = m.Disable(ctx, "http-probe")
}

func TestManagerDisableAlreadyDisabled(t *testing.T) {
	m := setupManager(t)
	ctx := context.Background()
	dir := writePluginDir(t, sampleMeta(), "")

	_, err := m.Install(ctx, dir)
	require.NoError(t, err)

	err = m.Disable(ctx, "http-probe")
	assert.ErrorIs(t, err, ErrPluginAlreadyDisabled)
}

func TestManagerEnableNotFound(t *testing.T) {
	m := setupManager(t)
	ctx := context.Background()
	err := m.Enable(ctx, "nope")
	assert.ErrorIs(t, err, ErrPluginNotFound)
}

func TestManagerRemove(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	m := setupManager(t)
	ctx := context.Background()
	dir := writePluginDir(t, sampleMeta(), "")

	_, err := m.Install(ctx, dir)
	require.NoError(t, err)

	require.NoError(t, m.Enable(ctx, "http-probe"))
	require.NoError(t, m.Remove(ctx, "http-probe"))

	_, err = m.Info(ctx, "http-probe")
	assert.ErrorIs(t, err, ErrPluginNotFound)
}

func TestManagerRemoveNotEnabled(t *testing.T) {
	m := setupManager(t)
	ctx := context.Background()
	dir := writePluginDir(t, sampleMeta(), "")

	_, err := m.Install(ctx, dir)
	require.NoError(t, err)

	require.NoError(t, m.Remove(ctx, "http-probe"))
}

func TestManagerInfo(t *testing.T) {
	m := setupManager(t)
	ctx := context.Background()
	meta := sampleMeta()
	meta.Description = "A test plugin"
	dir := writePluginDir(t, meta, "")

	_, err := m.Install(ctx, dir)
	require.NoError(t, err)

	rec, err := m.Info(ctx, "http-probe")
	require.NoError(t, err)
	assert.Equal(t, "A test plugin", rec.Description)
	assert.Equal(t, "1.0.0", rec.Version)
	assert.Equal(t, TypeGate, rec.Type)
}

func TestManagerInfoNotFound(t *testing.T) {
	m := setupManager(t)
	ctx := context.Background()
	_, err := m.Info(ctx, "nope")
	assert.ErrorIs(t, err, ErrPluginNotFound)
}

func TestManagerListRecords(t *testing.T) {
	m := setupManager(t)
	ctx := context.Background()

	for _, p := range []PluginMeta{
		{Name: "a", Version: "1", Type: TypeGate},
		{Name: "b", Version: "1", Type: TypeChannel},
	} {
		dir := writePluginDir(t, p, "")
		_, err := m.Install(ctx, dir)
		require.NoError(t, err)
	}

	records, err := m.ListRecords(ctx)
	require.NoError(t, err)
	require.Len(t, records, 2)
}

func TestManagerGetConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	m := setupManager(t)
	ctx := context.Background()
	dir := writePluginDir(t, sampleMeta(), "timeout: 10s\nretry: 3")

	_, err := m.Install(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, m.Enable(ctx, "http-probe"))

	cfg, err := m.GetConfig("http-probe")
	require.NoError(t, err)
	assert.Equal(t, "10s", cfg["timeout"])
	assert.Equal(t, 3, cfg["retry"])
}

func TestManagerGetSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	m := setupManager(t)
	ctx := context.Background()
	dir := writePluginDir(t, sampleMeta(), "")

	_, err := m.Install(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, m.Enable(ctx, "http-probe"))

	sb, err := m.GetSandbox("http-probe")
	require.NoError(t, err)
	assert.True(t, sb.IsRunning())
}

func TestManagerVersionCompatibility(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	m := setupManager(t)
	ctx := context.Background()

	// Plugin that requires host >= 2.0.0 but host is 1.0.0.
	meta := sampleMeta()
	meta.Name = "needs-v2"
	meta.MinHostVersion = "2.0.0"
	dir := writePluginDir(t, meta, "")

	_, err := m.Install(ctx, dir)
	require.NoError(t, err)

	err = m.Enable(ctx, "needs-v2")
	assert.ErrorIs(t, err, ErrIncompatibleVersion)

	rec, err := m.Info(ctx, "needs-v2")
	require.NoError(t, err)
	assert.Equal(t, StateError, rec.State)
}

func TestManagerClose(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sub-process test in short mode")
	}
	m := setupManager(t)
	ctx := context.Background()
	dir := writePluginDir(t, sampleMeta(), "")

	_, err := m.Install(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, m.Enable(ctx, "http-probe"))

	require.NoError(t, m.Close(ctx))

	// All plugins should be disabled.
	rec, err := m.Info(ctx, "http-probe")
	require.NoError(t, err)
	assert.Equal(t, StateDisabled, rec.State)
}

func TestManagerCloseIdempotent(t *testing.T) {
	m := setupManager(t)
	ctx := context.Background()
	require.NoError(t, m.Close(ctx))
	require.NoError(t, m.Close(ctx))
}

func TestParseConfigYAML(t *testing.T) {
	out, err := parseConfigYAML("timeout: 10s\nretry: 3")
	require.NoError(t, err)
	assert.Equal(t, "10s", out["timeout"])
	assert.Equal(t, 3, out["retry"])
}

func TestParseConfigYAMLEmpty(t *testing.T) {
	out, err := parseConfigYAML("")
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestParseConfigYAMLInvalid(t *testing.T) {
	out, err := parseConfigYAML(": : :")
	assert.Error(t, err)
	// Should still return an empty map, not nil.
	assert.NotNil(t, out)
}

func TestPluginTypeValidate(t *testing.T) {
	assert.True(t, TypeChannel.Validate())
	assert.True(t, TypeGate.Validate())
	assert.True(t, TypeModule.Validate())
	assert.True(t, TypeNotifier.Validate())
	assert.False(t, PluginType("bogus").Validate())
	assert.False(t, PluginType("").Validate())
}

func TestAllTypes(t *testing.T) {
	types := AllTypes()
	assert.Len(t, types, 4)
	assert.Contains(t, types, TypeChannel)
	assert.Contains(t, types, TypeGate)
	assert.Contains(t, types, TypeModule)
	assert.Contains(t, types, TypeNotifier)
}

func TestAllStates(t *testing.T) {
	states := AllStates()
	assert.Len(t, states, 4)
	assert.Contains(t, states, StateInstalled)
	assert.Contains(t, states, StateEnabled)
	assert.Contains(t, states, StateDisabled)
	assert.Contains(t, states, StateError)
}

func TestWrapError(t *testing.T) {
	err := wrapError("test", assert.AnError)
	assert.Contains(t, err.Error(), "test")
	assert.ErrorIs(t, err, assert.AnError)
}

func TestWrapErrorNil(t *testing.T) {
	assert.Nil(t, wrapError("test", nil))
}