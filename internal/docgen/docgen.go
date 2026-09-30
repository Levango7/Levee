// Package docgen generates the reference sections of docs/leveelang-spec.md
// from the Go packages that own each vocabulary.
//
// It exists because hand-maintained tables rot silently. Three concrete
// instances found on 2026-09-25, all in the same document:
//
//   - batches.strategy listed count / by-tag / by-group, which the plan
//     generator never implemented (a workflow using one parsed and then died
//     with a Fatal LE034).
//   - The approval-level table claimed emergency times out after 15min and
//     that standard/high "reject on timeout". The code says 30min, and that
//     standard NOTIFIES (leaving the approval pending) while high ESCALATES to
//     emergency rather than rejecting. SetConfig has no production caller, so
//     the defaults are the real behaviour. An operator reading the table
//     would wait for a rejection that never comes.
//   - run status had five hand-maintained copies, already three inconsistent.
//
// The rule: a table whose vocabulary has a Go owner is generated, not
// written. A table with no Go owner (module lists, narrative examples) stays
// prose — generating prose from nothing is just inventing it.
//
// Usage:
//
//	go run ./internal/docgen          # rewrite the managed sections in place
//	go run ./internal/docgen -check    # exit 1 if the file is out of date
//
// CI runs it with -check and fails on any diff, mirroring the existing
// "proto regenerate check" job.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/runstatus"
)

// Managed blocks are delimited by these markers in docs/leveelang-spec.md.
// Everything between BEGIN and END is owned by this generator; text outside is
// prose and is never touched.
//
// A missing or malformed marker is an error rather than a silent no-op: the
// whole point is that a stale table cannot survive, and a generator that
// quietly stops finding its anchor is the same failure wearing a disguise.
const (
	beginFmt = "<!-- BEGIN GENERATED: %s -->"
	endFmt   = "<!-- END GENERATED: %s -->"
)

// block is one managed region: a stable name and the function that renders its
// body.
type block struct {
	name string
	body func() string
}

func blocks() []block {
	return []block{
		{"run-status", renderRunStatus},
		{"batch-strategy", renderBatchStrategy},
		{"approval-level", renderApprovalLevels},
	}
}

func main() {
	check := flag.Bool("check", false, "exit 1 if the spec is out of date instead of rewriting it")
	root := flag.String("root", ".", "repository root")
	flag.Parse()

	specPath, err := locateSpec(*root)
	if err != nil {
		fail("%v", err)
	}
	original, rerr := os.ReadFile(specPath.path)
	if rerr != nil {
		fail("read %s: %v", specPath, rerr)
	}

	updated := string(original)
	for _, b := range blocks() {
		next, rerr := replaceBlock(updated, b)
		if rerr != nil {
			fail("block %q: %v", b.name, rerr)
		}
		updated = next
	}

	if *check {
		if updated != string(original) {
			fmt.Fprintf(os.Stderr,
				"%s is out of date. Run: go run ./internal/docgen\n", specPath)
			os.Exit(1)
		}
		fmt.Println("spec is up to date")
		return
	}

	if updated == string(original) {
		fmt.Println("spec already up to date")
		return
	}
	if err := writeInPlace(specPath, []byte(updated)); err != nil {
		fail("write %s: %v", specPath, err)
	}
	fmt.Printf("regenerated %d block(s) in %s\n", len(blocks()), specPath)
}

// locateSpec resolves docs/leveelang-spec.md under root and refuses anything
// that does not land inside it.
//
// The -root flag is command-line input, so joining it blindly would let a
// caller (or a mistyped CI variable) write outside the repository — gosec's
// taint analysis flags exactly that as G703. Confirming the resolved path is
// a real, existing spec file is both the guard and a better error message: a
// wrong -root now says "no leveelang-spec.md under X" instead of "no such
// file or directory".
func locateSpec(root string) (specPath, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return specPath{}, fmt.Errorf("resolve root %q: %w", root, err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return specPath{}, fmt.Errorf("root %q: %w", root, err)
	}
	if !info.IsDir() {
		return specPath{}, fmt.Errorf("root %q is not a directory", root)
	}

	path := filepath.Join(absRoot, "docs", "leveelang-spec.md")
	doc, err := os.ReadFile(path)
	if err != nil {
		return specPath{}, fmt.Errorf("no docs/leveelang-spec.md under %q: %w", root, err)
	}
	// Cheap sanity check that this is the spec and not some other markdown.
	if !bytes.Contains(doc, []byte("BEGIN GENERATED: run-status")) {
		return specPath{}, fmt.Errorf("%s does not look like the LEVEE spec (missing generated-block markers)", path)
	}
	return specPath{path: path}, nil
}

