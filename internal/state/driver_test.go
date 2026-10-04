// Tests for the store→driver mapping in driver.go. The interesting cases are
// the ones where a hand-written copy of this switch would have to guess: an
// unrecognised implementation and a nil store must not come back named.
package state

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notAStore satisfies Store by embedding the interface. Calling any method on
// it panics, which is exactly why StoreDriver must answer from the type alone.
type notAStore struct{ Store }

func TestStoreDriverNamesSQLite(t *testing.T) {
	store, err := NewSQLiteStore(context.Background(),
		filepath.Join(t.TempDir(), "driver.db"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	assert.Equal(t, DriverSQLite, StoreDriver(store))
}

func TestStoreDriverRefusesToNameAnUnknownImplementation(t *testing.T) {
	assert.Equal(t, "", StoreDriver(notAStore{}),
		"a store this package does not implement must not default to a backend name")
	assert.Equal(t, "", StoreDriver(nil))
	var typedNil Store
	assert.Equal(t, "", StoreDriver(typedNil))
}

func TestDriverNamesAreDistinct(t *testing.T) {
	require.NotEqual(t, DriverSQLite, DriverPostgres)
}

// The answer comes from the store's type and nothing else: a zero-value
// *SQLiteStore with no open handle is still sqlite. If a third backend is added
// to the constants without an arm in StoreDriver, the new store comes back ""
// and every status surface prints "unknown" for a deployment that is fine —
// which is the failure mode the empty return exists to make visible.
func TestStoreDriverDoesNotDependOnStoreState(t *testing.T) {
	assert.Equal(t, DriverSQLite, StoreDriver(&SQLiteStore{}))
}

func TestStoreDriverNamesPostgres(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()

	assert.Equal(t, DriverPostgres, StoreDriver(store),
		"a PGStore must never be reported as sqlite")
}

func TestRedactDSN(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{"user and password dropped", "postgres://levee:s3cr3t@db.internal:5432/levee?sslmode=require", "postgres@db.internal:5432/levee"},
		{"no user", "postgres://db.internal:5432/levee", "postgres@db.internal:5432/levee"},
		{"postgresql scheme kept", "postgresql://u:p@h:5433/d", "postgres@h:5433/d"},
		{"empty", "", "postgres (no DSN configured)"},
		// The keyword/value form carries password= in plain view and is not a
		// URL, so it is withheld whole instead of partially parsed.
		{"keyword form withheld", "host=db.internal user=levee password=s3cr3t dbname=levee", withheldDSN},
		{"junk withheld", "not a dsn at all", withheldDSN},
		{"other scheme withheld", "mysql://u:p@h:3306/d", withheldDSN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactDSN(tc.dsn)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "s3cr3t", "a redactor must never return the credential it was given")
			assert.NotContains(t, got, "sslmode", "query parameters are configuration detail, not identity")
		})
	}
}
