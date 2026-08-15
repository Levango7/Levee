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

// currentSchemaVersion is bumped whenever a forward migration is added.
const currentSchemaVersion = 1

// Migrate applies the embedded schema to the given database. It is idempotent:
// running it on an already-migrated database is a no-op. The schema_version
// table records the highest version applied so future migrations can skip
// already-applied steps.
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
	if applied >= currentSchemaVersion {
		// Already up to date.
		return nil
	}

	// Execute the embedded schema statement by statement. SQLite's exec
	// handles multiple statements but splitting explicitly yields clearer
	// errors when one statement fails.
	if err := execMultiStatement(ctx, db, schemaSQL); err != nil {
		return fmt.Errorf("state: apply schema: %w", err)
	}

	// Record the applied version. Use UPSERT so re-applying the same version
	// does not violate the primary key constraint.
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)
		 ON CONFLICT(version) DO UPDATE SET applied_at = excluded.applied_at`,
		currentSchemaVersion, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("state: record schema version: %w", err)
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

// execMultiStatement splits a script on semicolons and executes each non-empty
// statement individually. Comments (lines starting with --) are stripped
// conservatively: a line that begins with -- after trimming whitespace is
// dropped entirely. This is sufficient for the bundled schema.sql which never
// embeds -- inside string literals.
func execMultiStatement(ctx context.Context, db *sql.DB, script string) error {
	// Strip line comments first.
	lines := strings.Split(script, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			lines[i] = ""
		}
	}
	cleaned := strings.Join(lines, "\n")

	for _, stmt := range strings.Split(cleaned, ";") {
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
