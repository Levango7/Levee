// decision_test.go pins the inbound ChatOps approval path: a message in a
// chat client must record a real decision through the same approval state
// machine the console and CLI use, must refuse an identity that is not an
// approver, and must settle (or honestly report not settling) the run.
//
// The env mirrors internal/grpc's deeplink settle tests: a real SQLite
// store, a real approval.Service and a real ChangeService over the same
// store, because the settle step is the half that decides whether the
// approval actually moves the change.
package chatopsbridge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/state"
)

// decisionEnv bundles one wired test environment.
type decisionEnv struct {
	store     state.Store
	approvals *approval.Service
	changes   *grpc.ChangeService
}

func newDecisionEnv(t *testing.T) *decisionEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := state.NewSQLiteStore(ctx, dir+"/levee-chatops-test.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	apprSvc := approval.NewService(&envStoreAdapter{store: store})
	changes := grpc.NewChangeService(store, nil, apprSvc, nil)
	return &decisionEnv{store: store, approvals: apprSvc, changes: changes}
}

// envStoreAdapter bridges state.Store to approval.Store for these tests.
// It exists because approval.Store is not state.Store: the production
// adapter lives in cmd/levee (a main package), so the bridge package needs
// its own copy of the same six-line mapping rather than importing one.
type envStoreAdapter struct {
	store state.Store
}

func (a *envStoreAdapter) Create(ctx context.Context, ap *approval.Approval) error {
	sa, err := approval.StateApproval(ap)
	if err != nil {
		return err
	}
	return a.store.CreateApproval(ctx, sa)
}

func (a *envStoreAdapter) Get(ctx context.Context, id string) (*approval.Approval, error) {
	sa, err := a.store.GetApproval(ctx, id)
	if err != nil {
		return nil, err
	}
	if sa == nil {
		return nil, nil
	}
	return approval.ApprovalFromState(sa)
}

func (a *envStoreAdapter) Update(ctx context.Context, ap *approval.Approval) error {
	sa, err := approval.StateApproval(ap)
	if err != nil {
		return err
	}
	return a.store.UpdateApproval(ctx, sa)
}

func (a *envStoreAdapter) UpdateIfPending(ctx context.Context, ap *approval.Approval) (bool, error) {
	sa, err := approval.StateApproval(ap)
	if err != nil {
		return false, err
	}
	return a.store.UpdateApprovalIfPending(ctx, sa)
}

func (a *envStoreAdapter) ListPending(ctx context.Context) ([]*approval.Approval, error) {
	rows, err := a.store.ListApprovals(ctx, state.ApprovalFilter{Status: string(approval.StatusPending)})
	if err != nil {
		return nil, err
	}
	out := make([]*approval.Approval, 0, len(rows))
	for _, r := range rows {
		ap, err := approval.ApprovalFromState(r)
		if err != nil {
			continue
		}
		out = append(out, ap)
	}
	return out, nil
}

func (a *envStoreAdapter) ListByRun(ctx context.Context, runID string) ([]*approval.Approval, error) {
	rows, err := a.store.ListApprovals(ctx, state.ApprovalFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	out := make([]*approval.Approval, 0, len(rows))
	for _, r := range rows {
		ap, err := approval.ApprovalFromState(r)
		if err != nil {
			continue
		}
		out = append(out, ap)
	}
	// Newest first, the same total order the production adapter establishes.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			if out[j].CreatedAt.After(out[j-1].CreatedAt) ||
				(out[j].CreatedAt.Equal(out[j-1].CreatedAt) && out[j].ID > out[j-1].ID) {
				out[j], out[j-1] = out[j-1], out[j]
				continue
			}
			break
		}
	}
	return out, nil
}

