package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serve_gate_broadcast_test.go guards the LAST hop of the gate broadcast — the
// composition in buildServeServices. It is the hop that has no runtime test
// reachable from a unit run (serving a real change needs a store, an engine and
// a bot), and it is precisely the hop that has failed before in this codebase:
// chatops.EventGateResult shipped with a renderer and zero senders, and nothing
// noticed, because "the type exists" and "a call site exists" look alike in a
// review.
//
// The guard is textual rather than behavioural on purpose, and it asserts on
// ORDER, not just presence: an append that lands after wiring.NewEngine is
// silently dead, which is the failure this file exists to prevent.

func serveGateBroadcastSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("cmd_serve.go")
	require.NoError(t, err, "the composition gate reads cmd_serve.go next to itself")
	return string(src)
}

func TestServeInstallsGateBroadcastBeforeTheEngineIsBuilt(t *testing.T) {
	text := serveGateBroadcastSource(t)

	appendAt := strings.Index(text, "wiring.WithGateObserver(chatOpsBridge.OnGateVerdict)")
	require.Greater(t, appendAt, -1,
		"the serve path must install the gate broadcast; without this line the bridge is another EventGateResult-shaped no-op")
	buildAt := strings.Index(text, "eng = wiring.NewEngine(store, opts...)")
	require.Greater(t, buildAt, -1, "the engine construction site moved; re-read this guard rather than trust the index")
	assert.Less(t, appendAt, buildAt,
		"WithGateObserver must be appended to opts BEFORE NewEngine — after it, the option is computed and discarded")

	// And it must be conditional: a deployment with chatops off has no bots and
	// must end up with no observer rather than a nil-bridge method value that
	// makes "announcing is off" unobservable from the outside.
	guard := text[max(0, appendAt-400):appendAt]
	assert.Contains(t, guard, "if chatOpsBridge != nil",
		"the observer is installed only when the chatops mirror exists")
}

func TestServeBuildsChatOpsBotsBeforeTheEngine(t *testing.T) {
	text := serveGateBroadcastSource(t)

	botsAt := strings.Index(text, "buildServeChatOpsBots(cfg.Notify.ChatOps)")
	bridgeAt := strings.Index(text, "chatopsbridge.NewApprovalBridge(mgr)")
	approvalMirrorAt := strings.Index(text, "changeSvc.WithApprovalCreateObserver(chatOpsBridge.OnApprovalCreated)")

	require.Greater(t, botsAt, -1, "bots are still constructed from config")
	require.Greater(t, bridgeAt, -1, "one bridge serves both mirrors")
	require.Greater(t, approvalMirrorAt, -1, "the approval mirror is still attached to changeSvc")
	assert.Less(t, bridgeAt, approvalMirrorAt,
		"the bridge must exist before the approval observers attach to it")
	assert.Less(t, botsAt, strings.Index(text, "eng = wiring.NewEngine(store, opts...)"),
		"bots must be built before the engine, which reads one of their hooks at construction")

	// Enabled-flag gating is the documented default: nothing is installed and
	// nothing announces unless the operator asks for chatops.
	flags := strings.Count(text, "cfg.Notify.ChatOps.Enabled")
	assert.GreaterOrEqual(t, flags, 1, "chatops must stay opt-in in the serve path")
}

func TestServeStopPathStillOwnsTheBots(t *testing.T) {
	// Hoisting the bots earlier must not break shutdown: the manager is still
	// handed back through serveServices and still stopped by runServe.
	text := serveGateBroadcastSource(t)
	assert.Contains(t, text, "chatOpsMgr: chatOpsMgr",
		"serveServices still carries the started bots")
	assert.Contains(t, text, "defer stopServeChatOps(svcs.chatOpsMgr)",
		"runServe still stops them")
}
