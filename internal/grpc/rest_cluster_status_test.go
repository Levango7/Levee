package grpc

// rest_cluster_status_test.go pins what GET /system/cluster-status says about
// cluster coordination, and pins that it says it for the right reason.
//
// The endpoint used to answer with `backend` alone, and the console read that
// as "postgres means cluster". It stopped meaning that the moment a
// single-node PostgreSQL deployment became a supported shape: such an operator
// got a page telling them to start workers they never intended to run, and a
// real cluster with an empty registry got the same sentence — the one message
// that distinguishes "nothing to do here" from "coordination is broken" was
// unavailable, so the page could not make it.
//
// So `cluster_enabled` travels beside `backend`, sourced from the process's
// own wiring rather than from the storage. The tests below keep the two
// independent: the same backend and the same node rows are shown answering
// both ways, which any derivation from either would fail.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// seedClusterNodeForTest inserts one registry row on the SQLite test store.
// cluster_nodes is created by the PostgreSQL DDL in production, so the gateway
// tests that want a non-empty registry have to make the table themselves —
// ListClusterNodes answers (nil, nil) when it is missing, which is the shape a
// single-node deployment is in.
func seedClusterNodeForTest(t *testing.T, store state.Store) {
	t.Helper()
	dber, ok := store.(interface{ DB() *sql.DB })
	require.True(t, ok, "the test store must expose its handle to seed a registry row")
	now := time.Now().UTC()
	_, err := dber.DB().ExecContext(t.Context(), `
CREATE TABLE IF NOT EXISTS cluster_nodes (
	id             TEXT PRIMARY KEY,
	address        TEXT NOT NULL,
	role           TEXT NOT NULL,
	status         TEXT NOT NULL,
	last_heartbeat DATETIME NOT NULL,
	joined_at      DATETIME NOT NULL,
	capabilities   TEXT NOT NULL DEFAULT '[]'
)`)
	require.NoError(t, err)
	_, err = dber.DB().ExecContext(t.Context(),
		`INSERT INTO cluster_nodes (id, address, role, status, last_heartbeat, joined_at, capabilities)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"node-announce", "10.0.1.11:9090", "worker", "active", now, now, `["shell"]`)
	require.NoError(t, err)
}

func clusterStatusDoc(t *testing.T, url string) map[string]any {
	t.Helper()
	resp := doReq(t, http.MethodGet, url, "")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var doc map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
	return doc
}

func TestClusterStatusAnnouncesCoordination(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ServeGatewayConfig
		seedRow bool
		want    bool
		// why records what the case is guarding, because the three of them
		// together are the whole point: two different proxies were tried first
		// and both are wrong.
		why string
	}{
		{
			name:    "unwired gateway",
			cfg:     ServeGatewayConfig{},
			seedRow: false,
			want:    false,
			why:     "a gateway nobody connected to a cluster must not claim one",
		},
		{
			name:    "coordinating with an empty registry",
			cfg:     ServeGatewayConfig{ClusterEnabled: true},
			seedRow: false,
			want:    true,
			why:     "coordination on and no nodes registered is the incident the console must be able to name",
		},
		{
			name:    "single node beside a registered worker",
			cfg:     ServeGatewayConfig{},
			seedRow: true,
			want:    false,
			why:     "registry rows prove nothing about this process: they outlive it, and a stopped master still reads them",
		},
		{
			name:    "coordinating with a registered worker",
			cfg:     ServeGatewayConfig{ClusterEnabled: true},
			seedRow: true,
			want:    true,
			why:     "the normal cluster deployment",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw, srv, store := startTestGatewayFull(t, tc.cfg)
			gw.SetStore(store)
			if tc.seedRow {
				seedClusterNodeForTest(t, store)
			}

			doc := clusterStatusDoc(t, srv.URL+"/system/cluster-status")

			// The announced value, required to be PRESENT: decoding an absent
			// key into a bool assertion would otherwise read as false and let a
			// dropped field pass the "not coordinating" cases.
			raw, ok := doc["cluster_enabled"]
			require.True(t, ok, "payload has no cluster_enabled key: %v", tc.why)
			assert.Equal(t, tc.want, raw, "%s", tc.why)

			// And the backend label is still there and still SQLite in every
			// case, which is what makes the four rows above a statement about
			// independence rather than about storage.
			assert.Equal(t, "sqlite", doc["backend"],
				"the backend axis must stay reported beside the coordination axis")
			nodes, err := json.Marshal(doc["nodes"])
			require.NoError(t, err)
			if tc.seedRow {
				assert.Contains(t, string(nodes), "node-announce")
			} else {
				assert.NotContains(t, string(nodes), "node-announce")
			}
		})
	}
}

// TestClusterStatusPayloadKeys is the shape the wire contract records. It is
// asserted here rather than only in web/src/api/__contract__/wire.json so a
// renamed key fails a Go test in the same package that produces the snapshot.
func TestClusterStatusPayloadKeys(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{ClusterEnabled: true})
	gw.SetStore(store)

	doc := clusterStatusDoc(t, srv.URL+"/system/cluster-status")
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"backend", "cluster_enabled", "nodes", "summary"}, keys,
		"/system/cluster-status keys are the console's DTO; wire.json and ClusterStatus in web/src/api/index.ts "+
			"have to change with this line")
}
