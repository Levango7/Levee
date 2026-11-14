// markdown.go renders the DB Change Ticket as a document a human can be
// handed: the same facts the terminal renderer prints, in a layout that
// survives being pasted into a ticket, a mail or a compliance binder.
//
// Why a renderer in the read-model package and not in the CLI: the same
// document has to come out of two places — `levee dbchange show --format
// markdown` for an operator with the binary, and `GET /changes/<id>/ticket?
// format=markdown` for a browser that wants to print what it shows. Two
// renderers would drift, and a drifting document is worse than no document:
// the printed sheet would no longer say what the screen says.
//
// The honesty rules of the ticket carry over unchanged. A section that cannot
// be read says "unknown"; it never says "none". An empty SQL list and an
// unreadable plan artifact are two different facts and the document keeps
// them apart — the same distinction the CLI renderer and the three-state
// read model exist for.

package dbchange

import (
	"fmt"
	"strings"
	"time"
)

// MarkdownOptions tunes the document's non-factual parts.
//
// The factual content is fixed by the ticket; these knobs only decide how the
// document labels itself and where it came from.
type MarkdownOptions struct {
	// GeneratedAt stamps the document. Zero means "now" — the document is a
	// point-in-time statement, and saying when it was made is part of what
	// makes it quotable.
	GeneratedAt time.Time
	// SourceCommand is the command a reader can run to regenerate the
	// document. Empty omits the footer line.
	SourceCommand string
}

// Markdown renders the ticket as a Markdown document.
//
// The output is deterministic apart from the generation stamp: same ticket,
// same document. That is what makes it diffable — re-running the export for
// an unchanged change produces no diff, and any change in the diff is a
// change in the facts.
func Markdown(tk *Ticket, opts MarkdownOptions) string {
	if tk == nil {
		return ""
	}
	generated := opts.GeneratedAt
	if generated.IsZero() {
		generated = time.Now().UTC()
	}

	var b strings.Builder
	markdownHeader(&b, tk, generated)
	markdownSQL(&b, tk)
	markdownRollback(&b, tk)
	markdownWindow(&b, tk)
	markdownApproval(&b, tk)
	markdownExecution(&b, tk)
	markdownAudit(&b, tk)
	markdownFooter(&b, opts)
	return b.String()
}

// markdownHeader writes the identity block: who, what, which artifact, when.
func markdownHeader(b *strings.Builder, tk *Ticket, generated time.Time) {
	c := tk.Change
	fmt.Fprintf(b, "# 数据库变更发布单\n\n")
	fmt.Fprintf(b, "- 变更 ID: `%s`\n", c.ID)
	if c.Label != "" {
		fmt.Fprintf(b, "- 名称: %s\n", c.Label)
	}
	fmt.Fprintf(b, "- 状态: `%s`\n", c.Status)
	if c.Priority != "" {
		fmt.Fprintf(b, "- 优先级: %s\n", c.Priority)
	}
	if c.Environment != "" {
		fmt.Fprintf(b, "- 环境: %s\n", c.Environment)
	}
	if c.Creator != "" {
		fmt.Fprintf(b, "- 发起人: %s\n", c.Creator)
	}
	if c.Workflow != "" {
		fmt.Fprintf(b, "- 工作流: %s\n", c.Workflow)
	}
	if c.Template != "" {
		fmt.Fprintf(b, "- 模板: %s\n", c.Template)
	}
	fmt.Fprintf(b, "- 计划哈希: `%s`\n", orBlank(c.PlanHash))
	fmt.Fprintf(b, "- 计划状态: %s\n", planStateLabel(c.PlanState))
	fmt.Fprintf(b, "- 生成时间: %s\n\n", generated.UTC().Format(time.RFC3339))
}

