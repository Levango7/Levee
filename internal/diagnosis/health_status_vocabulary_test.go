// health_status_vocabulary_test.go — 系统健康值的 UI 镜像必须与 owning 常量一致。
//
// 为什么存在：SystemView 过去把 `status.status` 的线上原值直接当文案渲染，页面其余部分是中文。
// 修法是 web/src/utils/diagnosis.ts 里一张查表，而查表是**抄件**——抄件会漂，这一族已经出过
// 两次线上缺陷（批次状态两套词表 #88、chart 默认镜像 tag #73）。这里把抄件钉回源头。
//
// 源头是 internal/diagnosis/health_probe.go 的 `HealthStatus` 常量组。**从源码解析而不是复述
// 一份清单**：复述的话，新增一个 `StatusX HealthStatus = "…"` 不会有任何东西变红。
//
// 另一套词表（doctor 的 pass/warn/fail 判定）**故意不在这里**：它没有 owning 常量，生产者
// 是 internal/grpc/system_service.go 里的裸字面量，由 internal/grpc 的守卫单独钉。
// 两者混在一张表里就会重演 #92 的问题——`unknown` 在健康侧是"探针说不清"，判定侧根本没这个值。
package diagnosis

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func healthRepoFile(t *testing.T, rel string) string {
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

// declaredHealthStates parses `Status<X> HealthStatus = "<value>"` out of the
// owning source file.
func declaredHealthStates(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(healthRepoFile(t, "internal/diagnosis/health_probe.go"))
	require.NoError(t, err)
	out := map[string]string{}
	re := regexp.MustCompile(`(?m)^[ \t]*(\w+)[ \t]+HealthStatus[ \t]+=[ \t]"([^"]+)"`)
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	require.NotEmpty(t, out, "no HealthStatus constants parsed from health_probe.go — its shape changed")
	require.Len(t, out, 4, "expected the four declared health states, got %v", out)
	return out
}

func TestWebHealthMirrorMatchesHealthStatusConstants(t *testing.T) {
	raw, err := os.ReadFile(healthRepoFile(t, "web/src/utils/diagnosis.ts"))
	require.NoError(t, err)
	text := string(raw)

	decl := regexp.MustCompile(`(?m)^export const HEALTH_STATES = \[([^\]]*)\] as const$`).FindStringSubmatch(text)
	require.Len(t, decl, 2, "could not find the `export const HEALTH_STATES = [...] as const` declaration")
	ui := map[string]bool{}
	for _, m := range regexp.MustCompile(`['"]([^'"]+)['"]`).FindAllStringSubmatch(decl[1], -1) {
		ui[m[1]] = true
	}
	require.NotEmpty(t, ui, "HEALTH_STATES parsed empty")

	declared := map[string]bool{}
	for _, v := range declaredHealthStates(t) {
		declared[v] = true
		assert.True(t, ui[v], "health state %q has no entry in web/src/utils/diagnosis.ts HEALTH_STATES", v)
	}
	for v := range ui {
		assert.True(t, declared[v], "web HEALTH_STATES lists %q, which no HealthStatus constant declares", v)
	}
	assert.Equal(t, len(declared), len(ui), "the two vocabularies differ in size, so one side has an entry the other lacks")
}

func TestWebHealthLabelsCoverEveryState(t *testing.T) {
	raw, err := os.ReadFile(healthRepoFile(t, "web/src/utils/diagnosis.ts"))
	require.NoError(t, err)

	block := regexp.MustCompile(`(?m)^const HEALTH_LABEL: Record<HealthState, string> = \{\n([\s\S]*?)^\}$`).FindStringSubmatch(string(raw))
	require.Len(t, block, 2, "could not find `HEALTH_LABEL: Record<HealthState, string>` — its shape changed")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^[ \t]*(\w+):`).FindAllStringSubmatch(block[1], -1) {
		keys[m[1]] = true
	}
	require.NotEmpty(t, keys, "HEALTH_LABEL parsed empty")

	for name, value := range declaredHealthStates(t) {
		assert.True(t, keys[value], "%s (%q) has no healthLabel entry — the Chinese page would render %q verbatim", name, value, value)
	}
}
