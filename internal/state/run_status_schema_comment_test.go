// run_status_schema_comment_test.go — `runs.status` 的行内注释必须等于
// `internal/runstatus` 声明的那套值，两个引擎各自校验。
//
// 这条是第三次的同一形状（#91 的 batches.status、ZCode 的 steps.status 与
// runs.approval_status），而且落在**被读得最多**的那一列上。改前的注释写：
//
//	-- pending|planning|awaiting_approval|running|verifying|completed|failed|rolled_back|aborted
//
// 其中 `planning` / `awaiting_approval` / `verifying` / `aborted` **四个拼写在非测试
// 代码里零产出**（逐个 `git grep -c '"planning"' …` = 0），既没有任何 `runstatus.Status*`
// 常量声明它们，也没有任何写入点；反过来 owner 声明的 15 个里有 **10 个不在注释上**
// （draft / planned / approved / rejected / paused / cancelled / rolled_back_partial /
// rollback_incomplete / interrupted / archived）。按注释写查询的人会在四列不存在的名
// 字上得到空结果集，并且**不会报错**——这正是 #91 记下的代价。
//
// 名单一律从 `internal/runstatus/runstatus.go` **解析**，不在本文件复述：复述一份清单
// 会让新增常量静静通过。解析/表块作用域用本包已有的 `columnCommentList`，两处词表守卫
// 共用同一份解析实现——同包出现两份各自漂移的解析式，是我这族修复一直在防的。
package state

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runstatusDeclared parses the declared run-status vocabulary out of its owning
// package. Constant *names* are kept (not just values) so a failure message can point
// at the declaration a comment disagrees with.
func runstatusDeclared(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(schemaRepoFile(t, "internal/runstatus/runstatus.go"))
	require.NoError(t, err)
	// Anchored to a declaration line: `StatusDraft = "draft"`. Doc comments above each
	// constant repeat these very words, so an unanchored search would harvest prose too.
	re := regexp.MustCompile(`(?m)^[ \t]*(Status\w+)[ \t]*=[ \t]*"([^"]+)"\r?$`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	require.NotEmpty(t, out, "no Status* constants parsed from internal/runstatus/runstatus.go — the const block shape changed")
	require.Len(t, out, 15, "expected the fifteen declared run statuses, parsed %d — the vocabulary or this guard's pattern moved", len(out))
	return out
}

func TestRunsStatusCommentMatchesRunstatusConstants(t *testing.T) {
	constants := runstatusDeclared(t)
	declared := map[string]bool{}
	for _, value := range constants {
		declared[value] = true
	}

	for _, rel := range []string{"internal/state/schema.sql", "internal/state/pgschema.sql"} {
		listed := columnCommentList(t, rel, "runs", "status")
		seen := map[string]bool{}
		for _, name := range listed {
			assert.False(t, seen[name], "%s: %q listed twice in runs.status", rel, name)
			seen[name] = true
			assert.True(t, declared[name],
				"%s: runs.status comment lists %q, which no runstatus.Status* constant declares — a reader would query a state the engine cannot produce", rel, name)
		}
		for _, value := range constants {
			assert.True(t, seen[value], "%s: runs.status comment omits %q, which internal/runstatus does declare", rel, value)
		}
	}
}

// TestRunsStatusCommentIsNotAnotherColumnsComment 钉住"解析落在 runs 表块内"这件事。
// 同一份文件里 batches / steps / approvals 各有一行同形的 `status TEXT NOT NULL, -- …`
// 注释（#91 就是被这个坑到过：按"文件里第一条 status 行"解析会读到另一套词表）。
// 这里不假设行号，而是把四条列表都取出来，要求 runs 的那条与其余三条**都不同**，
// 且带着只有运行状态会有的成员。
func TestRunsStatusCommentIsNotAnotherColumnsComment(t *testing.T) {
	joined := func(list []string) string {
		out := ""
		for _, s := range list {
			out += s + "|"
		}
		return out
	}

	for _, rel := range []string{"internal/state/schema.sql", "internal/state/pgschema.sql"} {
		runs := joined(columnCommentList(t, rel, "runs", "status"))
		batches := joined(columnCommentList(t, rel, "batches", "status"))
		steps := joined(columnCommentList(t, rel, "steps", "status"))
		assert.NotEqual(t, batches, runs, "%s: runs.status parsed to the BATCHES vocabulary — the block scope broke", rel)
		assert.NotEqual(t, steps, runs, "%s: runs.status parsed to the STEPS vocabulary — the block scope broke", rel)
		// `draft` is produced only by the run path (CreateChange / CloneChange); no batch
		// or step row can carry it. If these two vanish, the parse left the runs block.
		assert.Contains(t, runs, "draft|", "%s: runs.status comment lost its own marker value", rel)
		assert.Contains(t, runs, "archived", "%s: runs.status comment lost its own marker value", rel)
	}
}

// TestRunsStatusCommentHasNoPhantomSpellings 是这次修复的具名回归条目：那四个零产出的
// 拼写一旦回到注释上，失败信息直接点名是哪个。主测试也抓得到，但这条给的是可读的归因。
func TestRunsStatusCommentHasNoPhantomSpellings(t *testing.T) {
	phantom := []string{"planning", "awaiting_approval", "verifying", "aborted"}
	for _, rel := range []string{"internal/state/schema.sql", "internal/state/pgschema.sql"} {
		seen := map[string]bool{}
		for _, name := range columnCommentList(t, rel, "runs", "status") {
			seen[name] = true
		}
		for _, name := range phantom {
			assert.False(t, seen[name],
				"%s: runs.status comment lists %q again — no runstatus constant declares it and nothing in the engine writes it", rel, name)
		}
	}
}
