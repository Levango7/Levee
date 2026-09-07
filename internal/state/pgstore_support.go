// pgstore_support.go contains private helpers used by pgstore.go. They are
// kept in a separate file so pgstore.go stays a 1:1 mirror of sqlite.go and
// reviewers can diff the two implementations column-by-column.
//
// All helpers are package-private; nothing here is part of the public Store
// surface.

package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgxParseConfig wraps pgx.ParseConfig so the import stays in this file only.
// It returns a *pgx.ConnConfig that stdlib.OpenDB can consume.
func pgxParseConfig(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyPGPoolConfig maps PGPoolConfig onto *sql.DB setters. Zero values are
// ignored so the database/sql defaults apply.
func applyPGPoolConfig(db *sql.DB, cfg PGPoolConfig) {
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	}
}

// pgJoinPlaceholders joins filter clauses (each containing a "$%d" template)
// into a single " AND "-separated string with sequential PostgreSQL
// placeholder numbers ($1, $2, ...). The clauses are processed in order and
// the placeholder index increments by 1 for each clause, matching the order
// in which the corresponding args were appended by the caller.
//
// Example:
//
//	clauses := []string{"status = $%d", "creator = $%d"}
//	pgJoinPlaceholders(clauses) // "status = $1 AND creator = $2"
func pgJoinPlaceholders(clauses []string) string {
	out := make([]string, len(clauses))
	for i, c := range clauses {
		out[i] = fmt.Sprintf(c, i+1)
	}
	return strings.Join(out, " AND ")
}

// pgMigrations lists the forward PostgreSQL upgrade steps, mirroring
// migrations on the SQLite side. Same shape rules apply: ascending, starting
// at pgBaseSchemaVersion+1, gapless, ending at pgCurrentSchemaVersion; and
// pgschema.sql gains the matching change so fresh databases land directly on
// pgCurrentSchemaVersion (see migrations for the full convention).
var pgMigrations = []migrationStep{
	{
		// SA-018: credentials.tags (see migrations on the SQLite side).
		// The statement is dialect-compatible; kept as its own table so a
		// future PG-only step (e.g. types, functions) can diverge freely.
		version: 2,
		stmts: []string{
			`ALTER TABLE credentials ADD COLUMN tags TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// A1 (engine wiring): runs.plan_json (see migrations on the
		// SQLite side). Statement is dialect-compatible.
		version: 3,
		stmts: []string{
			`ALTER TABLE runs ADD COLUMN plan_json TEXT NOT NULL DEFAULT ''`,
		},
	},
}

// pgSchemaDDLAdvisoryLockKey is the key of the session-level advisory lock
// that serialises all LEVEE schema DDL against one PostgreSQL database.
// Keep in sync with clusterSchemaDDLAdvisoryLockKey in
// internal/cluster/pg_registry.go.
const pgSchemaDDLAdvisoryLockKey int64 = 770_001

// pgMigrate applies the embedded PostgreSQL schema and any pending forward
// migrations to the given database. Idempotent; semantics mirror Migrate:
// fresh databases are built from pgschema.sql in one transaction and land on
// pgCurrentSchemaVersion, versioned databases run only the pending steps,
// each committed atomically with its version row. PostgreSQL DDL is fully
// transactional (including CREATE TRIGGER and function bodies), so every
// step is atomic.
//
// All DDL runs behind a session-level advisory lock: "CREATE ... IF NOT
// EXISTS" is not concurrency-safe (the existence check and the catalog
// insert are not atomic, so two sessions creating the same object abort on
// a catalog unique index such as pg_type_typname_nsp_index), several LEVEE
// nodes may share one database and migrate it at startup, and the cluster
// package applies overlapping objects (cluster_nodes) from another binary.
func pgMigrate(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("state: pg migrate: nil db handle")
	}

	// Reserve one connection and hold the advisory lock on it for the whole
	// migration; every statement below runs on that same connection so a
	// MaxOpenConns=1 pool cannot starve itself while the lock is held.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("state: pg migrate conn: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, pgSchemaDDLAdvisoryLockKey); err != nil {
		return fmt.Errorf("state: pg migrate advisory lock: %w", err)
	}
	// Unlock on a cancellation-proof context; even if the unlock itself
	// cannot run, closing the connection below releases the session lock.
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, pgSchemaDDLAdvisoryLockKey)
	}()

	// Ensure schema_version exists first so we can record progress even if
	// the very first run fails halfway through.
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("state: create pg schema_version: %w", err)
	}

	applied, err := pgAppliedSchemaVersion(ctx, conn)
	if err != nil {
		return fmt.Errorf("state: read pg schema version: %w", err)
	}
	// Mirrors Migrate: no fast path needed, the step loop is the no-op when
	// no step has a version above `applied`.

	if applied < pgBaseSchemaVersion {
		// Fresh database: build the complete schema from pgschema.sql inside
		// a single transaction. IF NOT EXISTS keeps the statements idempotent
		// for re-runs after a rolled-back attempt.
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("state: begin pg schema transaction: %w", err)
		}
		if err := pgExecMultiStatement(ctx, tx, pgSchemaSQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("state: apply pg schema: %w", err)
		}
		if err := pgRecordSchemaVersion(ctx, tx, pgCurrentSchemaVersion); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("state: commit pg schema transaction: %w", err)
		}
		return nil
	}

	// Upgrading database: one transaction per pending step.
	for _, step := range pgMigrations {
		if step.version <= applied {
			continue
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("state: begin pg migration v%d transaction: %w", step.version, err)
		}
		for _, stmt := range step.stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("state: apply pg migration v%d (%q): %w",
					step.version, firstLine(stmt), err)
			}
		}
		if err := pgRecordSchemaVersion(ctx, tx, step.version); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("state: commit pg migration v%d: %w", step.version, err)
		}
	}
	return nil
}

// pgRecordSchemaVersion upserts the applied version inside the given
// transaction. UPSERT so re-applying the same version does not violate the
// primary key constraint.
func pgRecordSchemaVersion(ctx context.Context, tx *sql.Tx, version int) error {
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES ($1, $2)
		 ON CONFLICT(version) DO UPDATE SET applied_at = EXCLUDED.applied_at`,
		version, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("state: record pg schema version %d: %w", version, err)
	}
	return nil
}

// pgAppliedSchemaVersion returns the highest version recorded in
// schema_version, or 0 if the table is empty.
func pgAppliedSchemaVersion(ctx context.Context, db dbExecutor) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("query max pg version: %w", err)
	}
	return version, nil
}