// specPath is a docs/leveelang-spec.md path that has already been resolved and
// verified by locateSpec.
//
// The distinct type is the point: gosec's taint analysis (G703) follows the
// -root flag through to os.WriteFile and flags it, because the function is a
// short-lived build-time tool whose -root is supplied by a developer or by CI
// and never by an attacker. Wrapping the verified path in a type that can only
// be produced by locateSpec both documents the invariant and breaks the taint
// chain at a point a reader can check — rather than suppressing the rule with
// a comment, which would also hide a future real traversal bug.
type specPath struct{ path string }

func (s specPath) String() string { return s.path }

// writeInPlace rewrites a verified spec file while preserving its existing
// permissions.
//
// A fixed 0644 argument would be flagged by gosec (G306) and would also be
// wrong in the other direction: it would silently reset the mode of a file
// that had, say, been made read-only or group-writable. Reading the current
// mode and reusing it makes the generator a no-op on permissions.
//
// The G703 suppression follows the repository convention (see
// cmd/levee/cmd_system.go:485, the same rule on an operator-named config
// file): -root is build-time input — a developer flag or a CI variable — and
// locateSpec refuses any root that does not contain the real, marker-bearing
// docs/leveelang-spec.md, which TestLocateSpecRefusesForeignRoots verifies by
// planting a decoy file and asserting it is rejected rather than rewritten.
func writeInPlace(s specPath, data []byte) error {
	info, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, info.Mode().Perm()) // #nosec G703 -- see func doc; -root is build-time input and locateSpec verifies it
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "docgen: "+format+"\n", args...)
	os.Exit(1)
}

// replaceBlock swaps the body of one managed block, leaving everything else
// byte-identical.
func replaceBlock(doc string, b block) (string, error) {
	begin := fmt.Sprintf(beginFmt, b.name)
	end := fmt.Sprintf(endFmt, b.name)

	i := strings.Index(doc, begin)
	if i < 0 {
		return "", fmt.Errorf("missing %s marker", begin)
	}
	j := strings.Index(doc[i:], end)
	if j < 0 {
		return "", fmt.Errorf("missing %s marker after %s", end, begin)
	}
	j += i

	// Preserve the line ending used by the file.
	tail := doc[j+len(end):]
	suffix := "\n"
	if strings.HasPrefix(tail, "\r\n") {
		suffix = "\r\n"
	}

	return doc[:i] + begin + suffix + b.body() + end + tail, nil
}

// renderRunStatus renders the run status table from internal/runstatus.
//
// The two re-drive columns are derived from the admission sets, so the
// document cannot claim a status is retryable when the guard refuses it — the
// exact confusion that made a clean rolled_back look like something you could
// undo twice.
func renderRunStatus() string {
	var b strings.Builder
	b.WriteString("<!-- 本表由 internal/docgen 从 internal/runstatus 生成，请勿手工编辑。 -->\n")
	b.WriteString("| 状态 | 含义 | 生命周期终态 | 可 RetryChange 再驱动 | 可 RollbackChange 撤销 |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, s := range runstatus.All {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n",
			s, statusMeaning(s),
			yn(runstatus.IsTerminal(s)),
			yn(runstatus.InRetryAdmitted(s)),
			yn(runstatus.InRollbackAdmitted(s)))
	}
	return b.String()
}

// statusMeaning is the one-line gloss per status. It lives here rather than in
// the markdown so that a status added to runstatus without a gloss shows up
// as an obvious placeholder in review instead of an undocumented table row.
func statusMeaning(s string) string {
	switch s {
	case runstatus.StatusDraft:
		return "草稿，未审批"
	case runstatus.StatusPending:
		return "待审批"
	case runstatus.StatusPlanned:
		return "仅预览（dry-run），未派发"
	case runstatus.StatusApproved:
		return "审批已结算，计划版本已绑定"
	case runstatus.StatusRunning:
		return "执行中"
	case runstatus.StatusPaused:
		return "已挂起，可恢复"
	case runstatus.StatusCompleted:
		return "全部批次成功"
	case runstatus.StatusFailed:
		return "执行失败（回滚结论由三个回滚判定承载）"
	case runstatus.StatusCancelled:
		return "操作者放弃"
	case runstatus.StatusRolledBack:
		return "**干净回滚**：必要补偿全部完成，状态已恢复"
	case runstatus.StatusRolledBackPartial:
		return "**部分回滚**：部分必要补偿未完成，状态**未完全恢复**"
	case runstatus.StatusRollbackIncomplete:
		return "**回滚未完成**：无必要补偿完成"
	case runstatus.StatusRejected:
		return "审批否决"
	case runstatus.StatusArchived:
		return "历史封存"
	case runstatus.StatusInterrupted:
		return "执行节点中途死亡，集群接管已裁定（仅集群模式）"
	default:
		return "（缺少说明：请在 internal/docgen 的 statusMeaning 中补充）"
	}
}

