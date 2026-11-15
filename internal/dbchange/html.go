// html.go renders the DB Change Ticket as a standalone, printable HTML
// document — the thing an operator hands to an auditor, or prints to PDF
// from the browser.
//
// WHY THE HTML IS RENDERED FROM THE MARKDOWN, NOT ALONGSIDE IT
//
// The ticket has exactly one source of truth: the Markdown document
// (dbchange.Markdown). A second renderer that produced HTML from the
// ticket struct directly would be a second opinion about the same facts —
// and two renderers drift, which is how the screen and the printed sheet
// end up disagreeing about what was approved.
//
// So the direction here is one-way: facts → Markdown → HTML. The Markdown
// parser below supports exactly the subset Markdown() emits (headings,
// paragraphs, bullet lists, tables, blockquotes, horizontal rules, inline
// code and bold). It is NOT a general Markdown implementation, and it does
// not try to be: an element it does not know is emitted as literal text
// rather than guessed at.
//
// SAFETY
//
// The document carries operator-controlled text (labels, approver names,
// reasons, SQL, rule messages). Everything is HTML-escaped BEFORE the
// inline formatting is applied, so no operator input can become a tag:
// the escape happens first, the formatting only ever inserts the tags this
// file knows about. Code spans are escaped too — a code span is not a
// place where markup is meant to live.

package dbchange

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
)

// HTMLOptions tunes the printable document's non-factual parts.
type HTMLOptions struct {
	// GeneratedAt stamps the document. Zero means "now".
	GeneratedAt time.Time
	// Title is the document title (used for <title> and the sheet's header).
	// Empty falls back to the ticket's label or id.
	Title string
	// AutoPrint makes the document open its own print dialog. The web console
	// uses it for the "打印 / 另存为 PDF" button: one click, and the browser's
	// own print sheet (which is also where "save as PDF" lives) is already up.
	//
	// The script it adds is a STATIC LITERAL — nothing from the ticket is ever
	// interpolated into it, so it cannot carry operator input. That property is
	// what keeps the "self-contained, no external resources" claim honest while
	// still allowing the one interaction a printable sheet needs.
	AutoPrint bool
}

