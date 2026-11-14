package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/dbchange"
)

func init() {
	RegisterCommand(newDBChangeCmd())
}

// newDBChangeCmd builds the `dbchange` command group.
//
// The group exists now with one member because the ticket is a composition
// surface: `show` reads one change, and the next members (`list`, `export`) are
// the same read model over a window or a printable document. Registering the
// group up front means those additions do not move the existing command path.
func newDBChangeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dbchange",
		Short: "数据库变更发布单（DB change ticket）",
		Long: "数据库变更发布单：把一个变更的 SQL 批、审核结论、回滚计划、执行窗口、审批链、" +
			"执行证据与审计链完整性合成一张可出示的单。全部字段都从既有制品派生，不写回、不重算。",
	}
	cmd.AddCommand(newDBChangeShowCmd())
	return cmd
}

func newDBChangeShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <change-id>",
		Short: "合成并打印一个变更的发布单",
		Args:  cobra.ExactArgs(1),
		RunE:  runDBChangeShow,
	}
}

func runDBChangeShow(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	tk, err := dbchange.Collect(ctx, store, args[0])
	if err != nil {
		if errors.Is(err, dbchange.ErrNotFound) {
			return fmt.Errorf("change %q not found", args[0])
		}
		return err
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  tk,
			"meta":  nil,
			"error": nil,
		})
	}
	printDBChangeTicket(os.Stdout, tk)
	return nil
}

// printDBChangeTicket renders the ticket for a human reader.
//
// Every section is printed even when empty, and an empty section says what it
// means ("no SQL", "no rollback declared") rather than disappearing: a reader
// must not have to know that a missing line is the signal.
func printDBChangeTicket(w io.Writer, tk *dbchange.Ticket) {
	c := tk.Change
	fmt.Fprintf(w, "数据库变更发布单: %s\n", c.ID)
	fmt.Fprintf(w, "  名称:     %s\n", orNone(c.Label))
	fmt.Fprintf(w, "  状态:     %s\n", c.Status)
	fmt.Fprintf(w, "  优先级:   %s\n", orNone(c.Priority))
	fmt.Fprintf(w, "  环境:     %s\n", orNone(c.Environment))
	fmt.Fprintf(w, "  发起人:   %s\n", orNone(c.Creator))
	fmt.Fprintf(w, "  工作流:   %s\n", orNone(c.Workflow))
	fmt.Fprintf(w, "  计划哈希: %s\n", orNone(c.PlanHash))

	fmt.Fprintln(w, "SQL（已审核）:")
	if !tk.SQL.HasSQL {
		fmt.Fprintln(w, "  无可审核的 SQL 步骤")
	} else {
		for _, s := range tk.SQL.Steps {
			fmt.Fprintf(w, "  步骤 %s:\n    %s\n", s.Step, s.SQL)
		}
	}
	if r := tk.SQL.Review; r != nil {
		fmt.Fprintf(w, "  审核结论: %s（fail %d / warn %d）\n", r.Verdict, r.FailCount, r.WarnCount)
		for _, f := range r.Findings {
			fmt.Fprintf(w, "    - [%s/%s] 步骤 %q 语句 %d: %s\n", f.Rule, f.Severity, f.Step, f.Statement, f.Message)
		}
	} else {
		fmt.Fprintln(w, "  审核结论: 无（该计划没有可审核的 SQL 面）")
	}

	fmt.Fprintln(w, "回滚计划:")
	if !tk.Rollback.HasUndo {
		fmt.Fprintln(w, "  未声明回滚")
	} else {
		fmt.Fprintf(w, "  策略: %s（失败处置 %s，回滚后验证 %v）\n",
			orNone(tk.Rollback.Strategy), orNone(tk.Rollback.OnFailure), tk.Rollback.VerifyAfter)
		for _, p := range tk.Rollback.SnapshotPaths {
			fmt.Fprintf(w, "  快照路径: %s\n", p)
		}
		for _, u := range tk.Rollback.StepUndos {
			fmt.Fprintf(w, "  步骤补偿: %s（%s，%d 个 undo 步骤）\n", u.Step, orNone(u.Strategy), u.UndoSteps)
		}
	}

	fmt.Fprintln(w, "执行窗口:")
	fmt.Fprintf(w, "  校验时机: %s\n", tk.Window.EnforcedAt)
	fmt.Fprintf(w, "  存放位置: %s\n", windowPersistenceLabel(tk.Window.Persistence))
	if tk.Window.Persistence != "run-column" {
		fmt.Fprintf(w, "  说明: %s\n", tk.Window.Note)
	}

	fmt.Fprintln(w, "审批链:")
	if !tk.Approval.HasApproval {
		fmt.Fprintln(w, "  未进入审批链")
	} else {
		fmt.Fprintf(w, "  级别: %s  状态: %s  绑定当前计划: %v\n",
			orNone(tk.Approval.Level), orNone(tk.Approval.Status), tk.Approval.MatchesPlan)
		fmt.Fprintf(w, "  审批人: %s（待决 %d / 已批 %d / 已拒 %d）\n",
			orNone(strings.Join(tk.Approval.Approvers, ", ")),
			tk.Approval.PendingCount, tk.Approval.ApprovedCount, tk.Approval.RejectedCount)
		for _, d := range tk.Approval.Decisions {
			fmt.Fprintf(w, "    - %s %s: %s\n", d.Approver, d.Action, orNone(d.Reason))
		}
	}

	fmt.Fprintln(w, "执行证据:")
	fmt.Fprintf(w, "  判定: %s\n", tk.Execution.Verdict)
	if tk.Execution.Reason != "" {
		fmt.Fprintf(w, "  说明: %s\n", tk.Execution.Reason)
	}
	fmt.Fprintf(w, "  批次 %d / 步骤 %d / 目标 %d\n",
		tk.Execution.Batches, tk.Execution.Steps, tk.Execution.TotalTargets)

	fmt.Fprintln(w, "审计链:")
	if !tk.Audit.Verified {
		if tk.Audit.Unverifiable {
			fmt.Fprintln(w, "  未验证: 该变更还没有 trace 记录")
		} else {
			fmt.Fprintln(w, "  未验证")
		}
	} else {
		fmt.Fprintf(w, "  已验证: %v（记录 %d，其中 legacy %d）\n", tk.Audit.Valid, tk.Audit.Count, tk.Audit.LegacyCount)
	}
}

// windowPersistenceLabel renders the machine value of the window's storage
// location as the sentence a reader expects. An unknown value is passed
// through rather than replaced, so a future shape surfaces instead of being
// silently mapped onto an old one.
func windowPersistenceLabel(p string) string {
	switch p {
	case "workflow-declaration":
		return "工作流声明（plan 期校验，不随 run 持久化）"
	case "run-column":
		return "run 行持久化（可回放准入窗口）"
	default:
		return orNone(p)
	}
}

// orNone renders an empty string as a visible placeholder. An empty value and
// a missing value are the same fact here; printing nothing would make the
// reader guess which one it was.
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(无)"
	}
	return s
}
