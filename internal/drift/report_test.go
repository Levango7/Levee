package drift

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- GenerateReport --------------------------------------------------------

func TestGenerateReport(t *testing.T) {
	results := []*DriftResult{
		{
			Host:        "web-01",
			Timestamp:   time.Now().UTC(),
			DriftCount:  2,
			TotalChecks: 5,
			Items: []StateItem{
				{CheckName: "a", Drifted: true},
				{CheckName: "b", Drifted: true},
				{CheckName: "c", Drifted: false},
			},
		},
		{
			Host:        "web-02",
			Timestamp:   time.Now().UTC(),
			DriftCount:  0,
			TotalChecks: 3,
		},
	}

	report := GenerateReport(results)
	assert.NotNil(t, report)
	assert.NotEmpty(t, report.ID)
	assert.False(t, report.Timestamp.IsZero())
	assert.Equal(t, 2, report.TotalHosts)
	assert.Equal(t, 2, report.TotalDrifts)
	assert.Equal(t, 8, report.TotalChecks)
	assert.Equal(t, 2, report.Summary["web-01"])
	assert.Equal(t, 0, report.Summary["web-02"])
}

func TestGenerateReport_Empty(t *testing.T) {
	report := GenerateReport(nil)
	assert.NotNil(t, report)
	assert.Equal(t, 0, report.TotalHosts)
	assert.Equal(t, 0, report.TotalDrifts)
	assert.False(t, report.HasDrift())
}

func TestGenerateReport_WithNilResults(t *testing.T) {
	results := []*DriftResult{nil, {Host: "web-01", DriftCount: 1, TotalChecks: 5}, nil}
	report := GenerateReport(results)
	assert.Equal(t, 1, report.TotalHosts)
	assert.Equal(t, 1, report.TotalDrifts)
}

// --- HasDrift --------------------------------------------------------------

func TestReport_HasDrift(t *testing.T) {
	r := &DriftReport{TotalDrifts: 0}
	assert.False(t, r.HasDrift())

	r2 := &DriftReport{TotalDrifts: 1}
	assert.True(t, r2.HasDrift())

	assert.False(t, (*DriftReport)(nil).HasDrift())
}

// --- ToJSON ----------------------------------------------------------------

func TestReport_ToJSON(t *testing.T) {
	report := &DriftReport{
		ID:          "rpt-1",
		Timestamp:   time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC),
		TotalHosts:  2,
		TotalDrifts: 1,
		TotalChecks: 5,
		Summary:     map[string]int{"web-01": 1, "web-02": 0},
	}

	data, err := report.ToJSON()
	require.NoError(t, err)
	assert.NotEmpty(t, data)

	// Verify it's valid JSON.
	var parsed DriftReport
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)
	assert.Equal(t, "rpt-1", parsed.ID)
	assert.Equal(t, 2, parsed.TotalHosts)
	assert.Equal(t, 1, parsed.TotalDrifts)
}

// --- ToTable ---------------------------------------------------------------

func TestReport_ToTable(t *testing.T) {
	report := &DriftReport{
		ID:        "rpt-1",
		Timestamp: time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC),
		Results: []*DriftResult{
			{Host: "web-01", DriftCount: 2, TotalChecks: 5},
			{Host: "web-02", DriftCount: 0, TotalChecks: 3},
		},
		TotalHosts:  2,
		TotalDrifts: 2,
		TotalChecks: 8,
	}

	table := report.ToTable()
	assert.NotEmpty(t, table)
	assert.Contains(t, table, "Drift Report")
	assert.Contains(t, table, "web-01")
	assert.Contains(t, table, "web-02")
	assert.Contains(t, table, "DRIFTED")
	assert.Contains(t, table, "OK")
	assert.Contains(t, table, "TOTAL")
}

func TestReport_ToTable_Empty(t *testing.T) {
	report := &DriftReport{
		ID:        "rpt-1",
		Timestamp: time.Now().UTC(),
	}
	table := report.ToTable()
	assert.Contains(t, table, "Drift Report")
	assert.Contains(t, table, "TOTAL")
}

// --- AnalyzeTrend ----------------------------------------------------------

func TestAnalyzeTrend_Stable(t *testing.T) {
	history := []*DriftReport{
		{Timestamp: time.Now().Add(-2 * time.Hour), Summary: map[string]int{"web-01": 2}},
		{Timestamp: time.Now().Add(-1 * time.Hour), Summary: map[string]int{"web-01": 2}},
	}

	trend := AnalyzeTrend(history, "web-01")
	assert.Equal(t, "web-01", trend.Host)
	assert.Equal(t, "stable", trend.TrendDirection)
	assert.Len(t, trend.Points, 2)
	assert.Equal(t, float64(2), trend.AverageDrift)
}

