// Package state provides the SQLite-backed persistence layer for LEVEE.
// It stores runs, batches, steps, audit trace, approvals, locks, credentials
// and audit log entries. The schema is embedded at build time and applied
// automatically when a store is opened.
package state

import (
	"context"
	"database/sql"
	_ "embed" // required for go:embed
	"fmt"
	"strings"
	"time"
)

// schemaSQL holds the embedded schema.sql content.
//
//go:embed schema.sql
var schemaSQL string

// baseSchemaVersion is the version that schema.sql alone describes: the
// full schema as of the initial release, with no forward steps applied.
const baseSchemaVersion = 1

// currentSchemaVersion is bumped whenever a forward migration step is added
// to migrations. It must always equal the version of the highest step (or
// baseSchemaVersion when the list is empty), and schema.sql must be kept in
// sync so that a fresh database built from it lands on this version.
const currentSchemaVersion = 2

// migrationStep is one forward schema upgrade, identified by the version it
// brings the database TO. stmts are plain single DDL/DML statements executed
// in order inside a single transaction that also records the version row,
// so a step either applies fully or not at all.
type migrationStep struct {
	version int
	stmts   []string
}

// migrations lists the forward steps that take a database from a previously
// applied version up to currentSchemaVersion. It must stay sorted ascending,
// start at baseSchemaVersion+1, have no gaps, and end at currentSchemaVersion.
//
// CONVENTION (guarded by TestMigrationsTable_Shape): whenever a step is
// added here, schema.sql gains the same change so fresh databases are built
// directly at currentSchemaVersion and never replay upgrade steps. That is
// why SQLite-only-idiomatic statements (ALTER TABLE ... ADD COLUMN, which
// lacks an IF NOT EXISTS form) are safe: steps only run on databases that
// predate them.
var migrations = []migrationStep{
	{
		// SA-018: credentials.tags persists the operator metadata map as
		// JSON text ('' when absent). Only runs on databases built before
		// schema.sql gained the column; fresh databases get it from
		// schema.sql directly.
		version: 2,
		stmts: []string{
			`ALTER TABLE credentials ADD COLUMN tags TEXT NOT NULL DEFAULT ''`,
		},
	},
}

// Migrate applies the embedded schema and any pending forward migrations to
// the given database. It is idempotent: running it on an already-migrated
// database is a no-op.
//
// A fresh database (no schema_version rows) is built by replaying schema.sql
// in one transaction and lands directly on currentSchemaVersion. A database
// that already carries a recorded version only runs the migration steps with
// a higher version, each step committed atomically with its version row, so
// an interrupted upgrade resumes at the exact step boundary.
func Migrate(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("state: migrate: nil db handle")
	}

	// Ensure schema_version exists first so we can record progress even if
	// the very first run fails halfway through.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("state: create schema_version: %w", err)
	}

	applied, err := appliedSchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("state: read schema version: %w", err)
	}
	// No separate "already current" fast path is needed: an up-to-date
	// database simply has no steps with a higher version, so the step loop
	// below is the no-op.

	if applied < baseSchemaVersion {
		// Fresh database: build the complete base schema from schema.sql.
		// Execute the embedded schema inside a single transaction so a failure
		// halfway through cannot leave a half-applied schema behind. SQLite's
		// modernc driver fully supports transactional DDL. IF NOT EXISTS keeps
		// the statements idempotent so re-running after a rolled-back attempt
		// (or on an already-migrated database) is safe.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("state: begin schema transaction: %w", err)
		}
		if err := execMultiStatement(ctx, tx, schemaSQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("state: apply schema: %w", err)
		}
		// schema.sql is maintained at the current shape, so a fresh database
		// lands on currentSchemaVersion without replaying upgrade steps.
		if err := recordSchemaVersion(ctx, tx, currentSchemaVersion); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("state: commit schema transaction: %w", err)
		}
		return nil
	}

	// Upgrading database: run each pending step in ascending order, one
	// transaction per step, so a crash resumes at the step boundary rather
	// than redoing (or skipping) whole steps.
	for _, step := range migrations {
		if step.version <= applied {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("state: begin migration v%d transaction: %w", step.version, err)
		}
		for _, stmt := range step.stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("state: apply migration v%d (%q): %w",
					step.version, firstLine(stmt), err)
			}
		}
		if err := recordSchemaVersion(ctx, tx, step.version); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("state: commit migration v%d: %w", step.version, err)
		}
	}
	return nil
}

// recordSchemaVersion upserts the applied schema version inside the given
// transaction. UPSERT so re-applying the same version does not violate the
// primary key constraint.
func recordSchemaVersion(ctx context.Context, tx *sql.Tx, version int) error {
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)
		 ON CONFLICT(version) DO UPDATE SET applied_at = excluded.applied_at`,
		version, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("state: record schema version %d: %w", version, err)
	}
	return nil
}

// appliedSchemaVersion returns the highest version recorded in schema_version,
// or 0 if the table is empty.
func appliedSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("query max version: %w", err)
	}
	return version, nil
}

// dbExecutor is satisfied by *sql.DB, *sql.Tx and *sql.Conn; it lets
// execMultiStatement run against a pooled handle, a reserved connection or
// a transaction.
type dbExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// execMultiStatement splits a script on semicolons and executes each non-empty
// statement individually against the given executor (a *sql.DB or an open
// *sql.Tx). Comments (lines starting with --) are stripped
// conservatively: a line that begins with -- after trimming whitespace is
// dropped entirely. This is sufficient for the bundled schema.sql which never
// embeds -- inside string literals.
//
// CREATE TRIGGER blocks (BEGIN...END) are kept as single statements because
// their bodies contain semicolons that must not be treated as statement
// separators.
func execMultiStatement(ctx context.Context, db dbExecutor, script string) error {
	// Strip line comments first.
	lines := strings.Split(script, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			lines[i] = ""
		}
	}
	cleaned := strings.Join(lines, "\n")

	statements := splitSQLStatements(cleaned)
	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("exec statement %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

// splitSQLStatements splits a SQL script into individual statements, respecting
// BEGIN...END blocks used in CREATE TRIGGER definitions. A naive split on ";"
// would break trigger bodies that contain semicolons (e.g. SELECT RAISE(...));
// this function keeps such blocks intact.
func splitSQLStatements(script string) []string {
	var statements []string
	var current strings.Builder
	inTrigger := false

	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)

		// Detect the start of a trigger body.
		if !inTrigger && strings.Contains(upper, "BEGIN") {
			// Check if this is a CREATE TRIGGER ... BEGIN block.
			// Look for "CREATE TRIGGER" in the accumulated current statement.
			if strings.Contains(strings.ToUpper(current.String()), "CREATE TRIGGER") {
				inTrigger = true
			}
		}

		current.WriteString(line)
		current.WriteString("\n")

		if inTrigger {
			// The trigger body ends with a line that is just "END" (possibly
			// followed by a semicolon).
			if upper == "END;" || upper == "END" {
				inTrigger = false
				statements = append(statements, current.String())
				current.Reset()
			}
		} else {
			// Outside a trigger, a trailing semicolon on this line ends the
			// statement.
			if strings.HasSuffix(trimmed, ";") {
				statements = append(statements, current.String())
				current.Reset()
			}
		}
	}

	// Handle any remaining text (unlikely in well-formed SQL).
	if remaining := strings.TrimSpace(current.String()); remaining != "" {
		statements = append(statements, remaining)
	}

	return statements
}

// firstLine returns the first non-empty line of a statement, used to make
// error messages readable without dumping the whole statement.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return s
}
