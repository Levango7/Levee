package inventory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeOwnership(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, OwnershipFileName)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoadOwnershipMissingFileIsEmptyRegistry(t *testing.T) {
	// The optional-file posture: no file means the platform has no ownership
	// facts, which keeps labels-and-default-env for deployments that have not
	// opted in. Missing must not be an error.
	reg, err := LoadOwnership(filepath.Join(t.TempDir(), "no-such-dir"))
	require.NoError(t, err)
	assert.False(t, reg.Configured())
	env, ok := reg.Lookup("db-1")
	assert.False(t, ok)
	assert.Empty(t, env)
}

func TestLoadOwnershipParsesAndNormalises(t *testing.T) {
	dir := t.TempDir()
	writeOwnership(t, dir, `
hosts:
  - hostname: db-1.prod.internal
    environment: prod
  - hostname: Web-1   # case + whitespace are operator noise, not identity
    environment: dev
`)
	reg, err := LoadOwnership(dir)
	require.NoError(t, err)
	assert.True(t, reg.Configured())

	env, ok := reg.Lookup("db-1.prod.internal")
	require.True(t, ok)
	assert.Equal(t, "prod", env)

	// Lookup is case-insensitive and trimmed on the lookup side too: the same
	// host spelled the way an alert's instance label spells it must still hit.
	env, ok = reg.Lookup("  web-1 ")
	require.True(t, ok)
	assert.Equal(t, "dev", env)

	// Unknown host: a miss, not an error.
	_, ok = reg.Lookup("not-registered")
	assert.False(t, ok)
}

func TestLoadOwnershipRejectsIncompleteFacts(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"no hostname", "hosts:\n  - environment: prod\n", "has no hostname"},
		{"no environment", "hosts:\n  - hostname: db-1\n", "has no environment"},
		{"duplicate host", "hosts:\n  - hostname: db-1\n    environment: prod\n  - hostname: db-1\n    environment: dev\n", "declared twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Half an ownership fact is worse than none: a deployment that opted
			// in and wrote a broken file must not silently degrade to
			// caller-asserted labels.
			_, err := LoadOwnership(func() string { d := t.TempDir(); writeOwnership(t, d, tc.body); return d }())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLoadOwnershipRejectsUnparsableFile(t *testing.T) {
	dir := t.TempDir()
	writeOwnership(t, dir, "hosts: [ {broken")
	_, err := LoadOwnership(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), OwnershipFileName)
}

func TestNilRegistryIsSafeEverywhere(t *testing.T) {
	// The resolver layer may hold a nil registry (no --ownership wiring). Every
	// method must answer, not panic: nil and empty are the same fact.
	var reg *OwnershipRegistry
	assert.False(t, reg.Configured())
	_, ok := reg.Lookup("db-1")
	assert.False(t, ok)
	assert.Nil(t, reg.Hostnames())
}