func TestAnalyzeTrend_Increasing(t *testing.T) {
	history := []*DriftReport{
		{Timestamp: time.Now().Add(-3 * time.Hour), Summary: map[string]int{"web-01": 1}},
		{Timestamp: time.Now().Add(-2 * time.Hour), Summary: map[string]int{"web-01": 3}},
		{Timestamp: time.Now().Add(-1 * time.Hour), Summary: map[string]int{"web-01": 5}},
	}

	trend := AnalyzeTrend(history, "web-01")
	assert.Equal(t, "increasing", trend.TrendDirection)
	assert.Len(t, trend.Points, 3)
	assert.InDelta(t, 3.0, trend.AverageDrift, 0.01)
}

func TestAnalyzeTrend_Decreasing(t *testing.T) {
	history := []*DriftReport{
		{Timestamp: time.Now().Add(-3 * time.Hour), Summary: map[string]int{"web-01": 5}},
		{Timestamp: time.Now().Add(-2 * time.Hour), Summary: map[string]int{"web-01": 3}},
		{Timestamp: time.Now().Add(-1 * time.Hour), Summary: map[string]int{"web-01": 1}},
	}

	trend := AnalyzeTrend(history, "web-01")
	assert.Equal(t, "decreasing", trend.TrendDirection)
}

func TestAnalyzeTrend_EmptyHistory(t *testing.T) {
	trend := AnalyzeTrend(nil, "web-01")
	assert.Equal(t, "web-01", trend.Host)
	assert.Equal(t, "stable", trend.TrendDirection)
	assert.Empty(t, trend.Points)
	assert.Equal(t, float64(0), trend.AverageDrift)
}

func TestAnalyzeTrend_SinglePoint(t *testing.T) {
	history := []*DriftReport{
		{Timestamp: time.Now(), Summary: map[string]int{"web-01": 3}},
	}

	trend := AnalyzeTrend(history, "web-01")
	assert.Equal(t, "stable", trend.TrendDirection) // single point is stable
	assert.Len(t, trend.Points, 1)
	assert.Equal(t, float64(3), trend.AverageDrift)
}

func TestAnalyzeTrend_HostNotFound(t *testing.T) {
	history := []*DriftReport{
		{Timestamp: time.Now(), Summary: map[string]int{"web-01": 3}},
	}

	trend := AnalyzeTrend(history, "web-99")
	assert.Equal(t, "stable", trend.TrendDirection)
	assert.Empty(t, trend.Points)
}

func TestAnalyzeTrend_FiltersByHost(t *testing.T) {
	history := []*DriftReport{
		{Timestamp: time.Now().Add(-1 * time.Hour), Summary: map[string]int{"web-01": 2, "web-02": 5}},
		{Timestamp: time.Now(), Summary: map[string]int{"web-01": 4, "web-02": 1}},
	}

	trend := AnalyzeTrend(history, "web-01")
	assert.Len(t, trend.Points, 2)
	assert.Equal(t, 2, trend.Points[0].DriftCount)
	assert.Equal(t, 4, trend.Points[1].DriftCount)
	assert.Equal(t, "increasing", trend.TrendDirection)
}

func TestAnalyzeTrend_WithNilReports(t *testing.T) {
	history := []*DriftReport{
		nil,
		{Timestamp: time.Now(), Summary: map[string]int{"web-01": 1}},
		nil,
	}

	trend := AnalyzeTrend(history, "web-01")
	assert.Len(t, trend.Points, 1)
}

// --- DriftTrend.ToJSON -----------------------------------------------------

func TestTrend_ToJSON(t *testing.T) {
	trend := &DriftTrend{
		Host:           "web-01",
		TrendDirection: "increasing",
		AverageDrift:   3.5,
		Points: []TrendPoint{
			{Timestamp: time.Now(), DriftCount: 2, HostCount: 1},
			{Timestamp: time.Now().Add(time.Hour), DriftCount: 5, HostCount: 1},
		},
	}

	data, err := trend.ToJSON()
	require.NoError(t, err)
	assert.NotEmpty(t, data)

	var parsed DriftTrend
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)
	assert.Equal(t, "web-01", parsed.Host)
	assert.Equal(t, "increasing", parsed.TrendDirection)
	assert.InDelta(t, 3.5, parsed.AverageDrift, 0.01)
	assert.Len(t, parsed.Points, 2)
}

// --- generateReportID ------------------------------------------------------

func TestGenerateReportID(t *testing.T) {
	id1 := generateReportID()
	assert.NotEmpty(t, id1)
	assert.True(t, strings.HasPrefix(id1, "rpt-"))
}