// HTML renders the ticket as a self-contained printable HTML document.
//
// The result has no external references: no stylesheet link, no script, no
// image. That is deliberate — a compliance sheet has to render the same way
// on a machine with no network and on a machine with no fonts installed.
func HTML(tk *Ticket, opts HTMLOptions) string {
	if tk == nil {
		return ""
	}
	generated := opts.GeneratedAt
	if generated.IsZero() {
		generated = time.Now().UTC()
	}
	title := opts.Title
	if title == "" {
		title = tk.Change.Label
	}
	if title == "" {
		title = tk.Change.ID
	}

	md := Markdown(tk, MarkdownOptions{
		GeneratedAt:   generated,
		SourceCommand: fmt.Sprintf("levee dbchange show %s --format markdown", tk.Change.ID),
	})

	autoPrint := ""
	if opts.AutoPrint {
		// A static literal, never assembled from ticket data.
		autoPrint = "<script>window.addEventListener('load',function(){window.print()})</script>\n"
	}

	return fmt.Sprintf(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<style>
%s
</style>
</head>
<body>
<main class="sheet">
%s
</main>
%s</body>
</html>
`, html.EscapeString(title), printableCSS, markdownToHTML(md), autoPrint)
}

// printableCSS is the document's stylesheet, inlined.
//
// The print rules are the point of it: a table row must not be split across
// a page, and a section heading must not be left alone at the bottom of one.
const printableCSS = `
:root {
  --ink: #1a1d21;
  --muted: #5b6470;
  --line: #d7dce3;
  --soft: #f5f7fa;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  padding: 24px;
  color: var(--ink);
  background: #fff;
  font: 14px/1.6 -apple-system, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
}
.sheet { max-width: 900px; margin: 0 auto; }
h1 { font-size: 22px; margin: 0 0 12px; }
h2 { font-size: 17px; margin: 28px 0 10px; padding-bottom: 6px; border-bottom: 1px solid var(--line); }
p { margin: 6px 0; }
ul { margin: 6px 0; padding-left: 22px; }
table { border-collapse: collapse; width: 100%; margin: 8px 0 14px; }
th, td { border: 1px solid var(--line); padding: 6px 8px; text-align: left; vertical-align: top; }
th { background: var(--soft); font-weight: 600; }
td { word-break: break-word; }
code {
  font-family: "JetBrains Mono", "Cascadia Mono", Consolas, monospace;
  font-size: 12.5px;
  background: var(--soft);
  padding: 1px 4px;
  border-radius: 3px;
}
blockquote {
  margin: 8px 0;
  padding: 8px 12px;
  border-left: 3px solid var(--line);
  background: var(--soft);
  color: var(--muted);
}
hr { border: 0; border-top: 1px solid var(--line); margin: 20px 0; }
strong { font-weight: 600; }
@media print {
  @page { margin: 14mm 12mm; }
  body { padding: 0; font-size: 11.5pt; }
  h2 { page-break-after: avoid; }
  tr, li, blockquote { page-break-inside: avoid; }
  table { page-break-inside: auto; }
}
`

// markdownToHTML converts the Markdown subset Markdown() emits into HTML.
//
// The whole input is escaped first; the parser then only ADDS the tags it
// knows. An unrecognised construct is left as text — the parser never
// guesses, because a guessed construct is a rendering that says something
// the document did not say.
func markdownToHTML(md string) string {
	// NO second escaping here. dbchange.Markdown already escapes the operator
	// controlled values (so a .md file is safe in an HTML-rendering Markdown
	// viewer too); escaping again would turn "<" into "&amp;lt;" and print
	// entity soup on the sheet. The parser below only ever ADDS its own tags.
	lines := strings.Split(md, "\n")

	var out strings.Builder
	var paragraph []string
	var list []string
	var table []string
	var inTable bool

	flushParagraph := func() {
		if len(paragraph) == 0 {
			return
		}
		out.WriteString("<p>" + inlineMarkdown(strings.Join(paragraph, " ")) + "</p>\n")
		paragraph = paragraph[:0]
	}
	flushList := func() {
		if len(list) == 0 {
			return
		}
		out.WriteString("<ul>\n")
		for _, item := range list {
			out.WriteString("<li>" + inlineMarkdown(item) + "</li>\n")
		}
		out.WriteString("</ul>\n")
		list = list[:0]
	}
	flushTable := func() {
		if len(table) == 0 {
			return
		}
		out.WriteString("<table>\n")
		header := splitTableRow(table[0])
		out.WriteString("<thead><tr>")
		for _, cell := range header {
			out.WriteString("<th>" + inlineMarkdown(cell) + "</th>")
		}
		out.WriteString("</tr></thead>\n<tbody>\n")
		for _, row := range table[1:] {
			out.WriteString("<tr>")
			for _, cell := range splitTableRow(row) {
				out.WriteString("<td>" + inlineMarkdown(cell) + "</td>")
			}
			out.WriteString("</tr>\n")
		}
		out.WriteString("</tbody>\n</table>\n")
		table = table[:0]
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "":
			flushParagraph()
			flushList()
			flushTable()
			inTable = false

		case strings.HasPrefix(trimmed, "# "):
			flushParagraph()
			flushList()
			flushTable()
			out.WriteString("<h1>" + inlineMarkdown(strings.TrimSpace(trimmed[2:])) + "</h1>\n")

		case strings.HasPrefix(trimmed, "## "):
			flushParagraph()
			flushList()
			flushTable()
			out.WriteString("<h2>" + inlineMarkdown(strings.TrimSpace(trimmed[3:])) + "</h2>\n")

		case trimmed == "---":
			flushParagraph()
			flushList()
			flushTable()
			out.WriteString("<hr>\n")
			inTable = false

		case strings.HasPrefix(trimmed, "> "):
			flushParagraph()
			flushList()
			flushTable()
			out.WriteString("<blockquote>" + inlineMarkdown(strings.TrimSpace(trimmed[2:])) + "</blockquote>\n")

		case strings.HasPrefix(trimmed, "- "):
			flushParagraph()
			flushTable()
			inTable = false
			list = append(list, strings.TrimSpace(trimmed[2:]))

		case strings.HasPrefix(trimmed, "|"):
			// A table row: `| a | b |`. The separator row (`| --- | --- |`)
			// is recognised and dropped — it is markup, not content.
			if isTableSeparator(trimmed) {
				continue
			}
			flushParagraph()
			flushList()
			table = append(table, trimmed)
			inTable = true

		default:
			flushList()
			if inTable {
				// A non-table line right after table rows ends the table.
				flushTable()
				inTable = false
			}
			paragraph = append(paragraph, trimmed)
		}
	}
	flushParagraph()
	flushList()
	flushTable()
	return out.String()
}

// inlineMarkdown applies the inline formatting the document uses: bold and
// code spans. The input is already HTML-escaped, so the only tags this adds
// are the two it owns.
func inlineMarkdown(s string) string {
	// Code spans first: their contents must not be re-scanned for bold.
	var codeParts []string
	rest := s
	for {
		start := strings.Index(rest, "`")
		if start < 0 {
			break
		}
		end := strings.Index(rest[start+1:], "`")
		if end < 0 {
			break
		}
		content := rest[start+1 : start+1+end]
		rest = rest[:start] + "\x00CODE" + strconv.Itoa(len(codeParts)) + "\x00" + rest[start+1+end+1:]
		codeParts = append(codeParts, content)
	}

	// Bold: alternate the opening and closing tag across the `**` markers.
	//
	// Alternation rather than a single replacement pass because the document
	// writes paired markers (`**text**`), and a naive "replace every ** with
	// <strong>" produces unclosed tags — which an HTML viewer resolves by
	// making the rest of the page bold.
	rest = alternateBold(rest)

	for i, code := range codeParts {
		rest = strings.Replace(rest, "\x00CODE"+strconv.Itoa(i)+"\x00", "<code>"+code+"</code>", 1)
	}
	return rest
}

