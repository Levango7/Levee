package wiring

// inline_change_plan_test.go is the end-to-end check for the recommendation →
// Change bridge.
//
// The bridge writes the recommendation's workflow into the run row as INLINE
// yaml — there is no file behind a conversation — while the plan path accepts
// a run row whose workflow source may be either inline yaml (template
// instantiation) or a file path (CreateChange). Nothing in the bridge's own
// unit tests can see whether those two ends agree, and the failure mode if they
// do not is the worst kind: a change that looks real, sits in the operator's
// queue under a plausible label, and only fails later at plan time.
//
// So both halves here are real: the real ChangeService over a real SQLite
// store, and the real plan-time resolver and plan generator. This file is in
// package wiring (not wiring_test) because resolveWorkflow is unexported, and
// the point is to exercise the exact function production calls.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/recommend"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

// bridgedWorkflowDraft is a minimal but complete LEVEELang document: valid
// enough to pass the bridge's own gate and the plan generator's.
const bridgedWorkflowDraft = `
name: ai-fix-e2e
version: "1.0"
target:
  type: host
  query: "os=linux"
batches:
  strategy: percent
  steps: [1, 10, 100]
steps:
  - name: upgrade
    action: pkg.upgrade
  - name: health_check
    action: shell.exec
    depends_on: ["upgrade"]
`

// bridgedRollbackScopeViolation declares compensation at workflow level
// (LE097): the bridge must refuse it, and nothing may reach the store.
const bridgedRollbackScopeViolation = `
name: ai-fix-e2e-bad
target:
  type: host
  query: "os=linux"
steps:
  - name: upgrade
    action: pkg.upgrade
rollback:
  on_failure: auto
  strategy: snapshot
  snapshot_paths:
    - /etc/app.conf
`

// newBridgeE2E wires a real store, a real ChangeService and a conversation
// engine whose bridge is the real adapter — no stubs anywhere in the path from
// the operator's keystroke to the persisted row.
func newBridgeE2E(t *testing.T) (state.Store, *grpc.ChangeService, *conversation.ConversationEngine) {
	t.Helper()
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "levee-bridge-e2e.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	changeSvc := grpc.NewChangeService(store, nil, nil, nil)
	engine := conversation.NewConversationEngine(conversation.ConversationEngineConfig{
		ChangeCreator: grpc.NewConversationChangeCreator(changeSvc),
	})
	t.Cleanup(func() { _ = engine.Close() })
	return store, changeSvc, engine
}

// reviewingSessionWith parks a session in reviewing with the given draft
// attached — the state the operator's 「执行」 arrives in.
func reviewingSessionWith(t *testing.T, e *conversation.ConversationEngine, draft string) *conversation.Session {
	t.Helper()
	sess, err := e.NewSession("user-e2e")
	require.NoError(t, err)
	sess.SetRecommendation(&recommend.Recommendation{
		ID:            "rec-e2e",
		Target:        "web-1",
		Summary:       "回滚导致的服务中断",
		WorkflowDraft: draft,
		RiskLevel:     recommend.RiskHigh,
	})
	sess.SetState(conversation.StateReviewing)
	return sess
}

// TestBridgedChangeIsPlannableEndToEnd walks the whole loop once: a confirmed
// recommendation becomes a persisted draft, and the plan path can read that
// draft back and build a plan from it.
func TestBridgedChangeIsPlannableEndToEnd(t *testing.T) {
	ctx := context.Background()
	store, _, engine := newBridgeE2E(t)
	sess := reviewingSessionWith(t, engine, bridgedWorkflowDraft)

	reply, err := engine.HandleMessage(ctx, sess.ID, "user-e2e", "执行")
	require.NoError(t, err)
	require.NotNil(t, reply.Action)
	changeID := reply.Action.Payload["change_id"]
	require.NotEmpty(t, changeID, "confirming must hand a real change id back to the caller")
	assert.Equal(t, conversation.StateDone, sess.GetState(), "the handoff completes the conversation")

	// Half one: a real, auditable draft row.
	run, err := store.GetRun(ctx, changeID)
	require.NoError(t, err)
	require.NotNil(t, run, "the conversation must have produced a persisted change")
	assert.Equal(t, runstatus.StatusDraft, run.Status,
		"the bridge stops at draft — planning and approval are not its business")
	assert.Equal(t, strings.TrimSpace(bridgedWorkflowDraft), run.WorkflowName,
		"the document is stored verbatim, so what the operator reviewed is what was approved")
	assert.NotEmpty(t, run.Creator, "audit attribution must never be blank")

	var params map[string]any
	require.NoError(t, json.Unmarshal([]byte(run.Params), &params))
	assert.Equal(t, "rec-e2e", params["recommendation_id"],
		"provenance survives the store round-trip, so audit never depends on the label")
	assert.Equal(t, "web-1", params["target"])
	assert.Equal(t, "user-e2e", params["requested_by"])

	// Half two: the plan path can read that row back. This is the assertion the
	// bridge's unit tests structurally cannot make.
	wf, err := resolveWorkflow(run)
	require.NoError(t, err, "a bridged draft (inline yaml) must resolve at plan time")
	assert.Equal(t, "ai-fix-e2e", wf.Meta.Name)

	generated, err := plan.NewGenerator().Generate(wf, []string{"web-1"})
	require.NoError(t, err, "a bridged draft must survive the plan-time gate (LE097 rollback scope)")
	assert.NotEmpty(t, generated.Batches, "the plan is a real plan, not an empty shell")
}

// TestBridgedInvalidDraftLeavesNoChangeRow is the fail-closed half, asserted
// against the real store instead of a stub: a draft that violates the
// rollback-scope rule must not leave a row an operator could mistake for
// something plannable, and must not report a change id.
func TestBridgedInvalidDraftLeavesNoChangeRow(t *testing.T) {
	ctx := context.Background()
	_, changeSvc, engine := newBridgeE2E(t)
	sess := reviewingSessionWith(t, engine, bridgedRollbackScopeViolation)

	reply, err := engine.HandleMessage(ctx, sess.ID, "user-e2e", "执行")
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "未通过校验", "the operator is told why, not left guessing")
	// The refused reply carries no action and no change id: nothing was
	// completed, and the approval itself is already in the session history.
	// What matters is that a refused draft is never announced as created.
	assert.NotContains(t, reply.Text, "已创建变更")
	if reply.Action != nil {
		assert.Empty(t, reply.Action.Payload["change_id"], "no change id may be reported for a refused draft")
	}
	assert.Equal(t, conversation.StateReviewing, sess.GetState(),
		"the operator can fix the draft and retry")

	listed, err := changeSvc.ListChanges(ctx, &pb.ListChangesRequest{})
	require.NoError(t, err)
	assert.Empty(t, listed.GetChanges(),
		"nothing may reach the store for a draft the bridge refused")
}
