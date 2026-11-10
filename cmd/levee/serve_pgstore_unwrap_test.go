// serve_pgstore_unwrap_test.go — wiring guard for pgStoreDB.
//
// serve hands startTakeoverAndFencing the funnel-wrapped store (runEvents;
// pinned by serve_funnel_wiring_test.go), so pgStoreDB receives a decorator,
// not the raw *state.PGStore. A direct type assertion on the parameter then
// yields nil, and both consumers go dark in a way only logs reveal: the
// execution guard fences nothing ("exec guard: nil db") and every takeover
// sweep fails ("expired executions: nil db"). The Unwrapper seam is the only
// way through; like the funnel guard, this is asserted at the source level
// because the defect lives in assembly, not in any single function's
// behaviour — booting the whole daemon is the only behavioural path.

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPgStoreDBUnwrapsDecorators(t *testing.T) {
	raw, err := os.ReadFile(serveRepoFile(t, "cmd/levee/cmd_serve.go"))
	require.NoError(t, err)
	src := string(raw)

	fnAt := strings.Index(src, "func pgStoreDB(store state.Store) *sql.DB {")
	require.GreaterOrEqual(t, fnAt, 0, "pgStoreDB must exist — the guard reads its body")
	body := src[fnAt : fnAt+800]
	assert.Contains(t, body, "state.Underlying(store)",
		"pgStoreDB must walk the Unwrapper seam: serve passes the funnel-wrapped store, and a direct *state.PGStore assertion yields nil db for the exec guard and the takeover sweep")
	assert.NotContains(t, body, "store.(*state.PGStore)",
		"direct type assertion on the parameter is the defect: decorators (RunEventStore, TenantStore) fall through to nil db")
}
