// session_state_vocabulary_test.go — 会话状态的 UI 镜像必须与 String() 的真值一致。
//
// ConversationView 曾经把 `s.state` 的线上原值直接当文案渲染（还自带一个只认识 failed/done
// 的三元判色），修法是 web/src/utils/session.ts 那张表。表是抄件，所以要钉回源头。
//
// 源头不是一个常量组，而是 `SessionState.String()` 的 **return 语句**（session.go:52-70）：
// 这个枚举是 int，落库/上线的是它的字符串名。因此守卫解析的就是那些 return 值——新增一个
// 枚举成员而没有在 String() 里命名，或命名了而 UI 没配标签，都会在这里变红。复述一份清单
// 则两种情况都抓不到。
package conversation

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sessionRepoFile(t *testing.T, rel string) string {
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

// stringMethodValues returns the quoted values SessionState.String() can produce,
// in source order, by taking the body of that one method rather than every
// `return "…"` in the file.
func stringMethodValues(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(sessionRepoFile(t, "internal/conversation/session.go"))
	require.NoError(t, err)

	body := regexp.MustCompile(`(?ms)^func \(s SessionState\) String\(\) string \{\n([\s\S]*?)^\}$`).FindStringSubmatch(string(raw))
	require.Len(t, body, 2, "could not find `func (s SessionState) String() string { … }` — its shape changed")

	var out []string
	for _, m := range regexp.MustCompile(`(?m)^[ \t]*return "([^"]+)"$`).FindAllStringSubmatch(body[1], -1) {
		out = append(out, m[1])
	}
	require.NotEmpty(t, out, "SessionState.String() has no return-with-literal lines")
	return out
}

func TestWebSessionMirrorMatchesStringMethodValues(t *testing.T) {
	raw, err := os.ReadFile(sessionRepoFile(t, "web/src/utils/session.ts"))
	require.NoError(t, err)
	text := string(raw)

	decl := regexp.MustCompile(`(?ms)^export const SESSION_STATES = \[\n?([^\]]*)\] as const$`).FindStringSubmatch(text)
	require.Len(t, decl, 2, "could not find the `export const SESSION_STATES = [...] as const` declaration")
	ui := map[string]bool{}
	for _, m := range regexp.MustCompile(`['"]([^'"]+)['"]`).FindAllStringSubmatch(decl[1], -1) {
		ui[m[1]] = true
	}
	require.NotEmpty(t, ui, "SESSION_STATES parsed empty")

	emitted := map[string]bool{}
	for _, v := range stringMethodValues(t) {
		emitted[v] = true
		assert.True(t, ui[v], "session state %q can be emitted by String() but has no entry in web/src/utils/session.ts", v)
	}
	for v := range ui {
		assert.True(t, emitted[v], "web SESSION_STATES lists %q, which SessionState.String() never emits", v)
	}
	assert.Equal(t, len(emitted), len(ui), "the two vocabularies differ in size, so one side has an entry the other lacks")
}

func TestWebSessionLabelsCoverEveryState(t *testing.T) {
	raw, err := os.ReadFile(sessionRepoFile(t, "web/src/utils/session.ts"))
	require.NoError(t, err)

	block := regexp.MustCompile(`(?m)^const LABEL_BY_STATE: Record<SessionStateName, string> = \{\n([\s\S]*?)^\}$`).FindStringSubmatch(string(raw))
	require.Len(t, block, 2, "could not find `LABEL_BY_STATE: Record<SessionStateName, string>` — its shape changed")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^[ \t]*(\w+):`).FindAllStringSubmatch(block[1], -1) {
		keys[m[1]] = true
	}
	require.NotEmpty(t, keys, "LABEL_BY_STATE parsed empty")

	for _, v := range stringMethodValues(t) {
		assert.True(t, keys[v], "session state %q has no sessionStateLabel entry — the Chinese page would render %q verbatim", v, v)
	}
}
