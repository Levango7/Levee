// assignment_status_vocabulary_test.go — 分配状态的 UI 镜像必须与 internal/state 的
// **状态组**逐字相等。
//
// 为什么这条必须与批次那条（batch_status_vocabulary_test.go）分开写：两套词表在拼写上
// 重叠（pending / done / interrupted 双跨），语义却不重叠——批次的 pending 是"还没开始跑"，
// 分配的 pending 是"还没被 worker 领取"；批次的 done 是历史拼写、分配的 done 是活跃终态。
// 一旦有人把两张表并成一张，读页面的人就会把"没派出去"看成"没跑起来"。
//
// 名单不再由本文件解析：那份解析搬进 owner 侧的 vocabulary_ownership_test.go，检查
// assignmentStateOrder 覆盖 store.go 的全部 Assign*State* 常量，并且**只认状态组**
// （标识符里必须含 "State"），所以 AssignResultCompleted / AssignResultFailed /
// AssignResultRolledBack（store.go:744-746，另一组值域，UI 这张表不渲染）不会被收进来。
package state

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWebAssignmentMirrorMatchesOwnerList reads the GENERATED vocabulary rather than
// assignment.ts, and separately asserts that assignment.ts takes the list instead of
// keeping one.
//
// The two-directional set comparison it replaces parsed assignment.ts's array and
// store.go's constants on every run — which could catch a copy disagreeing with the
// parser, but not the pair disagreeing with the server (#153).
func TestWebAssignmentMirrorMatchesOwnerList(t *testing.T) {
	assert.Equal(t, AssignmentStateNames(),
		webConstListMembers(t, "web/src/types/vocabulary.ts", "ASSIGNMENT_STATES"),
		"web/src/types/vocabulary.ts ASSIGNMENT_STATES has drifted from state.AssignmentStateNames(); regenerate with `go run ./internal/docgen`")

	assertWiredNotCopied(t, "web/src/utils/assignment.ts",
		[]string{"ASSIGNMENT_STATES"}, []string{"AssignmentState"})
}

func TestWebAssignmentLabelsCoverEveryState(t *testing.T) {
	raw := webSource(t, "web/src/utils/assignment.ts")

	block := regexp.MustCompile(`(?m)^const LABEL_BY_STATE: Record<AssignmentState, string> = \{\n([\s\S]*?)^\}$`).FindStringSubmatch(raw)
	require.Len(t, block, 2, "could not find `LABEL_BY_STATE: Record<AssignmentState, string>` — its shape changed")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^[ \t]*(\w+):`).FindAllStringSubmatch(block[1], -1) {
		keys[m[1]] = true
	}
	require.NotEmpty(t, keys, "LABEL_BY_STATE parsed empty")

	// The label table is the part with no Go owner: nothing can generate 待领取.
	// A state that reaches the panel without one prints the wire word in a Chinese
	// page, which is the defect this file was written for.
	for _, value := range AssignmentStateNames() {
		assert.Truef(t, keys[value], "%q has no assignmentLabel entry — the Chinese page would render it verbatim", value)
	}
}
