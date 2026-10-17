// vocabulary_ownership_test.go — state 的两张顺序清单必须覆盖 store.go 里各自那组常量。
//
// 为什么解析放在 owner 包里而不是放在跨语言守卫里：跨语言守卫的职责是阻止**第二份抄件**
// 漂移，办法是把抄件改成由数据生成（internal/docgen）。但数据本身仍可能不全——在
// store.go 里加一个 `BatchStateWaiting = "waiting"` 而没加进 batchStateOrder，就会有一个
// 线上值在前端表里根本没有名字。那是对**本包声明**的断言，所以在本包里、对着本包源码
// 查一次，而不是让每个下游产物各自再解析一遍。
//
// 这条替换的旧形状：batch / assignment 两条跨语言守卫原本每次运行都同时解析 store.go
// 与 web/src/utils/{batch,assignment}.ts——解析抄件只能发现抄件和解析器不一致，发现不了
// 两边一起和服务器不一致（#153 就是这么过去的）。
package state

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stateVocabRepoFile(t *testing.T, rel string) string {
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

// goConstValues 从 store.go 解析某一组的常量，**不复述清单**：复述就会让新增常量静静通过。
// 标识符模式由调用方给出，因为这几组共用一个 const 块，靠名字前缀才分得开。
func goConstValues(t *testing.T, identifierPattern string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(stateVocabRepoFile(t, "internal/state/store.go"))
	require.NoError(t, err)
	out := map[string]string{}
	re := regexp.MustCompile(`(?m)^[ \t]*(` + identifierPattern + `)[ \t]*=[ \t]*"([^"]+)"$`)
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	require.NotEmpty(t, out,
		"no constant matching %q parsed from internal/state/store.go — the const block shape changed and this guard went blind",
		identifierPattern)
	return out
}

// assertListOwnsGroup 是两条断言共用的一侧：清单与常量组必须同集、同数、无重复。
func assertListOwnsGroup(t *testing.T, listName string, listed []string, declared map[string]string) {
	t.Helper()

	require.Len(t, listed, len(declared),
		"%s lists %d values while store.go declares %d in this group — a new constant would reach the wire with no name",
		listName, len(listed), len(declared))
	set := map[string]bool{}
	for _, value := range listed {
		assert.Falsef(t, set[value], "%s names %q twice", listName, value)
		set[value] = true
	}
	for name, value := range declared {
		assert.Truef(t, set[value], "%s (%q) is declared in store.go but missing from %s", name, value, listName)
	}
}

func TestBatchStateListCoversEveryBatchConstant(t *testing.T) {
	assertListOwnsGroup(t, "batchStateOrder", BatchStateNames(), goConstValues(t, `BatchState\w+`))
}

func TestAssignmentStateListCoversEveryAssignmentConstant(t *testing.T) {
	assertListOwnsGroup(t, "assignmentStateOrder", AssignmentStateNames(), goConstValues(t, `Assign(?:ment)?State\w+`))
}

// TestAssignmentListExcludesTheResultGroup is the boundary the two groups share a
// file but not a meaning: AssignResultCompleted / AssignResultFailed /
// AssignResultRolledBack (store.go) are a THIRD value range — they collide with the
// batch vocabulary's spellings, which is exactly how one vocabulary once came to be
// assumed to cover the other's rows. The identifier pattern cannot match them (no
// "State" in the name); this asserts that it stays that way if someone renames one.
func TestAssignmentListExcludesTheResultGroup(t *testing.T) {
	results := goConstValues(t, `AssignResult\w+`)
	listed := map[string]bool{}
	for _, value := range AssignmentStateNames() {
		listed[value] = true
	}
	for name, value := range results {
		assert.Falsef(t, listed[value],
			"%s (%q) belongs to the assignment RESULT range; putting it in assignmentStateOrder would let the batch and assignment tables describe different rows with the same word",
			name, value)
	}
}

func TestStateNameListsAreCopies(t *testing.T) {
	batch := BatchStateNames()
	require.NotEmpty(t, batch)
	batch[0] = "clobbered"
	assert.NotEqual(t, "clobbered", BatchStateNames()[0],
		"BatchStateNames handed out the owner's own slice; a caller could reorder the vocabulary docgen reads")

	assignment := AssignmentStateNames()
	require.NotEmpty(t, assignment)
	assignment[0] = "clobbered"
	assert.NotEqual(t, "clobbered", AssignmentStateNames()[0], "AssignmentStateNames handed out the owner's own slice")

	// The three lists added with the steps / run-approval / result tables. Same
	// property, because docgen's output order IS this order: a caller that
	// reordered the returned slice would rewrite the generated table's sequence.
	for name, fn := range map[string]func() []string{
		"StepStatusNames":     StepStatusNames,
		"ApprovalStatusNames": ApprovalStatusNames,
		"AssignResultNames":   AssignResultNames,
	} {
		got := fn()
		require.NotEmptyf(t, got, "%s returned nothing", name)
		got[0] = "clobbered"
		assert.NotEqualf(t, "clobbered", fn()[0], "%s handed out the owner's own slice", name)
	}
}

// The three groups below make the same claim as the two above, for the lists that
// feed the generated console tables: a constant added to store.go without joining
// its order list must fail HERE, where it is one package's own declaration, rather
// than silently widening the wire vocabulary nobody documented.
//
// The run-approval case carries the extra reason these lists were swept from
// writers: schema.sql's comment for runs.approval_status advertises `timeout` and
// `skipped`, and neither is declared here — those spellings belong to the
// approvals table's status column (internal/approval, which names its own list).
// The assertion says owner list == owner constants; it does NOT agree with the
// comment, and the divergence is stated in store.go rather than smoothed over by
// adding a member nobody writes.
func TestStepStatusListCoversEveryStepConstant(t *testing.T) {
	assertListOwnsGroup(t, "stepStatusOrder", StepStatusNames(), goConstValues(t, `StepStatus\w+`))
}

func TestRunApprovalStatusListCoversEveryRunApprovalConstant(t *testing.T) {
	assertListOwnsGroup(t, "runApprovalStatusOrder", ApprovalStatusNames(), goConstValues(t, `ApprovalStatus\w+`))
}

func TestAssignResultListCoversEveryResultConstant(t *testing.T) {
	assertListOwnsGroup(t, "assignResultOrder", AssignResultNames(), goConstValues(t, `AssignResult\w+`))
}

// TestRunApprovalListStaysApartFromTheApprovalsTable is the boundary this batch
// could most easily get wrong: two columns whose prose both say "status".
// Merging them into one table would let the console render a run-side verdict
// against a member set that column can never hold (that side names `expired`,
// this side has no such writer), and hide that they are two vocabularies.
func TestRunApprovalListStaysApartFromTheApprovalsTable(t *testing.T) {
	declared := goConstValues(t, `ApprovalStatus\w+`)
	require.Len(t, declared, 3,
		"runs.approval_status declares %d constants; the note in store.go says three, and the fourth spelling lives in internal/approval", len(declared))
	for _, value := range ApprovalStatusNames() {
		assert.NotEqualf(t, "expired", value,
			"ApprovalStatusNames carries %q, which belongs to the approvals table's status column", value)
	}
}
