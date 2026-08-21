package topology

// pinpoint.go implements the PinpointCollector, the topology backend that
// talks to Pinpoint's REST API (Phase D1).
//
// Pinpoint exposes two endpoints we care about:
//
//	GET /getServerList?from=<ms>&to=<ms>            — service list
//	GET /getServerMapDataV2?from=<ms>&to=<ms>      — server map with links
//
// The server list gives us the nodes; the server map gives us the
// caller→callee links together with sampled counts. We issue both requests
// in sequence (server list first so we can resolve link IDs against known
// nodes) and project the merged result onto the unified Topology.
//
// The collector is safe for concurrent use: it carries no mutable state and
// delegates to *http.Client which is itself goroutine-safe.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
)

// PinpointCollector queries Naver Pinpoint for service topology.
//
// Endpoint is the REST base URL, e.g. "http://<host>". HttpClient is the
// transport used for outbound requests; a nil client is replaced with
// http.DefaultClient by NewPinpointCollector.
type PinpointCollector struct {
	endpoint   string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewPinpointCollector builds a collector targeting the given REST base URL.
// If httpClient is nil, http.DefaultClient is used. The returned collector is
// ready to use and safe for concurrent access.
func NewPinpointCollector(endpoint string, httpClient *http.Client) *PinpointCollector {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &PinpointCollector{
		endpoint:   endpoint,
		httpClient: httpClient,
		logger:     slog.Default(),
	}
}

// Name returns the stable collector identifier "pinpoint".
func (c *PinpointCollector) Name() string {
	return "pinpoint"
}

// Collect queries Pinpoint for the topology in the supplied time range.
//
// The time range is translated to Pinpoint's millisecond epoch parameters
// (from/to). Two HTTP requests are issued: getServerList for the nodes and
// getServerMapDataV2 for the edges. The merged result is projected onto the
// unified Topology model. An empty but non-nil Topology is returned when
// Pinpoint reports no servers for the window.
func (c *PinpointCollector) Collect(ctx context.Context, tr TimeRange) (*Topology, error) {
	from := tr.Start.UnixMilli()
	to := tr.End.UnixMilli()

	serverList, err := c.fetchServerList(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("pinpoint server list: %w", err)
	}

	serverMap, err := c.fetchServerMap(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("pinpoint server map: %w", err)
	}

	return c.project(serverList, serverMap), nil
}

// fetchServerList calls GET /getServerList and decodes the response into a
// pinpointServerList. A non-2xx status is reported as an error.
func (c *PinpointCollector) fetchServerList(ctx context.Context, from, to int64) (*pinpointServerList, error) {
	u := fmt.Sprintf("%s/getServerList?from=%d&to=%d", c.endpoint, from, to)
	raw, err := c.doGet(ctx, u)
	if err != nil {
		return nil, err
	}
	var out pinpointServerList
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode server list: %w", err)
	}
	return &out, nil
}

// fetchServerMap calls GET /getServerMapDataV2 and decodes the response into
// a pinpointServerMap. A non-2xx status is reported as an error.
func (c *PinpointCollector) fetchServerMap(ctx context.Context, from, to int64) (*pinpointServerMap, error) {
	u := fmt.Sprintf("%s/getServerMapDataV2?from=%d&to=%d", c.endpoint, from, to)
	raw, err := c.doGet(ctx, u)
	if err != nil {
		return nil, err
	}
	var out pinpointServerMap
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode server map: %w", err)
	}
	return &out, nil
}

// doGet performs a GET request and returns the raw body bytes. It centralises
// context wiring, header handling, body draining and status-code validation
// so the two fetch helpers stay small.
func (c *PinpointCollector) doGet(ctx context.Context, rawURL string) ([]byte, error) {
	if _, err := url.Parse(rawURL); err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			c.logger.Warn("pinpoint close response body", "err", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http status %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// project converts the Pinpoint payloads into the unified Topology.
//
// Nodes are sourced from the server list. Edges are sourced from the server
// map's link array; each link carries source/target IDs plus aggregate
// counters. Pinpoint reports latency in microseconds, so we divide by 1000
// to match the millisecond convention used by EdgeMetric.
func (c *PinpointCollector) project(serverList *pinpointServerList, serverMap *pinpointServerMap) *Topology {
	topo := &Topology{}

	for _, s := range serverList.ServerList {
		node := Node{
			ID:       s.ID,
			Name:     s.Name,
			Type:     s.Type,
			Endpoint: s.Endpoint,
			Metadata: make(map[string]string),
		}
		if s.Metadata != nil {
			for k, v := range s.Metadata {
				node.Metadata[k] = v
			}
		}
		topo.AddNode(node)
	}

	for _, l := range serverMap.LinkList {
		edge := Edge{
			Source: l.Source,
			Target: l.Target,
			Metric: EdgeMetric{
				CallCount:  l.CallCount,
				ErrorCount: l.ErrorCount,
				AvgLatency: l.AvgLatency / 1000.0,
			},
		}
		topo.AddEdge(edge)
	}

	return topo
}

// --- Pinpoint wire types ---------------------------------------------------

// pinpointServerList models the response of GET /getServerList.
type pinpointServerList struct {
	ServerList []pinpointServer `json:"serverList"`
}

// pinpointServer is a single service entry in the server list.
type pinpointServer struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Endpoint string            `json:"endpoint"`
	Metadata map[string]string `json:"metadata"`
}

// pinpointServerMap models the response of GET /getServerMapDataV2. We only
// decode the link array; the node array inside the server map duplicates the
// server list and is ignored to keep the projection single-sourced.
type pinpointServerMap struct {
	LinkList []pinpointLink `json:"linkList"`
}

// pinpointLink is a caller→callee edge in the server map.
type pinpointLink struct {
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	CallCount  int64   `json:"callCount"`
	ErrorCount int64   `json:"errorCount"`
	AvgLatency float64 `json:"avgLatency"`
}
