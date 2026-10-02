// The decision-observer slot used to be last-write-wins: serve gives it to
// the Jira mirror, so wiring a second mirror (the ChatOps approval bridge)
// would have SILENTLY DISPLACED Jira. These tests pin the fan-out: every
// install is kept, none displaces another, nil removes all, and one
// panicking mirror neither fails the decision nor blocks the healthy ones.
package approval

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithDecisionObserver_FanOutKeepsEveryInstall(t *testing.T) {
	svc, _ := newService(t)

	var fired []string
	svc.WithDecisionObserver(func(a *Approval, action string) { fired = append(fired, "first:"+action) })
	svc.WithDecisionObserver(func(a *Approval, action string) { fired = append(fired, "second:"+action) })

	created, err := svc.Create(bgCtx(), CreateRequest{
		RunID: "run-obs-1", Level: "standard", Approvers: []string{"alice"}, MinApprovers: 1,
	})
	require.NoError(t, err)
	require.NoError(t, svc.Approve(bgCtx(), created.ID, "alice"))

	assert.Equal(t, []string{"first:approve", "second:approve"}, fired,
		"both observers must fire — fan-out, not displacement")
}

func TestWithDecisionObserver_NilRemovesAll(t *testing.T) {
	svc, _ := newService(t)

	fired := 0
	svc.WithDecisionObserver(func(a *Approval, action string) { fired++ })
	svc.WithDecisionObserver(nil)

	created, err := svc.Create(bgCtx(), CreateRequest{
		RunID: "run-obs-2", Level: "standard", Approvers: []string{"alice"}, MinApprovers: 1,
	})
	require.NoError(t, err)
	require.NoError(t, svc.Approve(bgCtx(), created.ID, "alice"))

	assert.Equal(t, 0, fired, "nil must remove every installed observer")
}

func TestWithDecisionObserver_PanicDoesNotBreakDecision(t *testing.T) {
	svc, _ := newService(t)

	healthy := 0
	svc.WithDecisionObserver(func(a *Approval, action string) { panic("mirror on fire") })
	svc.WithDecisionObserver(func(a *Approval, action string) { healthy++ })

	created, err := svc.Create(bgCtx(), CreateRequest{
		RunID: "run-obs-3", Level: "standard", Approvers: []string{"alice"}, MinApprovers: 1,
	})
	require.NoError(t, err)
	require.NoError(t, svc.Approve(bgCtx(), created.ID, "alice"),
		"one panicking mirror must not fail the decision")
	assert.Equal(t, 1, healthy, "the healthy mirror must still fire after the panicking one")
}
