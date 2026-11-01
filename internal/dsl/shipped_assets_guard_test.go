package dsl_test

// Shipped-asset gates.
//
// examples_gate_test.go asks one question: "does it compile?". This file asks
// the questions a compile cannot answer, because the compile layer is not the
// layer where shipped assets actually break:
//
//	G1  取值层：工作流文档里不得出现打包层的 {{.x}} 槽位。
//	    模板层与语言层是两套取值机制（docs/leveelang-spec.md §2.1），把
//	    {{.new_primary}} 写进工作流文档，它在语言层永远不会被替换——值会
//	    原样送到目标机。
//	G2  取值链：声明的 input 必须被真正引用，引用的 input 必须被声明。
//	    两个方向都要：死声明让作者以为参数生效了；悬空引用让计划期取值
//	    直接失败。
//	G3  补偿键：step 级 rollback 不得含解析器不认识的键。yaml.v3 静默
//	    丢弃未知键，于是 `rollback: {action: ..., args: ...}` 这种旧形状
//	    会被解析成一个空壳补偿——注释说"这一步有补偿"，账本里没有。
//	G4  索引对齐：demo 索引（deploy/lab/DEMOS.md）与工作流文件双向对齐。
//	    索引漏了文件 = 场景没有实物；索引指向不存在的文件 = 演示会当场
//	    翻车。
//	G5  可执行集：动作必须在模块注册表的**可执行集**内。词表
//	    (actionSignatures) 与可执行集 (Module.Actions()) 是两个集合：词表
//	    里有、模块里没有的动作能过编译期，运行期才报 unsupported action。
//	G6  命令策略：shell.exec 的命令体与 verify 命令必须满足各自的运行期
//	    策略（shell 白名单 / verify 黑名单）。这两条策略原来只在运行期
//	    生效，编译期一律放过——于是已发布工作流"编译绿、apply 必炸"。
//
// 本文件在 package dsl_test（外部测试包）里：G5 需要读执行器的模块注册表，
// G6 需要读 shell 模块的命令策略，而 internal/executor 导入
// internal/dsl（executor/irreversible.go）——放在 package dsl 内部会成环。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/executor"
	_ "github.com/nexus/levee/internal/executor/modules/file"
	_ "github.com/nexus/levee/internal/executor/modules/mysql"
	_ "github.com/nexus/levee/internal/executor/modules/pkg"
	"github.com/nexus/levee/internal/executor/modules/shell"
	_ "github.com/nexus/levee/internal/executor/modules/svc"
	_ "github.com/nexus/levee/internal/executor/modules/user"
	"github.com/nexus/levee/internal/risk"
	"github.com/nexus/levee/internal/verify"
)

// repoRoot is the repository root relative to this package's directory.
const repoRoot = "../.."

// shippedDoc is one shipped YAML document under test.
type shippedDoc struct {
	// rel is the slash-separated path relative to the repository root, used
	// in failure messages so they point at a file the reader can open.
	rel string
	// abs is the path on disk.
	abs string
	src []byte
	wf  *dsl.Workflow
	// raw is the YAML decoded into generic maps — the only way to see keys
	// the DSL parser silently drops.
	raw map[string]any
	// isTemplatePayload marks files under examples/templates/: their
	// {{.name}} slots are substituted by the packaging layer when the
	// template record is instantiated, so the language-layer rules (G1/G2)
	// do not apply to them.
	isTemplatePayload bool
}

// shippedDocs collects every shipped workflow document: examples/** (minus
// the plugin manifests, which are a different document kind) and
// deploy/lab/workflows/**.
func shippedDocs(t *testing.T) []shippedDoc {
	t.Helper()
	var docs []shippedDoc
	for _, root := range []string{"examples", "deploy/lab/workflows"} {
		dir := filepath.Join(repoRoot, filepath.FromSlash(root))
		require.NoError(t, filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".yaml" && ext != ".yml" {
				return nil
			}
			slashed := filepath.ToSlash(path)
			if strings.Contains(slashed, "/plugins/") {
				return nil // plugin manifests belong to the plugin SDK shape
			}
			src, rerr := os.ReadFile(path)
			require.NoError(t, rerr)

			wf, perr := dsl.NewParser().ParseBytes(src)
			require.NoError(t, perr, "shipped document must parse: %s", slashed)

			var raw map[string]any
			require.NoError(t, yaml.Unmarshal(src, &raw), "shipped document must be valid YAML: %s", slashed)

			rel := strings.TrimPrefix(filepath.ToSlash(path), repoRoot+"/")
			docs = append(docs, shippedDoc{
				rel:               rel,
				abs:               path,
				src:               src,
				wf:                wf,
				raw:               raw,
				isTemplatePayload: strings.HasPrefix(rel, "examples/templates/"),
			})
			return nil
		}))
	}
	require.NotEmpty(t, docs, "no shipped documents found — the gates below would be vacuously green")
	return docs
}

