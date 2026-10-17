// node_vocabulary_test.go — /cluster 节点卡片用的两套词表（NodeStatus / NodeRole）。
//
// 三件事，各自一条：① **值集合**：web/src/types/vocabulary.ts 里那两张由
// internal/docgen 从本包 `NodeStatusNames()` / `NodeRoleNames()` 生成的数组，必须与
// owner 的数据逐项相等（含顺序）；② **接线**：web/src/utils/node.ts 必须从生成物取值，
// 不许在自己文件里再声明一份数组或联合；③ **没有 Go owner 的那一半**：每个状态在
// node.ts 的配色表里真被点名，而表里用到的每个 tone 都在 web/src/styles/base.css 里
// 真有 `.lv-dot--<tone>` 规则。
//
// 为什么要这条：改版前的模板写的是 `{{ n.role }}` 和 `status === 'active' ? 'ok' : 'bad'`——
// 前者把线上拼写直接印在中文页面上；后者把三种状态压成两种颜色，于是"正在优雅下线"的节点
// 被涂成故障色（`leaving` 在 internal/cluster 里根本没有写入方，等于替引擎宣布它没宣布过的
// 故障）。用查表代替表达式之后，"新增状态忘了配中文/配色"是 vue-tsc 的编译错误；本文件管
// 查表本身覆盖不到的那两件事：清单与 owner 是不是同一套、配色名在样式表里是否真的存在
// （`warning` 与真名 `warn` 只差一个字母，而写错的后果是一个没有背景的圆点）。
//
// 这个文件不再解析 Go 常量：那份解析搬回 owner 自己包里，成了
// vocabulary_ownership_test.go 里"清单必须覆盖 node.go 全部声明"的完整性测试。跨语言
// 这一侧从此只比数据与产物——比抄件是 #153 的形状（守卫核对的是自己的抄写，四处产物
// 互相自洽、一起与服务器不一致，全程没有一处报错）。
package cluster

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nodeVocabRepoFile 自己找仓库根：本包没有现成 helper，且这条守卫不依赖别的包的测试
// 文件是否已合。
func nodeVocabRepoFile(t *testing.T, rel string) string {
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

// webNodeStatusToneMap parses node.ts's `TONE_BY_STATUS` block and returns both sides:
// the statuses it names (keys) and the tone classes it can emit (values). Returning the
// keys too is what makes this a coverage check rather than a spelling check — vue-tsc
// enforces totality against the *union*, and only here can we compare it with what the
// Go registry actually declares.
func webNodeStatusToneMap(t *testing.T) (map[string]bool, map[string]bool) {
	t.Helper()
	raw, err := os.ReadFile(nodeVocabRepoFile(t, "web/src/utils/node.ts"))
	require.NoError(t, err)
	block := regexp.MustCompile(`(?m)^const TONE_BY_STATUS: Record<NodeStatus, DotTone> = \{\n([\s\S]*?)^\}$`).FindStringSubmatch(string(raw))
	require.Len(t, block, 2, "could not find `const TONE_BY_STATUS: Record<NodeStatus, DotTone> = {` — its shape changed")
	keys := map[string]bool{}
	tones := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^[ \t]*(\w+)[ \t]*:[ \t]*'([^']+)'`).FindAllStringSubmatch(block[1], -1) {
		keys[m[1]] = true
		tones[m[2]] = true
	}
	require.NotEmpty(t, keys, "TONE_BY_STATUS parsed empty — the map shape changed")
	require.NotEmpty(t, tones, "TONE_BY_STATUS has no tone values — the map shape changed")
	return keys, tones
}

