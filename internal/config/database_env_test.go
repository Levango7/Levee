package config

// database_env_test.go — a container must be able to name its database without
// putting the credential in a ConfigMap.
//
// `database.dsn` was the one database leaf missing from the LEVEE_* binding
// table, so `LEVEE_DATABASE_DSN` was silently unreachable: bindEnv only surfaces
// keys explicitly bound (viper's AllSettings, see the comment on bindEnv), and
// the operator's alternative was to write the DSN into the rendered config file —
// a plaintext credential in a ConfigMap, readable by anyone who can `kubectl get
// cm -o yaml`. That is the same leak class this repository already fixed for
// `levee backup` stdout and `system config get database.dsn`.

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAllKeys_CoversDatabase enumerates the DatabaseConfig fields through
// reflect instead of a remembered list, so a fifth field added to the struct
// cannot be left out of the binding table — that omission is exactly what hid
// `database.dsn`, and a hand-written want-list would have hidden it again.
func TestAllKeys_CoversDatabase(t *testing.T) {
	bound := map[string]bool{}
	for _, k := range allKeys() {
		bound[k] = true
	}

	rt := reflect.TypeOf(DatabaseConfig{})
	checked := 0
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("mapstructure")
		if tag == "" || tag == "-" {
			continue
		}
		key := "database." + tag
		checked++
		assert.True(t, bound[key], "%s must be bound to %s — a key absent from allKeys is "+
			"unreachable from the environment, which pushes operators toward putting "+
			"credentials in a ConfigMap", key, envNameFor(key))
	}
	assert.GreaterOrEqual(t, checked, 4,
		"the reflection must actually be reading fields; a struct tag change that made every "+
			"one invisible would turn this guard green without testing anything")
}

// TestDatabaseDSNFromEnvWinsOverFile is the behaviour the chart depends on: the
// ConfigMap carries the driver, the Secret supplies the DSN through the env, and
// the two combine into one valid configuration.
func TestDatabaseDSNFromEnvWinsOverFile(t *testing.T) {
	path := writeYAML(t, "database:\n  driver: postgres\n  path: \"\"\n")
	t.Setenv("LEVEE_DATABASE_DSN", "postgres://levee:from-env@db.internal:5432/levee?sslmode=require")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "postgres://levee:from-env@db.internal:5432/levee?sslmode=require", cfg.Database.DSN,
		"the bound env value must replace the file, not sit beside it")
	assert.NoError(t, Validate(cfg))
}

// TestDatabaseDriverFromEnvAlsoReachesUnmarshal guards the other half: a
// deployment that selects PostgreSQL purely from the environment must validate,
// because Validate reads the assembled Config, not the file.
func TestDatabaseDriverFromEnvAlsoReachesUnmarshal(t *testing.T) {
	path := writeYAML(t, "database:\n  driver: sqlite\n  path: /var/lib/levee/levee.db\n")
	t.Setenv("LEVEE_DATABASE_DRIVER", "postgres")
	t.Setenv("LEVEE_DATABASE_DSN", "postgres://levee:from-env@db.internal:5432/levee")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "postgres", cfg.Database.Driver)
	require.NoError(t, Validate(cfg))
}

// TestMissingDSNUnderPostgresFailsAtLoad is the fail-closed side: binding the
// DSN from the environment must not turn "no DSN" into a silent SQLite
// deployment — Load validates, and a driver with nothing to connect to is an
// error rather than a default.
func TestMissingDSNUnderPostgresFailsAtLoad(t *testing.T) {
	path := writeYAML(t, "database:\n  driver: postgres\n")

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database.dsn is required")
}

// TestDSNFromEnvUnderSqliteDriverIsRefused is why the chart needs no duplicate
// guard: naming a PostgreSQL credential while selecting SQLite is refused by the
// binary, so the deployment cannot quietly run against the wrong backend.
func TestDSNFromEnvUnderSqliteDriverIsRefused(t *testing.T) {
	path := writeYAML(t, "database:\n  driver: sqlite\n  path: /var/lib/levee/levee.db\n")
	t.Setenv("LEVEE_DATABASE_DSN", "postgres://levee:from-env@db.internal:5432/levee")

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database.dsn is only used when")
}
