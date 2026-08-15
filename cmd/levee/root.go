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

	// Dual-mode flags. --local (default) makes the CLI talk to an in-process
	// store; --remote flips it to gRPC client mode talking to a server
	// reachable at --server. --token supplies the Bearer token used by both
	// the legacy --api path and the new --remote path; it is shared so that
	// users only have one auth flag to remember.
	optLocal  bool
	optRemote bool
	optServer string
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

	// Dual-mode flags. --local is the default and keeps the CLI a single
	// zero-dependency binary; --remote switches to gRPC client mode. The
	// two are mutually exclusive; resolveServiceMode validates this at run
	// time. --server is only consulted in remote mode.
	pf.BoolVar(&optLocal, "local", true, "本地模式，直连 store（默认）")
	pf.BoolVar(&optRemote, "remote", false, "远程模式，通过 gRPC 连接 server")
	pf.StringVar(&optServer, "server", "localhost:9090", "gRPC server 地址，仅 --remote 时生效")

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
			"toolchain version. With --json the output is a structured document.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info := versionInfo{
				Version:    version,
				BuildTime:  buildTime,
				GoVersion:  goVersion,
				CommitHash: commitHash,
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
}

// printVersionHuman writes the human-readable version banner.
func printVersionHuman(w io.Writer, info versionInfo) {
	fmt.Fprintf(w, "levee %s\n", info.Version)
	fmt.Fprintf(w, "  build:    %s\n", info.BuildTime)
	fmt.Fprintf(w, "  commit:   %s\n", info.CommitHash)
	fmt.Fprintf(w, "  go:       %s\n", info.GoVersion)
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
