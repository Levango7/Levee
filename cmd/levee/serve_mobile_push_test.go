package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serve_mobile_push_test.go guards the hop that A-3 shipped without: the
// daemon must hand the approval-create moment to the mobile push fan-out.
//
// The failure mode this guards is not "the type is missing" but "the manager
// is held and never called" — #320 wired the PushManager into the mobile
// approval service, and every approval still pushed to nobody because
// ChangeService.kickoffApproval, which creates the chain, was never told to
// notify. Covering that hop at runtime needs a store plus an engine plus a
// planned change; the composition hop is guarded textually instead, the same
// way the gate broadcast is (serve_gate_broadcast_test.go).

func TestServeWiresMobileApprovalPushToApprovalKickoff(t *testing.T) {
	text := serveGateBroadcastSource(t)

	svcAt := strings.Index(text, "installServeMobileApproval(approvalSvc, cfg.Server.DataDir)")
	require.Greater(t, svcAt, -1,
		"the serve path must still build the mobile approval service from the deployment's push config")
	callAt := strings.Index(text, "wireServeMobileApprovalPush(ctx, changeSvc, mobileSvc, servePushMgr)")
	require.Greater(t, callAt, -1,
		"serve must wire the mobile push fan-out to the approval kickoff; without this line the push manager is held but never called, which is the exact A-3 gap")
	assert.Less(t, svcAt, callAt,
		"the mobile service must be built before it is wired — a call before construction captures a nil service")

	helperAt := strings.Index(text, "func wireServeMobileApprovalPush(")
	require.Greater(t, helperAt, -1,
		"the wiring helper moved; re-read this guard rather than trust the index")
	body := text[helperAt:]
	if end := strings.Index(body, "\n}\n"); end != -1 {
		body = body[:end]
	}
	assert.Contains(t, body, "changeSvc.WithApprovalCreateObserver",
		"the notify must attach to the approval-create moment, not to an unrelated lifecycle event")
	assert.Contains(t, body, "NotifyApprovalRequest",
		"the observer must call the notify method — an empty observer is the no-op this guard exists to catch")
	assert.Contains(t, body, "if pushMgr == nil",
		"no channel ⇒ no observer; otherwise a credential-less deployment logs 'push disabled' on every approval instead of once at startup")
}
