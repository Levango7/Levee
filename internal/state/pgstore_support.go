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

// pgMigrate applies the embedded PostgreSQL schema to the given database. It
// is idempotent: running it on an already-migrated database is a no-op. The
// schema_version table records the highest version applied so future
// migrations can skip already-applied steps.
func pgMigrate(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("state: pg migrate: nil db handle")
	}

	// Ensure schema_version exists first so we can record progress even if
	// the very first run fails halfway through.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("state: create pg schema_version: %w", err)
	}

	applied, err := pgAppliedSchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("state: read pg schema version: %w", err)
	}
	if applied >= pgCurrentSchemaVersion {
		return nil
	}

	// Execute the embedded schema statement by statement. PostgreSQL's
	// database/sql driver does not accept multiple statements in a single
	// Exec, so we split explicitly.
	if err := pgExecMultiStatement(ctx, db, pgSchemaSQL); err != nil {
		return fmt.Errorf("state: apply pg schema: %w", err)
	}

	// Record the applied version. Use UPSERT so re-applying the same version
	// does not violate the primary key constraint.
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES ($1, $2)
		 ON CONFLICT(version) DO UPDATE SET applied_at = EXCLUDED.applied_at`,
		pgCurrentSchemaVersion, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("state: record pg schema version: %w", err)
	}

	return nil
}

// pgAppliedSchemaVersion returns the highest version recorded in
// schema_version, or 0 if the table is empty.
func pgAppliedSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("query max pg version: %w", err)
	}
	return version, nil
}

// pgExecMultiStatement splits a script on semicolons and executes each
// non-empty statement individually. It handles PostgreSQL dollar-quoted
// function bodies ($$ ... $$) and BEGIN...END trigger blocks, neither of
// which can be split on a bare semicolon.
func pgExecMultiStatement(ctx context.Context, db *sql.DB, script string) error {
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
