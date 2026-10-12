// priority_web_vocabulary_test.go — 变更优先级的 TS 镜像必须与 internal/state 的
// Priority* 常量逐字相等。
//
// 为什么这条要有：`Priority` 联合（web/src/types/levee.ts）此前是**全仓唯一一份**
// 写成清单的优先级词表，Go 侧只有散落的裸字面量（创建路径的 `"normal"`）。TS 侧的
// `Record<Priority, string>` 能让"前端漏配一个标签"变成编译错误，但它看不见 Go 加了
// 第五个值——那正是这次拆列要防的那类漂移（同一个拼写服务两套语义，或者一套语义两个拼写）。
// 名单从 priority.go 解析、也从 TS 文件解析，两边都不复述。
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
