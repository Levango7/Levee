package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/engine"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/wiring"
)

// humanGatedSource declares a human gate, which the plan funnel refuses unless an
// approver transport is installed.
const humanGatedSource = `name: human-gated
version: "1.0"
target:
  type: host
  hosts: ["web-1"]
steps:
  - name: restart
    action: svc.restart
    verify:
      human:
        message: "确认后继续"
`

func serveGateEngine(t *testing.T, store state.Store, svc *approval.Service) *wiring.Engine {
	t.Helper()
	opts, err := buildEngineServeOptions(&config.Config{}, store, nil, nil, svc)
	require.NoError(t, err)
	return wiring.NewEngine(store, opts...)
}

// The door must be in the process, not just in a struct: this drives the real
// serve option builder and the real plan funnel, so a wiring regression (someone
// dropping the option, or reading a nil service) fails here rather than shipping.
func TestServeInstallsAHumanGateApprover(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	require.NoError(t, store.UpsertTarget(ctx, &state.Target{
		ID: "tgt-web-1", Hostname: "web-1", Port: 22, ChannelType: "ssh",
		Status: "active", CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-hgate", WorkflowName: humanGatedSource, Status: "draft",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}))

	svc := approval.NewService(newApprovalStoreAdapter(store))
	eng := serveGateEngine(t, store, svc)
	_, _, err = eng.GeneratePlan(ctx, "run-hgate", []string{"web-1"})
	require.NoError(t, err,
		"serve must install a human-gate approver, or every workflow declaring `human` is unplannable")

	// Control: without the approval service there is no approver, and the SAME
	// workflow is refused. This is what proves the acceptance above came from the
	// wiring rather than from the workflow being trivially plannable.
	engNo := serveGateEngine(t, store, nil)
	_, _, err = engNo.GeneratePlan(ctx, "run-hgate", []string{"web-1"})
	require.ErrorIs(t, err, engine.ErrGateNotExecutable,
		"with no approver the funnel must still refuse, naming the gate")
}