// baseCSSDotTones returns every tone for which web/src/styles/base.css defines
// `.lv-dot--<tone>`. Reading the stylesheet rather than a list written here is the point:
// an unmapped tone renders a dot with no background at all, which looks like "this row has
// no state" instead of "this state is unknown".
func baseCSSDotTones(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(nodeVocabRepoFile(t, "web/src/styles/base.css"))
	require.NoError(t, err)
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.lv-dot--([a-zA-Z][\w-]*)`).FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = true
	}
	require.NotEmpty(t, out, "no `.lv-dot--*` rule found in web/src/styles/base.css — the palette renamed itself")
	return out
}

// generatedVocabularyList reads one array out of the GENERATED vocabulary file.
//
// This is not the guard's old shape: it used to parse web/src/utils/node.ts, a
// hand-written transcription, and compare that with the constants it also parsed —
// a check that could only notice the copy disagreeing with the parser, never the
// two of them disagreeing with the server (#153). The file below is written by
// internal/docgen from this package's own data, so reading it verifies that the
// generator ran and that the shipped bytes still say what the registry says.
func generatedVocabularyList(t *testing.T, constName string) []string {
	t.Helper()
	raw, err := os.ReadFile(nodeVocabRepoFile(t, "web/src/types/vocabulary.ts"))
	require.NoError(t, err)
	block := regexp.MustCompile(`(?ms)^export const ` + regexp.QuoteMeta(constName) +
		` = \[([^\]]*)\] as const`).FindStringSubmatch(string(raw))
	require.Len(t, block, 2,
		"web/src/types/vocabulary.ts declares no `export const %s = […] as const` — the generator stopped emitting it, "+
			"or its shape changed and this guard went blind", constName)
	var out []string
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(block[1], -1) {
		out = append(out, m[1])
	}
	require.NotEmpty(t, out, "%s parsed empty from the generated vocabulary", constName)
	return out
}

func TestWebNodeStatusMirrorMatchesGoNodeStatuses(t *testing.T) {
	assert.Equal(t, NodeStatusNames(), generatedVocabularyList(t, "NODE_STATUSES"),
		"web/src/types/vocabulary.ts NODE_STATUSES has drifted from cluster.NodeStatusNames(); "+
			"add the status to internal/cluster (node.go + vocabulary.go), then run `go run ./internal/docgen`")
}

func TestWebNodeRoleMirrorMatchesGoRoles(t *testing.T) {
	assert.Equal(t, NodeRoleNames(), generatedVocabularyList(t, "NODE_ROLES"),
		"web/src/types/vocabulary.ts NODE_ROLES has drifted from cluster.NodeRoleNames(); regenerate with `go run ./internal/docgen`")
}

// TestWebNodeVocabularyIsWiredNotCopied is the half the value comparison cannot
// see: node.ts must obtain both lists from the generated file instead of keeping a
// transcription beside it. Moving the lists is only half of killing a copy — if the
// page re-declared them, the check above would stay green while the UI rendered a
// set the registry never produced.
func TestWebNodeVocabularyIsWiredNotCopied(t *testing.T) {
	raw, err := os.ReadFile(nodeVocabRepoFile(t, "web/src/utils/node.ts"))
	require.NoError(t, err)
	source := string(raw)

	for _, decl := range []string{"NODE_ROLES", "NODE_STATUSES"} {
		assert.NotRegexpf(t, `(?m)^export const `+regexp.QuoteMeta(decl)+`\s*=\s*\[`, source,
			"web/src/utils/node.ts re-declares %s as its own array — that is the second copy this file exists to remove", decl)
	}
	for _, typeName := range []string{"NodeRole", "NodeStatus"} {
		assert.NotRegexpf(t, `(?m)^export type `+regexp.QuoteMeta(typeName)+`\s*=`, source,
			"web/src/utils/node.ts declares its own %s union beside the generated one", typeName)
	}
	assert.Regexp(t, `import type \{[^}]*NodeStatus[^}]*\} from '@/types/vocabulary'`, source,
		"web/src/utils/node.ts must take NodeStatus from the generated vocabulary (import + export, so the tables below can use it)")
	assert.Regexp(t, `(?m)^export \{[^}]*NODE_STATUSES[^}]*\}`, source,
		"web/src/utils/node.ts must keep re-exporting NODE_STATUSES — ./node.spec.ts and the cluster page read it from here")
}

// TestNodeStatusTonesCoverEveryStateAndExistInStylesheet is what has no Go owner:
// the tone each state gets, and whether that tone is a real rule in the stylesheet.
// The status names now come from this package's data rather than from a parse of
// its own source, so the check is the same claim with one fewer copy in the path.
func TestNodeStatusTonesCoverEveryStateAndExistInStylesheet(t *testing.T) {
	styled := baseCSSDotTones(t)
	keys, tones := webNodeStatusToneMap(t)
	declared := map[string]bool{}
	for _, value := range NodeStatusNames() {
		declared[value] = true
	}

	// 方向一：注册表里每个状态都必须被配色表点名，否则它落到 nodeStatusTone 的兜底色，
	// 页面看起来正常而没人会去查配色表是不是漏了这一行。
	for value := range declared {
		assert.True(t, keys[value], "node status %q has no entry in web/src/utils/node.ts TONE_BY_STATUS — it silently takes the fallback colour", value)
	}
	// 方向二：配色表里不得留着注册表产不出的状态名。
	for status := range keys {
		assert.True(t, declared[status], "TONE_BY_STATUS names %q, which no cluster.NodeStatus constant declares", status)
	}
	// 方向三：实际会发出的每个 tone 都得在样式表里真有规则。`warning` 与真名 `warn` 只差
	// 一个字母，而写错的后果是一个没有背景的圆点——样式表读出来比对是唯一能发现它的方式。
	for tone := range tones {
		assert.True(t, styled[tone], "web/src/utils/node.ts emits tone %q but web/src/styles/base.css defines no `.lv-dot--%s` rule — the dot would render with no background", tone, tone)
	}
}
