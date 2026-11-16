package dbchange

// markdown_test.go — 文档版的诚实口径必须与终端版一致。
//
// 这份守卫盯的是「导出的单不能比屏幕上的单更乐观」：计划不可读时导出件
// 不能写成「没有 SQL / 没有回滚」，审计校验出错时不能写成「还没有 trace
// 记录」。文档是给人拿去出证的东西，一句「没有」在这种场合比在终端里
// 危险得多。

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/sqlreview"
)

func markdownTicket() *Ticket {
	return &Ticket{
		Change: ChangeSection{
			ID: "run-md-1", Label: "账户表迁移", Status: "completed",
			Priority: "high", Environment: "prod", Creator: "alice",
			Workflow: "db-migrate", Template: "tpl-migrate",
			PlanHash: "v2:abc123", PlanState: planStateReadable,
			CreatedAt: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 11, 15, 9, 5, 0, 0, time.UTC),
		},
		SQL: SQLSection{
			HasSQL: true,
			Steps: []SQLStep{
				{Step: "migrate", SQL: "UPDATE accounts SET tier='gold'\nWHERE id = 42"},
			},
			Review: &plan.SQLReviewSummary{
				Verdict: "warn", WarnCount: 1,
				Findings: []plan.SQLReviewFinding{{
					Step: "migrate", Rule: "SR-08", Severity: "warn", Statement: 1,
					Message: "UPDATE without a WHERE clause",
				}},
			},
		},
		Rollback: RollbackSection{
			Strategy: "snapshot", OnFailure: "abort", VerifyAfter: true,
			SnapshotPaths: []string{"/snap/accounts"},
			StepUndos:     []StepUndo{{Step: "migrate", Strategy: "undo-action", UndoSteps: 1}},
			HasUndo:       true,
		},
		Window: WindowSection{
			EnforcedAt: "plan", PersistedPerRun: false,
			Note: "变更窗口是工作流声明、plan 生成时校验，run 行不持久化该事实。",
		},
		Approval: ApprovalSection{
			Level: "standard", Status: "approved",
			Approvers:    []string{"alice", "bob"},
			PendingCount: 0, ApprovedCount: 1, RejectedCount: 0,
			MatchesPlan: true, HasApproval: true,
			Decisions: []ApprovalDecision{{Approver: "bob", Action: "approve", Reason: "已核对", At: time.Date(2026, 11, 15, 9, 3, 0, 0, time.UTC)}},
		},
		Execution: ExecutionSection{
			Verdict: "complete", Reason: "every step this run executed has an evidence row",
			Batches: 1, Steps: 1, TotalTargets: 2, HasEvidence: true,
		},
		Audit: AuditSection{
			Valid: true, Count: 3, LegacyCount: 0, Verified: true,
		},
	}
}

func TestMarkdownCarriesEverySection(t *testing.T) {
	doc := Markdown(markdownTicket(), MarkdownOptions{
		GeneratedAt:   time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC),
		SourceCommand: "levee dbchange show run-md-1 --format markdown",
	})

	for _, heading := range []string{
		"# 数据库变更发布单",
		"## 1. SQL 与审核",
		"## 2. 回滚计划",
		"## 3. 变更窗口",
		"## 4. 审批链",
		"## 5. 执行证据",
		"## 6. 审计链验证",
	} {
		assert.Contains(t, doc, heading)
	}
	assert.Contains(t, doc, "run-md-1")
	assert.Contains(t, doc, "v2:abc123")
	assert.Contains(t, doc, "UPDATE accounts SET tier='gold'")
	assert.Contains(t, doc, "levee dbchange show run-md-1 --format markdown")
}

// The static impact surface is rendered next to the verdict, and it is
// labelled as what it is: read off the SQL text, not measured. An approver who
// reads "orders: delete [whole table]" as a measurement would stop asking the
// one question this section deliberately does not answer.
func TestMarkdownRendersTheStaticImpactSurface(t *testing.T) {
	tk := markdownTicket()
	tk.SQL.Review.Impact = &sqlreview.Impact{Tables: []sqlreview.TableImpact{
		{Table: "accounts", Operations: []string{"update"}, WholeTable: false},
		{Table: "orders", Operations: []string{"delete"}, WholeTable: true},
	}}
	doc := Markdown(tk, MarkdownOptions{GeneratedAt: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)})

	assert.Contains(t, doc, "静态影响面（来自 SQL 文本，不是实测量）")
	assert.Contains(t, doc, "| accounts | update | 否 |")
	assert.Contains(t, doc, "| orders | delete | **是**")
	assert.Contains(t, doc, "锁时长与影响行数需要活库")
	assert.NotContains(t, doc, "影响面不完整")
}

