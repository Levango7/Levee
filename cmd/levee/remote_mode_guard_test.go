// remote_mode_guard_test.go pins the fail-fast half of the dual-mode flags.
//
// The bug this guards against is not "remote mode is incomplete" — it is that
// `--remote` was accepted by every command and consumed by three, so a
// local-only command ran happily against the local store while the operator
// believed they were talking to the server. That is a silent lie with a data
// consequence: runs, approvals and freeze periods land in a database the server
// never reads. The guard turns it into exit 2 before any store is opened.
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findCommand resolves a "levee a b" path against the real command tree, so the
// allowlist is checked against what is registered rather than against its own
// strings.
func findCommand(t *testing.T, path string) *cobra.Command {
	t.Helper()
	parts := strings.Fields(strings.TrimPrefix(path, "levee "))
	cmd, _, err := rootCmd.Find(parts)
	require.NoError(t, err, "名单里的命令解析不到: %s", path)
	require.NotNil(t, cmd)
	require.Equal(t, path, cmd.CommandPath(), "名单条目 %q 没有指向它自己声称的命令", path)
	return cmd
}

func TestRemoteCapabilityListResolves(t *testing.T) {
	require.NotEmpty(t, remoteCapableCommands,
		"空名单会让 --remote 在它真正支持的命令上也被拒")
	for _, path := range remoteCapableCommands {
		findCommand(t, path)
	}
}

// TestRemoteCapabilityListIsExactlyThese is the independent expectation.
// Iterating over the production slice alone cannot catch an entry being removed:
// the loop simply runs one fewer time and still reports green (measured — that
// is how this test came to exist).
func TestRemoteCapabilityListIsExactlyThese(t *testing.T) {
	assert.Equal(t,
		[]string{
			"levee agent list",
			"levee agent show",
			"levee agent remove",
			"levee version",
			"levee system version",
			"levee system status",
			"levee template list",
			"levee template show",
			"levee target list",
			"levee audit verify",
		},
		remoteCapableCommands,
		"名单变了就要在这里说清楚：谁能真的走 --remote，谁只是被误加进来")
}

func TestValidateServiceModeAllowsRemoteCapableCommands(t *testing.T) {
	for _, path := range remoteCapableCommands {
		t.Run(path, func(t *testing.T) {
			defer resetRootFlags()
			cmd := findCommand(t, path)
			optRemote = true
			require.NoError(t, validateServiceMode(cmd))
		})
	}
}

