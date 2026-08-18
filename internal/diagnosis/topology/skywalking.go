
package topology

// skywalking.go implements the SkyWalkingCollector, the topology backend that
// talks to Apache SkyWalking's GraphQL endpoint (Phase D1).
//
// SkyWalking exposes its service dependency graph through the
// `queryServiceTopology` GraphQL query. The query takes a Duration (start,
// end, step) and returns a list of nodes (services) and calls (edges). We
// project the response onto the unified Topology model so the rest of the
// engine never sees the SkyWalking-specific shape.
//
// The collector posts a single GraphQL document per Collect call. It does not
// paginate: SkyWalking already aggregates the window server-side. Errors from
// the HTTP layer are wrapped with %w so callers can use errors.Is/As against
// the underlying net.Error to detect timeouts.
//
// The collector is safe for concurrent use: it carries no mutable state and
// delegates to *http.Client which is itself goroutine-safe.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// SkyWalkingCollector queries Apache SkyWalking for service topology.
//
// Endpoint is the GraphQL URL, typically "http://<host>/graphql". HttpClient
// is the transport used for outbound requests; a nil client is replaced with
// http.DefaultClient by NewSkyWalkingCollector.
type SkyWalkingCollector struct {
	endpoint   string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewSkyWalkingCollector builds a collector targeting the given GraphQL
// endpoint. If httpClient is nil, http.DefaultClient is used. The returned
// collector is ready to use and safe for concurrent access.
func NewSkyWalkingCollector(endpoint string, httpClient *http.Client) *SkyWalkingCollector {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &SkyWalkingCollector{
		endpoint:   endpoint,
		httpClient: httpClient,
		logger:     slog.Default(),
	}
}

// Name returns the stable collector identifier "skywalking".
func (c *SkyWalkingCollector) Name() string {
	return "skywalking"
}

// Collect queries SkyWalking for the topology in the supplied time range.
//
// The time range is translated to SkyWalking's Duration{start, end, step}
// where step is "MINUTE" for windows under two hours and "HOUR" otherwise.
// The response is decoded into skywalkingTopology and projected onto the
// unified Topology model. An empty but non-nil Topology is returned when
// SkyWalking reports no nodes for the window.
func (c *SkyWalkingCollector) Collect(ctx context.Context, tr TimeRange) (*Topology, error) {
	body, err := c.buildRequestPayload(tr)
	if err != nil {
		return nil, fmt.Errorf("skywalking build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("skywalking new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("skywalking http do: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			c.logger.Warn("skywalking close response body", "err", closeErr)
		}
	}()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("skywalking read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("skywalking http status %d: %s", resp.StatusCode, string(raw))
	}

	var gqlResp skywalkingGraphQLResponse
	if err := json.Unmarshal(raw, &gqlResp); err != nil {
		return nil, fmt.Errorf("skywalking decode response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("skywalking graphql error: %v", gqlResp.Errors)
	}

	return c.project(&gqlResp.Data), nil
}

// buildRequestPayload serialises the GraphQL query and variables for the
// supplied time range. The query is intentionally minimal: we only request
// the fields the projection needs so the response stays small.
func (c *SkyWalkingCollector) buildRequestPayload(tr TimeRange) ([]byte, error) {
	step := "MINUTE"
	if tr.DurationMillis() >= int64(2*time.Hour/time.Millisecond) {
		step = "HOUR"
	}
	vars := map[string]any{
		"duration": map[string]any{
			"start": tr.Start.Format("2006-01-02 15:04"),
			"end":   tr.End.Format("2006-01-02 15:04"),
			"step":  step,
		},
	}
	payload := map[string]any{
		"query":     skywalkingTopologyQuery,
		"variables": vars,
	}
	return json.Marshal(payload)
}

// project converts the SkyWalking GraphQL payload into the unified Topology.
// Node IDs are taken verbatim from SkyWalking (numeric strings); Edge
// references use the same IDs. AvgLatency is converted from microseconds
// (SkyWalking's native unit) to milliseconds.
func (c *SkyWalkingCollector) project(data *skywalkingTopology) *Topology {
	topo := &Topology{}

	for _, n := range data.Nodes {
		node := Node{
			ID:       n.ID,
			Name:     n.Name,
			Type:     n.Type,
			Endpoint: n.Endpoint,
			Metadata: make(map[string]string),
		}
		if node.Metadata != nil {
			for k, v := range n.Metadata {
				node.Metadata[k] = v
			}
		}
		topo.AddNode(node)
	}

	for _, call := range data.Calls {
		edge := Edge{
			Source: call.Source,
			Target: call.Target,
			Metric: EdgeMetric{
				CallCount:  call.CallCount,
				ErrorCount: call.ErrorCount,
				AvgLatency: call.AvgLatency / 1000.0,
			},
		}
		topo.AddEdge(edge)
	}

	return topo
}

// skywalkingTopologyQuery is the GraphQL document sent to SkyWalking. It is
// kept as a constant so the payload size is bounded and so we can audit the
// fields we request in one place.
const skywalkingTopologyQuery = `query ($duration: Duration!) {
  topology: queryServiceTopology(duration: $duration) {
    nodes {
      id
      name
      type
      endpoint
      metadata
    }
    calls {
      source
      target
      callCount
      errorCount
      avgLatency
    }
  }
}`

// --- SkyWalking wire types -------------------------------------------------

// skywalkingGraphQLResponse is the top-level envelope returned by SkyWalking.
// Data carries the topology; Errors is non-empty on GraphQL failures.
type skywalkingGraphQLResponse struct {
	Data   skywalkingTopology  `json:"data"`
	Errors []skywalkingGQLError `json:"errors"`
}

// skywalkingGQLError is a single GraphQL error.
type skywalkingGQLError struct {
	Message string `json:"message"`
}

// String renders the error for inclusion in fmt.Errorf logs.
func (e skywalkingGQLError) String() string {
	return e.Message
}

// skywalkingTopology is the data payload of the GraphQL response.
type skywalkingTopology struct {
	Nodes []skywalkingNode `json:"nodes"`
	Calls []skywalkingCall `json:"calls"`
}

// skywalkingNode is a service in the topology.
type skywalkingNode struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Endpoint string            `json:"endpoint"`
	Metadata map[string]string `json:"metadata"`
}

// skywalkingCall is a dependency edge between two services.
type skywalkingCall struct {
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	CallCount  int64   `json:"callCount"`
	ErrorCount int64   `json:"errorCount"`
	AvgLatency float64 `json:"avgLatency"`
}