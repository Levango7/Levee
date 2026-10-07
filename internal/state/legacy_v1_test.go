package state

// legacy_v1_test.go provides a faithful SQLite fixture for a database written
// by the initial release (schema version 1).
//
// Why this exists: the multi-tenancy v6 step is the first migration that
// touches every tenant-owned table, so the legacy fixtures in the migration
// tests must contain all of them. Earlier hand-built fixtures declared only
// the handful of tables their own step happened to alter (credentials, runs,
// approvals) and got away with it purely because v2-v5 never referenced
// anything else. A fixture that is not a real v1 database cannot exercise a
// step that is a real v1→v6 upgrade, so this one is deliberately complete.
//
// The v1 shape here is derived from schema.sql by removing exactly what later
// steps add:
//   - credentials.tags        (v2)
//   - runs.plan_json          (v3)
//   - run_assignment table    (v4, created by the step, so absent at v1)
//   - approvals.plan_hash     (v5)
//   - approvals.revision      (v5)
//   - tenant_id on all tables (v6)
//   - audit prev/curr_hash    (v7)
//   - agents table            (v8, created by the step, so absent at v1)
//
// The v1 WORM triggers are reproduced WITHOUT tenant_id on purpose: that is
// the state a real v1 database is in, and it is what forces the v6 step to
// drop and recreate them. A fixture that already had the new trigger would
// silently skip the one statement most worth testing.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyV1DDL is the complete v1-era schema, in dependency order.
const legacyV1DDL = `
CREATE TABLE schema_version (
	version    INTEGER PRIMARY KEY,
	applied_at DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE runs (
	id              TEXT    PRIMARY KEY,
	workflow_name   TEXT    NOT NULL,
	template_name   TEXT    NOT NULL,
	params          TEXT    NOT NULL DEFAULT '{}',
	plan_hash       TEXT    NOT NULL,
	status          TEXT    NOT NULL,
	approval_status TEXT    NOT NULL DEFAULT 'pending',
	approval_level  TEXT    NOT NULL DEFAULT '',
	created_at      DATETIME NOT NULL,
	updated_at      DATETIME NOT NULL,
	creator         TEXT    NOT NULL,
	incident_id     TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE batches (
	id            TEXT    PRIMARY KEY,
	run_id        TEXT    NOT NULL,
	batch_no      INTEGER NOT NULL,
	status        TEXT    NOT NULL,
	total_hosts   INTEGER NOT NULL DEFAULT 0,
	succeeded     INTEGER NOT NULL DEFAULT 0,
	failed        INTEGER NOT NULL DEFAULT 0,
	started_at    DATETIME,
	completed_at  DATETIME,
	FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE CASCADE,
	UNIQUE (run_id, batch_no)
);

CREATE TABLE steps (
	id            TEXT    PRIMARY KEY,
	run_id        TEXT    NOT NULL,
	batch_id      TEXT    NOT NULL,
	host          TEXT    NOT NULL,
	step_name     TEXT    NOT NULL,
	action        TEXT    NOT NULL,
	status        TEXT    NOT NULL,
	exit_code     INTEGER,
	stdout        TEXT    NOT NULL DEFAULT '',
	stderr        TEXT    NOT NULL DEFAULT '',
	duration_ms   INTEGER NOT NULL DEFAULT 0,
	started_at    DATETIME,
	completed_at  DATETIME,
	FOREIGN KEY (run_id)   REFERENCES runs    (id) ON DELETE CASCADE,
	FOREIGN KEY (batch_id) REFERENCES batches (id) ON DELETE CASCADE
);

CREATE TABLE trace (
	id         TEXT    PRIMARY KEY,
	run_id     TEXT    NOT NULL,
	event      TEXT    NOT NULL,
	actor      TEXT    NOT NULL,
	detail     TEXT    NOT NULL DEFAULT '{}',
	prev_hash  TEXT    NOT NULL DEFAULT '',
	curr_hash  TEXT    NOT NULL,
	timestamp  DATETIME NOT NULL,
	FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE CASCADE
);

CREATE TRIGGER IF NOT EXISTS worm_prevent_trace_update
BEFORE UPDATE ON trace
WHEN NEW.id != OLD.id
  OR NEW.run_id != OLD.run_id
  OR NEW.event != OLD.event
  OR NEW.actor != OLD.actor
  OR NEW.detail != OLD.detail
  OR NEW.timestamp != OLD.timestamp
BEGIN
    SELECT RAISE(ABORT, 'WORM violation: trace content fields cannot be updated');
END;

CREATE TRIGGER IF NOT EXISTS worm_prevent_trace_delete
BEFORE DELETE ON trace
BEGIN
    SELECT RAISE(ABORT, 'WORM violation: trace records cannot be deleted');
END;

CREATE TABLE approvals (
	id         TEXT    PRIMARY KEY,
	run_id     TEXT    NOT NULL,
	level      TEXT    NOT NULL,
	approver   TEXT    NOT NULL,
	status     TEXT    NOT NULL,
	comment    TEXT    NOT NULL DEFAULT '',
	timeout_at DATETIME,
	acted_at   DATETIME,
	FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE CASCADE
);

CREATE TABLE locks (
	id           TEXT    PRIMARY KEY,
	scope        TEXT    NOT NULL,
	owner        TEXT    NOT NULL,
	ttl_seconds  INTEGER NOT NULL,
	acquired_at  DATETIME NOT NULL,
	expires_at   DATETIME NOT NULL,
	UNIQUE (scope)
);

CREATE TABLE credentials (
	id             TEXT    PRIMARY KEY,
	name           TEXT    NOT NULL,
	type           TEXT    NOT NULL,
	encrypted_data BLOB    NOT NULL,
	created_at     DATETIME NOT NULL,
	rotated_at     DATETIME,
	UNIQUE (name)
);

CREATE TABLE audit (
	id         TEXT    PRIMARY KEY,
	run_id     TEXT    NOT NULL DEFAULT '',
	action     TEXT    NOT NULL,
	actor      TEXT    NOT NULL,
	target     TEXT    NOT NULL DEFAULT '',
	result     TEXT    NOT NULL,
	timestamp  DATETIME NOT NULL
);

CREATE TABLE inventory_groups (
	id         TEXT    PRIMARY KEY,
	name       TEXT    NOT NULL UNIQUE,
	parent_id  TEXT,
	created_at TIMESTAMP NOT NULL
);

CREATE TABLE targets (
	id              TEXT    PRIMARY KEY,
	hostname        TEXT    NOT NULL,
	port            INTEGER NOT NULL DEFAULT 22,
	channel_type    TEXT    NOT NULL DEFAULT 'ssh',
	credential_ref  TEXT    NOT NULL DEFAULT '',
	labels          TEXT    NOT NULL DEFAULT '{}',
	group_id        TEXT,
	status          TEXT    NOT NULL DEFAULT 'active',
	reachable       INTEGER NOT NULL DEFAULT 0,
	last_checked_at TIMESTAMP,
	created_at      TIMESTAMP NOT NULL,
	UNIQUE (hostname, port),
	FOREIGN KEY (group_id) REFERENCES inventory_groups (id) ON DELETE SET NULL
);
`

