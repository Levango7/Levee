// batch_status_schema_comment_test.go — batches.status 的行内注释必须与
// internal/state 的常量一致。
//
// 这条守卫存在的理由是一条具体的假文档：两个引擎的 schema 都把这一列注释成
// `pending|running|completed|failed|skipped`。`skipped` 没有任何 BatchState 常量、
// 也没有任何批次行的写入点（internal/wiring/persist.go:222/429 的 "skipped" 是
// **步骤**状态，写进 steps.status），而真实写入的 rolled_back、以及 batchDoneStates
// 认账的 done / interrupted 反倒不在名单上。读注释的人因此会以为库里可能出现
// skipped，并据此写查询或 UI —— 这一族误读正是 #88 的成因（两套词表各自都绿）。
//
// 与 internal/runstatus/vocabulary_guard_test.go 同形态：那是 run 状态的注释/镜像
// 守卫，这是批次状态这一层的对应件。
package state

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaRepoFile walks up from the test working directory to locate a repository
// file, so the guard works from any package depth.
//
// Named apart from the sibling helper in batch_status_vocabulary_test.go on
// purpose: this branch must compile and run on its own (a PR stacked on another
// open PR gets no CI at all here — `on.pull_request.branches` filters by base),
// so it cannot borrow a helper that branch has not landed yet.
func schemaRepoFile(t *testing.T, rel string) string {
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

// schemaDeclaredBatchStates parses the constant block in store.go rather than
// restating it — a restatement would let a newly added constant pass unnoticed.
func schemaDeclaredBatchStates(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(schemaRepoFile(t, "internal/state/store.go"))
	require.NoError(t, err)
	out := map[string]string{}
	// \r? before $: the repository stores LF, but a Windows working copy may
	// materialise CRLF, and (?m)$ matches before the \n — leaving the \r to
	// break the match and turn this guard into a silent no-op on that machine.
	for _, m := range regexp.MustCompile(`(?m)^[ \t]*(BatchState\w+)[ \t]*=[ \t]*"([^"]+)"\r?$`).FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	require.NotEmpty(t, out, "no BatchState constants parsed from store.go — the const block shape changed")
	return out
}

// statusCommentList 从给定 schema 文件里取 batches 表 status 行的注释名单。
//
// 必须落在 batches 这张表的块里：steps 表也有一行同形的 `status TEXT NOT NULL, -- …`
// 注释，按"文件里第一条 status 行"解析会读到 steps 的词表。
func statusCommentList(t *testing.T, rel string) []string {
	t.Helper()
	raw, err := os.ReadFile(schemaRepoFile(t, rel))
	require.NoError(t, err)
	src := string(raw)

	block := regexp.MustCompile(`(?ms)CREATE TABLE IF NOT EXISTS batches \(.*?\n\);`).FindString(src)
	require.NotEmpty(t, block, "could not find the `CREATE TABLE IF NOT EXISTS batches (…) …);` block in "+rel)

	line := regexp.MustCompile(`(?m)^[ \t]*status[ \t]+TEXT[ \t]+NOT NULL,?[ \t]*--[ \t]*(.+?)\r?$`).FindStringSubmatch(block)
	require.Len(t, line, 2, "batches.status has no `TEXT NOT NULL, -- list` line in "+rel)

	var out []string
	for _, s := range strings.Split(line[1], "|") {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	require.NotEmpty(t, out, "parsed an empty comment list out of "+rel)
	return out
}

func TestBatchesStatusCommentMatchesConstants(t *testing.T) {
	constants := schemaDeclaredBatchStates(t)
	declared := map[string]bool{}
	for _, v := range constants {
		declared[v] = true
	}
	require.Len(t, constants, 7, "expected the seven declared batch states, got %d", len(constants))

	for _, rel := range []string{"internal/state/schema.sql", "internal/state/pgschema.sql"} {
		listed := statusCommentList(t, rel)
		assert.Len(t, listed, len(declared), "%s: the comment must list exactly the declared batch states", rel)

		seen := map[string]bool{}
		for _, name := range listed {
			assert.False(t, seen[name], "%s: %q listed twice", rel, name)
			seen[name] = true
			assert.True(t, declared[name],
				"%s: batches.status comment lists %q, which no BatchState constant declares — a reader would code against a state the engine cannot produce", rel, name)
		}
		for _, value := range constants {
			assert.True(t, seen[value], "%s: batches.status comment omits %q, which internal/state does declare", rel, value)
		}
	}
}

func TestBatchesStatusCommentIsNotTheStepsComment(t *testing.T) {
	// The two tables carry different vocabularies (steps have success/skipped, batches
	// never do). Keeping them apart is what makes the block-scoped parse above worth
	// something: if the parse ever slips onto the steps line, this goes red.
	raw, err := os.ReadFile(schemaRepoFile(t, "internal/state/schema.sql"))
	require.NoError(t, err)
	stepsBlock := regexp.MustCompile(`(?ms)CREATE TABLE IF NOT EXISTS steps \(.*?\n\);`).FindString(string(raw))
	require.NotEmpty(t, stepsBlock, "could not find the steps table block")
	assert.Contains(t, stepsBlock, "skipped", "the steps comment is the one that documents skipped")

	assert.NotContains(t, strings.Join(statusCommentList(t, "internal/state/schema.sql"), "|"), "skipped")
}
