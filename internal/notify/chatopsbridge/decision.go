// decision.go is the inbound half of the ChatOps approval mirror. The
// bridge's outgoing half (bridge.go) projects approval lifecycle into the
// chat room; this half is what makes the room able to ACT: it backs the
// /levee approve / reject / status verbs with the same approval state
// machine the console, the mobile deep link and the CLI use.
//
// Until this file the verbs registered on the command router were the
// built-in no-ops ("approve: no handler registered"): the adapters parse
// platform callbacks and the cards carry 通过/驳回 buttons, but a click
// reached a router with nothing behind it. Three properties are load-bearing
// here:
//
//   - The chat identity is resolved explicitly (mapping table, then
//     passthrough) and the approval service still enforces "this subject
//     must be an approver of this record". Chat is a new INPUT, not a new
//     authority.
//   - An unresolved or unmapped identity is refused, never guessed into a
//     subject. A chat id that happens to look like a username approving a
//     change is a misattribution waiting to be discovered in the audit log.
//   - The decision is recorded first and the run settled second; a settle
//     failure is reported as such (the vote is durable, the run is not
//     mirrored) rather than swallowed.
package chatopsbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/log"
)

// IdentityResolver maps a chat message to the LEVEE subject it acts as.
// It is a parameter (not a fixed rule) because the mapping from a
// platform-native user id (Feishu open_id, Slack member id, DingTalk
// staffId) to a LEVEE subject is a deployment decision: some deployments
// keep them identical, others keep a table.
type IdentityResolver func(msg chatops.IncomingMessage) string

// MapIdentity returns a resolver over per-platform tables (each bot
// carries its own, since ids are only unique within a platform). The
// fallback to the platform id itself is deliberate: a deployment whose IM
// ids already equal usernames should not need a table, and an unmapped id
// that is NOT an approver is refused by the approval service anyway — the
// failure mode of a missing entry is a refused vote, never an accepted one.
func MapIdentity(tables map[chatops.Platform]map[string]string) IdentityResolver {
	return func(msg chatops.IncomingMessage) string {
		key := strings.TrimSpace(msg.User)
		if key == "" {
			return ""
		}
		if table, ok := tables[msg.Platform]; ok {
			if subject, ok := table[key]; ok && strings.TrimSpace(subject) != "" {
				return strings.TrimSpace(subject)
			}
		}
		return key
	}
}

// DecisionRouter backs the chat command verbs with the approval state
// machine and the run settlement path. A nil approval service turns every
// verb into an explanatory refusal instead of a panic.
type DecisionRouter struct {
	approvals *approval.Service
	changes   *grpc.ChangeService
	resolve   IdentityResolver
}

// NewDecisionRouter wires the verbs. resolve may be nil (every decision is
// then refused for want of an identity — safer than defaulting to a shared
// subject).
func NewDecisionRouter(appr *approval.Service, change *grpc.ChangeService, resolve IdentityResolver) *DecisionRouter {
	if resolve == nil {
		resolve = func(chatops.IncomingMessage) string { return "" }
	}
	return &DecisionRouter{approvals: appr, changes: change, resolve: resolve}
}

// Handlers returns the verb → handler map to register on the command
// router (mgr.Router().Register(verb, h) for each entry). It replaces the
// built-in no-ops; verbs returned here are the ones a chat client can
// actually act on.
func (r *DecisionRouter) Handlers() map[string]chatops.CommandHandler {
	return map[string]chatops.CommandHandler{
		"approve": r.approve,
		"reject":  r.reject,
		"status":  r.status,
	}
}

