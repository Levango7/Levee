package inventory

// ownership_write_test.go covers the WRITE-side half of the ownership registry:
// what a target declaration must not do to a host the platform has already
// registered. The read-side resolution order is pinned in
// internal/grpc/target_env_ownership_test.go; the loader and startup audit in
// ownership_test.go. What is new here is the judgement of a write.
//
// The property under test is deliberately narrow, because the check is: for a
// host target-ownership.yaml declares, the env a write would LEAVE must equal
// the registry's env. Everything else — a host the registry does not mention,
// no registry at all — must behave exactly as before, which is why the
// "not declared" and "no registry" cases are control cases rather than
// afterthoughts.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func newOwnershipTestStore(t *testing.T) state.Store {
	t.Helper()
	store, err := state.NewSQLiteStore(context.Background(), filepath.Join(t.TempDir(), "inv.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newTestRegistry(t *testing.T, body string) *OwnershipRegistry {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, OwnershipFileName), []byte(body), 0o600))
	reg, err := LoadOwnership(dir)
	require.NoError(t, err)
	require.True(t, reg.Configured())
	return reg
}

func envConflictsFor(t *testing.T, store state.Store, reg *OwnershipRegistry, yaml string) []EnvConflict {
	t.Helper()
	f, err := ParseYAML([]byte(yaml))
	require.NoError(t, err)
	conflicts, err := NewImporter(store).EnvConflicts(context.Background(), reg, f)
	require.NoError(t, err)
	return conflicts
}

func TestEnvConflictsRefusesADeclaredHostsWrongEnvironment(t *testing.T) {
	store := newOwnershipTestStore(t)
	reg := newTestRegistry(t, "hosts:\n  - hostname: db-1\n    environment: prod\n")

	conflicts := envConflictsFor(t, store, reg, "targets:\n  - address: db-1\n    labels:\n      env: dev\n")
	require.Len(t, conflicts, 1)
	assert.Equal(t, "db-1", conflicts[0].Host)
	assert.Equal(t, 22, conflicts[0].Port)
	assert.Equal(t, "prod", conflicts[0].RegistryEnv)
	assert.Equal(t, "dev", conflicts[0].DeclaredEnv)
	assert.Equal(t, "targets[0]", conflicts[0].Row,
		"the row label is what tells an operator which line to fix in a multi-host file")

	// The refusal message must name BOTH facts and the file they disagree with;
	// "conflict" alone would leave the operator guessing which side is the
	// platform's.
	msg := conflicts[0].String()
	assert.Contains(t, msg, "db-1:22")
	assert.Contains(t, msg, "prod")
	assert.Contains(t, msg, "dev")
	assert.Contains(t, msg, OwnershipFileName)
}

func TestEnvConflictsAcceptsTheRegisteredEnvironment(t *testing.T) {
	store := newOwnershipTestStore(t)
	reg := newTestRegistry(t, "hosts:\n  - hostname: db-1\n    environment: prod\n")

	conflicts := envConflictsFor(t, store, reg, "targets:\n  - address: db-1\n    labels:\n      env: prod\n      role: db\n")
	assert.Empty(t, conflicts, "extra labels are not a conflict; only the env value is compared")
}

func TestEnvConflictsJudgesTheMergedLabelsNotJustTheDeclaration(t *testing.T) {
	// The subtle case: a declaration that names NO labels preserves the stored
	// ones when it also leaves the status alone (mergedLabels). Judging only
	// td.Labels would see "nothing declared" and let a stale, wrong env survive
	// a re-import forever.
	store := newOwnershipTestStore(t)
	reg := newTestRegistry(t, "hosts:\n  - hostname: db-1\n    environment: prod\n")
	require.NoError(t, store.UpsertTarget(context.Background(), &state.Target{
		ID: "tgt-db-1", Hostname: "db-1", Port: 22, ChannelType: "ssh",
		Status: state.StatusActive, Labels: map[string]string{"env": "dev"},
	}))

	conflicts := envConflictsFor(t, store, reg, "targets:\n  - address: db-1\n")
	require.Len(t, conflicts, 1, "a labels-free re-import must be judged on what the row would keep")
	assert.Equal(t, "dev", conflicts[0].DeclaredEnv, "the declared value is the merged result, not the file's silence")
}

func TestEnvConflictsIgnoresHostsTheRegistryDoesNotDeclare(t *testing.T) {
	// The bound of the whole feature: the registry's coverage. A host it does
	// not mention keeps labels-and-default-env behaviour, and refusing it would
	// turn "not opted in for this host" into an outage.
	store := newOwnershipTestStore(t)
	reg := newTestRegistry(t, "hosts:\n  - hostname: db-1\n    environment: prod\n")

	conflicts := envConflictsFor(t, store, reg, "targets:\n  - address: web-9\n    labels:\n      env: dev\n")
	assert.Empty(t, conflicts)
}

func TestEnvConflictsWithNoRegistryIsANoop(t *testing.T) {
	store := newOwnershipTestStore(t)
	reg, err := LoadOwnership(filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	require.False(t, reg.Configured())

	conflicts := envConflictsFor(t, store, reg, "targets:\n  - address: db-1\n    labels:\n      env: dev\n")
	assert.Empty(t, conflicts, "no registry means the deployment has not opted in, not that every write is wrong")

	// A nil registry must mean the same thing: callers may hold nil rather than
	// an empty value, and a nil dereference here would be a crash on a
	// deployment that never wrote the file.
	conflicts, err = NewImporter(store).EnvConflicts(context.Background(), nil, &File{
		Targets: []TargetDef{{Address: "db-1", Labels: map[string]string{"env": "dev"}}},
	})
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

func TestEnvConflictsSkipsRowsTheImporterWouldReject(t *testing.T) {
	// A row with a malformed address, an unknown status or an invalid channel
	// reaches no write, so it must not produce a conflict that blocks the rest of
	// the file. The importer reports those rows itself; this check must not
	// pre-empt it with a misleading ownership refusal.
	store := newOwnershipTestStore(t)
	reg := newTestRegistry(t, "hosts:\n  - hostname: db-1\n    environment: prod\n")

	conflicts := envConflictsFor(t, store, reg, `targets:
  - address: ""
    labels:
      env: dev
  - address: db-1
    status: bogus
    labels:
      env: dev
  - address: db-1
    channel_type: BadType
    labels:
      env: dev
`)
	assert.Empty(t, conflicts)
}

func TestFormatEnvConflictsIsBounded(t *testing.T) {
	many := make([]EnvConflict, 0, 9)
	for i := 0; i < 9; i++ {
		many = append(many, EnvConflict{Host: "h", Port: 22, RegistryEnv: "prod", DeclaredEnv: "dev"})
	}
	msg := FormatEnvConflicts(many)
	assert.Contains(t, msg, "and 4 more", "an unbounded refusal string is an outage of its own")

	hosts := EnvConflictHosts(many)
	assert.Contains(t, hosts, "+1 more")
	assert.NotContains(t, hosts, "h:22,h:22,h:22,h:22,h:22,h:22,h:22,h:22,h:22",
		"the audit target must be bounded too")
}
