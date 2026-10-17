package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexus/levee/internal/cluster"
	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

// locateWebVocabulary resolves web/src/types/vocabulary.ts under root and, like
// locateSpec, refuses any root whose candidate file does not carry this
// generator's markers.
//
// The fingerprint check matters more here than in the markdown case: a
// `types/` directory is a place where a same-named file could plausibly exist
// for other reasons, and writing into it because the path matched would put
// generated declarations into a file nobody regenerates. Refusing to proceed
// unless the file already declares itself as ours makes that a loud failure.
func locateWebVocabulary(root string) (genPath, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return genPath{}, fmt.Errorf("resolve root %q: %w", root, err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return genPath{}, fmt.Errorf("root %q: %w", root, err)
	}
	if !info.IsDir() {
		return genPath{}, fmt.Errorf("root %q is not a directory", root)
	}

	path := filepath.Join(absRoot, "web", "src", "types", "vocabulary.ts")
	doc, err := os.ReadFile(path)
	if err != nil {
		return genPath{}, fmt.Errorf("no web/src/types/vocabulary.ts under %q: %w", root, err)
	}
	if !bytes.Contains(doc, []byte("BEGIN GENERATED: vocabulary")) {
		return genPath{}, fmt.Errorf(
			"%s does not look like the generated vocabulary (missing generated-block markers)", path)
	}
	return genPath{path: path}, nil
}

// tsVocabulary is one generated TypeScript list and the owner it comes from.
//
// The table is the single place that knows which vocabularies the console mirrors,
// so the tests below can assert coverage of the table itself: a list dropped from
// here would otherwise just disappear from the generated file and every remaining
// assertion would still hold.
type tsVocabulary struct {
	constName string
	typeName  string
	doc       string
	owner     func() []string
}

// webVocabularies lists the generated tables in the order they are emitted.
//
// Order within the file is cosmetic (TypeScript resolves declarations by name), but
// the grouping is not: the batch and assignment tables sit apart on purpose because
// they share three spellings and mean different things by them.
func webVocabularies() []tsVocabulary {
	return []tsVocabulary{
		{"CHANGE_STATUSES", "ChangeStatus",
			"变更（run）状态，与 runs.status 列同源。",
			func() []string { return runstatus.All }},
		{"PRIORITIES", "Priority",
			"创建变更时的优先级主张；不是审批档位（档位见 standard/high/emergency）。",
			state.PriorityNames},
		{"BATCH_STATES", "BatchState",
			"批次进度。completed 是写入方实际产出的拼写，done 是 schema 注释仍记录的历史拼写。",
			state.BatchStateNames},
		{"ASSIGNMENT_STATES", "AssignmentState",
			"派发状态。与 BATCH_STATES 只是拼写重叠：这里的 pending 是「还没被 worker 领取」，done 是活跃终态而非历史拼写。",
			state.AssignmentStateNames},
		{"NODE_ROLES", "NodeRole",
			"集群节点角色，按权威顺序：主控节点在前。",
			cluster.NodeRoleNames},
		{"NODE_STATUSES", "NodeStatus",
			"集群节点生命周期。leaving 目前没有任何写入方，但仍在这里命名——把三态折成两色会把优雅下线的节点涂成故障。",
			cluster.NodeStatusNames},
		{"SESSION_STATES", "SessionStateName",
			"会话状态，取自 SessionState.String()；末位 unknown 是越界值的兜底拼写，不是状态机里的一个状态。",
			conversation.SessionStateNames},
	}
}

