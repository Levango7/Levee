package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// Build-time variables injected via -ldflags "-X main.version=...". They default
// to "dev" / "" so that a go build / go run without ldflags still works.
var (
	version    = "dev"
	buildTime  = "unknown"
	goVersion  = runtime.Version()
	commitHash = "unknown"
)

// Global option values. They are populated by cobra from the persistent flags
// defined on the root command and read by sub-commands through the Option*
// helpers. Keeping them as package-level variables (rather than threading them
// through every command) mirrors the cobra/viper idiom used by most Go CLIs.
var (
	optConfigPath string
	optJSON       bool
	optQuiet      bool
	optVerbose    bool
	optNoColor    bool
	optProfile    string
	optTimeout    string
	optAPIURL     string
	optAPIToken   string

	// Dual-mode flags. The CLI is local (single zero-dependency binary, talks to
	// an in-process store) unless --remote says otherwise. Neither flag is on by
	// default: naming both is an error, naming neither means local. That is what
	// makes `--remote` usable on its own — when --local defaulted to true, every
	// `--remote` invocation also set --local and the mutual-exclusion check could
	// never be wired up without refusing the documented agent commands.
	optLocal  bool
	optRemote bool
	optServer string

	// optAs names the identity local commands should be judged as. Empty (the
	// default) leaves local mode exactly as it was: no policy judgement at all.
	// Set, it makes the CLI adopt the same judgements the server would make for
	// that operator instead of being the one path that skips them.
	optAs string
)

// rootCmd is the cobra root command. It is exported only within the package so
// that tests can drive it without going through os.Args.
var rootCmd = &cobra.Command{
	Use:   "levee",
	Short: "LEVEE - 非云原生基础设施变更流水线引擎",
	Long: "LEVEE (Lifecycle Enforcement & Verification Engine)\n" +
		"面向非云原生基础设施的变更流水线引擎：计划 -> 审批 -> 分批执行 -> 验证门禁 -> 自动回滚 -> 审计留痕\n" +
		"默认无代理、CLI 优先，与 ArgoCD/Flux 分层互补。",
	SilenceUsage:  true,
	SilenceErrors: true,
}

// init wires up the persistent flags on the root command. It runs once at
// package initialisation time so that callers (main, tests) only need to
// invoke rootCmd.Execute().
func init() {
	pf := rootCmd.PersistentFlags()

	pf.StringVarP(&optConfigPath, "config", "c", "", "配置文件路径 (默认 ~/.levee/config.yaml)")
	pf.BoolVarP(&optJSON, "json", "j", false, "JSON 输出格式，机器可读")
	pf.BoolVarP(&optQuiet, "quiet", "q", false, "静默模式，仅输出对象 ID")
	pf.BoolVarP(&optVerbose, "verbose", "v", false, "详细输出，包含调试日志")
	pf.BoolVar(&optNoColor, "no-color", false, "禁用彩色输出，用于管道 / 日志归档")
	pf.StringVar(&optProfile, "profile", "default", "配置 profile，用于多环境切换")
	pf.StringVar(&optTimeout, "timeout", "30m", "单命令超时，超时退出码 8")
	pf.StringVar(&optAPIURL, "api", "", "后端 API 地址，用于 CLI 直连集群形态 server")
	pf.StringVar(&optAPIToken, "token", "", "API token，CLI 认证用")

	// Dual-mode flags. Naming neither means local (the default, zero-dependency
	// path); naming both is rejected by resolveServiceMode. --server is only
	// consulted in remote mode.
	pf.BoolVar(&optLocal, "local", false, "本地模式，直连 store（与 --remote 二选一；两者都不给时为本地）")
	pf.BoolVar(&optRemote, "remote", false, "远程模式，通过 gRPC 连接 server（当前仅 agent list/show/remove）")
	pf.StringVar(&optServer, "server", "localhost:9090", "gRPC server 地址，仅 --remote 时生效")

	// Refuse --remote on commands that do not consume it. Without this the flag
	// is accepted, ignored, and the command quietly operates on the local store —
	// which looks like success and leaves the operator's changes in a database
	// the server never reads.
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		return validateServiceMode(cmd)
	}

	// --as opt-in enforcement for local mode. It only means something locally:
	// remotely the subject comes from the credential, which is the whole point of
	// the distinction in internal/grpc/subject.go. It is NOT named --actor
	// because the CLI already has an actor concept for audit labels, and the two
	// are deliberately different things (see currentActor).
	pf.StringVar(&optAs, "as", "", "本地模式下以该身份接受策略判定（默认不判；仅影响走 gRPC 层的动作）")

	// Override cobra's default help / version templates so that --json produces
	// structured output as well. The default templates are kept for the
	// human-readable path.
	rootCmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if optJSON {
			// JSON help: emit a structured description of the command tree.
			help := buildHelpJSON(cmd)
			_ = PrintJSON(os.Stdout, help)
			return
		}
		printHumanHelp(cmd)
	})

	// Register the built-in version sub-command.
	RegisterCommand(newVersionCmd())
}