func (r *DecisionRouter) approve(ctx context.Context, msg chatops.IncomingMessage, args []string) (chatops.CommandResult, error) {
	runID := strings.TrimSpace(firstArg(args))
	if runID == "" {
		return chatops.CommandResult{Reply: "用法：/levee approve <变更 ID>"}, nil
	}
	subject, refusal := r.subject(msg)
	if refusal != "" {
		return chatops.CommandResult{Reply: refusal}, nil
	}
	if r.approvals == nil {
		return chatops.CommandResult{Reply: "审批服务未配置，无法记录审批。"}, nil
	}

	a, refusal := r.pendingApproval(ctx, runID)
	if refusal != "" {
		return chatops.CommandResult{Reply: refusal}, nil
	}
	if err := r.approvals.Approve(ctx, a.ID, subject); err != nil {
		return chatops.CommandResult{Reply: decisionErrorText(err, subject, a)}, nil
	}

	reply := fmt.Sprintf("已记录你对 %s 的批准（%s）。", runID, subject)
	settled, serr := r.settle(ctx, runID)
	if serr != nil {
		// The vote is durable; the run has not moved. Say that rather than
		// implying the change can proceed.
		log.Warn("chatops: approve settle failed (decision is durable; run state not mirrored)",
			"run_id", runID, "subject", subject, "error", serr)
		reply += "注意：run 状态同步失败，批准已落账但变更不会自动放行。"
	} else if settled == grpc.SettlePending {
		// Count from a FRESH read: the record this handler read before
		// voting is the pre-decision snapshot, and reporting its vote count
		// would tell the room "0/2" to an operator who just cast the first
		// vote — the same "settlement moment vs current state" distinction
		// the mobile deep link and the takeover timeline had to learn.
		votes, need := r.voteCount(ctx, runID, a.MinApprovers)
		reply += fmt.Sprintf("法定人数未到（%d/%d），还需其他审批人。", votes, need)
	} else {
		reply += "法定人数已满足，变更可以执行。"
	}
	return chatops.CommandResult{Reply: reply}, nil
}

func (r *DecisionRouter) reject(ctx context.Context, msg chatops.IncomingMessage, args []string) (chatops.CommandResult, error) {
	runID := strings.TrimSpace(firstArg(args))
	if runID == "" {
		return chatops.CommandResult{Reply: "用法：/levee reject <变更 ID> --reason <原因>"}, nil
	}
	subject, refusal := r.subject(msg)
	if refusal != "" {
		return chatops.CommandResult{Reply: refusal}, nil
	}
	if r.approvals == nil {
		return chatops.CommandResult{Reply: "审批服务未配置，无法记录审批。"}, nil
	}

	reason := parseReason(args[1:])
	if reason == "" {
		return chatops.CommandResult{Reply: "驳回必须填写原因：/levee reject <变更 ID> --reason <原因>。"}, nil
	}

	a, refusal := r.pendingApproval(ctx, runID)
	if refusal != "" {
		return chatops.CommandResult{Reply: refusal}, nil
	}
	if err := r.approvals.Reject(ctx, a.ID, subject, reason); err != nil {
		return chatops.CommandResult{Reply: decisionErrorText(err, subject, a)}, nil
	}

	// One-vote veto: the rejection always settles the run.
	reply := fmt.Sprintf("已记录你对 %s 的驳回（%s，原因：%s）。一票否决已生效。", runID, subject, reason)
	if _, serr := r.settle(ctx, runID); serr != nil {
		log.Warn("chatops: reject settle failed (decision is durable; run state not mirrored)",
			"run_id", runID, "subject", subject, "error", serr)
		reply += "注意：run 状态同步失败，驳回已落账但变更不会自动中止。"
	}
	return chatops.CommandResult{Reply: reply}, nil
}

func (r *DecisionRouter) status(_ context.Context, _ chatops.IncomingMessage, args []string) (chatops.CommandResult, error) {
	runID := strings.TrimSpace(firstArg(args))
	if runID == "" {
		return chatops.CommandResult{Reply: "用法：/levee status <变更 ID>"}, nil
	}
	if r.approvals == nil {
		return chatops.CommandResult{Reply: "审批服务未配置，无法查询审批状态。"}, nil
	}
	a, err := r.approvals.ApprovalForRun(context.Background(), runID)
	if err != nil {
		return chatops.CommandResult{Reply: fmt.Sprintf("查询 %s 审批状态失败：%v", runID, err)}, nil
	}
	if a == nil {
		return chatops.CommandResult{Reply: fmt.Sprintf("%s 当前没有审批记录。", runID)}, nil
	}

	lines := []string{
		fmt.Sprintf("%s 审批状态：%s", runID, statusLabel(a.Status)),
		fmt.Sprintf("等级 %s，需要 %d 人批准，已收集 %d 票。", a.Level, a.MinApprovers, approveVotes(a)),
		fmt.Sprintf("审批人：%s", strings.Join(a.Approvers, ", ")),
	}
	if !a.ExpiresAt.IsZero() {
		lines = append(lines, fmt.Sprintf("有效期至 %s。", a.ExpiresAt.UTC().Format(time.RFC3339)))
	}
	if a.Status == approval.StatusPending {
		lines = append(lines, "操作：/levee approve <ID> 或 /levee reject <ID> --reason <原因>")
	}
	return chatops.CommandResult{Reply: strings.Join(lines, "\n")}, nil
}

