// inventory_test.go — persistence tests for managed targets and inventory
// groups. SQLite paths run everywhere; PG paths are env-gated on
// LEVEE_PG_TEST_DSN like the rest of the PG suite.
package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInventoryGroupCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	g := &InventoryGroup{ID: "grp-1", Name: "prod/db", CreatedAt: time.Now().UTC()}
	require.NoError(t, store.UpsertInventoryGroup(ctx, g))

	got, err := store.GetInventoryGroup(ctx, "grp-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "prod/db", got.Name)

	// Upsert overwrites by ID; a second group with the same NAME fails.
	dup := &InventoryGroup{ID: "grp-2", Name: "prod/db"}
	err = store.UpsertInventoryGroup(ctx, dup)
	require.Error(t, err)

	all, err := store.ListInventoryGroups(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1)

	require.NoError(t, store.DeleteInventoryGroup(ctx, "grp-1"))
	gone, err := store.GetInventoryGroup(ctx, "grp-1")
	require.NoError(t, err)
	assert.Nil(t, gone)
}

func TestTargetCRUDAndUniqueAddress(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.UpsertInventoryGroup(ctx,
		&InventoryGroup{ID: "grp-1", Name: "prod", CreatedAt: time.Now().UTC()}))

	tg := &Target{
		ID:          "tgt-1",
		Hostname:    "10.0.0.5",
		Port:        22,
		ChannelType: "ssh",
		Labels:      map[string]string{"env": "prod", "app": "pay"},
		Status:      StatusActive,
		CreatedAt:   time.Now().UTC(),
	}
	require.NoError(t, store.UpsertTarget(ctx, tg))

	got, err := store.GetTarget(ctx, "tgt-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, map[string]string{"env": "prod", "app": "pay"}, got.Labels)
	assert.Equal(t, StatusActive, got.Status)

	// Same ID upsert updates in place.
	tg.Status = StatusFrozen
	tg.Labels = map[string]string{"env": "prod"}
	require.NoError(t, store.UpsertTarget(ctx, tg))
	got, _ = store.GetTarget(ctx, "tgt-1")
	assert.Equal(t, StatusFrozen, got.Status)

	// Different ID claiming the same address must fail with the sentinel.
	other := &Target{ID: "tgt-2", Hostname: "10.0.0.5", Port: 22, ChannelType: "ssh", CreatedAt: time.Now().UTC()}
	err = store.UpsertTarget(ctx, other)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDuplicateTarget), "got %v", err)

	byAddr, err := store.FindTargetByAddress(ctx, "10.0.0.5", 22)
	require.NoError(t, err)
	require.NotNil(t, byAddr)
	assert.Equal(t, "tgt-1", byAddr.ID)

	missing, err := store.FindTargetByAddress(ctx, "10.0.0.9", 22)
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func TestTargetFilterAndStatusAndReachability(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.UpsertInventoryGroup(ctx,
		&InventoryGroup{ID: "grp-1", Name: "prod", CreatedAt: time.Now().UTC()}))

	fixtures := []*Target{
		{ID: "a", Hostname: "h-a", Port: 22, Labels: map[string]string{"env": "prod"}, GroupID: "grp-1", Status: StatusActive},
		{ID: "b", Hostname: "h-b", Port: 22, Labels: map[string]string{"env": "dev"}, Status: StatusActive},
		{ID: "c", Hostname: "h-c", Port: 22, Labels: map[string]string{"env": "prod", "app": "db"}, Status: StatusRetired},
	}
	for _, tg := range fixtures {
		tg.CreatedAt = time.Now().UTC()
		require.NoError(t, store.UpsertTarget(ctx, tg))
	}

	// Label AND-matching.
	prods, err := store.ListTargets(ctx, TargetFilter{Labels: map[string]string{"env": "prod"}})
	require.NoError(t, err)
	assert.Len(t, prods, 2)

	both, err := store.ListTargets(ctx, TargetFilter{Labels: map[string]string{"env": "prod", "app": "db"}})
	require.NoError(t, err)
	assert.Len(t, both, 1)
	assert.Equal(t, "c", both[0].ID)

	// Group + status filters.
	inGroup, err := store.ListTargets(ctx, TargetFilter{GroupID: "grp-1"})
	require.NoError(t, err)
	assert.Len(t, inGroup, 1)

	activeOnly, err := store.ListTargets(ctx, TargetFilter{Status: StatusActive})
	require.NoError(t, err)
	assert.Len(t, activeOnly, 2)

	// Status update + reachability stamping.
	require.NoError(t, store.UpdateTargetStatus(ctx, "a", StatusFrozen))
	now := time.Now().UTC()
	require.NoError(t, store.SetTargetReachability(ctx, "a", true, now))

	got, _ := store.GetTarget(ctx, "a")
	assert.Equal(t, StatusFrozen, got.Status)
	require.NotNil(t, got.LastCheckedAt)
	assert.True(t, got.Reachable)

	n, err := store.CountTargetsInGroup(ctx, "grp-1")
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	require.NoError(t, store.DeleteTarget(ctx, "a"))
	gone, err := store.GetTarget(ctx, "a")
	require.NoError(t, err)
	assert.Nil(t, gone)
}

func TestPG_InventoryRoundTrip(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	tg := &Target{ID: "pg-tgt", Hostname: "pg-host", Port: 2222, ChannelType: "ssh",
		Labels: map[string]string{"env": "staging"}, Status: StatusActive, CreatedAt: time.Now().UTC()}
	require.NoError(t, store.UpsertTarget(ctx, tg))

	got, err := store.GetTarget(ctx, "pg-tgt")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, map[string]string{"env": "staging"}, got.Labels)

	filtered, err := store.ListTargets(ctx, TargetFilter{Labels: map[string]string{"env": "staging"}})
	require.NoError(t, err)
	assert.Len(t, filtered, 1)
}