// RegisterCommand adds a sub-command to the root command. Sub-command packages
// call it from their init() so that the binary automatically picks up every
// command linked into the build. This is the recommended cobra pattern for
// self-registering commands and keeps main.go free of import-side-effect
// ordering concerns.
func RegisterCommand(cmd *cobra.Command) {
	rootCmd.AddCommand(cmd)
}

// Execute runs the root command and returns the exit code appropriate for the
// error. It is the single entry point used by main and tests.
func Execute() int {
	if err := rootCmd.Execute(); err != nil {
		// In JSON mode the structured error has already been printed by the
		// command itself; fall back to a generic JSON envelope so that callers
		// always get a JSON document on stdout.
		if optJSON {
			_ = PrintJSON(os.Stdout, map[string]any{
				"data":  nil,
				"meta":  nil,
				"error": map[string]any{"code": 1, "message": err.Error()},
			})
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		return exitCodeFor(err)
	}
	return 0
}

// exitCodeFor maps an error to a stable exit code following the API doc
// chapter 11 table. Unknown errors default to 1.
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	// Look for a trailing "[exit=N]" marker that commands may attach to opt
	// into a specific code without importing the errors package here.
	if idx := strings.LastIndex(err.Error(), "[exit="); idx >= 0 {
		var code int
		if _, e := fmt.Sscanf(err.Error()[idx:], "[exit=%d]", &code); e == nil {
			return code
		}
	}
	return 1
}

// newVersionCmd builds the `levee version` sub-command. The output is either a
// human-readable banner or a JSON document depending on --json.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print LEVEE version information",
		Long: "Print the LEVEE binary version, build time, commit hash and Go " +
			"toolchain version. With --json the output is a structured document. " +
			"With --remote it reports the SERVER's build instead of this binary's.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := fetchVersionInfo(agentRunContext(cmd))
			if err != nil {
				return err
			}
			if optJSON {
				return PrintJSON(os.Stdout, map[string]any{
					"data":  info,
					"meta":  nil,
					"error": nil,
				})
			}
			printVersionHuman(os.Stdout, info)
			return nil
		},
	}
}

// versionInfo is the structured payload returned by `levee version --json`.
type versionInfo struct {
	Version    string `json:"version"`
	BuildTime  string `json:"build_time"`
	GoVersion  string `json:"go_version"`
	CommitHash string `json:"commit_hash"`
	// Source is "server <addr>" in remote mode and empty locally. Excluded from
	// JSON on purpose: the --json document is a contract scripts already parse
	// (four keys), and a human reading the banner is the one who needs to know
	// these numbers came from another machine.
	Source string `json:"-"`
}

// printVersionHuman writes the human-readable version banner.
func printVersionHuman(w io.Writer, info versionInfo) {
	fmt.Fprintf(w, "levee %s\n", info.Version)
	fmt.Fprintf(w, "  build:    %s\n", info.BuildTime)
	fmt.Fprintf(w, "  commit:   %s\n", info.CommitHash)
	fmt.Fprintf(w, "  go:       %s\n", info.GoVersion)
	if info.Source != "" {
		fmt.Fprintf(w, "  source:   %s\n", info.Source)
	}
}

