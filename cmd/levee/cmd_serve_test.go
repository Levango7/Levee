package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dispatch"
)

// resetServeFlags restores the serve command's package-level flags to their
// zero values so tests do not leak configuration into each other.
func resetServeFlags() {
	serveOptAddr = ""
	serveOptTLSCert = ""
	serveOptTLSKey = ""
	serveOptToken = ""
	serveOptInsecure = false
	serveOptCORSOrigins = nil
}

func TestServeCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("serve")
	require.NotNil(t, cmd, "serve subcommand should be registered")
	assert.Equal(t, "serve", cmd.Name())
}

func TestServeCmdSecurityFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("serve")
	require.NotNil(t, cmd)

	tokenFlag := cmd.Flags().Lookup("token")
	require.NotNil(t, tokenFlag, "serve should have a --token flag")
	assert.Equal(t, "", tokenFlag.DefValue, "--token must default to empty")

	insecureFlag := cmd.Flags().Lookup("insecure")
	require.NotNil(t, insecureFlag, "serve should have an --insecure flag")
	assert.Equal(t, "false", insecureFlag.DefValue, "--insecure must default to false")

	corsFlag := cmd.Flags().Lookup("cors-origin")
	require.NotNil(t, corsFlag, "serve should have a --cors-origin flag")
}

// TestRunServeRefusesWithoutToken pins the safety gate: starting the daemon
// with no --token, no LEVEE_TOKEN and no --insecure must fail loudly rather
// than silently exposing an unauthenticated API.
func TestRunServeRefusesWithoutToken(t *testing.T) {
	resetServeFlags()
	t.Cleanup(resetServeFlags)
	t.Setenv("LEVEE_TOKEN", "")

	cmd := findSub("serve")
	require.NotNil(t, cmd)

	err := runServe(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LEVEE_TOKEN",
		"the refusal message should point operators at the supported token sources")
}

// TestResolveServeToken covers the flag-over-env precedence.
func TestResolveServeToken(t *testing.T) {
	resetServeFlags()
	t.Cleanup(resetServeFlags)

	t.Run("empty everywhere yields empty", func(t *testing.T) {
		t.Setenv("LEVEE_TOKEN", "")
		assert.Empty(t, resolveServeToken())
	})

	t.Run("env var is used when flag unset", func(t *testing.T) {
		t.Setenv("LEVEE_TOKEN", "from-env")
		assert.Equal(t, "from-env", resolveServeToken())
	})

	t.Run("flag wins over env var", func(t *testing.T) {
		t.Setenv("LEVEE_TOKEN", "from-env")
		serveOptToken = "from-flag"
		assert.Equal(t, "from-flag", resolveServeToken())
	})
}

// TestRunServeErrorMessageShape makes sure the guidance text mentions both
// escape hatches (--token and --insecure).
func TestRunServeErrorMessageShape(t *testing.T) {
	resetServeFlags()
	t.Cleanup(resetServeFlags)
	t.Setenv("LEVEE_TOKEN", "")

	err := runServe(findSub("serve"), nil)
	require.Error(t, err)
	msg := err.Error()
	assert.True(t, strings.Contains(msg, "--token"), "message should mention --token")
	assert.True(t, strings.Contains(msg, "--insecure"), "message should mention --insecure")
}

// TestStartDispatchLoopsRefusedInSingleNode pins the contract stated in
// startDispatchAndWorkerLoops' own doc comment ("Both require cluster mode and
// the execution engine"). Without --cluster the manager is nil, and the
// leader-only sweep dereferences it on its first tick — so `serve
// --engine-enabled` on a single node panicked ~10s after startup. The
// assertions are on the out-params rather than on a waited-for tick, so the
// test is deterministic: a removed guard leaves a live loop and fails at once.
func TestStartDispatchLoopsRefusedInSingleNode(t *testing.T) {
	var loop *dispatch.Loop
	var worker *dispatch.WorkerLoop
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := startDispatchAndWorkerLoops(&loop, &worker, nil, nil, nil, "node-single",
		true, time.Millisecond, 4, time.Minute, ctx)

	require.NoError(t, err)
	assert.Nil(t, loop, "no dispatch loop may be started outside cluster mode")
	assert.Nil(t, worker, "no dispatch worker loop may be started outside cluster mode")
}
