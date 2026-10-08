// system_verdict_vocabulary_test.go — doctor 判定值（pass/warn/fail）的 UI 镜像守卫。
//
// 这套值**没有 owning 常量**：生产者是 internal/grpc/system_service.go 里的裸字面量
// （`overall := "pass"`、`c.Status = "fail"` 等，见 :247-:321）。所以源头只能是那些赋值
// 语句本身——守卫解析它们，而不是复述一份清单。等哪天有人给判定值建了常量组，这条守卫应
// 该改成解析常量（那才是真正的所有权），但在那之前，它能挡住唯一会咬人的漂移：
// 后端多产出一个判定值，而 web/src/utils/diagnosis.ts 的表没有它 ⇒ 中文页面直接显示线上原值，
// 或更糟——被 fallback 当成已知值着色。
//
// 与 internal/diagnosis 那条健康守卫**分开**是刻意的：健康侧有常量组（HealthStatus，四个值，
// 含 unknown），判定侧只有三个裸字面量且没有 unknown。两张表并成一张就会把"探针说不清"和
// "这一项没通过"混为一谈（#92 里批次/分配两套词表就是这个形状）。
package grpc

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func verdictRepoFile(t *testing.T, rel string) string {
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

// emittedVerdicts parses the values system_service.go assigns to a check's status
// and to the overall doctor verdict.
func emittedVerdicts(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(verdictRepoFile(t, "internal/grpc/system_service.go"))
	require.NoError(t, err)

	out := map[string]bool{}
	re := regexp.MustCompile(`(?m)^[ \t]+(?:c\.Status|overall)(?::=| =) "([^"]+)"`)
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = true
	}
	require.NotEmpty(t, out, "no verdict assignments parsed from system_service.go — its shape changed")
	return out
}

func TestWebVerdictMirrorMatchesEmittedVerdicts(t *testing.T) {
	raw, err := os.ReadFile(verdictRepoFile(t, "web/src/utils/diagnosis.ts"))
	require.NoError(t, err)
	text := string(raw)

	decl := regexp.MustCompile(`(?m)^export const CHECK_VERDICTS = \[([^\]]*)\] as const$`).FindStringSubmatch(text)
	require.Len(t, decl, 2, "could not find the `export const CHECK_VERDICTS = [...] as const` declaration")
	ui := map[string]bool{}
	for _, m := range regexp.MustCompile(`['"]([^'"]+)['"]`).FindAllStringSubmatch(decl[1], -1) {
		ui[m[1]] = true
	}
	require.NotEmpty(t, ui, "CHECK_VERDICTS parsed empty")

	emitted := emittedVerdicts(t)
	for v := range emitted {
		assert.True(t, ui[v], "doctor verdict %q is produced by system_service.go but has no entry in CHECK_VERDICTS", v)
	}
	for v := range ui {
		assert.True(t, emitted[v], "web CHECK_VERDICTS lists %q, which the doctor endpoint never produces", v)
	}
	assert.Equal(t, len(emitted), len(ui), "the two verdict vocabularies differ in size, so one side has an entry the other lacks")

	// 判定侧不该出现健康侧的值：两套词表各有各的 fallback 语义。
	assert.False(t, emitted["healthy"], "healthy is a HealthStatus value, not a check verdict")
}

func TestWebVerdictLabelsCoverEveryEmittedVerdict(t *testing.T) {
	raw, err := os.ReadFile(verdictRepoFile(t, "web/src/utils/diagnosis.ts"))
	require.NoError(t, err)

	block := regexp.MustCompile(`(?m)^const VERDICT_LABEL: Record<CheckVerdict, string> = \{\n([\s\S]*?)^\}$`).FindStringSubmatch(string(raw))
	require.Len(t, block, 2, "could not find `VERDICT_LABEL: Record<CheckVerdict, string>` — its shape changed")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^[ \t]*(\w+):`).FindAllStringSubmatch(block[1], -1) {
		keys[m[1]] = true
	}
	require.NotEmpty(t, keys, "VERDICT_LABEL parsed empty")

	for v := range emittedVerdicts(t) {
		assert.True(t, keys[v], "verdict %q has no verdictLabel entry — the Chinese page would render %q verbatim", v, v)
	}
}