// buildHelpJSON produces a structured representation of a cobra command and its
// sub-commands for `--json` help output.
func buildHelpJSON(cmd *cobra.Command) map[string]any {
	subs := make([]map[string]any, 0, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		if sub.IsAvailableCommand() {
			subs = append(subs, map[string]any{
				"name":   sub.Name(),
				"short":  sub.Short,
				"usage":  sub.UseLine(),
				"hidden": sub.Hidden,
			})
		}
	}
	return map[string]any{
		"name":        cmd.Name(),
		"short":       cmd.Short,
		"long":        cmd.Long,
		"usage":       cmd.UseLine(),
		"subcommands": subs,
	}
}

// printHumanHelp prints the default cobra help text. It delegates to cobra's
// built-in help printer by temporarily restoring the default HelpFunc for the
// duration of the call.
func printHumanHelp(cmd *cobra.Command) {
	// Use cobra's default help template by invoking the parent's HelpFunc
	// directly. We reach the original implementation through the command's
	// own Root().SetHelpFunc; to keep this simple we just print the long
	// description plus usage line and let cobra handle flag listing.
	if cmd.Long != "" {
		fmt.Fprintln(os.Stdout, cmd.Long)
	} else if cmd.Short != "" {
		fmt.Fprintln(os.Stdout, cmd.Short)
	}
	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "Usage:\n  %s\n", cmd.UseLine())
	if len(cmd.Commands()) > 0 {
		fmt.Fprintln(os.Stdout, "\nAvailable Commands:")
		for _, sub := range cmd.Commands() {
			if sub.IsAvailableCommand() {
				fmt.Fprintf(os.Stdout, "  %-12s %s\n", sub.Name(), sub.Short)
			}
		}
	}
	if cmd.HasAvailableFlags() {
		fmt.Fprintln(os.Stdout, "\nFlags:")
		fmt.Fprint(os.Stdout, cmd.Flags().FlagUsagesWrapped(0))
	}
}

// resolveServiceMode translates the --local / --remote flags into a
// serviceMode. The two flags are mutually exclusive: setting both to true is
// an error. When neither is set the default is modeLocal (the safe,
// zero-dependency path).
//
// We tolerate --local=false --remote=false (treat as local) so that users can
// write `--remote=false` in scripts without also having to set --local=true.
func resolveServiceMode() (serviceMode, error) {
	if optLocal && optRemote {
		return 0, fmt.Errorf("--local and --remote are mutually exclusive [exit=2]")
	}
	if optRemote {
		return modeRemote, nil
	}
	return modeLocal, nil
}

// remoteCapableCommands lists the command paths that actually consult the
// server when --remote is set. Everything else runs against the local store, so
// the flag there is refused rather than ignored (see validateServiceMode).
//
// Today: the three registry readers in cmd_agent_remote.go (which dial the
// gRPC client directly) plus `version` / `system version` (which go through
// service_factory). Keep this in step with both; TestRemoteCapabilityListResolves
// fails if an entry stops naming a real command, and
// TestRemoteCapabilityListIsExactlyThese pins the membership so a removed
// command cannot leave its coverage silently smaller.
var remoteCapableCommands = []string{
	"levee agent list",
	"levee agent show",
	"levee agent remove",
	"levee version",
	"levee system version",
	"levee template list",
}

// validateServiceMode is the PersistentPreRunE guard. It resolves the mode once
// and refuses --remote on commands that do not consume it.
//
// It runs before any RunE, which is the point: a refused command never loads the
// configuration and never opens a store, so "the operator asked for the server
// but the command wrote to the local database" cannot happen silently.
func validateServiceMode(cmd *cobra.Command) error {
	mode, err := resolveServiceMode()
	if err != nil {
		return err
	}
	if mode != modeRemote {
		return nil
	}
	// Parent commands and the generated help/completion commands print text and
	// touch nothing; refusing them would be noise, not safety.
	if cmd.Run == nil && cmd.RunE == nil {
		return nil
	}
	if cmd.Name() == "help" || cmd.Name() == "completion" {
		return nil
	}
	path := cmd.CommandPath()
	for _, supported := range remoteCapableCommands {
		if path == supported {
			return nil
		}
	}
	return fmt.Errorf(
		"--remote is not wired for `%s`: only %s read the remote server, every other command runs against the local store; run it without --remote [exit=2]",
		path, strings.Join(remoteCapableCommands, ", "))
}