// renderWebVocabulary emits the TypeScript value lists whose vocabulary has a
// Go owner. See the note on webVocabularies and the package doc.
//
// Every list is read from exported data — runstatus.All and the state / cluster /
// conversation `…Names()` accessors — rather than re-derived by parsing. That
// distinction is the whole point of keeping this in docgen instead of writing one
// more source-parsing guard: a generator that has to grep constant declarations is
// just a guard with extra steps, and it inherits the guard's failure mode — if the
// parse goes stale the output silently shrinks. Where a list's completeness could
// still be wrong (a constant declared but never added to its owner's list), the
// OWNER package checks it, against its own source, once: see
// internal/{cluster,state,conversation}/vocabulary_ownership_test.go.
//
// Order is preserved from the owner. For priorities that is load-bearing, not
// cosmetic: state.PriorityNames() documents itself as "the accepted set, the
// display order and the error text" in one place, and the instantiation
// wizard renders the dropdown straight from this array. The other lists are
// lifecycle/dispatch order for the same reason — sorting or alphabetising here
// would fork the display order away from the owner.
func renderWebVocabulary() string {
	var b strings.Builder
	b.WriteString("// 本节由 internal/docgen 生成，请勿手工编辑，也不要在这里追加手写声明。\n")
	b.WriteString("// 重新生成：go run ./internal/docgen\n")
	b.WriteString("// 归属（每张表的值集合与顺序都由它自己那份 owner 决定）：\n")
	b.WriteString("//   internal/runstatus.All、internal/state.PriorityNames()、\n")
	b.WriteString("//   internal/state.BatchStateNames()、internal/state.AssignmentStateNames()、\n")
	b.WriteString("//   internal/cluster.NodeRoleNames() / NodeStatusNames()、\n")
	b.WriteString("//   internal/conversation.SessionStateNames()\n")
	b.WriteString("//\n")
	b.WriteString("// 这些表此前是前端手抄件，由 Go 侧解析抄件再与 owner 比对来守卫。那正是\n")
	b.WriteString("// #153 的失败形状：守卫核对的是自己的抄写，四处产物互相自洽、一起与服务器\n")
	b.WriteString("// 不一致，全程没有一处报错。现在值集合只有一份，且是机器产出的。\n")
	b.WriteString("//\n")
	b.WriteString("// 同名不同物的两组（batch 与 assignment 都拼 pending / done / interrupted）\n")
	b.WriteString("// 刻意是两张表、两个类型名，别在这里合并——理由写在 owner 的清单注释里。\n\n")

	for _, v := range webVocabularies() {
		writeTsList(&b, v.constName, v.typeName, v.doc, v.owner())
	}

	return b.String()
}

// writeTsList renders one `as const` array plus the union derived from it.
//
// The union is derived rather than repeated because a second spelling of the
// same list is exactly the copy this file exists to remove. `as const` is what
// makes the derivation yield literal types instead of `string`, which in turn is
// what makes the console's Record<ChangeStatus, string> label tables fail to
// compile when a status is added without a label — the guard moves from a test
// that parses text to the type checker that builds the app.
func writeTsList(b *strings.Builder, constName, typeName, doc string, values []string) {
	fmt.Fprintf(b, "/** %s */\n", doc)
	fmt.Fprintf(b, "export const %s = [\n", constName)
	for _, v := range values {
		fmt.Fprintf(b, "  %s,\n", tsQuote(v))
	}
	b.WriteString("] as const\n\n")
	fmt.Fprintf(b, "/** %s 的成员集合，由上面那份数组派生。 */\n", typeName)
	fmt.Fprintf(b, "export type %s = (typeof %s)[number]\n\n", typeName, constName)
}

// tsQuote renders one Go string as a TypeScript string literal.
//
// These vocabularies are lowercase snake_case today, but a hand-rolled
// '"'+v+'"' would turn a future value containing a quote into a syntax error in
// a generated file nobody edits by hand — and the tool would report success.
// Using Go's own quoting keeps the output parseable; the single-quote style is
// what the rest of web/src uses, so it is rewritten rather than tolerated.
func tsQuote(v string) string {
	rendered := fmt.Sprintf("%q", v)
	if !strings.ContainsAny(v, "'\\") {
		return "'" + v + "'"
	}
	return rendered
}