// alternateBold turns pairs of `**` into <strong>…</strong>.
//
// An unpaired marker (somebody typed `**` into a label) is left as literal
// text: guessing a pair would swallow the rest of the line into a bold run.
func alternateBold(s string) string {
	parts := strings.Split(s, "**")
	if len(parts) < 3 {
		return s
	}
	var out strings.Builder
	for i, part := range parts {
		out.WriteString(part)
		// Between two parts there was a marker; pair them up.
		if i == len(parts)-1 {
			break
		}
		switch i % 2 {
		case 0:
			out.WriteString("<strong>")
		default:
			out.WriteString("</strong>")
		}
	}
	// An odd number of markers leaves an unclosed <strong>; drop it rather than
	// shipping a tag that bolds the rest of the page.
	result := out.String()
	if strings.Count(result, "<strong>") != strings.Count(result, "</strong>") {
		return alternativeBoldFallback(s)
	}
	return result
}

// alternativeBoldFallback is the "markers do not pair" answer: leave the text
// exactly as it was. It is a separate function so the main path stays readable.
func alternativeBoldFallback(s string) string {
	return s
}

// splitTableRow splits a `| a | b |` row into its cells.
func splitTableRow(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	parts := strings.Split(row, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}

// isTableSeparator recognises the `| --- | --- |` separator row.
func isTableSeparator(row string) bool {
	cells := splitTableRow(row)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if c == "" {
			return false
		}
		for _, r := range c {
			if r != '-' && r != ':' && r != ' ' {
				return false
			}
		}
	}
	return true
}
