// assignment_status_vocabulary_test.go — 分配状态的 UI 镜像必须与 internal/state 的
// **状态组**逐字相等。
//
// 为什么这条必须与批次那条（batch_status_vocabulary_test.go）分开写：两套词表在拼写上
// 重叠（pending / done / interrupted 双跨），语义却不重叠——批次的 pending 是"还没开始跑"，
// 分配的 pending 是"还没被 worker 领取"；批次的 done 是历史拼写、分配的 done 是活跃终态。
// 一旦有人把两张表并成一张，读页面的人就会把"没派出去"看成"没跑起来"。
//
// 解析只认 **状态组**：正则要求标识符里含 "State"，所以 AssignResultCompleted /
// AssignResultFailed / AssignResultRolledBack（store.go:744-746，另一组值域，UI 这张
// 表不渲染）不会被误收进来。
package state

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assignmentRepoFile 自己找仓库根：#90 里那个 repoFile 现在也在本包，但这条守卫不该
// 依赖"另一批是否已合"，所以本地留一份（同名复用会撞，故另起名）。
func assignmentRepoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("cannot locate %s from the test working directory", rel)
	return ""
}

// goAssignmentStates 从 store.go 解析分配**状态**常量，不复述清单：复述会让新增的
// 状态在这里静静通过。
func goAssignmentStates(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(assignmentRepoFile(t, "internal/state/store.go"))
	require.NoError(t, err)
	out := map[string]string{}
	re := regexp.MustCompile(`(?m)^[ \t]*(Assign(?:ment)?State\w+)[ \t]*=[ \t]*"([^"]+)"$`)
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	require.NotEmpty(t, out, "no Assign*State* constants parsed from store.go — the const block shape changed")
	// 结果组不得混进来（completed / failed / rolled_back 与批次词表同名不同物）。
	for name := range out {
		assert.NotContains(t, name, "Result", "%s is a result constant, not a state", name)
	}
	return out
}

func TestWebAssignmentMirrorMatchesGoAssignmentStates(t *testing.T) {
	raw, err := os.ReadFile(assignmentRepoFile(t, "web/src/utils/assignment.ts"))
	require.NoError(t, err)
	text := string(raw)

	decl := regexp.MustCompile(`(?m)^export const ASSIGNMENT_STATES = \[([^\]]*)\] as const$`).FindStringSubmatch(text)
	require.Len(t, decl, 2, "could not find the `export const ASSIGNMENT_STATES = [...] as const` declaration")
	ui := map[string]bool{}
	for _, m := range regexp.MustCompile(`['"]([^'"]+)['"]`).FindAllStringSubmatch(decl[1], -1) {
		ui[m[1]] = true
	}
	require.NotEmpty(t, ui, "ASSIGNMENT_STATES parsed empty")

	goStates := map[string]bool{}
	for _, v := range goAssignmentStates(t) {
		goStates[v] = true
		// 方向一：后端能产出的状态，UI 必须有名字，否则分布行直接显示线上值。
		assert.True(t, ui[v], "assignment state %q has no entry in web/src/utils/assignment.ts", v)
	}
	// 方向二：UI 不得凭空多出一个后端产不出的拼写（批次那边就曾经这样过）。
	for s := range ui {
		assert.True(t, goStates[s], "web ASSIGNMENT_STATES lists %q, which no Assign*State* constant declares", s)
	}
	assert.Equal(t, len(goStates), len(ui), "the two vocabularies differ in size, so one side has an entry the other lacks")
}

func TestWebAssignmentLabelsCoverEveryState(t *testing.T) {
	raw, err := os.ReadFile(assignmentRepoFile(t, "web/src/utils/assignment.ts"))
	require.NoError(t, err)

	block := regexp.MustCompile(`(?m)^const LABEL_BY_STATE: Record<AssignmentState, string> = \{\n([\s\S]*?)^\}$`).FindStringSubmatch(string(raw))
	require.Len(t, block, 2, "could not find `LABEL_BY_STATE: Record<AssignmentState, string>` — its shape changed")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^[ \t]*(\w+):`).FindAllStringSubmatch(block[1], -1) {
		keys[m[1]] = true
	}
	require.NotEmpty(t, keys, "LABEL_BY_STATE parsed empty")

	for name, value := range goAssignmentStates(t) {
		assert.True(t, keys[value], "%s (%q) has no assignmentLabel entry — the Chinese page would render %q verbatim", name, value, value)
	}
}