// --- helpers ---------------------------------------------------------------

// subject resolves the acting identity or explains why there is none. An
// empty resolution is refused, not defaulted: the audit log must be able
// to name who decided.
func (r *DecisionRouter) subject(msg chatops.IncomingMessage) (string, string) {
	subject := strings.TrimSpace(r.resolve(msg))
	if subject == "" {
		return "", "无法确定你的身份（未配置身份映射），已拒绝本次操作。"
	}
	return subject, ""
}

// pendingApproval returns the run's approval record when one exists and is
// still awaiting decisions.
func (r *DecisionRouter) pendingApproval(ctx context.Context, runID string) (*approval.Approval, string) {
	a, err := r.approvals.ApprovalForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Sprintf("查询 %s 的审批记录失败：%v", runID, err)
	}
	if a == nil {
		return nil, fmt.Sprintf("%s 没有审批记录，可能已经处理或未到审批环节。", runID)
	}
	if a.Status != approval.StatusPending {
		return nil, fmt.Sprintf("%s 的审批已终结（%s），不能重复决定。", runID, statusLabel(a.Status))
	}
	return a, ""
}

func (r *DecisionRouter) settle(ctx context.Context, runID string) (grpc.SettleState, error) {
	if r.changes == nil {
		return grpc.SettlePending, nil
	}
	return r.changes.SettleApproval(ctx, runID)
}

// voteCount reads the record back and returns (approve votes, quorum). A
// failed read degrades to the quorum alone with zero votes rather than
// failing the whole reply: the decision itself is already durable.
func (r *DecisionRouter) voteCount(ctx context.Context, runID string, fallbackNeed int) (int, int) {
	a, err := r.approvals.ApprovalForRun(ctx, runID)
	if err != nil || a == nil {
		return 0, fallbackNeed
	}
	return approveVotes(a), a.MinApprovers
}

// decisionErrorText turns an approval-service refusal into something a
// human in a chat window can act on. ErrUnauthorizedApprover names the
// approver list, because "you may not" without "who may" sends people
// looking for a permission that is not missing. ErrInitiatorExcluded gets
// its own line: it is a governance rule (the author may not approve their
// own change), not a missing permission, and conflating the two sends the
// operator hunting for a grant that must never be issued.
func decisionErrorText(err error, subject string, a *approval.Approval) string {
	switch {
	case errors.Is(err, approval.ErrInitiatorExcluded):
		return fmt.Sprintf("已拒绝：%s 是本变更的发起人，该等级要求独立审批（本人不能批自己的变更）。", subject)
	case errors.Is(err, approval.ErrUnauthorizedApprover):
		return fmt.Sprintf("已拒绝：%s 不在该变更的审批人名单里（%s）。", subject, strings.Join(a.Approvers, ", "))
	case errors.Is(err, approval.ErrDuplicateDecision):
		return "你已经投过票了，同一位审批人不能重复决定。"
	case errors.Is(err, approval.ErrInvalidTransition):
		return "该审批已终结，不能重复决定。"
	case errors.Is(err, approval.ErrNotFound):
		return "找不到对应的审批记录。"
	default:
		return fmt.Sprintf("记录决定失败：%v", err)
	}
}

func approveVotes(a *approval.Approval) int {
	votes := 0
	for _, d := range a.Decisions {
		if d.Action == approval.ActionApprove {
			votes++
		}
	}
	return votes
}

func statusLabel(s approval.Status) string {
	switch s {
	case approval.StatusApproved:
		return "已批准"
	case approval.StatusRejected:
		return "已驳回"
	case approval.StatusExpired:
		return "已过期"
	default:
		return "待审批"
	}
}

// firstArg returns the first positional argument (the change id).
func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// parseReason extracts a rejection reason from the argument tail. It
// accepts "--reason <text>", "--reason=<text>" and a bare tail. A flag with
// no value is an absent reason, not a reason that happens to spell the
// flag: falling back to the raw tail there would store "--reason" as the
// rejection cause in the audit record.
func parseReason(args []string) string {
	rest := strings.TrimSpace(strings.Join(args, " "))
	if rest == "" {
		return ""
	}
	if i := strings.Index(rest, "--reason"); i >= 0 {
		tail := strings.TrimSpace(strings.TrimPrefix(rest[i:], "--reason"))
		tail = strings.TrimSpace(strings.TrimPrefix(tail, "="))
		if tail != "" {
			return tail
		}
		return ""
	}
	return rest
}
