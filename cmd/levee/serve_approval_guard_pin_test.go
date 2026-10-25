package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServeInstallsTheApprovalDecisionGuard: the guard's behavioural tests
// (internal/approval, internal/grpc) all pass with or without the serve
// wiring — what the install line decides is whether real deep-link and
// gateway votes are judged at all. Losing it in a refactor is invisible to
// every other test, so it is pinned structurally, exactly the way
// TestServeInstallsTheAuthzDenialRecorder pins the recorder: the guard must
// be installed on approvalSvc from the same loaded authorizer the change
// service gets.
func TestServeInstallsTheApprovalDecisionGuard(t *testing.T) {
	raw, err := os.ReadFile("cmd_serve.go")
	require.NoError(t, err)
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")

	handoff := strings.Index(src, "changeSvc.WithAuthorizer(authzSvc)")
	require.GreaterOrEqual(t, handoff, 0,
		"cmd_serve.go no longer hands the authorizer to the change service; re-derive this guard instead of deleting it")

	rest := src[handoff:]
	assert.Contains(t, rest[:2000], "approvalSvc.WithDecisionGuard(&grpc.ApprovalDecisionGuard{",
		"the serving approval service must carry the decision guard (deep-link and gateway votes); install it where the authorizer is handed to the change service")
}
