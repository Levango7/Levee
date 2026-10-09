package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServeInstallsTheAuthzDenialRecorder: the authorizer refuses correctly
// with or without a denial recorder — what the recorder decides is whether the
// refusal exists anywhere besides the caller's error message (SA-007). That
// makes the install line easy to lose in a refactor and impossible to notice:
// every behavioural test still passes, because the refusal itself is identical.
// So it is pinned structurally, the way TestCLIHasOneSQLiteOpener pins the
// single store opener.
func TestServeInstallsTheAuthzDenialRecorder(t *testing.T) {
	raw, err := os.ReadFile("cmd_serve.go")
	require.NoError(t, err)
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")

	load := strings.Index(src, "authz.Load(")
	require.GreaterOrEqual(t, load, 0,
		"cmd_serve.go no longer builds the authorizer here; re-derive this guard instead of deleting it")

	rest := src[load:]
	end := strings.Index(rest, "changeSvc.WithAuthorizer(")
	require.GreaterOrEqual(t, end, 0,
		"cmd_serve.go no longer hands the authorizer to the change service; re-derive this guard instead of deleting it")

	assert.Contains(t, rest[:end], "WithDenialRecorder(audit.NewDenialRecorder(store))",
		"the serving authorizer must persist refusals: install audit.NewDenialRecorder(store) on it before it is handed to the services")
}