// lineOf returns the 1-based line number of the first occurrence of needle.
func lineOf(src []byte, needle string) int {
	idx := strings.Index(string(src), needle)
	if idx < 0 {
		return 0
	}
	return strings.Count(string(src[:idx]), "\n") + 1
}

// templateSlotRe matches a packaging-layer slot ({{.name}}).
var templateSlotRe = regexp.MustCompile(`\{\{\s*\.[A-Za-z_][A-Za-z0-9_]*\s*\}\}`)

// inputRefRe matches a language-layer reference ({{input.name}}).
var inputRefRe = regexp.MustCompile(`\{\{\s*input\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// anyPlaceholderRe matches any placeholder of either layer.
var anyPlaceholderRe = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// G1 — 取值层：工作流文档不得使用打包层的槽位。
func TestShippedWorkflowsDoNotUseTemplateLayerSlots(t *testing.T) {
	var offenders []string
	for _, d := range shippedDocs(t) {
		if d.isTemplatePayload {
			continue // the packaging layer owns {{.name}} there, by definition
		}
		for _, m := range templateSlotRe.FindAllString(string(d.src), -1) {
			offenders = append(offenders,
				fmt.Sprintf("%s:%d: template-layer slot %q in a workflow document — the language layer "+
					"resolves {{input.<name>}}, so this value would reach the target unexpanded",
					d.rel, lineOf(d.src, m), m))
		}
	}
	assert.Empty(t, offenders,
		"shipped workflow documents must use the language layer ({{input.<name>}}), not the packaging layer")
}

// G2 — 取值链：声明与引用必须双向闭合。
func TestShippedWorkflowInputsAreDeclaredAndUsed(t *testing.T) {
	for _, d := range shippedDocs(t) {
		if d.isTemplatePayload {
			continue
		}
		declared := map[string]bool{}
		for _, in := range d.wf.Inputs {
			declared[in.Name] = true
		}

		referenced := map[string]bool{}
		var stringsInDoc []string
		for _, tg := range d.wf.Targets {
			stringsInDoc = append(stringsInDoc, tg.Query)
		}
		stringsInDoc = append(stringsInDoc, d.wf.Meta.Description)
		for _, st := range d.wf.Steps {
			collectStrings(st.Args, &stringsInDoc)
			if st.Rollback != nil {
				for _, undo := range st.Rollback.Steps {
					collectStrings(undo.Args, &stringsInDoc)
				}
			}
		}
		for _, s := range stringsInDoc {
			for _, m := range inputRefRe.FindAllStringSubmatch(s, -1) {
				referenced[m[1]] = true
			}
		}

		for name := range declared {
			assert.True(t, referenced[name],
				"%s declares input %q but never references {{input.%s}} — the declaration is a dead end",
				d.rel, name, name)
		}
		for name := range referenced {
			assert.True(t, declared[name],
				"%s references {{input.%s}} without declaring it — the value cannot be resolved at plan time",
				d.rel, name)
		}
	}
}

// G3 — 补偿键：step 级 rollback 不得含解析器不认识的键。
func TestShippedWorkflowRollbackKeysAreKnown(t *testing.T) {
	known := map[string]bool{
		"strategy":       true,
		"on_failure":     true,
		"verify_after":   true,
		"step":           true,
		"steps":          true,
		"snapshot_paths": true,
	}
	for _, d := range shippedDocs(t) {
		if raw, ok := d.raw["rollback"].(map[string]any); ok {
			assertKnownRollbackKeys(t, d, "rollback", raw, known)
		}
		steps, _ := d.raw["steps"].([]any)
		for i, rawStep := range steps {
			m, ok := rawStep.(map[string]any)
			if !ok {
				continue
			}
			rb, ok := m["rollback"].(map[string]any)
			if !ok {
				continue
			}
			assertKnownRollbackKeys(t, d, fmt.Sprintf("steps[%d].rollback", i), rb, known)
			// The compensation contract itself may carry nested steps.
			for j, nested := range rollbackNestedSteps(rb) {
				nestedRb, ok := nested["rollback"].(map[string]any)
				if !ok {
					continue
				}
				assertKnownRollbackKeys(t, d, fmt.Sprintf("steps[%d].rollback.steps[%d].rollback", i, j), nestedRb, known)
			}
		}
	}
}

// assertKnownRollbackKeys fails for every key the parser would drop.
func assertKnownRollbackKeys(t *testing.T, d shippedDoc, field string, rb map[string]any, known map[string]bool) {
	t.Helper()
	for k := range rb {
		assert.True(t, known[k],
			"%s: rollback.%s is not a key the parser knows — yaml.v3 drops it silently, so the "+
				"compensation you wrote is not the compensation that runs", d.rel, k)
	}
}

// rollbackNestedSteps returns the steps nested under a rollback block
// (both the singular `step` and the plural `steps` forms).
func rollbackNestedSteps(rb map[string]any) []map[string]any {
	var out []map[string]any
	if s, ok := rb["step"].(map[string]any); ok {
		out = append(out, s)
	}
	if list, ok := rb["steps"].([]any); ok {
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// G5 — 可执行集：动作必须在模块注册表里真的能被执行。
func TestShippedWorkflowActionsAreExecutable(t *testing.T) {
	executable := map[string]map[string]bool{}
	for _, name := range executor.DefaultExecutor().Modules() {
		m, ok := executor.DefaultExecutor().Module(name)
		if !ok {
			continue
		}
		actions := map[string]bool{}
		for _, a := range m.Actions() {
			actions[a] = true
		}
		executable[name] = actions
	}
	require.NotEmpty(t, executable, "no modules registered — the executable set would be empty")

	for _, d := range shippedDocs(t) {
		for _, st := range d.wf.Steps {
			assertActionExecutable(t, d, st, executable)
			if st.Rollback != nil {
				for _, undo := range st.Rollback.Steps {
					assertActionExecutable(t, d, undo, executable)
				}
			}
		}
	}
}

// assertActionExecutable checks one step against the executable set.
func assertActionExecutable(t *testing.T, d shippedDoc, st dsl.Step, executable map[string]map[string]bool) {
	t.Helper()
	actions, ok := executable[st.Module]
	if !ok {
		assert.Fail(t, fmt.Sprintf("%s: step %q uses module %q, which no executor module provides",
			d.rel, st.Name, st.Module))
		return
	}
	assert.True(t, actions[st.Action],
		"%s: step %q uses %s.%s, which the %s module cannot execute (executable: %s)",
		d.rel, st.Name, st.Module, st.Action, st.Module, strings.Join(sortedKeys(actions), ", "))
}

// G6 — 命令策略：命令体必须满足各自的运行期策略。
func TestShippedWorkflowCommandsMatchRuntimePolicy(t *testing.T) {
	for _, d := range shippedDocs(t) {
		for _, st := range d.wf.Steps {
			assertShellCommandPolicy(t, d, st)
			assertGateCommandPolicy(t, d, st.Name, st.Gate)
			if st.Rollback != nil {
				for _, undo := range st.Rollback.Steps {
					assertShellCommandPolicy(t, d, undo)
				}
			}
		}
		assertGateCommandPolicy(t, d, "workflow", d.wf.Gate)
		if d.wf.Batches.Gate != nil {
			assertGateCommandPolicy(t, d, "batches", d.wf.Batches.Gate)
		}
	}
}

// assertShellCommandPolicy checks a shell.exec command against the shell
// module's runtime allowlist. Placeholders are substituted with a safe token
// first: their resolved value is what reaches the target, and the guard is
// about the literal parts of the command.
func assertShellCommandPolicy(t *testing.T, d shippedDoc, st dsl.Step) {
	t.Helper()
	if st.Module != "shell" || st.Action != "exec" {
		return
	}
	raw, ok := st.Args["cmd"]
	if !ok {
		return
	}
	cmd, ok := raw.(string)
	if !ok {
		return
	}
	resolved := anyPlaceholderRe.ReplaceAllString(cmd, "PLACEHOLDER")
	assert.NoError(t, shell.ValidateCommand(resolved),
		"%s: step %q runs a command the shell module refuses at runtime — "+
			"use shell.script for anything with quotes, pipes, redirection or &&",
		d.rel, st.Name)
}

// assertGateCommandPolicy checks every cmd gate of one gate spec against the
// verify gate's runtime policy.
func assertGateCommandPolicy(t *testing.T, d shippedDoc, owner string, gate *dsl.GateSpec) {
	t.Helper()
	if gate == nil {
		return
	}
	for _, check := range append(append([]dsl.GateCheck{}, gate.Pre...), gate.Post...) {
		if check.Type != "cmd" {
			continue
		}
		assert.NoError(t, verify.ValidateGateCommand(check.Command),
			"%s: %s carries a verify command the gate refuses at runtime", d.rel, owner)
	}
	for _, check := range gate.Batch {
		if check.Type != "cmd" {
			continue
		}
		assert.NoError(t, verify.ValidateGateCommand(check.Command),
			"%s: %s carries a batch verify command the gate refuses at runtime", d.rel, owner)
	}
}

// G7 — 审批档位与审批人：高危档必须声明足够的 approvers。
//
// The rule is the engine's, not this guard's: the approval tier is
// max(workflow declaration, plan floor), and an irreversible step puts the
// floor at high. A high-tier chain that cannot reach its quorum is refused
// at PLAN time — the plan is not persisted and no approval row is created
// (internal/grpc/change_service.go, kickoffApproval). Measured on the real
// binary: "approval under-provisioned: high-level change needs 2 independent
// approver(s) but the workflow declares 0 votable ([])".
//
// The floor can also rise with the risk score (blast radius, batch fan-out),
// which is a plan-time value a file-level guard cannot see. This guard
// therefore covers the two statically knowable cases: a declared high tier
// and the irreversible red line.
func TestShippedWorkflowsDeclareApproversForHighTier(t *testing.T) {
	for _, d := range shippedDocs(t) {
		spec := d.wf.Approval
		declared := ""
		if spec != nil {
			declared = spec.Level
		}
		floor := ""
		if hasIrreversibleStep(d.wf) {
			floor = approval.LevelHigh
		}
		tier := risk.MaxLevel(declared, floor)
		if tier != approval.LevelHigh {
			continue
		}
		required := 2 // the high tier's quorum (internal/approval/levels.go)
		got := 0
		if spec != nil {
			got = len(spec.Approvers)
			if spec.MinApprovers > required {
				required = spec.MinApprovers
			}
		}
		assert.GreaterOrEqual(t, got, required,
			"%s: 高危档（tier=%s）需要 %d 个独立审批人，但只声明了 %d 个——计划期会被拒"+
				"（approval under-provisioned），且发起人不能投",
			d.rel, tier, required, got)
	}
}

// hasIrreversibleStep reports whether any step is irreversible, either by
// author declaration or by the inherent verb list (dsl.DefaultIrreversibleActions).
func hasIrreversibleStep(wf *dsl.Workflow) bool {
	for _, st := range wf.Steps {
		if st.Irreversible || dsl.IsInherentIrreversible(st.Module, st.Action) {
			return true
		}
	}
	return false
}

// G4 — 索引对齐：demo 索引 ↔ 工作流文件双向对齐。
//
// DEMOS.md 的场景行以 `| A1 |` / `| B1 |` 开头，行内必须出现恰好一个
// deploy/lab/workflows/... 路径。守卫要求：每个工作流文件被恰好一个场景
// 行认领，且每个场景行指向的文件真实存在。
func TestLabDemoIndexMatchesLabWorkflows(t *testing.T) {
	const demoIndex = "deploy/lab/DEMOS.md"
	const labDir = "deploy/lab/workflows"

	idx, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(demoIndex)))
	require.NoError(t, err, "the demo index %s must exist — it is the map from scenario to artifact", demoIndex)

	onDisk := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(repoRoot, filepath.FromSlash(labDir)))
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		onDisk[labDir+"/"+e.Name()] = true
	}
	require.NotEmpty(t, onDisk, "no lab workflows found on disk")

	claimed := map[string][]string{}
	ids := map[string]bool{}
	for i, line := range strings.Split(string(idx), "\n") {
		m := demoRowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		id := m[1]
		assert.False(t, ids[id], "%s: scenario %s is declared twice", demoIndex, id)
		ids[id] = true
		paths := labPathRe.FindAllString(line, -1)
		assert.Len(t, paths, 1,
			"%s:%d: scenario %s must point at exactly one %s/*.yaml file", demoIndex, i+1, id, labDir)
		for _, p := range paths {
			assert.True(t, onDisk[p],
				"%s:%d: scenario %s points at %s, which does not exist", demoIndex, i+1, id, p)
			claimed[p] = append(claimed[p], id)
		}
	}

	for _, id := range []string{"A1", "A2", "A3", "A4"} {
		assert.True(t, ids[id], "%s must declare scenario %s — the four customer-facing scenarios", demoIndex, id)
	}

	for path := range onDisk {
		owners := claimed[path]
		assert.Len(t, owners, 1,
			"%s: lab workflow %s must be claimed by exactly one scenario row (found: %v)",
			demoIndex, path, owners)
	}
	for path, owners := range claimed {
		for _, owner := range owners {
			assert.True(t, onDisk[path], "%s: scenario %s claims a missing file %s", demoIndex, owner, path)
		}
	}
}

// demoRowRe matches a demo-index scenario row and captures its id.
var demoRowRe = regexp.MustCompile(`^\|\s*([AB]\d+)\s*\|`)

// labPathRe matches a lab workflow path mentioned in the demo index.
var labPathRe = regexp.MustCompile(`deploy/lab/workflows/[A-Za-z0-9._-]+\.yaml`)

// collectStrings appends every string reachable from v.
func collectStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case map[string]any:
		for _, vv := range t {
			collectStrings(vv, out)
		}
	case []any:
		for _, vv := range t {
			collectStrings(vv, out)
		}
	}
}

// sortedKeys returns the sorted keys of a set.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
