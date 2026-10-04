// driver.go names the two storage backends, reports which one a live store
// actually is, and describes a PostgreSQL DSN in a form that is safe to print.
//
// The names exist because three layers have to agree on them: `database.driver`
// in the configuration (internal/config validates it, `levee system status`
// prints it), the backup file's backend marker (internal/backup), and the store
// a process really holds. Reading the name off the *config* is the failure mode
// this file closes: `serve --cluster --pg-dsn …` opens a PGStore while
// `database.driver` keeps its "sqlite" default, so a status endpoint built from
// the config reported the wrong backend to whoever was deciding whether the
// deployment was healthy. The store cannot lie about its own type, so status
// asks the store.
package state

import (
	"net/url"
	"strings"
)

// The two supported backends. These are the canonical spellings;
// internal/config validates `database.driver` against them and internal/backup
// aliases them, so a new backend is added in one place.
const (
	// DriverSQLite names the embedded single-file backend (SQLiteStore).
	DriverSQLite = "sqlite"
	// DriverPostgres names the client/server backend (PGStore).
	DriverPostgres = "postgres"
)

// Unwrapper is implemented by a Store that decorates another Store.
// internal/tenant.TenantStore is the one in this repository: with
// `tenant.enabled: true` it becomes the handle every request-serving service
// holds, so anything that needs to know what is *underneath* it has to ask
// through this seam rather than type-switch on the decorator and give up.
type Unwrapper interface {
	Underlying() Store
}

// Underlying returns the innermost store, unwrapping decorators such as
// tenant.TenantStore. It stops at the first store that is not a decorator.
//
// The walk is bounded rather than checked for self-reference: comparing two
// Store values to detect a wrapper that returns itself can panic, because an
// interface holding an uncomparable value type does not support ==. A cycle
// therefore costs eight iterations and ends on whatever store the walk reached,
// which StoreDriver and calendarFor then report as "not a backend I recognise".
func Underlying(s Store) Store {
	for i := 0; i < maxStoreDepth; i++ {
		u, ok := s.(Unwrapper)
		if !ok {
			return s
		}
		inner := u.Underlying()
		if inner == nil {
			return s
		}
		s = inner
	}
	return s
}

// maxStoreDepth bounds Underlying's walk. One decorator deep is the deepest
// arrangement this repository has; anything beyond it is a bug worth stopping at
// rather than chasing.
const maxStoreDepth = 8

// StoreDriver reports the driver name of a live store, or "" when the store is
// nil or of a type this package does not recognise. Decorators are unwrapped
// first, so a multi-tenant deployment is still named by the database it runs on
// instead of reporting "unknown" because a wrapper sits in front.
//
// Empty is deliberately not defaulted to DriverSQLite. A store whose backend
// cannot be determined is a deployment whose status output would be a guess,
// and a guess printed as fact is what every consumer of this function exists to
// avoid; callers must surface "unknown" instead.
func StoreDriver(s Store) string {
	switch Underlying(s).(type) {
	case *SQLiteStore:
		return DriverSQLite
	case *PGStore:
		return DriverPostgres
	default:
		return ""
	}
}

// withheldDSN is what RedactDSN returns when it cannot be sure a string has no
// credentials in it.
const withheldDSN = "postgres (DSN withheld: not a parseable URL)"

// RedactDSN turns a PostgreSQL connection string into something safe to put on
// stdout, in a log line, in an error string or in an audit record: host, port
// and database survive; the user-info section and every query parameter
// (sslmode, application_name, search_path) are dropped.
//
// It lives next to the driver names because both answer the same question —
// "which database is this, in a form you may print" — and because anything that
// can open a DSN should be able to describe one without leaking it.
//
// Anything that is not a postgres URL is withheld whole rather than echoed.
// Keyword/value connection strings ("host=… password=…") carry the password in
// plain view, and a redactor that fails open prints it; failing closed costs the
// reader one detail they can already see in their own config file.
func RedactDSN(dsn string) string {
	if dsn == "" {
		return "postgres (no DSN configured)"
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return withheldDSN
	}
	out := "postgres@" + u.Host
	if db := strings.TrimPrefix(u.Path, "/"); db != "" {
		out += "/" + db
	}
	return out
}
