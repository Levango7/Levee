package grpc

// ownership_write_test.go covers the WRITE-side enforcement of
// target-ownership.yaml at the two RPCs that can create or rewrite inventory
// rows: AddTarget and ImportTargets.
//
// ADR-008 records why this exists and why it is P1 rather than P0: writing a
// false env label is not a privilege escape (the policy layer builds the labels
// it judges from the environment being judged, not from the row), but it does
// make the GATE judge the caller's claim instead of the platform's machine, and
// it makes every labels-based view of the inventory lie.
//
// Each test carries its own control, because the failure mode this batch could
// introduce is over-tightening: a check that also refused hosts the registry
// never mentions would be an outage dressed as a policy.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/inventory"
	"github.com/nexus/levee/internal/state"
)

func ownershipRegistryFor(t *testing.T, body string) *inventory.OwnershipRegistry {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, inventory.OwnershipFileName), []byte(body), 0o600))
	reg, err := inventory.LoadOwnership(dir)
	require.NoError(t, err)
	require.True(t, reg.Configured())
	return reg
}

func TestAddTargetRefusesAnEnvironmentTheRegistryContradicts(t *testing.T) {
	store := newTestStore(t)
	svc := NewTargetService(store, nil).WithOwnershipRegistry(
		ownershipRegistryFor(t, "hosts:\n  - hostname: db-1\n    environment: prod\n"))

	_, err := svc.AddTarget(context.Background(), &pb.AddTargetRequest{
		Id: "tgt-db-1", Hostname: "db-1", Labels: map[string]string{"env": "dev"},
	})
	require.Error(t, err, "registering a prod host as dev must not succeed")
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	assert.Contains(t, err.Error(), "prod", "the refusal must name the platform's fact")
	assert.Contains(t, err.Error(), "dev", "the refusal must name the claim it rejected")

	// Nothing was written: the check runs before the upsert, so a refusal is not
	// a half-registration.
	rows, lerr := store.ListTargets(context.Background(), state.TargetFilter{})
	require.NoError(t, lerr)
	assert.Empty(t, rows)
}

func TestAddTargetAcceptsTheRegisteredEnvironment(t *testing.T) {
	store := newTestStore(t)
	svc := NewTargetService(store, nil).WithOwnershipRegistry(
		ownershipRegistryFor(t, "hosts:\n  - hostname: db-1\n    environment: prod\n"))

	got, err := svc.AddTarget(context.Background(), &pb.AddTargetRequest{
		Id: "tgt-db-1", Hostname: "db-1", Labels: map[string]string{"env": "prod", "role": "db"},
	})
	require.NoError(t, err)
	assert.Equal(t, "db-1", got.Hostname)
}

func TestAddTargetKeepsLabelsBehaviourForUndeclaredHosts(t *testing.T) {
	// The control that keeps this feature from becoming an outage: the registry
	// declares db-1 only, so web-9 keeps the historical caller-asserted
	// behaviour. If this ever fails, the check has silently become "every add
	// must appear in target-ownership.yaml".
	store := newTestStore(t)
	svc := NewTargetService(store, nil).WithOwnershipRegistry(
		ownershipRegistryFor(t, "hosts:\n  - hostname: db-1\n    environment: prod\n"))

	_, err := svc.AddTarget(context.Background(), &pb.AddTargetRequest{
		Id: "tgt-web-9", Hostname: "web-9", Labels: map[string]string{"env": "dev"},
	})
	require.NoError(t, err, "the registry's coverage is the bound of the check, not a whitelist of主机")
}

func TestAddTargetWithNilRegistryKeepsPreviousBehaviour(t *testing.T) {
	// No registry (the default deployment): adding a host must behave exactly as
	// it did before this batch. A nil registry is "not opted in", never "refuse".
	store := newTestStore(t)
	svc := NewTargetService(store, nil)

	_, err := svc.AddTarget(context.Background(), &pb.AddTargetRequest{
		Id: "tgt-any", Hostname: "any-host", Labels: map[string]string{"env": "dev"},
	})
	require.NoError(t, err)
}

