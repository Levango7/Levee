package grpc

// rest_cluster_filter_test.go pins GET /system/cluster-status?capability=<name>.
//
// The registry has always carried a per-node capability column, and nothing in
// the product could fill it or ask about it: there was no way to declare what a
// node can execute, and no way to ask which nodes could. So the honest answer to
// "do we have any node that can run pkg" was unavailable, while the column made
// it look available.
//
// What is pinned here is the shape of the answer, and the two ways it could lie:
// a filter that also narrowed the assignment summary (implying the scheduler
// placed work by capability, which internal/dispatch does not do), and a filtered
// "no matches" that came back as null and crashed whatever iterated it — while
// the unfiltered path stayed exactly as it was, so nobody's existing client
// changes shape by accident.

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

// addNodeForFilterTest inserts one registry row with a given declaration.
func addNodeForFilterTest(t *testing.T, store state.Store, id, caps string) {
	t.Helper()
	dber, ok := store.(interface{ DB() *sql.DB })
	require.True(t, ok, "the test store must expose its handle")
	now := time.Now().UTC()
	_, err := dber.DB().ExecContext(t.Context(),
		`INSERT INTO cluster_nodes (id, address, role, status, last_heartbeat, joined_at, capabilities)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, id+":9090", "worker", "active", now, now, caps)
	require.NoError(t, err)
}

// nodeIDs returns the ids in a cluster-status payload's nodes array.
func nodeIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var doc struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(body, &doc))
	out := make([]string, 0, len(doc.Nodes))
	for _, n := range doc.Nodes {
		out = append(out, n.ID)
	}
	return out
}

func getClusterStatusBody(t *testing.T, url string) []byte {
	t.Helper()
	resp := doReq(t, http.MethodGet, url, "")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", url)
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

func TestClusterStatusCapabilityFilter(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	// The shared fixture creates cluster_nodes and one node declaring
	// ["shell","pkg"], so the DDL is not copied a second time here.
	seedWireFixture(t, store)
	addNodeForFilterTest(t, store, "node-shell-only", `["shell"]`)
	addNodeForFilterTest(t, store, "node-none", `[]`)
	// What the column used to DEFAULT to. Not a capability set, and it must not be
	// laundered into one that matches something.
	addNodeForFilterTest(t, store, "node-object", `{}`)

	base := srv.URL + "/system/cluster-status"
	all := nodeIDs(t, getClusterStatusBody(t, base))
	require.Len(t, all, 4, "the fixture plus three seeded rows")

	t.Run("no parameter leaves the payload alone", func(t *testing.T) {
		assert.Equal(t, all, nodeIDs(t, getClusterStatusBody(t, base+"?capability=")),
			"an empty capability must not be read as a filter for the empty string")
	})

	t.Run("filters to the nodes that declared it", func(t *testing.T) {
		got := nodeIDs(t, getClusterStatusBody(t, base+"?capability=pkg"))
		assert.Equal(t, []string{"node-wire"}, got)

		got = nodeIDs(t, getClusterStatusBody(t, base+"?capability=shell"))
		assert.ElementsMatch(t, []string{"node-wire", "node-shell-only"}, got,
			"two nodes declared shell, and the object-shaped row must not join them")
	})

	t.Run("nothing matches answers with an empty array", func(t *testing.T) {
		body := getClusterStatusBody(t, base+"?capability=nonexistent")
		assert.Contains(t, string(body), `"nodes":[]`,
			"a filtered no-match must be an array, not null — but only on the path that asked")
		assert.Empty(t, nodeIDs(t, body))

		// And the unfiltered shape is untouched: a SQLite deployment with no
		// cluster_nodes table still gets what it always got.
		assert.NotContains(t, string(getClusterStatusBody(t, base)), `"nodes":[]`)
	})

	t.Run("the assignment summary is not narrowed", func(t *testing.T) {
		// Deliberate: assignments are placed by load, not by capability. Echoing a
		// filtered node list beside deployment-wide counts is honest only because
		// the counts were never claimed to follow the filter — silently narrowing
		// them would imply the scheduler honours declarations, and it does not.
		filtered := getClusterStatusBody(t, base+"?capability=nonexistent")
		unfiltered := getClusterStatusBody(t, base)
		var f, u map[string]any
		require.NoError(t, json.Unmarshal(filtered, &f))
		require.NoError(t, json.Unmarshal(unfiltered, &u))
		assert.Equal(t, u["summary"], f["summary"],
			"summary must stay deployment-wide when nodes are filtered")
		assert.Equal(t, u["backend"], f["backend"])
	})
}

// TestClusterStatusCapabilityFilterIsStillGated keeps the filter from becoming a
// second, ungated door into the node registry: the authorization check runs
// before the query parameter is ever read.
func TestClusterStatusCapabilityFilterIsStillGated(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{
		AuthTokens: []TokenIdentity{{Token: "alice-secret", Subject: "alice"}},
	})
	gw.SetStore(store)
	seedWireFixture(t, store)

	gw.WithAuthorizer(authorizerWith(t,
		"users:\n  - name: alice\n    team: sre\n    role: operator\n",
		"teams:\n  - name: sre\n    environments:\n      - name: staging\n        actions: [view]\n",
		"prod"))

	resp := authedGet(t, srv.URL+"/system/cluster-status?capability=shell")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a query parameter must not widen access to the registry")
}
