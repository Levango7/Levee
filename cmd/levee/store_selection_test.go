// store_selection_test.go covers the single place the CLI decides which database
// to open (helpers.go openStoreFromConfig) and the consumers that now go through
// it: ordinary commands, `serve` without --cluster, `system status`/`doctor`, and
// the change calendar.
//
// The properties these tests exist to keep are "the configured backend is the
// one used" and "a deployment is never split across two databases by accident".
// Both are tested through what an operator can observe — whether a SQLite file
// got created, what an error names, what the notice prints — because a store
// choice that silently degrades to a local file leaves every command reporting
// success against a database the server never reads.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/calendar"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/tenant"
)

// unreachablePGDSN dials a port nothing listens on; connect_timeout keeps the
// test fast. The point is the shape of the failure, not a live server.
const unreachablePGDSN = "postgres://levee:s3cr3t@127.0.0.1:1/levee?sslmode=disable&connect_timeout=1"

// writeStoreConfig writes a config YAML naming the given driver and returns the
// config path plus the SQLite path postProcess derives from data_dir.
func writeStoreConfig(t *testing.T, driver, dsn string) (cfgPath, dbPath string) {
	t.Helper()
	dataDir := t.TempDir()
	dbPath = filepath.Join(dataDir, "levee.db")
	cfgPath = filepath.Join(t.TempDir(), "config.yaml")
	content := "server:\n  data_dir: '" + dataDir + "'\ndatabase:\n  driver: '" + driver + "'\n"
	if dsn != "" {
		content += "  dsn: '" + dsn + "'\n"
	}
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))
	return cfgPath, dbPath
}

// TestOpenStorePostgresConfigOpensPostgresNotSQLite is the regression for the
// split deployment: a config that says postgres must never end up with a local
// SQLite file. The file's absence is the assertion that matters — opening SQLite
// would create it — and the error must name PostgreSQL, because the DSN points at
// a port nothing listens on.
func TestOpenStorePostgresConfigOpensPostgresNotSQLite(t *testing.T) {
	defer resetRootFlags()
	cfgPath, dbPath := writeStoreConfig(t, "postgres", unreachablePGDSN)
	optConfigPath = cfgPath

	store, err := openStore(context.Background())
	require.Error(t, err, "an unreachable PostgreSQL must fail, not be replaced by a local file")
	assert.Nil(t, store)
	assert.Contains(t, err.Error(), "postgres")
	assert.NotContains(t, err.Error(), "s3cr3t", "the DSN carries a password and error strings reach stderr")
	_, statErr := os.Stat(dbPath)
	assert.True(t, os.IsNotExist(statErr),
		"%s was created although the config names postgres: that is the split deployment this function exists to prevent", dbPath)
}

