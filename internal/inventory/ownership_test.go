package inventory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/state"
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

// --- LabelConflicts: the startup audit ---------------------------------------
//
// Policy time is settled (registry wins), but the console still SHOWS the
// label — an operator reading the inventory sees "dev" while policy judges
// "prod". This audit is what keeps that gap visible instead of drifting.

func regWith(t *testing.T, dir, body string) *OwnershipRegistry {
	t.Helper()
	writeOwnership(t, dir, body)
	reg, err := LoadOwnership(dir)
	require.NoError(t, err)
	return reg
}

func rowWith(hostname, env string) *state.Target {
	labels := map[string]string{}
	if env != "" {
		labels["env"] = env
	}
	return &state.Target{ID: "tgt-" + hostname, Hostname: hostname, Port: 22, Labels: labels, Status: "active"}
}

func TestLabelConflictsFindsDisagreements(t *testing.T) {
	dir := t.TempDir()
	reg := regWith(t, dir, "hosts:\n  - hostname: db-1\n    environment: prod\n  - hostname: cache-1\n    environment: prod\n")

	rows := []*state.Target{
		rowWith("db-1", "dev"),    // disagrees
		rowWith("web-1", "prod"),  // not registered: not ours to judge
		rowWith("cache-1", ""),    // registered, row declares nothing: also a gap
		rowWith("other-1", "dev"), // not registered: not ours to judge
		nil,                       // tolerate nil rows in a snapshot
	}
	got := LabelConflicts(reg, rows)
	require.Len(t, got, 2)
	assert.Equal(t, "db-1", got[0].Hostname)
	assert.Equal(t, "prod", got[0].RegistryEnv)
	assert.Equal(t, "dev", got[0].LabelEnv)
	assert.Equal(t, "cache-1", got[1].Hostname)
	assert.Equal(t, "prod", got[1].RegistryEnv)
	assert.Empty(t, got[1].LabelEnv)
}

func TestLabelConflictsRegisteredWithoutRowIsNotAConflict(t *testing.T) {
	// "registered ahead of an import" is the supported flow (the resolver test
	// in grpc pins that the registry answers for such hosts). The audit is
	// about rows that EXIST and disagree — reporting the ahead-of-import case
	// would make the normal order of operations look like a problem.
	dir := t.TempDir()
	reg := regWith(t, dir, "hosts:\n  - hostname: future-db\n    environment: prod\n")
	assert.Nil(t, LabelConflicts(reg, []*state.Target{rowWith("web-1", "dev")}))
}

func TestLabelConflictsNilOrEmptyRegistryIsNil(t *testing.T) {
	assert.Nil(t, LabelConflicts(nil, []*state.Target{rowWith("db-1", "dev")}))
	assert.Nil(t, LabelConflicts(&OwnershipRegistry{}, []*state.Target{rowWith("db-1", "dev")}))
}

func TestLabelConflictsSameEnvDifferentExtraLabelsIsConsistent(t *testing.T) {
	dir := t.TempDir()
	reg := regWith(t, dir, "hosts:\n  - hostname: db-1\n    environment: prod\n")
	row := rowWith("db-1", "prod")
	row.Labels["app"] = "pay" // extra labels don't make a conflict
	assert.Nil(t, LabelConflicts(reg, []*state.Target{row}))
}