// markdownSQL writes the SQL surface and the review verdict frozen into the
// plan. An unreadable plan says "unknown"; it never says "none".
func markdownSQL(b *strings.Builder, tk *Ticket) {
	c := tk.Change
	b.WriteString("## 1. SQL 与审核\n\n")
	switch {
	case c.PlanState == planStateUnreadable:
		b.WriteString("> 计划制品不可读（plan_json 解析失败）：SQL 与审核结论**未知**，不能读作「没有 SQL」。\n\n")
	case !tk.SQL.HasSQL:
		b.WriteString("该计划没有可审核的 SQL 步骤。\n\n")
	default:
		b.WriteString("| 步骤 | SQL |\n| --- | --- |\n")
		for _, s := range tk.SQL.Steps {
			fmt.Fprintf(b, "| %s | `%s` |\n", s.Step, oneLine(s.SQL))
		}
		b.WriteString("\n")
	}

	r := tk.SQL.Review
	switch {
	case r != nil:
		fmt.Fprintf(b, "**审核结论（计划时固化，绑定 plan_hash）**: `%s`（fail %d / warn %d）\n\n",
			r.Verdict, r.FailCount, r.WarnCount)
		if len(r.Findings) > 0 {
			b.WriteString("| 规则 | 级别 | 步骤 | 语句 | 说明 |\n| --- | --- | --- | --- | --- |\n")
			for _, f := range r.Findings {
				fmt.Fprintf(b, "| %s | %s | %s | %d | %s |\n",
					f.Rule, f.Severity, f.Step, f.Statement, oneLine(f.Message))
			}
			b.WriteString("\n")
		}
	case c.PlanState == planStateUnreadable:
		b.WriteString("**审核结论**: 未知（计划制品不可读）。\n\n")
	default:
		b.WriteString("**审核结论**: 无（该计划没有可审核的 SQL 面）。\n\n")
	}
}

// markdownRollback writes the rollback policy and the per-step compensations.
func markdownRollback(b *strings.Builder, tk *Ticket) {
	b.WriteString("## 2. 回滚计划\n\n")
	switch {
	case tk.Change.PlanState == planStateUnreadable:
		b.WriteString("> 计划制品不可读（plan_json 解析失败）：回滚计划**未知**，不能读作「没有回滚」。\n\n")
	case !tk.Rollback.HasUndo:
		b.WriteString("未声明回滚。\n\n")
	default:
		fmt.Fprintf(b, "- 策略: %s\n", orBlank(tk.Rollback.Strategy))
		fmt.Fprintf(b, "- 失败处置: %s\n", orBlank(tk.Rollback.OnFailure))
		fmt.Fprintf(b, "- 回滚后校验: %v\n", tk.Rollback.VerifyAfter)
		if len(tk.Rollback.SnapshotPaths) > 0 {
			b.WriteString("- 快照路径:\n")
			for _, p := range tk.Rollback.SnapshotPaths {
				fmt.Fprintf(b, "  - `%s`\n", p)
			}
		}
		b.WriteString("\n")
		if len(tk.Rollback.StepUndos) > 0 {
			b.WriteString("| 步骤 | 策略 | undo 步数 |\n| --- | --- | --- |\n")
			for _, u := range tk.Rollback.StepUndos {
				fmt.Fprintf(b, "| %s | %s | %d |\n", u.Step, orBlank(u.Strategy), u.UndoSteps)
			}
			b.WriteString("\n")
		}
	}
}

// markdownWindow writes the window section, including the fact that the
// window is NOT persisted per run.
func markdownWindow(b *strings.Builder, tk *Ticket) {
	b.WriteString("## 3. 变更窗口\n\n")
	fmt.Fprintf(b, "- 校验时机: %s\n", orBlank(tk.Window.EnforcedAt))
	fmt.Fprintf(b, "- 随 run 持久化: %v\n", tk.Window.PersistedPerRun)
	if tk.Window.Note != "" {
		fmt.Fprintf(b, "- 说明: %s\n", tk.Window.Note)
	}
	b.WriteString("\n")
}

