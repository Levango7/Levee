package state

// Constraint-conflict mapping (SA-014 / C-7).
//
// The trace table enforces write-once semantics through the PRIMARY KEY on
// id: a duplicate CreateTrace surfaces as a driver-level constraint error.
// Callers (notably audit.WORMStore.Append) must be able to distinguish that
// expected rejection from genuine store failures, so both backends translate
// the conflict into the package sentinel ErrTraceExists instead of leaking
// driver error types upward.

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	moderncsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrTraceExists is returned by CreateTrace when a trace with the same id
// is already present. It wraps no driver-specific details; use errors.Is.
var ErrTraceExists = errors.New("state: trace already exists")

// pgUniqueViolation is PostgreSQL's SQLSTATE for unique_violation.
const pgUniqueViolation = "23505"

// isUniqueConstraint reports whether err is a UNIQUE/PRIMARY KEY constraint
// violation from either supported backend:
//   - SQLite (modernc.org/sqlite): *sqlite.Error with code
//     SQLITE_CONSTRAINT_PRIMARYKEY (1555, the observed code for a duplicate
//     trace id), SQLITE_CONSTRAINT_UNIQUE (2067, non-PK unique indexes), or
//     bare SQLITE_CONSTRAINT (19, when extended result codes are disabled).
//     The design's `Code&0xFF == 19` mask is deliberately narrowed: every
//     extended constraint subcode — including the FOREIGN KEY one — shares
//     the 19 base, so masking would misclassify an FK violation (e.g. a
//     trace for a missing run) as a duplicate id.
//   - PostgreSQL (pgx): *pgconn.PgError with Code == "23505".
//
// The driver's error is returned unwrapped by database/sql and stores wrap
// it with %w, so errors.As sees through. The literal
// "UNIQUE constraint failed" string check is a deliberate last resort for
// third-party wrappers that lose the typed error; it is never the primary
// path.
func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	var serr *moderncsqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		case sqlite3.SQLITE_CONSTRAINT,
			sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY,
			sqlite3.SQLITE_CONSTRAINT_UNIQUE:
			return true
		}
		return false
	}
	var perr *pgconn.PgError
	if errors.As(err, &perr) {
		return perr.Code == pgUniqueViolation
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
