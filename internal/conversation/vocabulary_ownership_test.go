// vocabulary_ownership_test.go — SessionStateNames() 必须覆盖 session.go 里声明的每个状态。
//
// 清单本身是从 String() 现取的（见 vocabulary.go），所以这里要钉的是两件解析也救不了的
// 事：① 枚举的**上界**——`sessionStateLast` 是手写的，在 StateFailed 之后再声明一个状态而
// 不放宽它，那个状态既不会出现在清单里也不会报错；② 每个状态的 String() 是不是**各自不同**
// 的——漏写一个 case，String() 会落到 default 分支返回 "unknown"，于是清单里出现两个 unknown，
// 而控制台把"引擎从没到过的状态"当成一个正经档位来配色。
//
// 状态数是从 session.go 的 iota 块解析出来的：这是对**本包声明**的断言，只在本包解析一次；
// 跨语言那一侧（internal/docgen 与 web/src/utils/session.ts）从此只吃数据，不再解析抄件。
package conversation

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declaredSessionStates 读出 iota 块里的状态名，顺序即声明顺序（iota 没有空洞，所以第 k 个
// 名字的值就是 k）。扫描从 `SessionState = iota` 那一行开始，到块结束的 ")" 为止——整份
// session.go 里还有别的 State* 标识符（消息角色、引擎阶段），不收窄就会一起进来。
func declaredSessionStates(t *testing.T) []string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	var raw []byte
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, "internal", "conversation", "session.go")
		if _, statErr := os.Stat(p); statErr == nil {
			raw, err = os.ReadFile(p)
			break
		}
		dir = filepath.Dir(dir)
	}
	require.NoError(t, err)
	require.NotEmpty(t, raw, "cannot locate internal/conversation/session.go")

	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, ln := range lines {
		if strings.Contains(ln, "SessionState = iota") {
			start = i
			break
		}
	}
	require.NotEqual(t, -1, start, "`SessionState = iota` not found in session.go — the enum is declared differently now")

	entry := regexp.MustCompile(`^[ \t]+(\w+)(?:[ \t]+SessionState[ \t]*=[ \t]*iota)?[ \t]*$`)
	var names []string
	for _, ln := range lines[start:] {
		trimmed := strings.TrimSpace(ln)
		if trimmed == ")" {
			break
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if m := entry.FindStringSubmatch(ln); m != nil {
			names = append(names, m[1])
		}
	}
	require.NotEmpty(t, names, "no SessionState constant parsed from the iota block — the guard went blind")
	return names
}

func TestSessionStateNamesCoverEveryDeclaredState(t *testing.T) {
	declared := declaredSessionStates(t)

	// ① 上界：清单遍历到 sessionStateLast，所以它必须正好是最后一个声明的状态。
	assert.EqualValuesf(t, len(declared)-1, sessionStateLast,
		"sessionStateLast is %d but session.go declares %d states (%s … %s) — the last one is unnamed everywhere downstream",
		int(sessionStateLast), len(declared), declared[0], declared[len(declared)-1])

	names := SessionStateNames()
	require.Len(t, names, len(declared)+1,
		"SessionStateNames returns %d entries for %d declared states plus the out-of-range fallback", len(names), len(declared))

	// ② 每个状态一个**互不相同且不是兜底**的名字。
	seen := map[string]bool{}
	for i, name := range names[:len(declared)] {
		assert.NotEqualf(t, SessionStateUnknownName, name,
			"%s (value %d) has no case in String(): it would be reported as %q, the same word an out-of-range row gets",
			declared[i], i, SessionStateUnknownName)
		assert.Falsef(t, seen[name], "%q is the String() of two different states", name)
		seen[name] = true
	}
	assert.Equal(t, SessionStateUnknownName, names[len(declared)],
		"the fallback spelling must stay last in SessionStateNames — the console renders the list in order")
}

func TestSessionStateNamesIsACopy(t *testing.T) {
	names := SessionStateNames()
	require.NotEmpty(t, names)
	names[0] = "clobbered"
	assert.NotEqual(t, "clobbered", SessionStateNames()[0],
		"SessionStateNames handed out a slice a caller could reorder before docgen reads it")
}
