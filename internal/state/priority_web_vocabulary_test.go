// priority_web_vocabulary_test.go — 变更优先级的 TypeScript 侧必须与 internal/state 的
// Priority* 常量逐字相等，且只能是派生物，不能是第二份手抄。
//
// 为什么这条要有：`Priority` 联合（web/src/types/levee.ts）此前是**全仓唯一一份**
// 写成清单的优先级词表，Go 侧只有散落的裸字面量（创建路径的 `"normal"`）。TS 侧的
// `Record<Priority, string>` 能让"前端漏配一个标签"变成编译错误，但它看不见 Go 加了
// 第五个值——那正是这次拆列要防的那类漂移（同一个拼写服务两套语义，或者一套语义两个拼写）。
// 名单从 priority.go 解析、也从 TS 文件解析，两边都不复述。
//
// 现在值清单只有一份，而且是 internal/docgen 从 `PriorityNames()` 生成的
// web/src/types/vocabulary.ts；`Priority` 联合由那份数组派生，levee.ts 只再导出。
// 所以这里的四个方向变成：生成的数组 == owner（含顺序，下拉顺序是契约的一部分）、
// 联合是派生的、消费文件里不再有手抄拼写，以及 `PRIORITY_LABEL` / `PRIORITY_COLOR`
// 两张表和 ApprovalView 里手写的 `.cell-prio--<拼写>` 样式规则仍然逐个覆盖。
// 后三类是真正的第二语言产物：中文标签与 CSS 规则名没有 Go 归属，生成器产不出它们，
// 所以只能靠断言。规则名对着一个引擎永远不会产出的值，就是"看起来支持"的死 CSS
// （同族教训见 internal/cluster 的节点守卫）。
package state

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webConstListMembers extracts the quoted members of a generated
// `export const NAME = [ 'a', 'b', … ] as const` array, in file order.
//
// It replaces webUnionMembers, which parsed the hand-written `Priority` union out
// of levee.ts. Line-anchored opener and closer, and the class stops at "]": an
// unanchored `.*` would reach into the next declaration and report its members as
// this array's — which is how the predecessor reported a missing value as an
// extra one.
func webConstListMembers(t *testing.T, rel, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(repoFile(t, rel))
	require.NoError(t, err)
	block := regexp.MustCompile(`(?ms)^export const ` + regexp.QuoteMeta(name) +
		` = \[([^\]]*)\] as const`).FindStringSubmatch(string(raw))
	require.Len(t, block, 2,
		"could not find `export const %s = […] as const` in %s — the generator stopped emitting it, "+
			"or its shape changed and this guard went blind", name, rel)
	var out []string
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(block[1], -1) {
		out = append(out, m[1])
	}
	require.NotEmpty(t, out, "%s parsed empty in %s", name, rel)
	return out
}

// webSource reads a checked-in TypeScript source from the repository root.
func webSource(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(repoFile(t, rel))
	require.NoError(t, err)
	return string(raw)
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

// TestWebPriorityVocabularyIsDerivedNotReSpelled is the wiring half: moving the
// list into a generated file only kills the copy if nothing re-states it. A
// hand-written union beside the generated one would keep the value comparison in
// this file green while the pages read a set the engine never produces — which is
// the failure shape this whole file family was built to stop.
func TestWebPriorityVocabularyIsDerivedNotReSpelled(t *testing.T) {
	vocab := webSource(t, "web/src/types/vocabulary.ts")
	assert.Contains(t, vocab, "export type Priority = (typeof PRIORITIES)[number]",
		"the Priority union must be derived from the generated array; a hand-written union is a second copy")

	// The type every consumer imports must come from the generated file. Matched
	// as a property rather than as one spelling: `export type { X } from '…'` does
	// not bring X into the declaring file's own scope, so the working form here is
	// an `import type` plus an `export type` (and pinning only the re-export shape
	// is what made this guard fail on the run that fixed the scope).
	consumer := webSource(t, "web/src/types/levee.ts")
	assert.Regexp(t, `(?:import|export)\s+type\s*\{[^}]*\bPriority\b[^}]*\}\s+from\s+'\./vocabulary'`, consumer,
		"web/src/types/levee.ts must obtain Priority from the generated vocabulary (import or re-export)")
	assert.NotRegexp(t, `export type Priority\s*=`, consumer,
		"web/src/types/levee.ts declares its own Priority union beside the generated one")
	for _, value := range PriorityNames() {
		assert.NotContains(t, consumer, "'"+value+"'",
			"web/src/types/levee.ts spells the priority %q by hand — that is the copy this file exists to remove", value)
	}
}

func TestWebPriorityLabelsAndColorsCoverEveryPriority(t *testing.T) {
	labels := recordKeys(t, "web/src/utils/format.ts", "PRIORITY_LABEL")
	colors := recordKeys(t, "web/src/utils/format.ts", "PRIORITY_COLOR")
	for _, value := range PriorityNames() {
		assert.True(t, labels[value], "%q has no PRIORITY_LABEL entry — the Chinese console would render the wire spelling", value)
		assert.True(t, colors[value], "%q has no PRIORITY_COLOR entry", value)
	}
}

// TestWebPriorityRuntimeArrayMatchesTheVocabulary covers the array the instantiate
// wizard renders its <el-option> list from. It exists because the wizard used to
// hand-copy four option lines, which is a third owner of the vocabulary that a
// fourth Go value would silently miss — the union cannot be iterated at runtime, so
// an array is the only way the template can read the list from its owner.
//
// The array is the generated one, so this is the check that the generator was
// actually run: an owner value added to PriorityNames() and not regenerated lands
// here as a length mismatch, and the wizard would silently not offer it.
func TestWebPriorityRuntimeArrayMatchesTheVocabulary(t *testing.T) {
	listed := webConstListMembers(t, "web/src/types/vocabulary.ts", "PRIORITIES")
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
