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

	// Record the applied version. Use UPSERT so re-applying the same version
	// does not violate the primary key constraint.
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)
		 ON CONFLICT(version) DO UPDATE SET applied_at = excluded.applied_at`,
		currentSchemaVersion, time.Now().UTC(),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("state: record schema version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit schema transaction: %w", err)
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

// dbExecutor is satisfied by both *sql.DB and *sql.Tx; it lets
// execMultiStatement run against either a bare connection or a transaction.
type dbExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
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
