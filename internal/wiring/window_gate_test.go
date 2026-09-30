package wiring

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// windowedWorkflow renders a complete, plannable workflow whose window is
// derived from the current UTC instant, so the gate is exercised the way an
// operator's clock would exercise it instead of depending on a fixed date.
// `open` picks a window containing now (now-30m..now+30m) or excluding it
// (now+2h..now+3h); days stays empty (every day), which keeps both correct
// across midnight.
func windowedWorkflow(name string, open bool) string {
	now := time.Now().UTC()
	var start, end time.Time
	if open {
		start, end = now.Add(-30*time.Minute), now.Add(30*time.Minute)
	} else {
		start, end = now.Add(2*time.Hour), now.Add(3*time.Hour)
	}
	return "name: " + name + "\n" +
		"target:\n  type: host\n  hosts: [\"web-1\"]\n" +
		"window:\n  start: \"" + start.Format("15:04") + "\"\n  end: \"" + end.Format("15:04") + "\"\n" +
		"steps:\n  - name: restart\n    action: svc.restart\n"
}

const workflowWithBadWindow = `name: bad-window
target:
  type: host
  hosts: ["web-1"]
window:
  start: "25:99"
  end: "06:00"
steps:
  - name: restart
    action: svc.restart
`

func planForWindow(t *testing.T, src string) (*Engine, state.Store, string) {
	t.Helper()
	eng, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-win", src)
	return eng, store, "run-win"
}

func TestGeneratePlan_RefusedOutsideWindow(t *testing.T) {
	ctx := context.Background()
	eng, store, runID := planForWindow(t, windowedWorkflow("closed-window", false))

	p, stored, err := eng.GeneratePlan(ctx, runID, []string{"web-1"})
	require.Error(t, err, "a change outside its declared window must not produce a plan")
	assert.Nil(t, p)
	assert.Nil(t, stored)
	assert.True(t, errors.Is(err, dsl.ErrWindowClosed),
		"the refusal must be matchable as a window refusal, got: %v", err)
	assert.Contains(t, err.Error(), "change window is closed")
	assert.Contains(t, err.Error(), "window", "the message has to quote the declared window")

	// No artifact may exist behind the refusal: a persisted plan is what apply
	// executes, and a plan the operator never got would be invisible drift.
	run, err := store.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Empty(t, run.PlanJSON, "a refused plan leaves no artifact on the run")
	assert.Empty(t, run.PlanHash)
}

func TestGeneratePlan_AllowedInsideWindow(t *testing.T) {
	ctx := context.Background()
	eng, _, runID := planForWindow(t, windowedWorkflow("open-window", true))

	p, stored, err := eng.GeneratePlan(ctx, runID, []string{"web-1"})
	require.NoError(t, err, "now is inside the declared window")
	require.NotNil(t, p)
	require.NotNil(t, stored)
	assert.NotEmpty(t, stored.Hash)
	assert.NotEmpty(t, p.Batches, "the window gate must not change the plan itself")
}

// TestGeneratePlan_NoWindowKeepsPreviousBehaviour is the compatibility guard:
// most existing workflows declare no window, and for them planning must be
// what it was before the gate existed.
func TestGeneratePlan_NoWindowKeepsPreviousBehaviour(t *testing.T) {
	ctx := context.Background()
	eng, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-nowin", testWorkflowYAML)

	_, stored, err := eng.GeneratePlan(ctx, "run-nowin", []string{"web-1"})
	require.NoError(t, err, "an undeclared window means no constraint (§4.2)")
	require.NotNil(t, stored)
}

func TestGeneratePlan_UnusableWindowFailsClosed(t *testing.T) {
	// The plan path never runs the validator, so a garbage window can reach the
	// gate. "Cannot judge" must not become "allowed" — that is how a declared
	// constraint turns inert again.
	ctx := context.Background()
	eng, _, runID := planForWindow(t, workflowWithBadWindow)

	_, _, err := eng.GeneratePlan(ctx, runID, []string{"web-1"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, dsl.ErrWindowClosed), "got: %v", err)
	assert.Contains(t, err.Error(), "cannot be judged")
}