func TestOpenStoreFromConfigRejectsUnknownDriver(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Server.DataDir = dir
	cfg.Database.Driver = "cockroachdb"
	cfg.Database.Path = filepath.Join(dir, "levee.db")

	store, err := openStoreFromConfig(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cockroachdb")
	assert.Nil(t, store)
	_, statErr := os.Stat(cfg.Database.Path)
	assert.True(t, os.IsNotExist(statErr), "an unknown driver must not be answered with SQLite")
}

func TestOpenStoreFromConfigSQLiteNamesItsBackend(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Server.DataDir = dir
	cfg.Database.Driver = "sqlite"
	cfg.Database.Path = filepath.Join(dir, "levee.db")

	store, err := openStoreFromConfig(context.Background(), cfg)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	assert.Equal(t, state.DriverSQLite, state.StoreDriver(store))
	_, statErr := os.Stat(cfg.Database.Path)
	assert.NoError(t, statErr, "sqlite mode must open the configured file")
}

// TestCalendarForSeesThroughTheTenantDecorator is the multi-tenant hole in the
// freeze gate. With tenant.enabled the handle every request-serving service holds
// is a tenant.TenantStore around the real store, and matching the wrapper instead
// of the database behind it made calendarFor return "unknown state store type",
// which serve logs and then plans without any freeze enforcement. A governance
// control that turns itself off when isolation is switched on is the failure this
// pins shut.
func TestCalendarForSeesThroughTheTenantDecorator(t *testing.T) {
	ctx := context.Background()
	base, err := state.NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "tenant-cal.db"))
	require.NoError(t, err)
	defer func() { _ = base.Close() }()

	wrapped := tenant.NewTenantStore(base, tenant.NewResolver(false), nil)
	require.Equal(t, state.DriverSQLite, state.StoreDriver(wrapped),
		"the decorator must not erase the backend the deployment runs on")

	svc, err := calendarFor(ctx, wrapped)
	require.NoError(t, err, "calendarFor refused a tenant-wrapped store")

	// Not merely "opened": a freeze written through the wrapped handle has to be
	// the verdict that handle reports, which is what makes the gate real.
	now := time.Now().UTC().Truncate(time.Second)
	err = svc.CreateWindow(ctx, &calendar.Window{
		ID: "cal-tenant-freeze", Name: "change freeze", TargetLabels: []string{"payments"},
		StartTime: now.Add(-time.Hour), EndTime: now.Add(time.Hour), IsFrozen: true,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	err = svc.AssertNotFrozen(ctx, []string{"payments"}, false)
	require.Error(t, err, "the wrapped store accepted the freeze but does not enforce it")
	assert.True(t, errors.Is(err, calendar.ErrFrozen), "got %v", err)
}

// restoreServeOpts puts the `serve` option globals back. They are package-level
// cobra targets, and no other test in this package writes them; a leaked
// serveOptCluster would silently move an unrelated serve test onto the cluster
// branch.
func restoreServeOpts(t *testing.T) {
	t.Helper()
	cluster, dsn, id, addr := serveOptCluster, serveOptPGDSN, serveOptNodeID, serveOptNodeAddr
	t.Cleanup(func() {
		serveOptCluster, serveOptPGDSN, serveOptNodeID, serveOptNodeAddr = cluster, dsn, id, addr
	})
}

func TestServeRefusesPGDSNWithoutCluster(t *testing.T) {
	defer resetRootFlags()
	restoreServeOpts(t)
	cfgPath, dbPath := writeStoreConfig(t, "sqlite", "")
	optConfigPath = cfgPath
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)

	serveOptPGDSN = unreachablePGDSN
	serveOptCluster = false
	store, mgr, err := openServeStore(context.Background(), cfg)
	require.Error(t, err, "--pg-dsn without --cluster must not silently open the local file")
	assert.Nil(t, store)
	assert.Nil(t, mgr)
	assert.Contains(t, err.Error(), "--cluster")
	assert.NotContains(t, err.Error(), "s3cr3t")
	_, statErr := os.Stat(dbPath)
	assert.True(t, os.IsNotExist(statErr), "the refusal must happen before any store is opened")
}

func TestServeRefusesTwoDifferentClusterDSNs(t *testing.T) {
	defer resetRootFlags()
	restoreServeOpts(t)
	cfgPath, _ := writeStoreConfig(t, "postgres", "postgres://levee@127.0.0.1:1/otherdb?sslmode=disable&connect_timeout=1")
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)

	serveOptCluster = true
	serveOptNodeID = "n1"
	serveOptNodeAddr = "127.0.0.1:9090"
	serveOptPGDSN = "postgres://levee@127.0.0.1:1/levedb?sslmode=disable&connect_timeout=1"

	store, mgr, err := openServeStore(context.Background(), cfg)
	require.Error(t, err, "--pg-dsn and database.dsn name different servers; the calendar would be read from neither")
	assert.Nil(t, store)
	assert.Nil(t, mgr)
	assert.Contains(t, err.Error(), "database.dsn")
	assert.NotContains(t, err.Error(), "otherdb?sslmode", "the refusal names the endpoints, not the connection parameters")
}

// TestResolveBackupManagerFollowsConfiguredBackend keeps `levee backup` from
// copying a dead SQLite file for a PostgreSQL deployment and calling that a
// successful backup.
func TestResolveBackupManagerFollowsConfiguredBackend(t *testing.T) {
	defer resetRootFlags()
	t.Setenv("LEVEE_PG_DSN", "")
	cfgPath, dbPath := writeStoreConfig(t, "postgres", unreachablePGDSN)
	optConfigPath = cfgPath

	mgr, err := resolveBackupManager("")
	require.NoError(t, err)
	assert.Equal(t, "postgres", mgr.Driver(),
		"a postgres deployment has no local file to back up; the configured backend must win")
	assert.NotContains(t, mgr.SafeSource(), "s3cr3t")
	assert.NotContains(t, mgr.SafeSource(), dbPath)

	// The sqlite-configured profile still backs up its file, so the change above
	// is a driver decision and not a blanket refusal to use SQLite.
	resetRootFlags()
	sqliteCfgPath, sqliteDB := writeStoreConfig(t, "sqlite", "")
	optConfigPath = sqliteCfgPath
	mgr, err = resolveBackupManager("")
	require.NoError(t, err)
	assert.Equal(t, "sqlite", mgr.Driver())
	assert.Equal(t, sqliteDB, mgr.Source())
}