// createLegacyV1DB builds a complete v1-era database on path and records it as
// being exactly at schema version 1, so opening a store against it replays
// every pending forward step (2, 3, 4, 5, 6).
func createLegacyV1DB(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(ctx, legacyV1DDL)
	require.NoError(t, err, "legacy v1 DDL must apply")
	_, err = db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (1)`)
	require.NoError(t, err)
}

// tenantOwnedTables lists every table the v6 step adds tenant_id to. It is
// deliberately explicit rather than derived: the whole point of the
// fresh-vs-upgraded shape test is that someone adding a table has to think
// about whether it is tenant-owned, so the list is the checklist.
var tenantOwnedTables = []string{
	"runs", "batches", "steps", "trace", "approvals", "audit",
	"credentials", "inventory_groups", "targets", "run_assignment",
}

// nonTenantOwnedTables are platform-scoped on purpose; see the note in
// schema.sql. Listing them here makes that decision testable — if someone
// "helpfully" adds tenant_id to locks, this fails.
//
// agents is here for the same reason as locks: one agent is one process
// serving the whole deployment, so there is no tenant to scope its record by
// (state.Agent has no tenant field at all). It is also the first table added
// by a step that CREATEs rather than ALTERs since run_assignment (v4), so
// listing it is what puts the fresh-vs-upgraded shape comparison on the v8
// CREATE TABLE text — the step and schema.sql must agree column-for-column.
var nonTenantOwnedTables = []string{"locks", "schema_version", "agents"}
