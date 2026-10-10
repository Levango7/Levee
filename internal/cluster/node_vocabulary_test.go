// node_vocabulary_test.go — /cluster 节点卡片用的两套词表（NodeStatus / NodeRole）
// 必须与 web/src/utils/node.ts 的声明逐字相等，并且 node.ts 实际用到的每个圆点配色都要
// 在 web/src/styles/base.css 里真有规则。
//
// 为什么要这条：改版前的模板写的是 `{{ n.role }}` 和 `status === 'active' ? 'ok' : 'bad'`——
// 前者把线上拼写直接印在中文页面上；后者把三种状态压成两种颜色，于是"正在优雅下线"的节点
// 被涂成故障色（`leaving` 在 internal/cluster 里根本没有写入方，等于替引擎宣布它没宣布过的
// 故障）。用查表代替表达式之后，"新增状态忘了配中文/配色"是 vue-tsc 的编译错误；本文件再补
// 上查表本身覆盖不到的两件事：镜像清单与 Go 常量是否还是同一套词表、配色名在样式表里是否
// 真的存在（`warning` 与真名 `warn` 只差一个字母，而写错的后果是一个没有背景的圆点）。
//
// 名单一律**解析**而不是在这里复述：复述会让新增常量静静通过。两个方向都查——
// Go 有而 UI 没有 ⇒ 线上值直接怼到操作员眼前；UI 有而 Go 没有 ⇒ 前端在为一个引擎
// 产不出的拼写配色（批次那条守卫当初抓到的就是这个形状）。
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

// goNodeEnumValues parses declarations shaped like `StatusActive NodeStatus = "active"`
// in internal/cluster/node.go, keyed by the type name so the two groups stay apart.
func goNodeEnumValues(t *testing.T, typeName string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(nodeVocabRepoFile(t, "internal/cluster/node.go"))
	require.NoError(t, err)
	out := map[string]string{}
	re := regexp.MustCompile(`(?m)^[ \t]*(\w+)[ \t]+` + typeName + `[ \t]*=[ \t]*"([^"]+)"$`)
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	require.NotEmpty(t, out, "no %s constant parsed from internal/cluster/node.go — the declaration shape changed", typeName)
	return out
}

// webNodeMirror parses `export const NODE_STATUSES = [...] as const` (or the NODE_ROLES
// twin) out of web/src/utils/node.ts.
func webNodeMirror(t *testing.T, decl string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(nodeVocabRepoFile(t, "web/src/utils/node.ts"))
	require.NoError(t, err)
	line := regexp.MustCompile(`(?m)^export const ` + decl + ` = \[([^\]]*)\] as const$`).FindStringSubmatch(string(raw))
	require.Len(t, line, 2, "could not find the `export const %s = [...] as const` declaration in web/src/utils/node.ts", decl)
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`['"]([^'"]+)['"]`).FindAllStringSubmatch(line[1], -1) {
		out[m[1]] = true
	}
	require.NotEmpty(t, out, "%s parsed empty", decl)
	return out
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

func TestWebNodeStatusMirrorMatchesGoNodeStatuses(t *testing.T) {
	goStatuses := goNodeEnumValues(t, "NodeStatus")
	ui := webNodeMirror(t, "NODE_STATUSES")

	goValues := map[string]bool{}
	for _, value := range goStatuses {
		goValues[value] = true
		assert.True(t, ui[value], "node status %q has no entry in web/src/utils/node.ts NODE_STATUSES — the page would fall back to the raw wire value", value)
	}
	for value := range ui {
		assert.True(t, goValues[value], "web NODE_STATUSES lists %q, which no cluster.NodeStatus constant declares", value)
	}
	assert.Equal(t, len(goValues), len(ui), "the two status vocabularies differ in size, so one side has an entry the other lacks")
}

func TestWebNodeRoleMirrorMatchesGoRoles(t *testing.T) {
	goRoles := goNodeEnumValues(t, "NodeRole")
	ui := webNodeMirror(t, "NODE_ROLES")

	goValues := map[string]bool{}
	for _, value := range goRoles {
		goValues[value] = true
		assert.True(t, ui[value], "node role %q has no entry in web/src/utils/node.ts NODE_ROLES — the Chinese page would render it verbatim", value)
	}
	for value := range ui {
		assert.True(t, goValues[value], "web NODE_ROLES lists %q, which no cluster.NodeRole constant declares", value)
	}
	assert.Equal(t, len(goValues), len(ui), "the two role vocabularies differ in size, so one side has an entry the other lacks")
}

func TestNodeStatusTonesCoverEveryStateAndExistInStylesheet(t *testing.T) {
	styled := baseCSSDotTones(t)
	keys, tones := webNodeStatusToneMap(t)
	declared := map[string]bool{}
	for _, value := range goNodeEnumValues(t, "NodeStatus") {
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
