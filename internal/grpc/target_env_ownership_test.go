package grpc

// Resolution order for "which environment does this host belong to", once a
// platform-side ownership registry is wired (inventory.LoadOwnership):
//
//   1. the registry — the platform's own fact, the one input nothing in a
//      request can assert;
//   2. the row's labels["env"] — caller-asserted, now only for hosts the
//      registry does not name;
//   3. default_env — authz's fallback (not tested here; that is authz's own).
//
// These cases pin the ORDER, because the order is the security property: a
// registry that exists but is consulted after labels would make every
// re-labelled row win over the platform's fact, and the file would be
// decoration.

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/inventory"
	"github.com/nexus/levee/internal/state"
)

// writeOwnershipYaml drops a target-ownership.yaml into dir and returns the
// path. grpc tests cannot use inventory's own helper (it is package-private),
// and duplicating a 3-line helper is cheaper than exporting a test hook.
func writeOwnershipYaml(dir, body string) error {
	return os.WriteFile(dir+"/target-ownership.yaml", []byte(body), 0o600)
}

func seedOwnedRow(t *testing.T, store state.Store, id, hostname string, labels map[string]string) {
	t.Helper()
	require.NoError(t, store.UpsertTarget(context.Background(), &state.Target{
		ID: id, Hostname: hostname, Port: 22, ChannelType: "ssh",
		Labels: labels, Status: state.StatusActive,
	}))
}

func TestOwnershipRegistryWinsOverLabels(t *testing.T) {
	store := newTestStore(t)
	// The row CLAIMS dev. The platform says prod.
	seedOwnedRow(t, store, "tgt-1", "db-1", map[string]string{"env": "dev"})

	dir := t.TempDir()
	require.NoError(t, writeOwnershipYaml(dir, "hosts:\n  - hostname: db-1\n    environment: prod\n"))
	reg, err := inventory.LoadOwnership(dir)
	require.NoError(t, err)

	resolve := TargetEnvResolver(store, reg)
	env, known := resolve(context.Background(), "db-1")
	require.True(t, known)
	assert.Equal(t, "prod", env, "the platform's fact must win over a caller-asserted label")

	// Same answer whichever spelling the request arrives in.
	env, known = resolve(context.Background(), "tgt-1")
	require.True(t, known)
	assert.Equal(t, "prod", env, "id-spelled requests resolve through the row's hostname into the registry")
}

func TestUnregisteredHostFallsBackToLabels(t *testing.T) {
	store := newTestStore(t)
	seedOwnedRow(t, store, "tgt-2", "web-9", map[string]string{"env": "staging"})

	dir := t.TempDir()
	require.NoError(t, writeOwnershipYaml(dir, "hosts:\n  - hostname: db-1\n    environment: prod\n"))
	reg, err := inventory.LoadOwnership(dir)
	require.NoError(t, err)

	resolve := TargetEnvResolver(store, reg)
	env, known := resolve(context.Background(), "web-9")
	require.True(t, known)
	assert.Equal(t, "staging", env, "a host the registry does not name keeps the labels answer — the file must not blank out rows it knows nothing about")
}

func TestNilRegistryIsExactlyTheOldResolver(t *testing.T) {
	store := newTestStore(t)
	seedOwnedRow(t, store, "tgt-3", "cache-1", map[string]string{"env": "prod"})

	resolve := TargetEnvResolver(store, nil)
	env, known := resolve(context.Background(), "cache-1")
	require.True(t, known)
	assert.Equal(t, "prod", env)

	// And an unknown host is still "not in the inventory", the refusal case the
	// diagnosis gate depends on.
	_, known = resolve(context.Background(), "no-such-host")
	assert.False(t, known)
}

func TestRegistryAnswersHostsTheStoreDoesNotHold(t *testing.T) {
	// A host can be registered before the inventory row exists (the operator
	// writes ownership ahead of an import). The registry's fact still answers:
	// the platform said this host is prod, and "not in the inventory yet" is not
	// a licence to fall back to whatever a request asserts.
	store := newTestStore(t)
	dir := t.TempDir()
	require.NoError(t, writeOwnershipYaml(dir, "hosts:\n  - hostname: future-db\n    environment: prod\n"))
	reg, err := inventory.LoadOwnership(dir)
	require.NoError(t, err)

	resolve := TargetEnvResolver(store, reg)
	env, known := resolve(context.Background(), "future-db")
	require.True(t, known, "a registered fact is knowledge even without an inventory row")
	assert.Equal(t, "prod", env)
}
