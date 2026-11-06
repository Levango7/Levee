// serve_funnel_wiring_test.go — a wiring guard, not a behaviour test.
//
// The run lifecycle funnel (internal/wiring/runeventstore.go) lists
// "takeover interrupt" among the six write sites it exists to observe,
// and its own doc claims "every successful run-status write, whatever
// flow wrote it, crosses exactly one decorator". Serve used to hand the
// background loops the RAW store under the tenancy rationale ("trusted
// in-process code ... wrapping them would only make them fail closed
// against themselves") — which is true of the tenancy wrapper and
// irrelevant to the funnel. The consequence was a doc-vs-reality gap:
// settleOne's running → interrupted CAS never emitted, so the ChatOps
// bridge (which explicitly handles runstatus.IsTerminal and comments
// "interrupted by takeover") and the webhook mirror had zero producers
// again — the exact regression the funnel was built to end.
//
// The mistake lives in serve's assembly order, so a unit test cannot
// reach it without booting the whole daemon. A source-level assertion
// can: the funnel must be constructed BEFORE the loop starters, and both
// loop starters must be handed runEvents rather than the raw store.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serveRepoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("cannot locate %s from the test working directory", rel)
	return ""
}

func TestServeBackgroundLoopsCrossTheRunEventFunnel(t *testing.T) {
	raw, err := os.ReadFile(serveRepoFile(t, "cmd/levee/cmd_serve.go"))
	require.NoError(t, err)
	src := string(raw)

	// 1. The funnel exists and is built before the loops start.
	funnelAt := strings.Index(src, "wiring.NewRunEventStore(store)")
	require.GreaterOrEqual(t, funnelAt, 0,
		"serve must construct the run lifecycle funnel — without it no loop or service emits transitions")

	// Call-site anchors (the `&` first argument): the function
	// DEFINITIONS also contain these names and sit above the funnel.
	for _, call := range []string{"startTakeoverAndFencing(&", "startDispatchAndWorkerLoops(&"} {
		callAt := strings.Index(src, call)
		require.GreaterOrEqual(t, callAt, 0, "%s call site not found — did it move?", call)
		assert.Less(t, funnelAt, callAt,
			"%s runs BEFORE the funnel is constructed: those loops would write run statuses outside the event seam", call)

		// 2. The call's own argument list must name runEvents, and must
		//    not name the raw store. Argument extraction stops at the
		//    trailing ctx argument both call sites share.
		args := src[callAt:min(callAt+400, len(src))]
		if ctxAt := strings.Index(args, "ctx)"); ctxAt >= 0 {
			args = args[:ctxAt]
		}
		assert.Contains(t, args, "runEvents",
			"%s must be handed the funnel-wrapped store", call)
		assert.NotContains(t, args, ", store,",
			"%s is handed the RAW store: writes from that loop emit no lifecycle events (chatops/webhook consumers go dark)", call)
		assert.NotContains(t, args, ", store)",
			"%s is handed the RAW store: writes from that loop emit no lifecycle events (chatops/webhook consumers go dark)", call)
	}
}
