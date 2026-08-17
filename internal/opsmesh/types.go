
// types.go defines the request and response payloads exchanged between levee
// and the OpsMesh platform. All structs use standard encoding/json tags so
// they can be marshalled without custom adapters. Time values are RFC 3339
// encoded through time.Time's default behaviour and durations are encoded as
// nanosecond integers through time.Duration's default int64 representation.
package opsmesh

import "time"

// FixResult is the remediation outcome reported back to OpsMesh after levee
// finishes executing a workflow for a given alert. It captures both the
// high-level success/failure flag and the detailed step-level metrics that
// OpsMesh uses to train its recommendation engine.
type FixResult struct {
	AlertID      string             `json:"alert_id"`
	Success      bool               `json:"success"`
	Summary      string             `json:"summary"`
	WorkflowID   string             `json:"workflow_id"`
	Duration     time.Duration      `json:"duration"`
	StepsTotal   int                `json:"steps_total"`
	StepsFailed  int                `json:"steps_failed"`
	RollbackUsed bool               `json:"rollback_used"`
	MetricsBefore map[string]float64 `json:"metrics_before"`
	MetricsAfter  map[string]float64 `json:"metrics_after"`
	Timestamp    time.Time          `json:"timestamp"`
	Error        string             `json:"error,omitempty"`
}

// Topology is the service topology returned by OpsMesh. It describes the
// relevant nodes (hosts, containers, VMs or services) and the directed edges
// between them that levee uses to scope remediation actions.
type Topology struct {
	Service   string         `json:"service"`
	Nodes     []TopologyNode `json:"nodes"`
	Edges     []TopologyEdge `json:"edges"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// TopologyNode is a single vertex in the topology graph.
type TopologyNode struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"` // host / container / vm / service
	IP       string            `json:"ip"`
	Metadata map[string]string `json:"metadata"`
}

// TopologyEdge is a directed relationship between two nodes.
type TopologyEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"` // depends / connects / replicates
}

// Metrics is the monitoring metric payload returned by OpsMesh. It wraps the
// queried series together with the effective time range so callers can
// correlate the response with their request.
type Metrics struct {
	Query     string         `json:"query"`
	Series    []MetricSeries `json:"series"`
	TimeRange TimeRange      `json:"time_range"`
}

// MetricSeries is a single labelled time series.
type MetricSeries struct {
	Labels map[string]string `json:"labels"`
	Points []MetricPoint     `json:"points"`
}

// MetricPoint is a single (timestamp, value) sample.
type MetricPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// TimeRange is a half-open [Start, End) time interval used by GetMetrics. A
// TimeRange is valid only when End is strictly after Start.
type TimeRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}