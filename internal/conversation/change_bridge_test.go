package conversation

// change_bridge_test.go covers the recommendation→change bridge: the cases
// that matter most are the ones where NO change must be created. A
// model-authored draft that does not compile must never leave a Change row
// behind, because that row looks actionable to an operator while nothing can
// ever plan it.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/recommend"
	"github.com/nexus/levee/internal/runstatus"
)

// validBridgeDraft is a trimmed version of the canonical workflow document
// (the same shape internal/dsl's parser tests use): parseable AND
// structurally valid, so it exercises the success path rather than a gate.
const validBridgeDraft = `
name: ai-fix-mvp
version: "1.0"
input:
  - name: pkg_name
    type: string
    required: true
target:
  type: host
  query: "os=linux AND env=prod"
batches:
  strategy: percent
  steps: [1, 10, 100]
approval:
  level: high
steps:
  - name: upgrade
    action: pkg.upgrade
    args:
      name: kernel
      version: 5.15.0-91
  - name: health_check
    action: shell.exec
    args:
      cmd: "uname -r"
    depends_on: ["upgrade"]
    verify:
      cmd:
        run: "uname -r | grep -q 5.15"
        expect_exit: 0
`

// rollbackScopeViolationDraft declares compensation at workflow level, which
// LE097 forbids: it would have to be projected onto every PlanStep, and would
// attribute a snapshot baseline to steps that never created one.
const rollbackScopeViolationDraft = `
name: ai-fix-rollback-scope
target:
  type: host
  query: "os=linux"
steps:
  - name: upgrade
    action: pkg.upgrade
rollback:
  on_failure: auto
  verify_after: true
  strategy: snapshot
  snapshot_paths:
    - /etc/app.conf
`

// fakeChangeCreator records the drafts it is asked to create.
type fakeChangeCreator struct {
	calls  []ChangeDraft
	id     string
	status string
	err    error
}

func (f *fakeChangeCreator) CreateChangeDraft(_ context.Context, draft ChangeDraft) (string, string, error) {
	f.calls = append(f.calls, draft)
	if f.err != nil {
		return "", "", f.err
	}
	id, status := f.id, f.status
	if id == "" {
		id = "chg-bridge-1"
	}
	if status == "" {
		status = runstatus.StatusDraft
	}
	return id, status, nil
}

// newBridgeSession returns a session parked in reviewing with rec attached —
// the state the bridge is reached from.
func newBridgeSession(draft string) *Session {
	sess := newSession("user-1", "alert-1")
	sess.SetRecommendation(&recommend.Recommendation{
		ID:            "rec-1",
		Target:        "web-1",
		Summary:       "回滚导致的服务中断",
		WorkflowDraft: draft,
		RiskLevel:     recommend.RiskHigh,
	})
	sess.SetState(StateReviewing)
	return sess
}

// TestPromoteRecommendationCreatesChange is the happy path: the draft compiles,
// so it becomes a draft Change carrying full provenance, and the session
// finishes (further conversation would be noise).
func TestPromoteRecommendationCreatesChange(t *testing.T) {
	creator := &fakeChangeCreator{}
	e := NewConversationEngine(ConversationEngineConfig{ChangeCreator: creator})
	sess := newBridgeSession(validBridgeDraft)

	reply, err := e.promoteRecommendation(context.Background(), sess, sess.GetRecommendation())
	require.NoError(t, err)
	require.NotNil(t, reply)

	require.Len(t, creator.calls, 1, "exactly one change must be created")
	got := creator.calls[0]
	assert.Equal(t, "回滚导致的服务中断", got.Label, "label carries the recommendation summary")
	assert.Equal(t, strings.TrimSpace(validBridgeDraft), strings.TrimSpace(got.WorkflowFile),
		"the draft is submitted verbatim (trimmed of surrounding blank lines)")
	assert.Equal(t, "rec-1", got.Params["recommendation_id"])
	assert.Equal(t, "web-1", got.Params["target"])
	assert.Equal(t, "user-1", got.Params["requested_by"], "provenance names who asked for it")
	assert.Equal(t, "conversation:recommend", got.Params["source"])

	assert.Contains(t, reply.Text, "chg-bridge-1", "reply names the created change")
	assert.Contains(t, reply.Text, runstatus.StatusDraft, "reply names the change status")
	assert.Equal(t, StateDone, sess.GetState(), "the handoff completes the conversation")
	require.NotNil(t, reply.Action)
	assert.Equal(t, "chg-bridge-1", reply.Action.Payload["change_id"])
	assert.Equal(t, runstatus.StatusDraft, reply.Action.Payload["change_status"],
		"the change is a DRAFT: the bridge must never skip plan or approval")
}

