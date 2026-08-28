// pg_registry.go persists cluster membership in the PostgreSQL cluster_nodes
// table so every node shares one view of the cluster. The in-process
// NodeRegistry stays as a fast local cache; the table is the source of truth
// for cross-node visibility.
//
// Lifecycle: Join upserts the node row, the health-check loop refreshes the
// heartbeat and marks peers whose heartbeat aged past the timeout as
// offline, and Leave deletes the row. Leader "election" is convergent rather
// than consensus-based: every node applies the same deterministic policy
// (smallest ID among active masters, then workers) to the same table view,
// so all nodes agree on the leader without a voting round.

package cluster

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// clusterSchemaSQL creates the coordination tables. It is idempotent and is
// applied by the ClusterManager before the first database operation, so the
// cluster package does not depend on the state package's schema migration.
const clusterSchemaSQL = `
CREATE TABLE IF NOT EXISTS cluster_nodes (
	id             TEXT PRIMARY KEY,
	address        TEXT NOT NULL,
	role           TEXT NOT NULL DEFAULT 'worker',
	status         TEXT NOT NULL DEFAULT 'active',
	last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	joined_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS cluster_locks (
	key           TEXT PRIMARY KEY,
	owner         TEXT NOT NULL,
	fence_token   BIGINT NOT NULL,
	acquired_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	lease_expires TIMESTAMPTZ NOT NULL
);

CREATE SEQUENCE IF NOT EXISTS cluster_locks_fence_seq;
`

// ensureClusterSchema applies clusterSchemaSQL. All statements are IF NOT
// EXISTS, so concurrent application by several nodes is safe.
func ensureClusterSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("cluster: ensure schema: nil db")
	}
	if _, err := db.ExecContext(ctx, clusterSchemaSQL); err != nil {
		return fmt.Errorf("cluster: ensure schema: %w", err)
	}
	return nil
}

// upsertNode inserts or refreshes a node row. On conflict the mutable fields
// are updated and the heartbeat stamped; joined_at is preserved.
func upsertNode(ctx context.Context, db *sql.DB, node Node) error {
	role := string(node.Role)
	if role == "" {
		role = string(RoleWorker)
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO cluster_nodes (id, address, role, status, last_heartbeat, joined_at)
VALUES ($1, $2, $3, 'active', NOW(), NOW())
ON CONFLICT (id) DO UPDATE
SET address = EXCLUDED.address,
    role = EXCLUDED.role,
    status = 'active',
    last_heartbeat = NOW()`,
		node.ID, node.Address, role)
	if err != nil {
		return fmt.Errorf("cluster: upsert node: %w", err)
	}
	return nil
}

// heartbeatNode refreshes the heartbeat of one node and revives it to
// active if it was marked offline. A missing row (e.g. after Leave) is not
// an error — the caller re-registers via upsertNode on the next Join.
func heartbeatNode(ctx context.Context, db *sql.DB, nodeID string) error {
	_, err := db.ExecContext(ctx, `
UPDATE cluster_nodes SET last_heartbeat = NOW(), status = 'active' WHERE id = $1`,
		nodeID)
	if err != nil {
		return fmt.Errorf("cluster: heartbeat node: %w", err)
	}
	return nil
}

// listNodes returns every node row, ordered by ID for deterministic
// processing across nodes.
func listNodes(ctx context.Context, db *sql.DB) ([]Node, error) {
	rows, err := db.QueryContext(ctx, `
SELECT id, address, role, status, last_heartbeat, joined_at
FROM cluster_nodes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("cluster: list nodes: %w", err)
	}
	defer rows.Close()

	var nodes []Node
	for rows.Next() {
		var n Node
		var role, status string
		if err := rows.Scan(&n.ID, &n.Address, &role, &status, &n.LastHeartbeat, &n.JoinedAt); err != nil {
			return nil, fmt.Errorf("cluster: list nodes: scan: %w", err)
		}
		n.Role = NodeRole(role)
		n.Status = NodeStatus(status)
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cluster: list nodes: rows: %w", err)
	}
	return nodes, nil
}

// markStaleNodes flips nodes whose heartbeat is older than maxAge to
// offline and returns the IDs flipped by THIS sweep. It is idempotent:
// already-offline nodes are not reported again.
func markStaleNodes(ctx context.Context, db *sql.DB, maxAge time.Duration) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
UPDATE cluster_nodes SET status = 'offline'
WHERE status <> 'offline' AND last_heartbeat < NOW() - ($1::bigint * INTERVAL '1 millisecond')
RETURNING id`,
		maxAge.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("cluster: mark stale: %w", err)
	}
	defer rows.Close()

	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("cluster: mark stale: scan: %w", err)
		}
		stale = append(stale, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cluster: mark stale: rows: %w", err)
	}
	return stale, nil
}

// deleteNode removes a node row (graceful leave). Missing rows are ignored.
func deleteNode(ctx context.Context, db *sql.DB, nodeID string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM cluster_nodes WHERE id = $1`, nodeID); err != nil {
		return fmt.Errorf("cluster: delete node: %w", err)
	}
	return nil
}
