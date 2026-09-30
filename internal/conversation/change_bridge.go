package conversation

// change_bridge.go closes the AI loop: a confirmed recommendation becomes a
// real Change record instead of a reassuring sentence.
//
// Until this existed, approving a recommendation in the reviewing state only
// recorded the approval and replied "建议已确认，尚未启动执行" (P2-3) — the
// conversation had no way to reach the change table, so the operator still had
// to retype the draft by hand as a `levee change` invocation. Now the
// recommendation's workflow draft is handed to the same CreateChange path the
// CLI uses, which records it in status `draft` and therefore puts it into the
// standard governance chain: plan → approval → apply.
//
// The bridge is deliberately NOT an execution path. It never runs a workflow,
// it does not bypass approval, and an unapproved draft cannot move. Two
// invariants shape the implementation:
//
//   - Fail closed on the draft. A recommendation's draft is model-authored
//     text. If it does not parse, or fails structural validation (including
//     LE097's rollback-scope rule), no Change is created and the operator is
//     told why, with the session left in reviewing so it can be modified. A
//     Change whose workflow cannot plan is a row nobody can act on.
//   - Depend on nothing above. internal/grpc imports this package (its
//     ConversationService wraps this engine), so the bridge declares the
//     narrow ChangeCreator interface it needs and cmd/levee adapts the real
//     *grpc.ChangeService onto it.

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/recommend"
	"github.com/nexus/levee/internal/runstatus"
)

// ChangeDraft is the minimal description of a change the bridge submits.
type ChangeDraft struct {
	// Label is the human-facing change name shown in `levee change list`.
	Label string
	// WorkflowFile is the LEVEELang document, passed through as an inline
	// document (the change service resolves inline YAML before it touches
	// the filesystem), so the bridge never writes a temp file for text that
	// came out of a conversation.
	WorkflowFile string
	// Params is stored verbatim with the run. It is where the provenance of
	// the change lands — which recommendation produced it, for which target,
	// at whose request — so the audit trail does not depend on the label.
	Params map[string]string
}

// ChangeCreator records a change draft. It is the subset of the change
// management service the bridge calls; *grpc.ChangeService satisfies it
// through the adapter in cmd/levee.
type ChangeCreator interface {
	CreateChangeDraft(ctx context.Context, draft ChangeDraft) (id, status string, err error)
}

// promoteRecommendation compiles rec's workflow draft and, when it is a valid
// LEVEELang document, records it as a draft Change.
//
// Every failure path returns a user-facing Reply rather than an error: the
// session stays in reviewing, so the operator can modify the recommendation,
// retry, or reject it. Only the state transition and the recorded action
// differ between the paths.
func (e *ConversationEngine) promoteRecommendation(ctx context.Context, sess *Session, rec *recommend.Recommendation) (*Reply, error) {
	draft := strings.TrimSpace(rec.WorkflowDraft)
	if draft == "" {
		e.log.Warn("recommendation carries no workflow draft; not creating change",
			"recommendation_id", rec.ID)
		return &Reply{Text: "该建议没有附带工作流草案（workflow_draft 为空），未创建变更。" +
			"可回复「修改」补充执行步骤，或「拒绝」终止建议。"}, nil
	}

	// The same two gates `levee compile` runs in strict mode: parse, then
	// structural validation (which includes the rollback-scope rule LE097, so
	// a draft declaring compensation at workflow level is rejected here rather
	// than discovered at plan time).
	wf, err := dsl.NewParser().ParseBytes([]byte(draft))
	if err != nil {
		e.log.Warn("recommendation draft did not parse; not creating change",
			"recommendation_id", rec.ID, "error", err)
		return &Reply{Text: fmt.Sprintf("工作流草案解析失败，未创建变更：%v\n可回复「修改」调整建议内容。", err)}, nil
	}
	if verrs := dsl.NewValidator().Validate(wf); len(verrs) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "工作流草案未通过校验（%d 项），未创建变更：", len(verrs))
		for _, ve := range verrs {
			fmt.Fprintf(&b, "\n- %s", ve.Error())
		}
		b.WriteString("\n可回复「修改」调整建议内容。")
		e.log.Warn("recommendation draft failed validation; not creating change",
			"recommendation_id", rec.ID, "errors", len(verrs))
		return &Reply{Text: b.String()}, nil
	}

	label := strings.TrimSpace(rec.Summary)
	if label == "" {
		label = "AI 建议 " + rec.ID
	}
	id, status, err := e.changeCreator.CreateChangeDraft(ctx, ChangeDraft{
		Label:        label,
		WorkflowFile: draft,
		Params: map[string]string{
			"recommendation_id": rec.ID,
			"target":            rec.Target,
			"risk_level":        string(rec.RiskLevel),
			"source":            "conversation:recommend",
			"requested_by":      sess.UserID,
		},
	})
	if err != nil {
		// The approval is already recorded, but the handoff failed. Keep the
		// session in reviewing so the operator can retry, and say plainly
		// what broke instead of surfacing a bare transport error.
		e.log.Error("create change from recommendation failed",
			"recommendation_id", rec.ID, "error", err)
		return &Reply{Text: fmt.Sprintf("创建变更失败：%v。会话仍在评审阶段，可重试「执行」或「拒绝」。", err)}, nil
	}
	if status == "" {
		// A create that reports no status has still produced a draft record;
		// name it with the runstatus vocabulary rather than an empty string.
		status = runstatus.StatusDraft
	}

	action := &Action{Type: ActionApprove, Payload: map[string]string{
		"recommendation_id": rec.ID,
		"change_id":         id,
		"change_status":     status,
	}}
	sess.AddMessageWithAction(RoleSystem, "recommendation promoted to change "+id, action)
	// The conversation's job is done — the change now lives in the standard
	// governance chain, and further messages in this session are noise. The
	// next steps are CLI/REST commands, deliberately not auto-issued.
	sess.SetState(StateDone)

	return &Reply{
		Text: fmt.Sprintf("已创建变更 %s（状态 %s）。后续走标准治理链：计划 → 审批 → 应用"+
			"（`levee plan %s`）。审批与应用不会由本对话自动发起。", id, status, id),
		Action: action,
	}, nil
}