// TestPromoteRecommendationRefusesUncompilableDraft is the fail-closed case
// that matters most: an invalid draft must leave no Change row at all.
func TestPromoteRecommendationRefusesUncompilableDraft(t *testing.T) {
	cases := []struct {
		name       string
		draft      string
		wantSubstr string
	}{
		{"empty draft", "   \n\t ", "没有附带工作流草案"},
		{"unparsable", "name: [unclosed\n  steps: ???", "解析失败"},
		{"workflow-level rollback scope", rollbackScopeViolationDraft, "未通过校验"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creator := &fakeChangeCreator{}
			e := NewConversationEngine(ConversationEngineConfig{ChangeCreator: creator})
			sess := newBridgeSession(tc.draft)

			reply, err := e.promoteRecommendation(context.Background(), sess, sess.GetRecommendation())
			require.NoError(t, err, "a bad draft is a user-facing reply, not a transport error")
			assert.Empty(t, creator.calls, "no change may be created from a draft that does not compile")
			assert.Contains(t, reply.Text, tc.wantSubstr)
			assert.Equal(t, StateReviewing, sess.GetState(),
				"the operator stays in reviewing and can fix the draft")
		})
	}
}

// TestPromoteRecommendationSurfacesCreateFailure keeps the session usable when
// the change service itself fails: the approval is recorded, but the operator
// is told the handoff did not happen and can retry.
func TestPromoteRecommendationSurfacesCreateFailure(t *testing.T) {
	creator := &fakeChangeCreator{err: errors.New("store unavailable")}
	e := NewConversationEngine(ConversationEngineConfig{ChangeCreator: creator})
	sess := newBridgeSession(validBridgeDraft)

	reply, err := e.promoteRecommendation(context.Background(), sess, sess.GetRecommendation())
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "store unavailable", "the real reason is shown")
	assert.Equal(t, StateReviewing, sess.GetState(), "a failed handoff is retryable")
}

// TestReviewingConfirmCreatesChange exercises the wiring: the "执行" branch of
// the reviewing state must route through the bridge, not the old dead end.
func TestReviewingConfirmCreatesChange(t *testing.T) {
	creator := &fakeChangeCreator{}
	e := NewConversationEngine(ConversationEngineConfig{ChangeCreator: creator})
	sess := newBridgeSession(validBridgeDraft)

	reply, err := e.handleReviewing(context.Background(), sess, "执行")
	require.NoError(t, err)
	require.Len(t, creator.calls, 1, "confirming must create the change")
	assert.Contains(t, reply.Text, "已创建变更")
	assert.Equal(t, StateDone, sess.GetState())
}

// TestReviewingConfirmWithoutBridgeKeepsHonestReply pins the degraded path: a
// deployment without a change creator must not claim anything was submitted,
// and must not leave the session in a state that suggests execution started.
func TestReviewingConfirmWithoutBridgeKeepsHonestReply(t *testing.T) {
	e := NewConversationEngine(ConversationEngineConfig{})
	sess := newBridgeSession(validBridgeDraft)

	reply, err := e.handleReviewing(context.Background(), sess, "执行")
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "尚未启动执行")
	assert.NotContains(t, reply.Text, "已创建变更")
	assert.Equal(t, StateReviewing, sess.GetState())
}