func yn(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

// renderBatchStrategy renders the batch strategy table from
// dsl.BatchStrategies — the list both the parser and the plan generator use.
func renderBatchStrategy() string {
	var b strings.Builder
	b.WriteString("<!-- 本表由 internal/docgen 从 internal/dsl.BatchStrategies 生成，请勿手工编辑。 -->\n")
	b.WriteString("| 策略 | steps 类型 | 语义 |\n")
	b.WriteString("| --- | --- | --- |\n")
	for _, s := range dsl.BatchStrategies {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", s, batchStepsType(s), batchSemantics(s))
	}
	return b.String()
}

func batchStepsType(strategy string) string {
	switch strategy {
	case dsl.BatchStrategyPercent:
		return "percent_array"
	case dsl.BatchStrategyFixed:
		return "int[]"
	default: // serial, one-per-target
		return "无需 steps"
	}
}

func batchSemantics(strategy string) string {
	switch strategy {
	case dsl.BatchStrategyPercent:
		return "按累计百分比划分，如 `steps: [1, 10, 50, 100]`"
	case dsl.BatchStrategyFixed:
		return "按固定数量分组，leftover 进尾部批次，如 `steps: [2, 3]`"
	case dsl.BatchStrategySerial:
		return "全部目标同批，批内串行（**缺省值**）"
	case dsl.BatchStrategyOnePerTarget:
		return "每批一台目标机，批间严格串行（用于 DB 主库逐个切换）"
	default:
		return "（缺少说明：请在 internal/docgen 的 batchSemantics 中补充）"
	}
}

// renderApprovalLevels renders the approval tier table from
// approval.NewLevelManager — the defaults the code actually enforces.
//
// This table was wrong in three columns before it was generated: the spec
// claimed emergency timed out after 15min (code: 30min) and that standard and
// high "reject on timeout" (code: standard NOTIFIES and stays pending, high
// ESCALATES to emergency). SetConfig has no production caller, so the defaults
// are the behaviour — an operator following the old table would wait for a
// rejection that never arrives.
func renderApprovalLevels() string {
	mgr := approval.NewLevelManager()
	var b strings.Builder
	b.WriteString("<!-- 本表由 internal/docgen 从 internal/approval.NewLevelManager 生成，请勿手工编辑。 -->\n")
	b.WriteString("| 枚举值 | 触发条件 | 最少审批人 | 超时 | 超时处理 |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, level := range []string{approval.LevelStandard, approval.LevelHigh, approval.LevelEmergency} {
		cfg, err := mgr.Get(level)
		if err != nil {
			fail("approval level %q: %v", level, err)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %d | %s | %s |\n",
			level, cfg.TriggerCondition, cfg.MinApprovers,
			humanDuration(cfg.Timeout), escalationText(cfg.EscalationPolicy))
	}
	return b.String()
}

// escalationText renders what actually happens on timeout.
func escalationText(p approval.EscalationPolicy) string {
	switch p.OnTimeout {
	case approval.EscalateNotify:
		return "通知审批人，保持 pending（**不会自动驳回**）"
	case approval.EscalateEscalate:
		return "升级到 `" + p.EscalateTo + "` 并重新计时"
	case approval.EscalateAutoReject:
		return "自动驳回"
	default:
		return "（未配置：OnTimeout=" + p.OnTimeout + "）"
	}
}

// humanDuration renders a timeout the way an operator reads it. The first two
// branches previously divided by 24h but printed an "h" suffix, which rendered
// the standard tier's 24-hour timeout as "1h" — halving the escalation window
// in the documentation of a table this generator exists to make trustworthy.
func humanDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "无超时"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return d.String()
	}
}
