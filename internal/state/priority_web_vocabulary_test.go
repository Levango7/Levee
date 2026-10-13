// priority_web_vocabulary_test.go — 变更优先级的 TS 镜像必须与 internal/state 的
// Priority* 常量逐字相等。
//
// 为什么这条要有：`Priority` 联合（web/src/types/levee.ts）此前是**全仓唯一一份**
// 写成清单的优先级词表，Go 侧只有散落的裸字面量（创建路径的 `"normal"`）。TS 侧的
// `Record<Priority, string>` 能让"前端漏配一个标签"变成编译错误，但它看不见 Go 加了
// 第五个值——那正是这次拆列要防的那类漂移（同一个拼写服务两套语义，或者一套语义两个拼写）。
// 名单从 priority.go 解析、也从 TS 文件解析，两边都不复述。
//
// 现在有四个方向（每个都是一份独立的抄件，所以各自一条）：`Priority` 联合、
// `PRIORITY_LABEL` / `PRIORITY_COLOR` 两张表、向导下拉实际渲染的 `PRIORITIES` 数组，
// 以及 ApprovalView 里手写的 `.cell-prio--<拼写>` 样式规则。最后一条是拼写面：
// 规则名对着一个引擎永远不会产出的值，就是"看起来支持"的死 CSS（同族教训见
// internal/cluster 的节点守卫）。
package state

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webUnionMembers parses `export type X = 'a' | 'b' | …` out of one file.
func webUnionMembers(t *testing.T, rel, typeName string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(repoFile(t, rel))
	require.NoError(t, err)
	// `[^\n;]+`, not `[^;]+`: in Go's regexp a negated class still matches "\n",
	// so the shorter form runs past the end of the line and swallows members of
	// whichever declaration comes next.
	line := regexp.MustCompile(`(?m)^export type ` + typeName + ` = ([^\n;]+)$`).FindStringSubmatch(string(raw))
	require.Len(t, line, 2, "could not find `export type %s = …` in %s — the declaration shape changed", typeName, rel)
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(line[1], -1) {
		out[m[1]] = true
	}
	require.NotEmpty(t, out, "%s parsed empty in %s", typeName, rel)
	return out
}

// recordKeys parses the key set of `export const NAME: Record<…> = { … }`.
func recordKeys(t *testing.T, rel, name string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(repoFile(t, rel))
	require.NoError(t, err)
	block := regexp.MustCompile(`(?m)^export const ` + name + `: Record<\w+, \w+> = \{\n([\s\S]*?)^\}$`).FindStringSubmatch(string(raw))
	require.Len(t, block, 2, "could not find `export const %s: Record<…, …> = {` in %s — its shape changed", name, rel)
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^[ \t]*(\w+):`).FindAllStringSubmatch(block[1], -1) {
		out[m[1]] = true
	}
	require.NotEmpty(t, out, "%s parsed empty", name)
	return out
}

func TestWebPriorityUnionMatchesGoPriorityConstants(t *testing.T) {
	declared := map[string]bool{}
	for _, value := range PriorityNames() {
		declared[value] = true
	}
	ui := webUnionMembers(t, "web/src/types/levee.ts", "Priority")

	for _, value := range PriorityNames() {
		assert.True(t, ui[value], "priority %q has no entry in web/src/types/levee.ts Priority — the UI could never send or label it", value)
	}
	for value := range ui {
		assert.True(t, declared[value], "web Priority union lists %q, which no state.Priority* constant declares", value)
	}
	assert.Equal(t, len(declared), len(ui), "the two priority vocabularies differ in size, so one side has an entry the other lacks")
}

func TestWebPriorityLabelsAndColorsCoverEveryPriority(t *testing.T) {
	labels := recordKeys(t, "web/src/utils/format.ts", "PRIORITY_LABEL")
	colors := recordKeys(t, "web/src/utils/format.ts", "PRIORITY_COLOR")
	for _, value := range PriorityNames() {
		assert.True(t, labels[value], "%q has no PRIORITY_LABEL entry — the Chinese console would render the wire spelling", value)
		assert.True(t, colors[value], "%q has no PRIORITY_COLOR entry", value)
	}
}

// webArrayMembers parses `export const NAME: readonly T[] = ['a', 'b']`.
func webArrayMembers(t *testing.T, rel, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(repoFile(t, rel))
	require.NoError(t, err)
	// The class stops at "]" and the pattern is line-anchored: an unanchored
	// `.*` would happily reach into the next declaration and report its members.
	line := regexp.MustCompile(`(?m)^export const ` + name + `: readonly \w+\[\] = \[([^\]\n]*)\]$`).FindStringSubmatch(string(raw))
	require.Len(t, line, 2, "could not find `export const %s: readonly …[] = [...]` in %s — its shape changed", name, rel)
	var out []string
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(line[1], -1) {
		out = append(out, m[1])
	}
	require.NotEmpty(t, out, "%s parsed empty in %s", name, rel)
	return out
}

// TestWebPriorityRuntimeArrayMatchesTheVocabulary covers the array the instantiate
// wizard renders its <el-option> list from. It exists because the wizard used to
// hand-copy four option lines, which is a third owner of the vocabulary that a
// fourth Go value would silently miss — the union cannot be iterated at runtime, so
// an array is the only way the template can read the list from its owner.
func TestWebPriorityRuntimeArrayMatchesTheVocabulary(t *testing.T) {
	listed := webArrayMembers(t, "web/src/utils/format.ts", "PRIORITIES")
	declared := PriorityNames()

	require.Len(t, listed, len(declared),
		"PRIORITIES lists %d values while internal/state declares %d — the wizard would offer or hide one", len(listed), len(declared))
	for i, value := range declared {
		assert.Equal(t, value, listed[i], "PRIORITIES member %d must be %q (urgency order is part of the contract)", i, value)
	}

	// The array and the label table must be the same set, or the wizard can offer a
	// value the console then prints as a raw spelling.
	labels := recordKeys(t, "web/src/utils/format.ts", "PRIORITY_LABEL")
	set := map[string]bool{}
	for _, value := range listed {
		assert.True(t, labels[value], "PRIORITIES offers %q, which has no PRIORITY_LABEL entry", value)
		set[value] = true
	}
	for value := range labels {
		assert.True(t, set[value], "PRIORITY_LABEL covers %q, which PRIORITIES does not offer", value)
	}
}

// TestApprovalPriorityCssClassesAreDeclaredPriorities is the spelling direction, the
// one internal/cluster's node guard learned first: the approval queue colours the
// priority cell with `.cell-prio--<wire spelling>` rules written by hand in the view.
// A rule named after a value nothing produces is dead CSS that reads like support, and
// a real fifth priority would arrive with no rule at all.
func TestApprovalPriorityCssClassesAreDeclaredPriorities(t *testing.T) {
	raw, err := os.ReadFile(repoFile(t, "web/src/views/ApprovalView.vue"))
	require.NoError(t, err)

	declared := map[string]bool{}
	for _, value := range PriorityNames() {
		declared[value] = true
	}

	found := regexp.MustCompile(`\.cell-prio--([a-zA-Z][\w-]*)`).FindAllStringSubmatch(string(raw), -1)
	require.NotEmpty(t, found, "no `.cell-prio--*` rule found in ApprovalView.vue — the styling was renamed, and this guard went blind")
	seen := map[string]bool{}
	for _, m := range found {
		name := m[1]
		assert.True(t, declared[name],
			"ApprovalView styles .cell-prio--%s, which no state.Priority* constant produces — dead CSS that reads like support", name)
		assert.False(t, seen[name], ".cell-prio--%s is declared twice", name)
		seen[name] = true
	}
}