// markdownApproval writes the approval chain and its plan-binding truth.
func markdownApproval(b *strings.Builder, tk *Ticket) {
	b.WriteString("## 4. 审批链\n\n")
	if !tk.Approval.HasApproval {
		b.WriteString("未进入审批链。\n\n")
		return
	}
	fmt.Fprintf(b, "- 档位: %s\n", orBlank(tk.Approval.Level))
	fmt.Fprintf(b, "- 状态: %s\n", orBlank(tk.Approval.Status))
	fmt.Fprintf(b, "- 绑定当前计划: %v\n", tk.Approval.MatchesPlan)
	if tk.Approval.LegacyApproval {
		b.WriteString("- 注意: 存在未绑定计划的审批行（legacy，未记录 plan_hash）——它不对任何计划负责\n")
	}
	if tk.Approval.StaleApproval {
		b.WriteString("- 注意: 存在陈旧审批行（绑定的是本变更已不携带的计划）\n")
	}
	fmt.Fprintf(b, "- 审批人: %s\n", orBlank(strings.Join(tk.Approval.Approvers, ", ")))
	fmt.Fprintf(b, "- 待决 %d / 已批 %d / 已拒 %d\n",
		tk.Approval.PendingCount, tk.Approval.ApprovedCount, tk.Approval.RejectedCount)
	b.WriteString("\n")
	if len(tk.Approval.Decisions) > 0 {
		b.WriteString("| 审批人 | 动作 | 理由 | 决定时间 |\n| --- | --- | --- | --- |\n")
		for _, d := range tk.Approval.Decisions {
			fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
				d.Approver, d.Action, oneLine(d.Reason), formatTime(d.At))
		}
		b.WriteString("\n")
	}
}

// markdownExecution writes the execution evidence section.
func markdownExecution(b *strings.Builder, tk *Ticket) {
	b.WriteString("## 5. 执行证据\n\n")
	fmt.Fprintf(b, "- 判定: `%s`\n", tk.Execution.Verdict)
	if tk.Execution.Reason != "" {
		fmt.Fprintf(b, "- 说明: %s\n", tk.Execution.Reason)
	}
	fmt.Fprintf(b, "- 批次 %d / 步骤 %d / 目标 %d\n",
		tk.Execution.Batches, tk.Execution.Steps, tk.Execution.TotalTargets)
	fmt.Fprintf(b, "- 有证据: %v\n\n", tk.Execution.HasEvidence)
}

// markdownAudit writes the audit-chain verdict, keeping "cannot verify" and
// "verification error" apart.
func markdownAudit(b *strings.Builder, tk *Ticket) {
	b.WriteString("## 6. 审计链验证\n\n")
	switch {
	case tk.Audit.VerifyError != "":
		fmt.Fprintf(b, "- 结论: **校验出错** — %s\n", tk.Audit.VerifyError)
	case !tk.Audit.Verified && tk.Audit.Unverifiable:
		b.WriteString("- 结论: 未验证（该变更还没有 trace 记录）\n")
	case tk.Audit.Verified:
		fmt.Fprintf(b, "- 结论: 已验证（链有效: %v）\n", tk.Audit.Valid)
	default:
		b.WriteString("- 结论: 未验证\n")
	}
	fmt.Fprintf(b, "- trace 条数: %d（其中 legacy %d）\n", tk.Audit.Count, tk.Audit.LegacyCount)
	b.WriteString("\n")
}

// markdownFooter states where the document came from and how to rebuild it.
func markdownFooter(b *strings.Builder, opts MarkdownOptions) {
	b.WriteString("---\n\n")
	b.WriteString("本文档由 LEVEE 从既有制品派生（run 行、计划产物、审批行、trace 链），不含人工填写内容。\n")
	if opts.SourceCommand != "" {
		fmt.Fprintf(b, "重新生成: `%s`\n", opts.SourceCommand)
	}
}

// planStateLabel renders the plan artifact state in the document's own words.
func planStateLabel(state string) string {
	switch state {
	case planStateReadable:
		return "可读"
	case planStateUnreadable:
		return "不可读（plan_json 解析失败）"
	case planStateNone:
		return "尚无计划制品"
	default:
		return orBlank(state)
	}
}

// orBlank renders an empty string as a visible placeholder. An empty value
// and a missing value are the same fact in a document, and printing nothing
// would leave the reader guessing which one it was.
func orBlank(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(无)"
	}
	return s
}

// oneLine folds a multi-line value onto one line so it cannot break a
// Markdown table row. The value stays the same fact; only its layout changes.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.TrimSpace(s)
}

// formatTime renders a timestamp for the document, or a placeholder when it
// is unset. A zero time is "not recorded", not "the epoch".
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "(无)"
	}
	return t.UTC().Format(time.RFC3339)
}
