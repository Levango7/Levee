package main

// serve_gateway_wiring_test.go closes the gap between the gateway's
// cluster_enabled field and the process that is supposed to fill it.
//
// The payload and its consumer are covered in internal/grpc; a field can be
// correct there and still be decoration here, because `serve` is the only
// producer. Two things would otherwise go unnoticed:
//
//   - newServeGateway computing the announcement from something other than the
//     cluster manager. The backend label is the tempting wrong answer, and it
//     is wrong for exactly the deployment a single-node PostgreSQL server
//     creates: storage says postgres, coordination says nothing.
//   - runServe's call site passing something that is not the manager its
//     takeover and dispatch loops received, so the console announces "no
//     coordination" while two loops are sweeping leases.
//
// The first is a behaviour test. The second cannot be: runServe is a long
// startup sequence that a unit test cannot drive, so it is pinned by reading
// the call site and requiring the same identifier the loops are given.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/cluster"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/state"
)

func newWiringTestStore(t *testing.T) *state.SQLiteStore {
	t.Helper()
	store, err := state.NewSQLiteStore(context.Background(), filepath.Join(t.TempDir(), "wiring.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestNewServeGatewayAnnouncesCoordinationFromTheManager(t *testing.T) {
	store := newWiringTestStore(t)

	// Single-node serve: openServeStore returned no manager, so nothing may be
	// announced regardless of which backend answers.
	single := newServeGateway(grpc.ServeGatewayConfig{}, store, nil, nil)
	assert.False(t, single.ClusterEnabled(),
		"a gateway built without a cluster manager must not announce coordination")

	// Cluster serve: a manager exists, so /system/cluster-status has to say so.
	// NewClusterManager does not touch the handle before Start, which keeps this
	// a construction test rather than a database test.
	mgr := cluster.NewClusterManager(store.DB(), cluster.ManagerConfig{SelfID: "node-a"})
	clustered := newServeGateway(grpc.ServeGatewayConfig{}, store, nil, mgr)
	assert.True(t, clustered.ClusterEnabled(),
		"a gateway built with a cluster manager must announce coordination")

	// A caller that set the field itself gets it overwritten: the manager is the
	// source, not the literal. This is what keeps ServeGatewayConfig from
	// carrying a second, drift-prone statement of what serve already knows.
	lying := newServeGateway(grpc.ServeGatewayConfig{ClusterEnabled: true}, store, nil, nil)
	assert.False(t, lying.ClusterEnabled(),
		"newServeGateway must derive the announcement from the manager, not trust the literal")
}

// serveGatewayCall returns runServe's `newServeGateway(...)` invocation,
// argument text included, by taking every line from the construction up to and
// including the line that names the store argument.
func serveGatewayCall(t *testing.T, src string) string {
	t.Helper()
	lines := strings.Split(src, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "gw := newServeGateway(") {
			start = i
			break
		}
	}
	require.NotEqual(t, -1, start, "runServe no longer builds the gateway through newServeGateway; update this gate")
	for i := start; i < len(lines) && i < start+40; i++ {
		if strings.Contains(lines[i], "svcs.gateSvc") {
			return strings.Join(lines[start:i+1], "\n")
		}
	}
	t.Fatal("the newServeGateway call site does not close on a line naming svcs.gateSvc")
	return ""
}

func TestServeCallSitePassesTheLiveManager(t *testing.T) {
	// Tests run with the package directory as cwd, so this is the same file the
	// compiler used; no path is guessed.
	src, err := os.ReadFile("cmd_serve.go")
	require.NoError(t, err, "the wiring gate reads cmd_serve.go next to itself")
	text := string(src)

	call := serveGatewayCall(t, text)
	closing := strings.TrimSpace(call)
	closing = strings.TrimSpace(strings.TrimSuffix(closing, ")"))
	lastArg := strings.TrimSpace(closing[strings.LastIndex(closing, ",")+1:])
	assert.Equal(t, "clusterMgr", lastArg,
		"the last argument to newServeGateway must be the cluster manager; anything else (nil, a copy, a "+
			"re-derived value) makes the announcement independent of what this process actually runs")

	// The same identifier feeds the coordination loops. If the two ever take
	// different arguments, the page and the process disagree about whether a
	// cluster exists.
	for _, anchor := range []string{"startTakeoverAndFencing(", "startDispatchAndWorkerLoops("} {
		i := callIndex(t, text, anchor)
		window := text[i:]
		if j := strings.Index(window, "\n"); j >= 0 {
			window = window[:j]
		}
		assert.Contains(t, window, "clusterMgr",
			anchor+" is not given clusterMgr on its call line, so the announced flag and the coordination "+
				"loops are fed from different values")
	}

	assert.Contains(t, text, "store, clusterMgr, err := openServeStore(ctx, cfg)",
		"clusterMgr must stay the value openServeStore reports for this process")
}

// callIndex finds where anchor is INVOKED, not where it is declared: the
// definitions in this file break their parameter list across lines, so matching
// the first occurrence would read the signature and report the manager missing.
func callIndex(t *testing.T, text, anchor string) int {
	t.Helper()
	for off := 0; ; {
		i := strings.Index(text[off:], anchor)
		if i < 0 {
			break
		}
		abs := off + i
		if abs < 5 || text[abs-5:abs] != "func " {
			return abs
		}
		off = abs + len(anchor)
	}
	t.Fatalf("runServe no longer calls %s; update this gate", anchor)
	return -1
}
