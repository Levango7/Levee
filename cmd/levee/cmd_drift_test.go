package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/drift"
)

// --- Command registration --------------------------------------------------

func TestDriftCmd_Registered(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"drift"})
	require.NoError(t, err)
	assert.NotNil(t, cmd)
	assert.Equal(t, "drift", cmd.Name())
}

func TestDriftCmd_Subcommands(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"drift"})
	require.NoError(t, err)

	subs := cmd.Commands()
	names := make([]string, 0, len(subs))
	for _, s := range subs {
		names = append(names, s.Name())
	}
	assert.Contains(t, names, "detect")
	assert.Contains(t, names, "baseline")
	assert.Contains(t, names, "schedule")
	assert.Contains(t, names, "report")
}

func TestDriftBaselineCmd_Subcommands(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"drift", "baseline"})
	require.NoError(t, err)

	subs := cmd.Commands()
	names := make([]string, 0, len(subs))
	for _, s := range subs {
		names = append(names, s.Name())
	}
	assert.Contains(t, names, "set")
	assert.Contains(t, names, "auto")
	assert.Contains(t, names, "list")
	assert.Contains(t, names, "show")
	assert.Contains(t, names, "delete")
}

func TestDriftScheduleCmd_Subcommands(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"drift", "schedule"})
	require.NoError(t, err)

	subs := cmd.Commands()
	names := make([]string, 0, len(subs))
	for _, s := range subs {
		names = append(names, s.Name())
	}
	assert.Contains(t, names, "add")
	assert.Contains(t, names, "list")
	assert.Contains(t, names, "remove")
	assert.Contains(t, names, "run")
}

// --- localFileProber -------------------------------------------------------

func TestLocalFileProber_FileCheck(t *testing.T) {
	// Create a temp file with known content.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.txt")
	content := "hello world"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	prober := newLocalFileProber()
	items, err := prober.Probe(nil, "localhost", []drift.Check{
		{Name: "test", Type: drift.CheckTypeFile, Path: path, ExpectedValue: content},
	})
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, content, items[0].ActualValue)
	assert.False(t, items[0].Drifted)
}

func TestLocalFileProber_FileDrift(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.txt")
	require.NoError(t, os.WriteFile(path, []byte("actual"), 0o644))

	prober := newLocalFileProber()
	items, err := prober.Probe(nil, "localhost", []drift.Check{
		{Name: "test", Type: drift.CheckTypeFile, Path: path, ExpectedValue: "expected"},
	})
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, "actual", items[0].ActualValue)
	// Drifted is determined by the detector, not the prober; the prober
	// just returns the actual value.
}

func TestLocalFileProber_FileNotFound(t *testing.T) {
	prober := newLocalFileProber()
	items, err := prober.Probe(nil, "localhost", []drift.Check{
		{Name: "test", Type: drift.CheckTypeFile, Path: "/nonexistent/path", ExpectedValue: "x"},
	})
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.True(t, items[0].Drifted)
	assert.Contains(t, items[0].Diff, "read file")
}

func TestLocalFileProber_UnsupportedType(t *testing.T) {
	prober := newLocalFileProber()
	items, err := prober.Probe(nil, "localhost", []drift.Check{
		{Name: "test", Type: drift.CheckTypeService, Path: "nginx", ExpectedValue: "active"},
	})
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.True(t, items[0].Drifted)
	assert.Contains(t, items[0].Diff, "not supported")
}

// --- loadBaselineYAML ------------------------------------------------------

func TestLoadBaselineYAML_ListFormat(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "baseline.yaml")
	content := `- check_name: nginx.conf
  type: file
  path: /etc/nginx/nginx.conf
  expected_value: "hash123"
- check_name: nginx service
  type: service
  path: nginx
  expected_value: active
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	items, err := loadBaselineYAML(path)
	require.NoError(t, err)
	assert.Len(t, items, 2)
	assert.Equal(t, "nginx.conf", items[0].CheckName)
	assert.Equal(t, drift.CheckTypeFile, items[0].Type)
	assert.Equal(t, "/etc/nginx/nginx.conf", items[0].Path)
	assert.Equal(t, "hash123", items[0].ExpectedValue)
}

func TestLoadBaselineYAML_WrapperFormat(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "baseline.yaml")
	content := `items:
  - check_name: test
    type: file
    path: /test
    expected_value: "1"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	items, err := loadBaselineYAML(path)
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, "test", items[0].CheckName)
}

func TestLoadBaselineYAML_NotFound(t *testing.T) {
	_, err := loadBaselineYAML("/nonexistent/file.yaml")
	assert.Error(t, err)
}

func TestLoadBaselineYAML_EmptyItems(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "baseline.yaml")
	require.NoError(t, os.WriteFile(path, []byte("items: []"), 0o644))

	_, err := loadBaselineYAML(path)
	assert.Error(t, err)
}

// --- driftDataDir ----------------------------------------------------------

func TestDriftDataDir(t *testing.T) {
	// This test verifies that driftDataDir creates the directory structure.
	// It depends on the config being loadable; skip if config is not available.
	dir, err := driftDataDir()
	if err != nil {
		t.Skipf("config not available: %v", err)
	}
	assert.NotEmpty(t, dir)
	assert.DirExists(t, dir)
}

// --- Output helpers --------------------------------------------------------

func TestBaselineToMap(t *testing.T) {
	b := &drift.Baseline{
		ID:        "bl-1",
		Host:      "web-01",
		CreatedAt: time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC),
		Items: []drift.BaselineItem{
			{CheckName: "a", Type: drift.CheckTypeFile, Path: "/a", ExpectedValue: "1"},
		},
	}
	m := baselineToMap(b)
	assert.Equal(t, "bl-1", m["id"])
	assert.Equal(t, "web-01", m["host"])
	assert.Len(t, m["items"], 1)
}

func TestJobToMap(t *testing.T) {
	j := &drift.DriftJob{
		ID:       "job-1",
		Name:     "test",
		CronExpr: "0 * * * *",
		Hosts:    []string{"web-01"},
		Enabled:  true,
	}
	m := jobToMap(j)
	assert.Equal(t, "job-1", m["id"])
	assert.Equal(t, "test", m["name"])
	assert.Equal(t, "0 * * * *", m["cron_expr"])
	assert.True(t, m["enabled"].(bool))
}

func TestReportToMap(t *testing.T) {
	r := &drift.DriftReport{
		ID:          "rpt-1",
		Timestamp:   time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC),
		TotalHosts:  2,
		TotalDrifts: 1,
		TotalChecks: 5,
		Summary:     map[string]int{"web-01": 1, "web-02": 0},
	}
	m := reportToMap(r)
	assert.Equal(t, "rpt-1", m["id"])
	assert.Equal(t, 2, m["total_hosts"])
	assert.Equal(t, 1, m["total_drifts"])
}

func TestTrendToMap(t *testing.T) {
	tr := &drift.DriftTrend{
		Host:           "web-01",
		TrendDirection: "increasing",
		AverageDrift:   3.5,
		Points: []drift.TrendPoint{
			{Timestamp: time.Now(), DriftCount: 2, HostCount: 1},
		},
	}
	m := trendToMap(tr)
	assert.Equal(t, "web-01", m["host"])
	assert.Equal(t, "increasing", m["trend_direction"])
	assert.Equal(t, 3.5, m["average_drift"])
}

// --- fileSnapshotSource ----------------------------------------------------

func TestFileSnapshotSource_NotFound(t *testing.T) {
	src := newFileSnapshotSource()
	_, err := src.ExtractItems("web-01", "run-nonexistent")
	assert.Error(t, err)
}