// A surface read from a statement the engine only partly understood is a
// LOWER BOUND, and the document must say so — taking an incomplete list at
// face value is how an approver clears a change whose second table was never
// read.
func TestMarkdownFlagsAnIncompleteImpactSurface(t *testing.T) {
	tk := markdownTicket()
	tk.SQL.Review.Impact = &sqlreview.Impact{
		Tables: []sqlreview.TableImpact{{Table: "accounts", Operations: []string{"alter"}, WholeTable: true}},
		//
		Incomplete: true,
	}
	doc := Markdown(tk, MarkdownOptions{GeneratedAt: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)})
	assert.Contains(t, doc, "**影响面不完整**")
	assert.Contains(t, doc, "上表是下界，不是全集")
}

// A batch with no tables at all (SET/USE) still gets a line: "the engine
// looked and found no table" is a fact, and silence would read as "not
// analysed".
func TestMarkdownSaysWhenSQLNamesNoTable(t *testing.T) {
	tk := markdownTicket()
	tk.SQL.Review.Impact = &sqlreview.Impact{Tables: []sqlreview.TableImpact{}}
	doc := Markdown(tk, MarkdownOptions{GeneratedAt: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)})
	assert.Contains(t, doc, "未指名任何表")
}

// A stored summary from before the impact surface existed carries none, and
// the document must not invent an empty one.
func TestMarkdownOmitsTheImpactSectionWhenThereIsNone(t *testing.T) {
	doc := Markdown(markdownTicket(), MarkdownOptions{GeneratedAt: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)})
	assert.NotContains(t, doc, "静态影响面")
}

func TestMarkdownSaysUnknownWhenThePlanIsUnreadable(t *testing.T) {
	tk := markdownTicket()
	tk.Change.PlanState = planStateUnreadable
	tk.SQL = SQLSection{}
	tk.SQL.Review = nil
	tk.Rollback = RollbackSection{}

	doc := Markdown(tk, MarkdownOptions{})

	assert.Contains(t, doc, "未知")
	assert.NotContains(t, doc, "该计划没有可审核的 SQL 步骤")
	assert.NotContains(t, doc, "未声明回滚")
	assert.Contains(t, doc, "不可读（plan_json 解析失败）")
}

func TestMarkdownSeparatesAVerifyErrorFromNoTraces(t *testing.T) {
	tk := markdownTicket()
	tk.Audit = AuditSection{Verified: false, Unverifiable: true, VerifyError: "audit chain reader: db down"}

	doc := Markdown(tk, MarkdownOptions{})
	assert.Contains(t, doc, "校验出错")
	assert.Contains(t, doc, "db down")
	assert.NotContains(t, doc, "该变更还没有 trace 记录")
}

func TestMarkdownSaysNoTracesWhenThereAreNone(t *testing.T) {
	tk := markdownTicket()
	tk.Audit = AuditSection{Verified: false, Unverifiable: true}

	doc := Markdown(tk, MarkdownOptions{})
	assert.Contains(t, doc, "该变更还没有 trace 记录")
	assert.NotContains(t, doc, "校验出错")
}

func TestMarkdownFoldsMultiLineSQLIntoOneRow(t *testing.T) {
	// A SQL statement with a newline must not break the table row: the
	// document is read in a Markdown viewer, and a broken row would hide the
	// statement entirely.
	doc := Markdown(markdownTicket(), MarkdownOptions{})
	row := ""
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "| migrate |") && strings.Contains(line, "UPDATE") {
			row = line
		}
	}
	require.NotEmpty(t, row, "the SQL row must exist")
	assert.NotContains(t, row, "\n", "the row must be one line")
	assert.Contains(t, row, "UPDATE accounts")
}

func TestMarkdownIsDeterministicForTheSameTicket(t *testing.T) {
	opts := MarkdownOptions{GeneratedAt: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	assert.Equal(t, Markdown(markdownTicket(), opts), Markdown(markdownTicket(), opts),
		"the same ticket must render the same document — a diff in the export means a diff in the facts")
}
