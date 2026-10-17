// session_state_vocabulary_test.go — 会话状态的 UI 侧必须用生成物，且标签表要覆盖每个状态。
//
// ConversationView 曾经把 `s.state` 的线上原值直接当文案渲染（还自带一个只认识 failed/done
// 的三元判色），修法是 web/src/utils/session.ts 那张表。表原本是抄件，所以要钉回源头；现在
// 源头到前端的值是 internal/docgen 从本包 `SessionStateNames()` 生成到
// web/src/types/vocabulary.ts 的，因此这里剩两件事可查：① 生成物与本包数据仍然逐项相等
// （生成器跑过、产物没被手改）；② session.ts 只是接线，不再自己声明一份，而**标签表**
// 必须覆盖每个状态——中文措辞没有 Go owner，是唯一还得手工钉的那一半。
//
// 源头不是一个常量组，而是 `SessionState.String()` 的返回值（session.go:52-70）：这个枚举是
// int，落库/上线的是它的字符串名。SessionStateNames() 直接从 String() 取值，所以没有第二份
// 清单可忘；"枚举上界没跟着放宽"与"某个状态漏了 case（于是和越界兜底同名）"由本包的
// vocabulary_ownership_test.go 钉住。
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

// readSessionFile returns a checked-in source file as text. Both web-side tests
// below read the same way, so a read failure is a locate error rather than an
// assertion about an empty string.
func readSessionFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(sessionRepoFile(t, rel))
	require.NoError(t, err)
	return string(raw)
}

// generatedVocabularyList reads one array out of the GENERATED vocabulary file.
//
// Reading the shipped artefact is not the guard's old shape: it used to parse
// session.ts, a hand-written transcription, and compare that with String()'s return
// values it also parsed. That can only see the copy disagreeing with the parser —
// never the two of them disagreeing with the server, which is how #153 shipped.
func generatedVocabularyList(t *testing.T, constName string) []string {
	t.Helper()
	raw, err := os.ReadFile(sessionRepoFile(t, "web/src/types/vocabulary.ts"))
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

func TestWebSessionMirrorMatchesStringMethodValues(t *testing.T) {
	assert.Equal(t, SessionStateNames(), generatedVocabularyList(t, "SESSION_STATES"),
		"web/src/types/vocabulary.ts SESSION_STATES has drifted from conversation.SessionStateNames(); "+
			"add the state (and its String() case) in session.go, then run `go run ./internal/docgen`")

	source := readSessionFile(t, "web/src/utils/session.ts")
	assert.NotRegexp(t, `(?m)^export const SESSION_STATES\s*=\s*\[`, source,
		"web/src/utils/session.ts re-declares SESSION_STATES as its own array — that is the second copy the generated vocabulary exists to remove")
	assert.NotRegexp(t, `(?m)^export type SessionStateName\s*=`, source,
		"web/src/utils/session.ts declares its own SessionStateName union beside the generated one")
	assert.Regexp(t, `import type \{[^}]*\bSessionStateName\b[^}]*\} from '@/types/vocabulary'`, source,
		"web/src/utils/session.ts must take SessionStateName from the generated vocabulary (import + export, so the label table can use it)")
	assert.Regexp(t, `(?m)^export \{[^}]*\bSESSION_STATES\b[^}]*\}`, source,
		"web/src/utils/session.ts must keep re-exporting SESSION_STATES — ./session.spec.ts and ConversationView read it from here")
}

func TestWebSessionLabelsCoverEveryState(t *testing.T) {
	source := readSessionFile(t, "web/src/utils/session.ts")

	block := regexp.MustCompile(`(?m)^const LABEL_BY_STATE: Record<SessionStateName, string> = \{\n([\s\S]*?)^\}$`).FindStringSubmatch(source)
	require.Len(t, block, 2, "could not find `LABEL_BY_STATE: Record<SessionStateName, string>` — its shape changed")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^[ \t]*(\w+):`).FindAllStringSubmatch(block[1], -1) {
		keys[m[1]] = true
	}
	require.NotEmpty(t, keys, "LABEL_BY_STATE parsed empty")

	for _, v := range SessionStateNames() {
		assert.Truef(t, keys[v], "session state %q has no sessionStateLabel entry — the Chinese page would render %q verbatim", v, v)
	}
}