// pgExecMultiStatement splits a script on semicolons and executes each
// non-empty statement individually against the given executor (a *sql.DB or
// an open *sql.Tx). It handles PostgreSQL dollar-quoted
// function bodies ($$ ... $$) and BEGIN...END trigger blocks, neither of
// which can be split on a bare semicolon.
func pgExecMultiStatement(ctx context.Context, db dbExecutor, script string) error {
	// Strip line comments first.
	lines := strings.Split(script, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			lines[i] = ""
		}
	}
	cleaned := strings.Join(lines, "\n")

	statements := pgSplitSQLStatements(cleaned)
	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("exec pg statement %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

// pgSplitSQLStatements splits a PostgreSQL script into individual statements,
// respecting:
//  1. Dollar-quoted function bodies ($$ ... $$ or $tag$ ... $tag$).
//  2. BEGIN...END blocks inside CREATE FUNCTION / CREATE TRIGGER.
//
// A naive split on ";" would break both, so we walk the script character by
// character and only treat ";" as a separator when not inside a dollar-quote
// or BEGIN...END block.
func pgSplitSQLStatements(script string) []string {
	var statements []string
	var current strings.Builder

	inDollarQuote := false
	dollarTag := "" // empty means $$ ... $$
	inBeginEnd := 0 // nesting depth of BEGIN...END

	i := 0
	for i < len(script) {
		ch := script[i]

		// Detect start/end of dollar quote. A dollar quote is $tag$ ... $tag$
		// where tag is optional (e.g. $$). We scan forward to find the
		// matching closing tag.
		if ch == '$' {
			end := indexDollarQuoteEnd(script, i)
			if end > i {
				tag := script[i : end+1]
				if !inDollarQuote {
					inDollarQuote = true
					dollarTag = tag
					current.WriteString(tag)
					i = end + 1
					continue
				} else if tag == dollarTag {
					inDollarQuote = false
					dollarTag = ""
					current.WriteString(tag)
					i = end + 1
					continue
				}
			}
		}

		if inDollarQuote {
			current.WriteByte(ch)
			i++
			continue
		}

		// Track BEGIN...END nesting outside dollar quotes. We compare
		// case-insensitively on word boundaries.
		if ch == 'B' || ch == 'b' {
			if hasWordAt(script, i, "BEGIN") {
				inBeginEnd++
				current.WriteString(script[i : i+5])
				i += 5
				continue
			}
		}
		if ch == 'E' || ch == 'e' {
			if hasWordAt(script, i, "END") {
				if inBeginEnd > 0 {
					inBeginEnd--
				}
				current.WriteString(script[i : i+3])
				i += 3
				continue
			}
		}

		if ch == ';' && inBeginEnd == 0 {
			current.WriteByte(ch)
			statements = append(statements, current.String())
			current.Reset()
			i++
			continue
		}

		current.WriteByte(ch)
		i++
	}

	if remaining := strings.TrimSpace(current.String()); remaining != "" {
		statements = append(statements, remaining)
	}
	return statements
}

// indexDollarQuoteEnd returns the index of the last '$' of the opening dollar
// quote starting at start (e.g. for "$$" it returns start+1, for "$body$" it
// returns start+5). Returns -1 if no dollar quote starts here.
func indexDollarQuoteEnd(script string, start int) int {
	if start >= len(script) || script[start] != '$' {
		return -1
	}
	// Scan forward to the next '$'.
	for j := start + 1; j < len(script); j++ {
		if script[j] == '$' {
			return j
		}
		// Tag characters must be letters/digits/underscore.
		c := script[j]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return -1
	}
	return -1
}

// hasWordAt reports whether the case-insensitive word w starts at index i in
// script and is followed by a word boundary (non-letter/digit character or
// end of string).
func hasWordAt(script string, i int, w string) bool {
	if i+len(w) > len(script) {
		return false
	}
	for k := 0; k < len(w); k++ {
		sc := script[i+k]
		wc := w[k]
		if sc >= 'a' && sc <= 'z' {
			sc = sc - 32
		}
		if wc >= 'a' && wc <= 'z' {
			wc = wc - 32
		}
		if sc != wc {
			return false
		}
	}
	// Check word boundary.
	if i+len(w) < len(script) {
		next := script[i+len(w)]
		if (next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') || (next >= '0' && next <= '9') || next == '_' {
			return false
		}
	}
	return true
}
