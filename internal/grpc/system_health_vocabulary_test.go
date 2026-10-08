package grpc

// system_health_vocabulary_test.go — /system/status 的取值必须来自 owning 常量，
// 而且前端类型声明不许自造词。
//
// 起因：`GetStatus` 以前写的是裸字面量 `"healthy"` / `"degraded"`，而这套值的常量组在
// internal/diagnosis（health_probe.go:39-49）。同仓已经有一模一样的缺陷落了两次：
// #88（state 按 "done" 判完成、引擎写 "completed"，两套词表各自都有绿测试）与
// #91 里 template/clone.go 用本包常量写 state 的列。手抄的代价是"拼写巧合对齐"，
// 不是契约。本批把两处换成 diagnosis 常量，这条测试把关系钉住：
//
//	① GetStatus 能产出的值，必须逐个来自 diagnosis 的常量（不再是字面量）；
//	② 这些值必须在 web 的类型声明里存在（前端少一个值就是渲染原值/漏分支）；
//	③ web 的类型不许出现 diagnosis 常量组之外的词。
//
// ②③ 是双向的：只测"前端有"会漏掉前端自造词，只测"后端有"会漏掉前端少值。
// 已知的宽松处：类型里现在有 `unhealthy`，而本端点当前只会产出 healthy/degraded
// （warnings 非空即降级，见 system_service.go:150-153）——所以断言方向是
// "产出 ⊆ 类型"，不是相等；这条差异如实写在这里而不是被断言抹平。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/diagnosis"
)

// emittedByGetStatus mirrors what the handler can assign today. It references the
// constants rather than repeating strings, so a value change in diagnosis shows
// up here instead of silently passing.
func emittedByGetStatus() []string {
	return []string{string(diagnosis.StatusHealthy), string(diagnosis.StatusDegraded)}
}

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

// declaredHealthValues parses the HealthStatus constant group from its source,
// so a newly added constant is visible without editing this test.
func declaredHealthValues(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(healthRepoFile(t, "internal/diagnosis/health_probe.go"))
	require.NoError(t, err)
	out := map[string]bool{}
	re := regexp.MustCompile(`(?m)^[ \t]*(\w+)[ \t]+HealthStatus[ \t]+=[ \t]"([^"]+)"`)
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[2]] = true
	}
	require.NotEmpty(t, out, "no HealthStatus constants parsed -- the const block shape changed")
	return out
}

// getStatusBody returns the source of the GetStatus handler, by scanning from its
// signature to the first line that is a bare `}` in column 0. A regex over the
// whole body was tried first and silently matched nothing -- line scanning fails
// loudly instead.
func getStatusBody(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(healthRepoFile(t, "internal/grpc/system_service.go"))
	require.NoError(t, err)
	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "func (s *SystemService) GetStatus(") {
			start = i
			break
		}
	}
	require.NotEqual(t, -1, start, "could not find `func (s *SystemService) GetStatus(` in system_service.go")
	for j := start + 1; j < len(lines); j++ {
		if lines[j] == "}" {
			return strings.Join(lines[start:j+1], "\n")
		}
	}
	t.Fatalf("GetStatus body has no closing `}` at column 0 (line %d onward)", start+1)
	return ""
}

func TestGetStatusEmitsOnlyOwnedConstants(t *testing.T) {
	body := getStatusBody(t)

	// The two values this handler produces must come from the owning constants.
	assert.Contains(t, body, "string(diagnosis.StatusHealthy)",
		"GetStatus should assign diagnosis.StatusHealthy, not spell the value out")
	assert.Contains(t, body, "string(diagnosis.StatusDegraded)",
		"GetStatus should assign diagnosis.StatusDegraded, not spell the value out")

	// And no `status` field of the response may be handed a bare string. Scoped to
	// the response on purpose: `state.RunFilter{Status: "running"}` a few lines down
	// is the run vocabulary, which this guard must not touch.
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Status:") && !strings.HasPrefix(trimmed, "resp.Status =") {
			continue
		}
		assert.NotRegexp(t, `(Status:|resp\.Status =)\s*"`, line,
			"bare literal assigned to the health status; use the diagnosis constants")
	}

	for _, v := range emittedByGetStatus() {
		assert.True(t, declaredHealthValues(t)[v], "%q is not a declared HealthStatus value", v)
	}
}

func TestWebSystemStatusTypeMatchesEmittedValues(t *testing.T) {
	raw, err := os.ReadFile(healthRepoFile(t, "web/src/types/levee.ts"))
	require.NoError(t, err)

	// No terminating semicolon in this file's interface members -- a `;` in the
	// pattern would make the whole match vanish and the test would report a
	// missing anchor rather than a wrong expectation.
	m := regexp.MustCompile(`(?ms)export interface SystemStatus \{.*?^\s+status:\s*(.+)$`).FindStringSubmatch(string(raw))
	require.Len(t, m, 2, "could not find the `status:` member of `export interface SystemStatus` in web/src/types/levee.ts")

	union := map[string]bool{}
	for _, v := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(m[1], -1) {
		union[v[1]] = true
	}
	require.NotEmpty(t, union, "SystemStatus.status union parsed empty from %q", m[1])

	declared := declaredHealthValues(t)
	// 方向一：端点能产出的每个值，前端类型里都得有，否则消费方会在自己的分支里漏一条。
	for _, v := range emittedByGetStatus() {
		assert.True(t, union[v], "GetStatus can emit %q but web's SystemStatus.status union does not allow it", v)
	}
	// 方向二：前端类型不许出现常量组之外的自造词。
	for v := range union {
		assert.True(t, declared[v], "web's SystemStatus.status allows %q, which no HealthStatus constant declares", v)
	}
}
