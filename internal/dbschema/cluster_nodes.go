// Package dbschema holds the DDL fragments that more than one package must
// apply, so "more than one" never means "more than one copy".
//
// It is a leaf package on purpose: internal/state and internal/cluster both
// consume it and neither may come to depend on the other (the cluster package
// documents that it must not rely on the state package's migration path, and
// state must keep working with clustering disabled).
package dbschema

// ClusterNodesDDL is the single definition of the cluster membership table.
//
// It lives here because both internal/state (pgschema.sql) and
// internal/cluster (pg_registry.go) create it: the table is written by the
// cluster registry and read by the state store's cluster-status view. Both
// sites apply it with CREATE TABLE IF NOT EXISTS, which means whichever runs
// first on a given database decides the actual shape — the copies had already
// drifted (state carried a `capabilities` column and UNIQUE (address) that
// cluster did not, while cluster defaulted role / status / last_heartbeat that
// state left NOT NULL without a default).
//
// This version is the union of the two copies' columns, with every NOT NULL
// column defaulted so no writer can trip a missing default — EXCEPT
// UNIQUE (address), which only the state copy declared and which is now
// deliberately gone: node identity is `id`, and re-registering a listener
// address under a new node id is a legitimate move (it is exactly what the
// takeover suite does). Measured on a live PostgreSQL: keeping the constraint
// made ten internal/takeover cases fail with SQLSTATE 23505
// (cluster_nodes_address_key). The cluster copy — the shape that actually
// governed every cluster-first database — never had it.
//
// It changes nothing about databases that already exist (IF NOT EXISTS is a
// no-op there); it only guarantees that a freshly created database has one
// shape no matter which package got there first. State-first databases that
// predate this single-sourcing did carry the constraint, and PostgreSQL
// schema v7 drops it — the constraint plus the same-named index, idempotently
// (see pgMigrations in internal/state/pgstore_support.go), so address reuse no
// longer depends on which package created the table.
// Adding a column that production code actually reads requires a migration
// for existing databases too; TestClusterNodesSchemaIsSingleSourced guards
// against a second copy appearing in the meantime.
const ClusterNodesDDL = `
CREATE TABLE IF NOT EXISTS cluster_nodes (
    id              TEXT PRIMARY KEY,
    address         TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active',   -- active|leaving|offline
    role            TEXT NOT NULL DEFAULT 'worker',   -- master|worker
    last_heartbeat  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    capabilities    TEXT NOT NULL DEFAULT '{}',       -- JSON encoded
    joined_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_cluster_nodes_status ON cluster_nodes (status);
CREATE INDEX IF NOT EXISTS idx_cluster_nodes_role   ON cluster_nodes (role);
`
