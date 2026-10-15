package main

// serve_cluster_node_test.go covers what a `serve` process declares about itself
// into the cluster registry.
//
// `cluster_nodes.capabilities` had no producer: the one construction of
// cluster.Node in this repository is the join inside openServeStore, and it named
// id, address, status and role only. So the console's node cards were right that
// nothing was declared, and no operator could have said otherwise. These tests
// pin the declaration path that replaces that — including the two ways a
// declaration could still end up meaningful-looking but inert: the flag never
// reaching the join (a field set on a local nobody reads), and a value the
// registry cannot parse being stored anyway.

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/capability"
	"github.com/nexus/levee/internal/cluster"
	"github.com/nexus/levee/internal/config"
)

// restoreClusterOpts puts the cluster option globals back. restoreServeOpts in
// store_selection_test.go covers id/addr/dsn/cluster; capabilities join it rather
// than becoming a second list that can drift from the first.
func restoreClusterOpts(t *testing.T) {
	t.Helper()
	caps := serveOptNodeCapabilities
	t.Cleanup(func() { serveOptNodeCapabilities = caps })
}

func TestClusterNodeForServe(t *testing.T) {
	defer resetRootFlags()
	restoreServeOpts(t)
	restoreClusterOpts(t)
	serveOptNodeID = "node-serve-1"
	serveOptNodeAddr = "10.0.1.11:9090"
	serveOptNodeRole = "worker"

	t.Run("nothing declared is the column's empty value", func(t *testing.T) {
		serveOptNodeCapabilities = nil
		node, err := clusterNodeForServe()
		require.NoError(t, err)
		assert.Equal(t, capability.Empty, node.Capabilities)
		assert.Equal(t, "node-serve-1", node.ID)
		assert.Equal(t, "10.0.1.11:9090", node.Address)
		assert.Equal(t, cluster.RoleWorker, node.Role)
		assert.Equal(t, cluster.StatusActive, node.Status)
	})

	t.Run("the flag is normalised into what readers expect", func(t *testing.T) {
		// Typed in one order, stored as a set: the same list re-typed differently
		// must not look like a changed declaration on the next restart.
		serveOptNodeCapabilities = []string{"shell", " pkg ", "shell"}
		node, err := clusterNodeForServe()
		require.NoError(t, err)
		assert.Equal(t, `["pkg","shell"]`, node.Capabilities)
	})

	t.Run("a blank name refuses the start", func(t *testing.T) {
		// Cobra's CSV splitting produces the empty entry from "shell,,pkg"; storing
		// it would put a capability on the card that matches no module and no
		// filter, which is a decoration with a number next to it.
		serveOptNodeCapabilities = []string{"shell", " "}
		_, err := clusterNodeForServe()
		require.Error(t, err, "an empty capability name must fail startup, not be stored")
		assert.Contains(t, err.Error(), "--node-capabilities")
		assert.Contains(t, err.Error(), "must not be empty")
	})
}

// TestServeRefusesNodeCapabilitiesWithoutCluster is the same posture the --pg-dsn
// refusal takes: a flag that only has a meaning inside cluster coordination is
// refused rather than accepted and dropped.
func TestServeRefusesNodeCapabilitiesWithoutCluster(t *testing.T) {
	defer resetRootFlags()
	restoreServeOpts(t)
	restoreClusterOpts(t)

	cfgPath, dbPath := writeStoreConfig(t, "sqlite", "")
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)

	serveOptCluster = false
	serveOptNodeCapabilities = []string{"shell"}

	store, mgr, err := openServeStore(context.Background(), cfg)
	require.Error(t, err, "--node-capabilities without --cluster declares into a registry that does not exist")
	assert.Nil(t, store)
	assert.Nil(t, mgr)
	assert.Contains(t, err.Error(), "--cluster")
	assert.Contains(t, err.Error(), "shell", "the refusal must name what it is refusing")
	_, statErr := os.Stat(dbPath)
	assert.True(t, os.IsNotExist(statErr), "the refusal must happen before any store is opened")
}

// TestServeClusterJoinDeclaresCapabilities is the end-to-end half: it drives the
// real openServeStore against PostgreSQL and reads back the row the join wrote.
//
// Without this, every other test in this file could pass while the declaration
// stopped at clusterNodeForServe — the function would be correct and uncalled,
// which is the failure shape this repository has now hit four times.
//
// Runs in a private schema (pgScopedDSN), so it neither sees nor leaves rows in
// the cluster_nodes table the state, cluster and takeover suites share; the CI
// postgres job carries it as its own step.
func TestServeClusterJoinDeclaresCapabilities(t *testing.T) {
	dsn := pgScopedDSN(t)
	defer resetRootFlags()
	restoreServeOpts(t)
	restoreClusterOpts(t)

	nodeID := "serve-caps-" + time.Now().UTC().Format("150405.000")
	cfgPath, _ := writeStoreConfig(t, "postgres", dsn)
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)

	serveOptCluster = true
	serveOptPGDSN = dsn
	serveOptNodeID = nodeID
	serveOptNodeAddr = "10.0.9.9:9090"
	serveOptNodeRole = "worker"
	serveOptNodeCapabilities = []string{"shell", "pkg"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, mgr, err := openServeStore(ctx, cfg)
	require.NoError(t, err, "serve must be able to join the cluster it was told to join")
	require.NotNil(t, mgr)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = mgr.Stop(stopCtx)
		_ = store.Close()
	})

	// Read the row back through a separate handle: the claim is about what the
	// registry stored, not about what the manager believes in memory.
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var stored string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT capabilities FROM cluster_nodes WHERE id = $1`, nodeID).Scan(&stored))
	assert.Equal(t, `["pkg","shell"]`, stored,
		"the --node-capabilities flag must reach cluster_nodes.capabilities, canonicalised")

	// And the same value is what a reader can filter on: Has is the gateway's
	// predicate, so a declaration it cannot match is a fact nobody can ask about.
	assert.True(t, capability.Has(stored, "pkg"))
	assert.False(t, capability.Has(stored, "file"))
}

// TestServeClusterJoinWithoutCapabilitiesKeepsToday keeps the additive change
// additive: a deployment that never hears about the new flag stores the column's
// empty value, exactly as it did before the flag existed.
func TestServeClusterJoinWithoutCapabilitiesKeepsToday(t *testing.T) {
	dsn := pgScopedDSN(t)
	defer resetRootFlags()
	restoreServeOpts(t)
	restoreClusterOpts(t)

	nodeID := "serve-nocaps-" + time.Now().UTC().Format("150405.000")
	cfgPath, _ := writeStoreConfig(t, "postgres", dsn)
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)

	serveOptCluster = true
	serveOptPGDSN = dsn
	serveOptNodeID = nodeID
	serveOptNodeAddr = "10.0.9.8:9090"
	serveOptNodeCapabilities = nil

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, mgr, err := openServeStore(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = mgr.Stop(stopCtx)
		_ = store.Close()
	})

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var stored string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT capabilities FROM cluster_nodes WHERE id = $1`, nodeID).Scan(&stored))
	assert.Equal(t, "[]", stored)
}
