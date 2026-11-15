package dbchange

// html_test.go — 可打印单页的结构与**安全**承重。
//
// 这份文档是要拿出去出证的，所以两件事必须同时成立：
//
//   ① 它说的是同一张单（六个分段都在，SQL、审核结论、审批链、审计结论都在）——
//      印出来的纸与屏幕上的页不能是两份不同的东西；
//   ② 操作者可控文本**不能**变成标记。标签、审批人、理由、SQL、规则说明都是
//      人写的，其中任何一处出现 `<script>` 或 `<b>` 都必须被转义，否则这张单
//      在浏览器里打开时就是一次注入。
//
// ② 不是"顺便测一下"：它正是在把 Markdown 文档接成 HTML 时被抓到的真问题
// （审核发现的 message 走了不转义的布局辅助函数，`<b>bold</b>` 原样透出）。

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nexus/levee/internal/plan"
)

func htmlTicket() *Ticket {
	tk := markdownTicket()
	tk.Change.Label = "账户表迁移"
	tk.SQL.Review = &plan.SQLReviewSummary{
		Verdict: "warn", WarnCount: 1,
		Findings: []plan.SQLReviewFinding{{
			Step: "migrate", Rule: "SR-15", Severity: "warn", Statement: 1,
			Message: "UPDATE has a WHERE but no LIMIT",
		}},
	}
	return tk
}

func TestHTMLCarriesEverySection(t *testing.T) {
	doc := HTML(htmlTicket(), HTMLOptions{
		GeneratedAt: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC),
	})

	assert.Contains(t, doc, "<!doctype html>")
	assert.Contains(t, doc, `<html lang="zh-CN">`)
	for _, heading := range []string{
		"<h1>数据库变更发布单</h1>",
		"<h2>1. SQL 与审核</h2>",
		"<h2>2. 回滚计划</h2>",
		"<h2>3. 变更窗口</h2>",
		"<h2>4. 审批链</h2>",
		"<h2>5. 执行证据</h2>",
		"<h2>6. 审计链验证</h2>",
	} {
		assert.Contains(t, doc, heading)
	}
	// The tables the document exists for.
	assert.Contains(t, doc, "<table>")
	assert.Contains(t, doc, "<th>步骤</th>")
	assert.Contains(t, doc, "SR-15")
}

func TestHTMLEscapesOperatorControlledText(t *testing.T) {
	tk := htmlTicket()
	// Every one of these lands on the sheet outside a code span.
	tk.Change.Label = `<script>alert('label')</script>`
	tk.Change.Creator = `<img src=x onerror=alert(1)>`
	tk.Approval.Decisions = []ApprovalDecision{{
		Approver: `<b>mallory</b>`, Action: "approve", Reason: `<i>looks fine</i>`,
		At: time.Now(),
	}}
	tk.SQL.Review = &plan.SQLReviewSummary{
		Verdict: "warn", WarnCount: 1,
		Findings: []plan.SQLReviewFinding{{
			Step: "migrate", Rule: "SR-15", Severity: "warn", Statement: 1,
			Message: `no LIMIT <b>bold</b>`,
		}},
	}

	doc := HTML(tk, HTMLOptions{})

	// No operator-supplied tag may survive as a tag.
	assert.NotContains(t, doc, "<script>alert")
	assert.NotContains(t, doc, "<img src=x")
	assert.NotContains(t, doc, "<b>mallory</b>")
	assert.NotContains(t, doc, "<i>looks fine</i>")
	assert.NotContains(t, doc, "<b>bold</b>")

	// And the text is still readable (escaped, not dropped).
	assert.Contains(t, doc, "&lt;script&gt;")
	assert.Contains(t, doc, "&lt;b&gt;bold&lt;/b&gt;")
}

func TestHTMLOnlyOwnsItsOwnScript(t *testing.T) {
	// The AutoPrint script is a static literal. With it off, the document has
	// no script at all; with it on, exactly one — and it mentions nothing from
	// the ticket.
	off := HTML(htmlTicket(), HTMLOptions{})
	assert.NotContains(t, off, "<script")

	injected := htmlTicket()
	injected.Change.Label = "label</script><script>alert(1)</script>"
	on := HTML(injected, HTMLOptions{AutoPrint: true})
	assert.Equal(t, 1, strings.Count(on, "<script>"),
		"the document must carry exactly one script: the static print call")
	assert.Contains(t, on, "window.print()")
	// The injected text is present as ESCAPED text (readable on the sheet), and
	// it must never appear as a tag. That is the real property: the document
	// may quote `<script>` at a reader, it may not become one.
	assert.NotContains(t, on, "<script>alert")
	assert.Contains(t, on, "&lt;script&gt;alert(1)")
}

func TestHTMLKeepsBoldPaired(t *testing.T) {
	// The document writes `**text**`; an unclosed <strong> would make the rest
	// of the sheet bold in a browser. Both counts must match.
	doc := HTML(htmlTicket(), HTMLOptions{})
	assert.Equal(t, strings.Count(doc, "<strong>"), strings.Count(doc, "</strong>"),
		"every <strong> must be closed")
}

func TestHTMLIsSelfContained(t *testing.T) {
	// A compliance sheet has to render on a machine with no network and no
	// extra assets: no stylesheet link, no image, no @import.
	doc := HTML(htmlTicket(), HTMLOptions{})
	assert.NotContains(t, doc, "<link")
	assert.NotContains(t, doc, "<img")
	assert.NotContains(t, doc, "@import")
	assert.NotContains(t, doc, "http://")
	assert.NotContains(t, doc, "https://")
}

func TestHTMLCarriesThePrintRules(t *testing.T) {
	// The print rules are the point of the printable form: a table row must not
	// split across pages and a heading must not be stranded at a page bottom.
	doc := HTML(htmlTicket(), HTMLOptions{})
	assert.Contains(t, doc, "@media print")
	assert.Contains(t, doc, "page-break-after: avoid")
	assert.Contains(t, doc, "page-break-inside: avoid")
}

func TestHTMLIsDeterministicForTheSameTicket(t *testing.T) {
	opts := HTMLOptions{GeneratedAt: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	assert.Equal(t, HTML(htmlTicket(), opts), HTML(htmlTicket(), opts))
}

func TestHTMLFallsBackToTheIDWhenThereIsNoLabel(t *testing.T) {
	tk := htmlTicket()
	tk.Change.Label = ""
	doc := HTML(tk, HTMLOptions{})
	assert.Contains(t, doc, "<title>"+ticketIDForTest(tk)+"</title>")
}

func ticketIDForTest(tk *Ticket) string { return tk.Change.ID }