func TestAddTargetHasNoEnvConflictExemption(t *testing.T) {
	// The exemption is granted to ImportTargets only, per ADR-008 answer 5: an
	// exemption on the single-host path would be a per-request bypass of the
	// gate itself. The proto carries no such field, so what this pins is that
	// the refusal happens regardless of anything the caller could set.
	store := newTestStore(t)
	svc := NewTargetService(store, nil).WithOwnershipRegistry(
		ownershipRegistryFor(t, "hosts:\n  - hostname: db-1\n    environment: prod\n"))

	_, err := svc.AddTarget(context.Background(), &pb.AddTargetRequest{
		Id: "tgt-db-1", Hostname: "db-1", Labels: map[string]string{"env": "dev"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no env-conflict exemption",
		"the refusal should say why the caller cannot force it through")
}

func TestImportTargetsRefusesEnvConflictsByDefault(t *testing.T) {
	store := newTestStore(t)
	svc := NewInventoryService(store).WithOwnershipRegistry(
		ownershipRegistryFor(t, "hosts:\n  - hostname: db-1\n    environment: prod\n"))

	_, err := svc.ImportTargets(context.Background(), &pb.ImportTargetsRequest{
		YamlContent: "targets:\n  - address: db-1\n    labels:\n      env: dev\n",
	})
	require.Error(t, err, "a bulk sync must not silently contradict the platform's fact")
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	assert.Contains(t, err.Error(), "allow_env_conflict",
		"the refusal must name the escape hatch, or an operator is stuck with no path forward")

	// Refusal happens before the first write, so the inventory is untouched.
	rows, lerr := store.ListTargets(context.Background(), state.TargetFilter{})
	require.NoError(t, lerr)
	assert.Empty(t, rows)
}

func TestImportTargetsAllowEnvConflictWritesAndAudits(t *testing.T) {
	store := newTestStore(t)
	svc := NewInventoryService(store).WithOwnershipRegistry(
		ownershipRegistryFor(t, "hosts:\n  - hostname: db-1\n    environment: prod\n"))

	resp, err := svc.ImportTargets(asSubject("dave"), &pb.ImportTargetsRequest{
		YamlContent:      "targets:\n  - address: db-1\n    labels:\n      env: dev\n",
		AllowEnvConflict: true,
	})
	require.NoError(t, err, "the explicit per-call override must work: %v", err)
	assert.Equal(t, int32(1), resp.Created)

	// The row was written as declared — the override is an override, not a
	// silent correction.
	row, err := store.FindTargetByAddress(context.Background(), "db-1", 22)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "dev", row.Labels["env"])

	// And it is evidence, not just a write: an override with no audit row is the
	// "silent default" ADR-008 rejects, wearing a flag name.
	entries, err := store.ListAudits(context.Background(), state.AuditFilter{Action: state.AuditActionImportEnvConflict})
	require.NoError(t, err)
	require.Len(t, entries, 1, "the override must leave exactly one audit row")
	assert.Equal(t, "dave", entries[0].Actor, "the audit row must name the caller who chose to override")
	assert.Contains(t, entries[0].Target, "db-1:22", "the audit row must name the host it covered")
}

func TestImportTargetsWithoutConflictsWritesNoAudit(t *testing.T) {
	// The control: a consistent import is not an override and must not litter the
	// trail with rows that make every sync look like a decision.
	store := newTestStore(t)
	svc := NewInventoryService(store).WithOwnershipRegistry(
		ownershipRegistryFor(t, "hosts:\n  - hostname: db-1\n    environment: prod\n"))

	_, err := svc.ImportTargets(context.Background(), &pb.ImportTargetsRequest{
		YamlContent: "targets:\n  - address: db-1\n    labels:\n      env: prod\n",
	})
	require.NoError(t, err)

	entries, err := store.ListAudits(context.Background(), state.AuditFilter{Action: state.AuditActionImportEnvConflict})
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestImportTargetsAllowEnvConflictStillEnforcesTheStoredEnvGuard(t *testing.T) {
	// The override must not be a master key. Moving a host out of an environment
	// the caller cannot administer stays refused even when the caller asks to
	// ignore the registry — otherwise the new flag would reopen the hole the
	// stored-env check closed.
	store := newTestStore(t)
	seedTarget(t, store, "tgt-prod-9", "prod-9", "prod")
	svc := NewInventoryService(store).WithAuthorizer(newServiceAuthorizer(t)).
		WithOwnershipRegistry(ownershipRegistryFor(t, "hosts:\n  - hostname: prod-9\n    environment: prod\n"))

	_, err := svc.ImportTargets(asSubject("dave"), &pb.ImportTargetsRequest{
		YamlContent:      "targets:\n  - address: prod-9\n    labels:\n      env: dev\n",
		AllowEnvConflict: true,
	})
	requireDenied(t, err, "ImportTargets (override must not bypass the stored-env admin check)")

	row, ferr := store.FindTargetByAddress(context.Background(), "prod-9", 22)
	require.NoError(t, ferr)
	require.NotNil(t, row)
	assert.Equal(t, "prod", row.Labels["env"], "a refused import must not relabel the row")
}

func TestImportTargetsUndeclaredHostsStayUnchecked(t *testing.T) {
	store := newTestStore(t)
	svc := NewInventoryService(store).WithOwnershipRegistry(
		ownershipRegistryFor(t, "hosts:\n  - hostname: db-1\n    environment: prod\n"))

	resp, err := svc.ImportTargets(context.Background(), &pb.ImportTargetsRequest{
		YamlContent: "targets:\n  - address: web-9\n    labels:\n      env: dev\n",
	})
	require.NoError(t, err, "a host the registry does not declare keeps labels behaviour: %v", err)
	assert.Equal(t, int32(1), resp.Created)
}
