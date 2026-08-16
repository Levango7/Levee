// Report and trend analysis for the LEVEE drift package.
//
// This file defines DriftReport (an aggregate of one or more DriftResults),
// TrendPoint and DriftTrend (historical drift analysis). Reports can be
// rendered as JSON or as a human-readable table; trends classify the drift
// direction as increasing, decreasing or stable.

package drift

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// --- DriftReport -----------------------------------------------------------

// DriftReport aggregates the DriftResults of a single detection run (which may
// cover multiple hosts) and exposes summary statistics used by the CLI and the
// notification framework.
type DriftReport struct {
	// ID is the unique report identifier.
	ID string `json:"id"`
	// Timestamp is when the report was generated (UTC).
	Timestamp time.Time `json:"timestamp"`
	// Results is the per-host detection results.
	Results []*DriftResult `json:"results"`
	// TotalHosts is the number of hosts covered by the report.
	TotalHosts int `json:"total_hosts"`
	// TotalDrifts is the total number of drifted checks across all hosts.
	TotalDrifts int `json:"total_drifts"`
	// TotalChecks is the total number of checks across all hosts.
	TotalChecks int `json:"total_checks"`
	// Summary maps host -> drift count for quick inspection.
	Summary map[string]int `json:"summary"`
}

// GenerateReport builds a DriftReport from the given results. It computes the
// summary statistics and assigns a generated ID and the current timestamp.
func GenerateReport(results []*DriftResult) *DriftReport {
	r := &DriftReport{
		ID:        generateReportID(),
		Timestamp: time.Now().UTC(),
		Results:   results,
		Summary:   make(map[string]int, len(results)),
	}
	for _, res := range results {
		if res == nil {
			continue
		}
		r.TotalHosts++
		r.TotalDrifts += res.DriftCount
		r.TotalChecks += res.TotalChecks
		r.Summary[res.Host] = res.DriftCount
	}
	return r
}

// HasDrift reports whether the report contains any drifted items.
func (r *DriftReport) HasDrift() bool {
	return r != nil && r.TotalDrifts > 0
}

// ToJSON serialises the report as indented JSON.
func (r *DriftReport) ToJSON() ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("drift: report to json: %w", err)
	}
	return data, nil
}

// ToTable renders the report as a human-readable table. The table has one row
// per host plus a totals row.
func (r *DriftReport) ToTable() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Drift Report %s\n", r.ID)
	fmt.Fprintf(&b, "Generated: %s\n\n", r.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(&b, "%-24s %-12s %-12s %-12s\n", "HOST", "DRIFTS", "CHECKS", "STATUS")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 62))
	for _, res := range r.Results {
		if res == nil {
			continue
		}
		status := "OK"
		if res.DriftCount > 0 {
			status = "DRIFTED"
		}
		fmt.Fprintf(&b, "%-24s %-12d %-12d %-12s\n",
			res.Host, res.DriftCount, res.TotalChecks, status)
	}
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 62))
	fmt.Fprintf(&b, "%-24s %-12d %-12d\n",
		"TOTAL", r.TotalDrifts, r.TotalChecks)
	return b.String()
}

// --- TrendPoint ------------------------------------------------------------

// TrendPoint is a single data point in a drift trend. It captures the drift
// count and the number of hosts covered at a point in time.
type TrendPoint struct {
	// Timestamp is when the data point was recorded.
	Timestamp time.Time `json:"timestamp"`
	// DriftCount is the total number of drifted checks at this point.
	DriftCount int `json:"drift_count"`
	// HostCount is the number of hosts covered at this point.
	HostCount int `json:"host_count"`
}

// --- DriftTrend ------------------------------------------------------------

// DriftTrend is the historical drift analysis for a single host. It classifies
// the drift direction so operators can tell whether drift is getting better or
// worse over time.
type DriftTrend struct {
	// Host is the target host the trend applies to.
	Host string `json:"host"`
	// Points is the chronological list of trend data points.
	Points []TrendPoint `json:"points"`
	// TrendDirection is "increasing", "decreasing" or "stable".
	TrendDirection string `json:"trend_direction"`
	// AverageDrift is the mean drift count across all data points.
	AverageDrift float64 `json:"average_drift"`
}

// ToJSON serialises the trend as indented JSON.
func (t *DriftTrend) ToJSON() ([]byte, error) {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("drift: trend to json: %w", err)
	}
	return data, nil
}

// AnalyzeTrend computes the drift trend for a single host from a chronological
// list of reports. Only data points that cover the given host are included.
// When no data points are available the trend direction is "stable" with a
// zero average.
//
// The trend direction is determined by comparing the drift count of the last
// data point to the first:
//   - "increasing" when the last count is strictly greater than the first
//   - "decreasing" when the last count is strictly less than the first
//   - "stable" when they are equal or there are fewer than two points
func AnalyzeTrend(history []*DriftReport, host string) *DriftTrend {
	trend := &DriftTrend{
		Host:           host,
		TrendDirection: "stable",
	}

	var (
		points []TrendPoint
		sum    int
	)
	for _, report := range history {
		if report == nil {
			continue
		}
		count, ok := report.Summary[host]
		if !ok {
			continue
		}
		points = append(points, TrendPoint{
			Timestamp:  report.Timestamp,
			DriftCount: count,
			HostCount:  report.TotalHosts,
		})
		sum += count
	}

	trend.Points = points
	if len(points) > 0 {
		trend.AverageDrift = float64(sum) / float64(len(points))
	}

	if len(points) < 2 {
		return trend
	}

	first := points[0].DriftCount
	last := points[len(points)-1].DriftCount
	switch {
	case last > first:
		trend.TrendDirection = "increasing"
	case last < first:
		trend.TrendDirection = "decreasing"
	default:
		trend.TrendDirection = "stable"
	}
	return trend
}

// --- ID generation ---------------------------------------------------------

// generateReportID produces a unique report ID based on the current wall-clock
// nanosecond timestamp. Two reports generated in the same nanosecond will share
// an ID; in practice this is unlikely because report generation involves
// non-trivial work, and the ID is only used for display / correlation.
func generateReportID() string {
	now := time.Now().UTC().UnixNano()
	return fmt.Sprintf("rpt-%d", now)
}
