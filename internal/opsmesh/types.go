// types.go mirrors the OpsMesh platform's wire contracts — verified against
// the platform's own source (services/device-svc catalog endpoints and the
// aio-svc Prometheus proxy), not against the aspirational sketch in
// docs/opsmesh-integration-design.md §6.3:
//
//	GET  /api/v1/catalog/topology?tenantID=<t>   → CatalogGraph
//	POST /api/v1/prometheus/query {"query": q}   → PrometheusResponse
//	POST /api/v1/alerts/{id}/resolution          → (see ReportResult's note:
//	                                                the platform implements
//	                                                ack/silence today)
//
// Keeping the shapes byte-compatible with the platform is the whole point:
// the previous revision of this file modeled an invented contract (a
// `service`-keyed topology and a series query with a time range) that would
// have 404'd against the real deployment. Durations encode as nanosecond
// integers through time.Duration's default int64 representation; time values
// are RFC 3339 via time.Time's default behaviour.
package opsmesh

import "time"

// FixResult is the remediation outcome reported back to OpsMesh after levee
// fixes an alert-driven incident. OpsMesh uses it to close the alert and to
// train its recommendation engine.
type FixResult struct {
	AlertID       string             `json:"alert_id"`
	Success       bool               `json:"success"`
	Summary       string             `json:"summary"`
	WorkflowID    string             `json:"workflow_id"`
	Duration      time.Duration      `json:"duration"`
	StepsTotal    int                `json:"steps_total"`
	StepsFailed   int                `json:"steps_failed"`
	RollbackUsed  bool               `json:"rollback_used"`
	MetricsBefore map[string]float64 `json:"metrics_before"`
	MetricsAfter  map[string]float64 `json:"metrics_after"`
	Timestamp     time.Time          `json:"timestamp"`
	Error         string             `json:"error,omitempty"`
}

// CatalogNode is a single vertex in the platform's asset/service catalog.
type CatalogNode struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"` // host / service / database / ...
	Status   string            `json:"status"`
	Metadata map[string]string `json:"metadata"`
	Children []string          `json:"children"`
}

// CatalogEdge is a directed relationship between two catalog nodes.
type CatalogEdge struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"` // depends / connects / replicates / ...
}

// CatalogGraph is the platform's full topology graph for a tenant.
type CatalogGraph struct {
	Nodes    []CatalogNode `json:"nodes"`
	Edges    []CatalogEdge `json:"edges"`
	TenantID string        `json:"tenantID"`
}

// PrometheusResponse is the platform's Prometheus proxy envelope.
type PrometheusResponse struct {
	Status string         `json:"status"`
	Data   PrometheusData `json:"data"`
}

// PrometheusData carries the query result payload.
type PrometheusData struct {
	ResultType string             `json:"resultType"`
	Result     []PrometheusSeries `json:"result"`
}

// PrometheusSeries is one instant-vector series: a label set plus the
// [timestamp, value] pair Prometheus emits for instant queries. Value is kept
// raw because the platform passes it through verbatim — on the wire the value
// element is a JSON string ("42.5", "NaN") per the Prometheus API.
type PrometheusSeries struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

// Metrics is the normalized view of a Prometheus instant-vector response:
// one MetricSample per series, with the value parsed and the timestamp
// decoded so callers never touch the raw [ts, "value"] pair.
type Metrics struct {
	Query  string         `json:"query"`
	Series []MetricSample `json:"series"`
}

// MetricSample is one series at the instant the platform evaluated the query.
type MetricSample struct {
	Labels    map[string]string `json:"labels"`
	Value     float64           `json:"value"`
	Timestamp time.Time         `json:"timestamp"`
}