// seedApproval creates a pending approval for runID with the given
// approvers, plus the run row the settle step needs.
func (env *decisionEnv) seedApproval(t *testing.T, runID string, approvers []string, minApprovers int) *approval.Approval {
	t.Helper()
	require.NoError(t, env.store.CreateRun(context.Background(), &state.Run{
		ID: runID, WorkflowName: "wf", Status: "draft", Creator: approvers[0],
	}))
	a, err := env.approvals.Create(context.Background(), approval.CreateRequest{
		RunID: runID, Level: approval.LevelStandard,
		Approvers: approvers, MinApprovers: minApprovers,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	require.NoError(t, err)
	return a
}

// seedIndependentApproval creates an approval whose initiator is excluded
// from deciding it (the exclude_initiator rule).
func (env *decisionEnv) seedIndependentApproval(t *testing.T, runID, initiator string) *approval.Approval {
	t.Helper()
	require.NoError(t, env.store.CreateRun(context.Background(), &state.Run{
		ID: runID, WorkflowName: "wf", Status: "draft", Creator: initiator,
	}))
	a, err := env.approvals.Create(context.Background(), approval.CreateRequest{
		RunID: runID, Level: approval.LevelHigh,
		Approvers: []string{initiator}, MinApprovers: 1,
		Initiator: initiator, ExcludeInitiator: true,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	require.NoError(t, err)
	return a
}

func chatMsg(user, text string) chatops.IncomingMessage {
	return chatMsgOn(chatops.PlatformFeishu, user, text)
}

func chatMsgOn(platform chatops.Platform, user, text string) chatops.IncomingMessage {
	return chatops.IncomingMessage{
		Platform: platform,
		Channel:  "oc_1",
		User:     user,
		UserName: user,
		Text:     text,
	}
}

// --- approve ---------------------------------------------------------------

func TestDecisionApprove_RecordsVoteAndSettlesRun(t *testing.T) {
	env := newDecisionEnv(t)
	env.seedApproval(t, "run-c1", []string{"alice"}, 1)
	r := NewDecisionRouter(env.approvals, env.changes, MapIdentity(nil))

	res, err := r.approve(context.Background(), chatMsg("alice", "/levee approve run-c1"), []string{"run-c1"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "已记录你对 run-c1 的批准")
	assert.Contains(t, res.Reply, "法定人数已满足")

	run, err := env.store.GetRun(context.Background(), "run-c1")
	require.NoError(t, err)
	assert.Equal(t, "approved", run.Status, "the vote must move the run, not just the approval row")
}

func TestDecisionApprove_PartialQuorumSaysSo(t *testing.T) {
	env := newDecisionEnv(t)
	env.seedApproval(t, "run-c2", []string{"alice", "bob"}, 2)
	r := NewDecisionRouter(env.approvals, env.changes, MapIdentity(nil))

	res, err := r.approve(context.Background(), chatMsg("alice", "/levee approve run-c2"), []string{"run-c2"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "法定人数未到", "a 1/2 vote must not claim approval")
	assert.Contains(t, res.Reply, "1/2")

	run, err := env.store.GetRun(context.Background(), "run-c2")
	require.NoError(t, err)
	assert.Equal(t, "draft", run.Status)
}

func TestDecisionApprove_NonApproverRefusedWithList(t *testing.T) {
	env := newDecisionEnv(t)
	env.seedApproval(t, "run-c3", []string{"alice"}, 1)
	r := NewDecisionRouter(env.approvals, env.changes, MapIdentity(nil))

	res, err := r.approve(context.Background(), chatMsg("mallory", "/levee approve run-c3"), []string{"run-c3"})
	require.NoError(t, err, "a refusal is an answer, not a dispatch failure")
	assert.Contains(t, res.Reply, "不在该变更的审批人名单里")
	assert.Contains(t, res.Reply, "alice", "saying who may is the point")

	run, err := env.store.GetRun(context.Background(), "run-c3")
	require.NoError(t, err)
	assert.Equal(t, "draft", run.Status, "a refused vote must not move the run")
}

func TestDecisionApprove_InitiatorCannotApproveOwnChange(t *testing.T) {
	env := newDecisionEnv(t)
	env.seedIndependentApproval(t, "run-c4", "alice")

	r := NewDecisionRouter(env.approvals, env.changes, MapIdentity(nil))
	res, err := r.approve(context.Background(), chatMsg("alice", "/levee approve run-c4"), []string{"run-c4"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "发起人")
	assert.Contains(t, res.Reply, "独立审批")
}

func TestDecisionApprove_IdentityMappingUsed(t *testing.T) {
	env := newDecisionEnv(t)
	env.seedApproval(t, "run-c5", []string{"alice"}, 1)
	// The chat id is not the LEVEE subject; the table says who it is.
	r := NewDecisionRouter(env.approvals, env.changes,
		MapIdentity(map[chatops.Platform]map[string]string{
			chatops.PlatformFeishu: {"ou_openid_1": "alice"},
		}))

	res, err := r.approve(context.Background(), chatMsg("ou_openid_1", "/levee approve run-c5"), []string{"run-c5"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "alice")

	run, err := env.store.GetRun(context.Background(), "run-c5")
	require.NoError(t, err)
	assert.Equal(t, "approved", run.Status)
}

func TestDecisionApprove_UnresolvedIdentityRefused(t *testing.T) {
	env := newDecisionEnv(t)
	env.seedApproval(t, "run-c6", []string{"alice"}, 1)
	// A resolver that cannot resolve: refuse, do not fall back to a shared
	// subject.
	r := NewDecisionRouter(env.approvals, env.changes,
		func(chatops.IncomingMessage) string { return "" })

	res, err := r.approve(context.Background(), chatMsg("", "/levee approve run-c6"), []string{"run-c6"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "无法确定你的身份")
	assert.Contains(t, res.Reply, "已拒绝")
}

func TestDecisionApprove_NoApprovalRecord(t *testing.T) {
	env := newDecisionEnv(t)
	r := NewDecisionRouter(env.approvals, env.changes, MapIdentity(nil))

	res, err := r.approve(context.Background(), chatMsg("alice", "/levee approve run-ghost"), []string{"run-ghost"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "没有审批记录")
}

func TestDecisionApprove_MissingArgIsUsage(t *testing.T) {
	env := newDecisionEnv(t)
	r := NewDecisionRouter(env.approvals, env.changes, MapIdentity(nil))

	res, err := r.approve(context.Background(), chatMsg("alice", "/levee approve"), nil)
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "用法")
}

// --- reject ----------------------------------------------------------------

func TestDecisionReject_VetoesAndSettles(t *testing.T) {
	env := newDecisionEnv(t)
	env.seedApproval(t, "run-r1", []string{"alice"}, 1)
	r := NewDecisionRouter(env.approvals, env.changes, MapIdentity(nil))

	res, err := r.reject(context.Background(),
		chatMsg("alice", "/levee reject run-r1 --reason 窗口冲突"), []string{"run-r1", "--reason", "窗口冲突"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "一票否决已生效")

	run, err := env.store.GetRun(context.Background(), "run-r1")
	require.NoError(t, err)
	assert.Equal(t, "rejected", run.Status)
}

func TestDecisionReject_RequiresReason(t *testing.T) {
	env := newDecisionEnv(t)
	env.seedApproval(t, "run-r2", []string{"alice"}, 1)
	r := NewDecisionRouter(env.approvals, env.changes, MapIdentity(nil))

	res, err := r.reject(context.Background(), chatMsg("alice", "/levee reject run-r2"), []string{"run-r2"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "驳回必须填写原因")

	a, err := env.approvals.ApprovalForRun(context.Background(), "run-r2")
	require.NoError(t, err)
	assert.Equal(t, approval.StatusPending, a.Status, "a refusal to decide must not decide")
}

func TestDecisionReject_BareTailIsTheReason(t *testing.T) {
	env := newDecisionEnv(t)
	env.seedApproval(t, "run-r3", []string{"alice"}, 1)
	r := NewDecisionRouter(env.approvals, env.changes, MapIdentity(nil))

	// Slack card buttons carry the verb; the tail carries the reason.
	res, err := r.reject(context.Background(),
		chatMsg("alice", "/levee reject run-r3 与备案窗口冲突"), []string{"run-r3", "与备案窗口冲突"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "一票否决已生效")
}

// --- status ----------------------------------------------------------------

func TestDecisionStatus_PendingListsApprovers(t *testing.T) {
	env := newDecisionEnv(t)
	env.seedApproval(t, "run-s1", []string{"alice", "bob"}, 2)
	r := NewDecisionRouter(env.approvals, env.changes, MapIdentity(nil))

	res, err := r.status(context.Background(), chatMsg("alice", "/levee status run-s1"), []string{"run-s1"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "待审批")
	assert.Contains(t, res.Reply, "需要 2 人批准，已收集 0 票")
	assert.Contains(t, res.Reply, "alice, bob")
}

func TestDecisionStatus_AfterDecision(t *testing.T) {
	env := newDecisionEnv(t)
	env.seedApproval(t, "run-s2", []string{"alice"}, 1)
	r := NewDecisionRouter(env.approvals, env.changes, MapIdentity(nil))
	_, err := r.approve(context.Background(), chatMsg("alice", "/levee approve run-s2"), []string{"run-s2"})
	require.NoError(t, err)

	res, err := r.status(context.Background(), chatMsg("alice", "/levee status run-s2"), []string{"run-s2"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "已批准")
	assert.NotContains(t, res.Reply, "/levee approve", "a decided change offers no action buttons")
}

// --- identity --------------------------------------------------------------

func TestMapIdentity_FallsBackToPlatformID(t *testing.T) {
	resolve := MapIdentity(nil)
	assert.Equal(t, "alice", resolve(chatMsg("alice", "")))
	assert.Equal(t, "ou_1", resolve(chatMsg("ou_1", "")))
}

func TestMapIdentity_PerPlatformTables(t *testing.T) {
	resolve := MapIdentity(map[chatops.Platform]map[string]string{
		chatops.PlatformFeishu:   {"ou_1": "alice"},
		chatops.PlatformDingtalk: {"123": "bob"},
	})
	assert.Equal(t, "alice", resolve(chatMsgOn(chatops.PlatformFeishu, "ou_1", "")))
	assert.Equal(t, "bob", resolve(chatMsgOn(chatops.PlatformDingtalk, "123", "")))
	// The same id string on a platform with no table entry stays itself,
	// which is why the table is per platform and not flat.
	assert.Equal(t, "ou_1", resolve(chatMsgOn(chatops.PlatformSlack, "ou_1", "")))
}

func TestMapIdentity_EmptyTableEntryIgnored(t *testing.T) {
	resolve := MapIdentity(map[chatops.Platform]map[string]string{
		chatops.PlatformFeishu: {"ou_1": "  "},
	})
	assert.Equal(t, "ou_1", resolve(chatMsg("ou_1", "")),
		"a blank mapping must not resolve to a blank subject")
}

func TestParseReason(t *testing.T) {
	assert.Equal(t, "窗口冲突", parseReason([]string{"--reason", "窗口冲突"}))
	assert.Equal(t, "窗口冲突", parseReason([]string{"--reason=窗口冲突"}))
	assert.Equal(t, "窗口冲突", parseReason([]string{"窗口冲突"}))
	assert.Equal(t, "", parseReason(nil))
	assert.Equal(t, "", parseReason([]string{"--reason"}))
}

// --- nil dependencies ------------------------------------------------------

func TestDecisionRouter_NilApprovalsRefuses(t *testing.T) {
	r := NewDecisionRouter(nil, nil, MapIdentity(nil))
	res, err := r.approve(context.Background(), chatMsg("alice", "/levee approve run-x"), []string{"run-x"})
	require.NoError(t, err)
	assert.Contains(t, res.Reply, "审批服务未配置")
}

// --- Handlers map ----------------------------------------------------------

func TestDecisionRouter_HandlersReplaceTheNoOps(t *testing.T) {
	r := NewDecisionRouter(nil, nil, MapIdentity(nil))
	verbs := r.Handlers()
	for _, v := range []string{"approve", "reject", "status"} {
		h, ok := verbs[v]
		require.True(t, ok, "verb %s must be backed", v)
		res, err := h(context.Background(), chatMsg("alice", "/levee "+v+" run-x"), []string{"run-x"})
		require.NoError(t, err)
		assert.NotContains(t, res.Reply, "no handler registered",
			"a verb wired to a real handler must not answer with the stub text")
	}
}