func TestValidateServiceModeRefusesLocalOnlyCommand(t *testing.T) {
	defer resetRootFlags()
	cmd := findCommand(t, "levee list")
	optRemote = true

	err := validateServiceMode(cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not wired for `levee list`")
	assert.Contains(t, err.Error(), "[exit=2]")
	assert.Equal(t, 2, exitCodeFor(err))
}

func TestValidateServiceModeRefusesBothFlagsEvenForRemoteCommand(t *testing.T) {
	defer resetRootFlags()
	cmd := findCommand(t, remoteCapableCommands[0])
	optLocal, optRemote = true, true

	err := validateServiceMode(cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestValidateServiceModeIsSilentInLocalMode(t *testing.T) {
	defer resetRootFlags()
	require.NoError(t, validateServiceMode(findCommand(t, "levee version")))
}

// TestRemoteFlagIsRefusedBeforeTheStoreOpens drives the real root command, so it
// also proves the guard is wired into PersistentPreRunE rather than merely
// defined. The temporary config is what makes "no database was created" safe to
// assert: if the guard regresses, the command writes into t.TempDir() instead of
// the developer's own ~/.levee/data/levee.db.
func TestRemoteFlagIsRefusedBeforeTheStoreOpens(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dataDir, "levee.db")
	cfg := "server:\n  data_dir: " + filepath.ToSlash(dataDir) +
		"\ndatabase:\n  driver: sqlite\n  path: " + filepath.ToSlash(dbPath) + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	// The directory must exist, or a regressed guard fails on "unable to open
	// database file" and the NoFileExists assertion below stops being the thing
	// that catches it.
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	_, _, err := runSplit(t, "--config", cfgPath, "--remote", "list")
	require.Error(t, err, "--remote 在只有本地语义的 list 上必须失败")
	assert.Contains(t, err.Error(), "not wired for `levee list`")
	assert.Equal(t, 2, exitCodeFor(err), "必须是用法错误码 2，不是笼统的 1")
	assert.NoFileExists(t, dbPath, "被拒的命令不该打开（更不该创建）本地库")
}

// TestRemoteFlagHelpStaysAlignedWithTheCapabilityList closes the loop the list
// tests leave open: the list is pinned, so a command can leave it only
// deliberately — but until 2026-11-02 the --remote help text was a hand-written
// second copy that said "当前仅 agent list/show/remove" three commands after
// the list had grown to six. cli-reference.md was right; `levee --help` lied.
// An operator reads the help text first, so a stale one is the version that
// gets acted on.
//
// The production side now renders the usage from the list (remoteFlagUsage).
// This test still earns its place by asserting the two things rendering could
// get wrong later: every listed command is actually NAMED in the help, and the
// help names no command the list does not contain. A future edit that
// reintroduces a hardcoded string fails the second assertion; one that drops a
// consumer from the rendering fails the first.
func TestRemoteFlagHelpStaysAlignedWithTheCapabilityList(t *testing.T) {
	f := rootCmd.PersistentFlags().Lookup("remote")
	require.NotNil(t, f, "the --remote persistent flag must exist")
	usage := f.Usage

	require.NotEmpty(t, remoteCapableCommands, "an empty list would render an unusable help string")
	for _, path := range remoteCapableCommands {
		short := strings.TrimPrefix(path, "levee ")
		assert.Contains(t, usage, short,
			"the --remote help must name %s; the capability list is the source of truth for what the flag consumes", short)
	}

	// The help must not claim a command the guard would refuse. Naming one is
	// the same failure the hand-written string had: it told operators to try
	// something that exits 2. This parses the rendered consumer list back out
	// of the string, so a re-hardcoded usage naming, say, "target list" fails
	// here even while the first loop above is still green.
	rendered := renderedRemoteConsumers(t, usage)
	require.NotEmpty(t, rendered, "the usage must render at least one consumer")
	for _, name := range rendered {
		assert.Contains(t, remoteCapableCommands, "levee "+name,
			"the --remote help names %q, which the capability list does not contain — "+
				"either the guard must learn the command or the help must stop advertising it", name)
	}
	// ...and the wording must not regress to an exclusive claim. "当前仅" was the
	// exact phrase that went stale; the rendered string says "当前由 ... 消费".
	assert.NotContains(t, usage, "当前仅",
		"the help must not state an exhaustive small set in prose — the set grows, prose does not")
	assert.Contains(t, usage, "被拒绝",
		"the help must still warn that other commands refuse the flag rather than silently running locally")
}

// renderedRemoteConsumers extracts the command names the --remote help advertises,
// by reading the segment the production renderer wraps around the list. It is a
// test-only parser for a test-only purpose: proving the help names exactly what
// the list names, without trusting that the string was rendered in the first
// place.
func renderedRemoteConsumers(t *testing.T, usage string) []string {
	t.Helper()
	open := strings.Index(usage, "当前由 ")
	require.GreaterOrEqual(t, open, 0, "the usage must introduce its consumer list (当前由 …)")
	rest := usage[open+len("当前由 "):]
	end := strings.Index(rest, " 消费")
	require.GreaterOrEqual(t, end, 0, "the consumer list must be terminated (… 消费)")
	body := strings.TrimSpace(rest[:end])
	var out []string
	for _, part := range strings.Split(body, "、") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TestRemoteConsumersInCliReferenceMatchTheList pins the SECOND place operators
// read the consumer list: docs/cli-reference.md's global-options table. root.go's
// help is now rendered from the list (so it cannot drift), but that table is
// hand-written — and it is the first thing an operator opening the reference
// sees, so a stale one misleads in exactly the way the flag help did.
//
// It matters on a schedule: PR #272 (target list / audit verify / template show)
// will lengthen remoteCapableCommands, and whoever lands it has to remember this
// table too. The list guard turns that into a red test rather than a note.
//
// The parser understands the one shape this row uses — backticked groups whose
// later parts share the first part's prefix ("`agent list / show / remove`",
// "`version` / `system version`"). It is deliberately small and would rather fail
// loudly than silently parse nothing: an empty result is a failed assertion, not
// a green one.
func TestRemoteConsumersInCliReferenceMatchTheList(t *testing.T) {
	row := cliReferenceFlagRow(t, "--remote")

	// Both directions reconcile the same two sets: what the row names, and what
	// the capability list holds. Comparing like with like is what makes a single
	// parser enough — and it is why the row's shared-prefix spelling
	// ("`agent list / show / remove`") can be reconciled without a second
	// substring rule that would pass "version" for "system version".
	named := backtickedCommandNames(row)
	require.NotEmpty(t, named, "the --remote row must name at least one consumer; an empty parse means the parser or the row broke")

	for _, path := range remoteCapableCommands {
		short := strings.TrimPrefix(path, "levee ")
		assert.Contains(t, named, short,
			"docs/cli-reference.md must name %q as a --remote consumer (the capability list is the source of truth); "+
				"row names: %v", short, named)
	}
	for _, name := range named {
		assert.Contains(t, remoteCapableCommands, "levee "+name,
			"docs/cli-reference.md advertises %q for --remote, which the capability list does not contain — "+
				"either the guard must learn it or the table must stop claiming it", name)
	}
}

// cliReferenceFlagRow returns the markdown table row whose first cell names the
// flag, so a moved flag still finds its row and a removed flag fails the test.
func cliReferenceFlagRow(t *testing.T, flag string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli-reference.md"))
	require.NoError(t, err, "docs/cli-reference.md must be readable from the module root")
	want := "`" + flag + "`"
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "| "+want+" ") {
			return line
		}
	}
	t.Fatalf("no global-options row for %s in docs/cli-reference.md", flag)
	return ""
}

// backtickedCommandNames expands the row's backticked command groups into flat
// command names. A later part shares the first part's prefix unless the first
// part has no prefix of its own (a bare "version"), which is exactly how the row
// writes both shapes.
//
// Spans that are not commands are dropped: the same row also backticks flags
// (`--server`) and exit-code notation (`[exit=2]`), and neither names a command.
// The filter is by shape, so a new notation fails this test rather than being
// silently ignored.
func backtickedCommandNames(row string) []string {
	var out []string
	for _, m := range backtickRE.FindAllStringSubmatch(row, -1) {
		body := m[1]
		// Body-level: drop flag and notation spans before any expansion, so a
		// "/" separator inside a command group survives to be expanded.
		if body == "" || strings.HasPrefix(body, "-") || strings.HasPrefix(body, "[") ||
			strings.HasPrefix(body, "<") {
			continue
		}
		// Drop a parenthetical aside ("（读 daemon 注册表）") — it explains the
		// command, it is not part of its name.
		if i := strings.IndexAny(body, "（("); i >= 0 {
			body = body[:i]
		}
		parts := strings.Split(body, "/")
		for i, p := range parts {
			p = strings.TrimSpace(p)
			if !looksLikeCommandName(p) {
				continue
			}
			if i == 0 {
				out = append(out, p)
				continue
			}
			fields := strings.Fields(parts[0])
			if len(fields) > 1 {
				out = append(out, strings.Join(fields[:len(fields)-1], " ")+" "+p)
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// backtickRE matches a single-backticked span. Flags like `--server` are ignored
// by the caller's filtering: they start with a dash and never equal a command path.
var backtickRE = regexp.MustCompile("`([^`]+)`")

// looksLikeCommandName is the per-part shape filter, kept separate because the
// same row also carries a bare `false` (the default-value column) that is neither
// flag nor command. A command name in this table is lowercase words joined by a
// hyphen or space ("version", "agent list", "template list"); anything else is
// notation and gets dropped before it can masquerade as a command.
func looksLikeCommandName(name string) bool {
	if name == "" {
		return false
	}
	switch name {
	case "false", "true", "auto", "table", "json":
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "[") || strings.HasPrefix(name, "<") {
		return false
	}
	for _, r := range name {
		// Space joins the words, hyphen joins one word. Everything else must be
		// a lowercase ASCII letter.
		if r == ' ' || r == '-' {
			continue
		}
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}