func TestGeneratePlan_WindowRefusalIsNotMaskedByInventory(t *testing.T) {
	// The gate sits before inventory validation, so the operator hears about
	// the closed window rather than about the host list.
	ctx := context.Background()
	eng, _, runID := planForWindow(t, windowedWorkflow("closed-and-unknown-host", false))

	_, _, err := eng.GeneratePlan(ctx, runID, []string{"not-in-inventory"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, dsl.ErrWindowClosed),
		"the window refusal must win, got: %v", err)
}

// TestPlanChange_ReportsWindowRefusalAsFailedPrecondition pins the RPC shape:
// a closed window is a refusal the caller can act on, not a server fault.
func TestPlanChange_ReportsWindowRefusalAsFailedPrecondition(t *testing.T) {
	ctx := context.Background()
	eng, store, runID := planForWindow(t, windowedWorkflow("grpc-closed", false))
	svc := grpc.NewChangeService(store, eng.Adapter(), nil, nil)

	_, err := svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId:    runID,
		TargetHosts: []string{"web-1"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Convert(err).Code(),
		"a window refusal must not surface as Internal, got: %v", err)
	assert.Contains(t, err.Error(), "change window is closed")
}

func TestPlanChange_InsideWindowSucceeds(t *testing.T) {
	ctx := context.Background()
	eng, store, runID := planForWindow(t, windowedWorkflow("grpc-open", true))
	svc := grpc.NewChangeService(store, eng.Adapter(), nil, nil)

	p, err := svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId:    runID,
		TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)
	require.NotNil(t, p)

	run, err := store.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.NotEmpty(t, run.PlanJSON, "an inside-window plan is persisted as usual")
}

// TestWindowGateHasExactlyOneEnforcementPoint guards the two structural claims
// this fix rests on: every plan path passes the gate (so no entry point can
// mint an artifact outside a window), and rollback never does (so a failed
// change is always recoverable, as §4.2 requires). Source-scanning is how this
// repository already pins other cross-layer invariants.
func TestWindowGateHasExactlyOneEnforcementPoint(t *testing.T) {
	hits := map[string]int{}
	rollbackBody := ""

	for _, root := range []string{"../../internal", "../../cmd"} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel := relativePOSIX(path)
			if n := strings.Count(string(src), ".OpenAt("); n > 0 {
				hits[rel] = n
			}
			if strings.HasSuffix(rel, "internal/wiring/run.go") {
				rollbackBody = functionBody(string(src), "func (e *Engine) rollbackChange")
			}
			return nil
		})
	}

	assert.Equal(t, map[string]int{"internal/wiring/plan.go": 1}, hits,
		"OpenAt must be consulted by exactly one production call site: the plan funnel")
	require.NotEmpty(t, rollbackBody, "rollbackChange must be found to prove it stays outside the gate")
	assert.NotContains(t, rollbackBody, "GeneratePlan",
		"rollback loads the stored plan; regenerating it would put recovery behind a time gate")
	assert.NotContains(t, rollbackBody, "OpenAt")
}

// relativePOSIX renders a walk path as repo-root-relative POSIX so the
// assertion stays readable on Windows and Linux alike.
func relativePOSIX(path string) string {
	out := filepath.ToSlash(path)
	for strings.HasPrefix(out, "../") {
		out = strings.TrimPrefix(out, "../")
	}
	return out
}

// functionBody returns src from the given signature up to the next top-level
// func, which is the whole body for this file's layout.
func functionBody(src, signature string) string {
	i := strings.Index(src, signature)
	if i < 0 {
		return ""
	}
	rest := src[i+len(signature):]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		return rest[:j]
	}
	return rest
}