// TestCalendarFreezeNoticeNamesTheDatabaseItWrote covers the advisory an
// operator sees after `calendar create --frozen`: it has to say which database
// holds the row, because "enforcement applies to a server reading this same
// database" is only useful once you know what "this" is. The SQLite case carries
// the extra warning because a local file is where a freeze goes silently
// out-of-reach of a cluster server.
func TestCalendarFreezeNoticeNamesTheDatabaseItWrote(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	start := time.Now().UTC().Truncate(time.Second)
	end := start.Add(time.Hour)
	rfc := func(tm time.Time) string { return tm.Format(time.RFC3339) }

	stdout, stderr, err := runSplit(t, append([]string{"calendar", "create",
		"--name", "change-freeze", "--start", rfc(start), "--end", rfc(end),
		"--targets", "prod", "--frozen"}, cfg...)...)
	require.NoError(t, err)

	assert.Contains(t, stderr, "notice: freeze")
	assert.Contains(t, stderr, "sqlite "+e.dbPath,
		"the notice must name the driver and the exact file the row went into")
	assert.Contains(t, stderr, "a local file")
	assert.Contains(t, stdout, "Created window")
}

// TestSystemStatusReportsTheStoreItProbed pins the output contract of
// `levee system status` for a postgres profile: it must say postgres, must say
// the redacted endpoint, must not invent a db_path for a database that has no
// file, and must report the probe as unreachable rather than green.
func TestSystemStatusReportsTheStoreItProbed(t *testing.T) {
	defer resetRootFlags()
	cfgPath, dbPath := writeStoreConfig(t, "postgres", unreachablePGDSN)

	res, out, err := freshJSON(t, "system", "status", "--json", "--config", cfgPath)
	require.NoError(t, err, "status reports an unreachable store as a field, not a failure: %s", out)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok, "envelope has no data object: %s", out)

	assert.Equal(t, "unreachable", data["db_status"])
	assert.Equal(t, "postgres", data["db_driver"])
	assert.Equal(t, "postgres@127.0.0.1:1/levee", data["db_location"])
	assert.NotContains(t, data, "db_path",
		"a key called db_path must not carry a connection string")
	assert.NotContains(t, out, "s3cr3t")
	_, statErr := os.Stat(dbPath)
	assert.True(t, os.IsNotExist(statErr), "the status probe must not create the file it is not using")
}

// TestSystemConfigGetDSNIsNotVerbatim keeps the config-reader command honest.
// `system config get` prints whatever getConfigValue returns, so once
// database.dsn exists an answerable key that hands back a full connection
// string puts a credential on stdout, in the scrollback, and in whatever
// captured it — while the operator believes they are only inspecting
// configuration.
func TestSystemConfigGetDSNIsNotVerbatim(t *testing.T) {
	defer resetRootFlags()
	cfgPath, _ := writeStoreConfig(t, "postgres", unreachablePGDSN)

	stdout, stderr, err := runSplit(t, "system", "config", "get", "database.dsn", "--config", cfgPath)
	require.NoError(t, err, "reading one's own database endpoint must work: %s", stderr)
	assert.Contains(t, stdout, "postgres@127.0.0.1:1/levee")
	assert.NotContains(t, stdout, "s3cr3t")
	assert.NotContains(t, stdout, "sslmode", "connection parameters are not identity, and one of them may be a token")
}

// TestCLIHasOneSQLiteOpener is the structural half: the decision must have one
// implementation. A command that opens SQLite on its own would keep passing every
// local test and read the wrong database in the deployment.
func TestCLIHasOneSQLiteOpener(t *testing.T) {
	openers := map[string]int{}
	require.NoError(t, filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if n := strings.Count(string(data), "state.NewSQLiteStore("); n > 0 {
			openers[filepath.Base(path)] += n
		}
		return nil
	}))

	require.Contains(t, openers, "helpers.go", "openStoreFromConfig no longer opens SQLite")
	for file, n := range openers {
		assert.Equal(t, "helpers.go", file,
			"%s opens a SQLite store directly; the backend choice belongs in openStoreFromConfig", file)
		assert.Equal(t, 1, n, "%s opens SQLite more than once", file)
	}
}
