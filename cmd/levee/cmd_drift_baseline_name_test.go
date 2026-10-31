package main

// Tests for named baseline versions on the CLI side: what lands on disk, what
// `detect` resolves, and the two ways this re-keying could silently destroy
// an operator's stored baselines.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/drift"
)

// driftBaselinesDir returns the on-disk baselines directory of e. The commands
// create it lazily, so tests that seed files write it themselves.
func driftBaselinesDir(t *testing.T, e *cliEnv) string {
	t.Helper()
	dir := filepath.Join(e.dataDir, "drift", "baselines")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	return dir
}

// writeStoredBaseline puts one baseline record at the given file stem, the way
// saveBaselinesToDisk would. A record without a name is what deployments
// written before named versions look like.
func writeStoredBaseline(t *testing.T, dir, stem string, b drift.Baseline) string {
	t.Helper()
	data, err := json.MarshalIndent(b, "", "  ")
	require.NoError(t, err)
	path := filepath.Join(dir, stem+".json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// listBaselineStems returns the *.json stems currently on disk.
func listBaselineStems(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var stems []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if name := entry.Name(); filepath.Ext(name) == ".json" {
			stems = append(stems, name[:len(name)-len(".json")])
		}
	}
	return stems
}

// TestDriftBaselineName_LegacyFileLoadsAsDefault is the upgrade contract: a
// baseline written before this feature has no name field, and it must arrive
// as the host's default version rather than vanishing or needing a migration.
func TestDriftBaselineName_LegacyFileLoadsAsDefault(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	dir := driftBaselinesDir(t, e)

	legacy := drift.Baseline{
		ID:          "bl-legacy",
		Host:        "web-01",
		SourceRunID: "run-old",
		Items:       []drift.BaselineItem{{CheckName: "cfg", Type: drift.CheckTypeFile, Path: "/etc/cfg", ExpectedValue: "hash123"}},
	}
	legacyPath := writeStoredBaseline(t, dir, "web-01", legacy)

	res, _, err := freshJSON(t, append([]string{"drift", "baseline", "show", "--host", "web-01", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok, "show should return one baseline")
	assert.Equal(t, drift.DefaultBaselineName, data["name"],
		"a stored record with no name is the default version")
	assert.Equal(t, "bl-legacy", data["id"], "the record itself is untouched")

	// Re-saving (here via a delete of another host's baseline, which rewrites
	// the whole directory) must keep the legacy file under the same stem —
	// the directory is reconciled by deleting what is not expected.
	mustRun(t, append([]string{"drift", "baseline", "set", "--host", "web-02",
		"--file", writeBaselineFile(t, filepath.Join(t.TempDir(), "ignored"), "x")}, cfg...)...)

	_, err = os.Stat(legacyPath)
	assert.NoError(t, err, "the legacy baseline file survived an unrelated write")

	out := mustRun(t, append([]string{"drift", "baseline", "show", "--host", "web-01"}, cfg...)...)
	assert.Contains(t, out, "Name: default")
}

// TestDriftBaselineName_TwoVersionsCoexistOnDisk is the feature's point, and
// the delete assertion is the data-loss guard: deleting one version must not
// remove the other file, because the same code path is what prunes stale
// files after a delete.
func TestDriftBaselineName_TwoVersionsCoexistOnDisk(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	dir := driftBaselinesDir(t, e)

	contentPath := filepath.Join(t.TempDir(), "nginx.conf")
	require.NoError(t, os.WriteFile(contentPath, []byte("hardened"), 0o644))

	mustRun(t, append([]string{"drift", "baseline", "set", "--host", "web-01",
		"--file", writeBaselineFile(t, contentPath, "hash123")}, cfg...)...)
	mustRun(t, append([]string{"drift", "baseline", "set", "--host", "web-01",
		"--baseline-name", "cis",
		"--file", writeBaselineFile(t, contentPath, "hardened")}, cfg...)...)

	stems := listBaselineStems(t, dir)
	assert.ElementsMatch(t, []string{"web-01", "web-01.cis"}, stems,
		"one file per version, and the default keeps the pre-naming stem")

	res, _, err := freshJSON(t, append([]string{"drift", "baseline", "list", "--host", "web-01", "--json"}, cfg...)...)
	require.NoError(t, err)
	rows, ok := res["data"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 2)
	names := map[string]bool{}
	for _, row := range rows {
		names[row.(map[string]any)["name"].(string)] = true
	}
	assert.True(t, names[drift.DefaultBaselineName] && names["cis"])

	// The two versions hold different expectations, so both are real records
	// rather than one row copied twice.
	got, err := os.ReadFile(filepath.Join(dir, "web-01.cis.json"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "hardened")

	// Delete the named version: the default must still be on disk.
	del, _, err := freshJSON(t, append([]string{"drift", "baseline", "delete", "--host", "web-01",
		"--baseline-name", "cis", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := del["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "cis", data["name"])
	assert.Equal(t, true, data["deleted"])
	assert.Equal(t, float64(1), data["remaining_for_host"],
		"the output says what is left, not only what was removed")

	assert.ElementsMatch(t, []string{"web-01"}, listBaselineStems(t, dir))
	err = runErr(t, append([]string{"drift", "baseline", "show", "--host", "web-01",
		"--baseline-name", "cis"}, cfg...)...)
	assert.Error(t, err, "the deleted version is gone")
}

// TestDriftBaselineName_DetectResolvesPinnedVersion proves the label is
// consumed by detection: the same host on the same disk reports drift against
// one version and clean against the other.
func TestDriftBaselineName_DetectResolvesPinnedVersion(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	_ = driftBaselinesDir(t, e)

	contentPath := filepath.Join(t.TempDir(), "nginx.conf")
	require.NoError(t, os.WriteFile(contentPath, []byte("hardened"), 0o644))

	mustRun(t, append([]string{"drift", "baseline", "set", "--host", "web-01",
		"--file", writeBaselineFile(t, contentPath, "hash123")}, cfg...)...)
	mustRun(t, append([]string{"drift", "baseline", "set", "--host", "web-01",
		"--baseline-name", "cis",
		"--file", writeBaselineFile(t, contentPath, "hardened")}, cfg...)...)

	res, _, err := freshJSON(t, append([]string{"drift", "detect", "--host", "web-01", "--json"}, cfg...)...)
	require.NoError(t, err)
	assert.Equal(t, float64(1), firstResultDriftCount(t, res),
		"the default version does not match the host")

	res, _, err = freshJSON(t, append([]string{"drift", "detect", "--host", "web-01",
		"--baseline-name", "cis", "--json"}, cfg...)...)
	require.NoError(t, err)
	assert.Equal(t, float64(0), firstResultDriftCount(t, res),
		"the pinned version matches the host")

	// A name that was never stored is a refusal that says what the host has.
	err = runErr(t, append([]string{"drift", "detect", "--host", "web-01",
		"--baseline-name", "soc-cis"}, cfg...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cis", "the not-found error lists the versions that do exist")
}

func firstResultDriftCount(t *testing.T, res map[string]any) float64 {
	t.Helper()
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	results, ok := data["results"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, results, "detect should report the one host")
	first, ok := results[0].(map[string]any)
	require.True(t, ok)
	return first["drift_count"].(float64)
}

// TestDriftBaselineName_FileSourceRejectsPin: a YAML path is a complete
// baseline, so a version label beside it would be silently dropped.
func TestDriftBaselineName_FileSourceRejectsPin(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	contentPath := filepath.Join(t.TempDir(), "nginx.conf")
	require.NoError(t, os.WriteFile(contentPath, []byte("hardened"), 0o644))
	baselineFile := writeBaselineFile(t, contentPath, "hardened")

	out := mustRun(t, append([]string{"drift", "detect", "--host", "web-01",
		"--baseline", baselineFile}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))

	err := runErr(t, append([]string{"drift", "detect", "--host", "web-01",
		"--baseline", baselineFile, "--baseline-name", "cis"}, cfg...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--baseline-name applies only to --baseline auto")
}

// TestDriftBaselineName_InvalidLabelRefusedAtWriteTime keeps a name that could
// never resolve off the disk: neither a baseline nor a job may store one.
func TestDriftBaselineName_InvalidLabelRefusedAtWriteTime(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	dir := driftBaselinesDir(t, e)

	contentPath := filepath.Join(t.TempDir(), "nginx.conf")
	require.NoError(t, os.WriteFile(contentPath, []byte("hardened"), 0o644))
	baselineFile := writeBaselineFile(t, contentPath, "hardened")

	for _, bad := range []string{"CIS", "cis 2026", "a/b", "prefix/../escape"} {
		err := runErr(t, append([]string{"drift", "baseline", "set", "--host", "web-01",
			"--file", baselineFile, "--baseline-name", bad}, cfg...)...)
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "invalid baseline name", bad)
	}
	assert.Empty(t, listBaselineStems(t, dir), "a refused label stores nothing")

	err := runErr(t, append([]string{"drift", "schedule", "add", "--name", "nightly",
		"--cron", "0 2 * * *", "--hosts", "web-01", "--baseline-name", "CIS"}, cfg...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid baseline name")

	jobsDir := filepath.Join(e.dataDir, "drift", "jobs")
	_, err = os.Stat(jobsDir)
	if err == nil {
		entries, readErr := os.ReadDir(jobsDir)
		require.NoError(t, readErr)
		assert.Empty(t, entries, "a job whose baseline pin can never resolve is not persisted")
	}
}

// TestDriftScheduleAdd_PinnedVersionRoundTrips keeps the pin in the job file
// and in the JSON output, and pins the empty-means-default reading of jobs
// written before the field existed.
func TestDriftScheduleAdd_PinnedVersionRoundTrips(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	out := mustRun(t, append([]string{"drift", "schedule", "add", "--name", "cis-nightly",
		"--cron", "0 2 * * *", "--hosts", "web-01", "--baseline-name", "cis"}, cfg...)...)
	assert.Contains(t, out, "Baseline version: cis")

	res, _, err := freshJSON(t, append([]string{"drift", "schedule", "list", "--json"}, cfg...)...)
	require.NoError(t, err)
	rows, ok := res["data"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 1)
	assert.Equal(t, "cis", rows[0].(map[string]any)["baseline_name"])

	// A job file predating the field loads with no pin, which the scheduler
	// resolves to the default version (asserted in internal/drift).
	jobsDir := filepath.Join(e.dataDir, "drift", "jobs")
	require.NoError(t, os.MkdirAll(jobsDir, 0o750))
	legacy, err := json.Marshal(drift.DriftJob{
		ID: "job-legacy", Name: "legacy", CronExpr: "0 3 * * *",
		Hosts: []string{"web-02"}, Enabled: true,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(jobsDir, "job-legacy.json"), legacy, 0o600))

	res, _, err = freshJSON(t, append([]string{"drift", "schedule", "list", "--json"}, cfg...)...)
	require.NoError(t, err)
	rows, ok = res["data"].([]any)
	require.True(t, ok)
	var found bool
	for _, row := range rows {
		if row.(map[string]any)["id"] == "job-legacy" {
			found = true
			assert.Empty(t, row.(map[string]any)["baseline_name"],
				"the output reports the stored value, not an invented default")
		}
	}
	assert.True(t, found, "the legacy job still loads")
}

// TestDriftBaselineStemCollision_RefusesWholeWrite covers the one way the
// (host, name) → file mapping is not injective. syncJSONDir deletes every file
// it was not asked to keep, so two versions that map to one stem would not
// just overwrite each other — the loser would be pruned. The write is refused
// before anything is written or removed.
func TestDriftBaselineStemCollision_RefusesWholeWrite(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	dir := driftBaselinesDir(t, e)

	item := drift.BaselineItem{CheckName: "cfg", Type: drift.CheckTypeFile, Path: "/etc/cfg", ExpectedValue: "v"}
	// Both records claim the stem "web-01.prod".
	a := writeStoredBaseline(t, dir, "web-01.prod", drift.Baseline{
		ID: "bl-a", Host: "web-01", Name: "prod", Items: []drift.BaselineItem{item},
	})
	b := writeStoredBaseline(t, dir, "hand-edited", drift.Baseline{
		ID: "bl-b", Host: "web-01.prod", Items: []drift.BaselineItem{item},
	})

	// An unrelated write on a third host has to fail too: the collision is in
	// the set being saved, and saving is the whole directory.
	err := runErr(t, append([]string{"drift", "baseline", "set", "--host", "web-09",
		"--file", writeBaselineFile(t, filepath.Join(t.TempDir(), "ignored"), "x")}, cfg...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both map to file")

	for _, path := range []string{a, b} {
		_, statErr := os.Stat(path)
		assert.NoError(t, statErr, "%s must survive a refused write", path)
	}
	assert.NotContains(t, listBaselineStems(t, dir), "web-09", "nothing was written either")
}
