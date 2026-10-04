package main

// cli_reference_flags_test.go is the gate that keeps docs/cli-reference.md and
// quickstart.md naming flags that actually exist.
//
// It exists because the reference documented `levee push send --deep-link <url>`
// for a command that registers only --user/--title/--body: a reader copy-pastes
// the line, cobra answers "unknown flag", and nothing in CI ever noticed because
// no test had ever opened the docs and compared them against the command tree.
// The same class caught a Helm chart passing a flag the binary does not define.
//
// Direction matters: docs may under-document flags (a long tail), but they must
// never document a flag that is not registered.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// docFlagRE matches a `--name` token (optionally with =value) in prose.
var docFlagRE = regexp.MustCompile(`--([a-z0-9][a-z0-9-]*)`)

// docUsageRE matches a command line inside the references: it must start with
// `levee` at the beginning of a line or after a backtick.
var docUsageRE = regexp.MustCompile(`(?m)^\s*(?:\$\s*)?levee([^\n]*)$`)

func TestDocumentedFlagsAreRegistered(t *testing.T) {
	require.NotNil(t, rootCmd)

	type finding struct {
		file, cmd string
		line      int
		flag      string
		why       string
	}
	var findings []finding
	checked := 0

	for _, doc := range []string{
		filepath.Join("..", "..", "docs", "cli-reference.md"),
		filepath.Join("..", "..", "docs", "quickstart.md"),
	} {
		src, err := os.ReadFile(doc)
		require.NoError(t, err, "the docs the gate reads must exist: %s", doc)

		for i, line := range strings.Split(string(src), "\n") {
			uses := docUsageRE.FindAllStringSubmatch(line, -1)
			for _, u := range uses {
				args := parseDocArgs(u[1])
				if len(args) == 0 {
					continue
				}
				cmd, walked, flags := descend(rootCmd, args)
				if cmd == nil {
					findings = append(findings, finding{
						file: filepath.Base(doc), cmd: strings.Join(walked, " "),
						line: i + 1, flag: "-", why: "the documented command does not exist in the cobra tree",
					})
					continue
				}
				for _, f := range flags {
					checked++
					if lookupFlag(cmd, f) {
						continue
					}
					findings = append(findings, finding{
						file: filepath.Base(doc),
						cmd:  cmd.CommandPath(), line: i + 1,
						flag: f,
						why:  "documented but not registered (" + strings.Join(knownFlags(cmd), ", ") + ")",
					})
				}
			}
		}
	}

	for _, f := range findings {
		t.Errorf("%s:%d: %s: %s — %s", f.file, f.line, f.cmd, f.flag, f.why)
	}
	assert.GreaterOrEqual(t, checked, 40,
		"the scan must keep covering the references; a regex that matches nothing would make "+
			"this gate vacuously green")
	sort.Slice(findings, func(i, j int) bool { return findings[i].cmd < findings[j].cmd })
	if len(findings) == 0 {
		t.Logf("cross-checked %d documented flag mentions against the cobra tree", checked)
	}
}

// parseDocArgs strips brackets, defaults and placeholders from a documented
// invocation, leaving ordered tokens.
func parseDocArgs(s string) []string {
	s = strings.ReplaceAll(s, "[", " ")
	s = strings.ReplaceAll(s, "]", " ")
	s = strings.ReplaceAll(s, "<", " ")
	s = strings.ReplaceAll(s, ">", " ")
	s = strings.ReplaceAll(s, "`", " ")
	var out []string
	for _, tok := range strings.Fields(s) {
		if strings.HasPrefix(tok, "#") || tok == "\\" {
			break
		}
		out = append(out, strings.SplitN(tok, "=", 2)[0])
	}
	return out
}

// descend walks the cobra tree consuming the leading tokens that name
// sub-commands, and returns the resolved command plus every flag token found
// from that point on. Tokens that name neither a child nor a flag are positional
// arguments (a documented `run-abc123`, a placeholder like `STATUS`), so they
// neither fail the walk nor are checked — the docs may under-specify arguments,
// they may not invent flags.
func descend(root *cobra.Command, args []string) (*cobra.Command, []string, []string) {
	cmd := root
	var walked, flags []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "-"):
			// "[--strict|--lenient]" documents a choice: score each side.
			// The leading dashes must come off every alternative, not just
			// the first — otherwise the right-hand side is looked up as the
			// name "--lenient" and a registered flag is reported missing.
			name := strings.TrimLeft(a, "-")
			for _, part := range strings.Split(name, "|") {
				part = strings.TrimLeft(part, "-")
				if part != "" {
					flags = append(flags, part)
				}
			}
		default:
			if next, ok := childByName(cmd, a); ok {
				cmd = next
				walked = append(walked, a)
			}
		}
	}
	return cmd, walked, flags
}

func childByName(parent *cobra.Command, name string) (*cobra.Command, bool) {
	for _, c := range parent.Commands() {
		if c.Name() == name || c.Use == name || hasAlias(c, name) {
			return c, true
		}
	}
	return nil, false
}

func hasAlias(c *cobra.Command, name string) bool {
	for _, a := range c.Aliases {
		if a == name {
			return true
		}
	}
	return false
}

// lookupFlag resolves a flag on the command or its inherited (persistent)
// ancestors, which is how cobra itself resolves it at runtime. Nothing is
// hard-coded: the root's persistent flags reach sub-commands through
// InheritedFlags, so a copied list of "global flags" cannot drift from reality.
func lookupFlag(cmd *cobra.Command, name string) bool {
	if cmd.Flags().Lookup(name) != nil || cmd.InheritedFlags().Lookup(name) != nil {
		return true
	}
	// A documented shorthand (`-f` for --follow) is the same flag: cobra
	// registers it as `Shorthand`, not as a name.
	if len(name) == 1 {
		return cmd.Flags().ShorthandLookup(name) != nil || cmd.InheritedFlags().ShorthandLookup(name) != nil
	}
	return false
}

func knownFlags(cmd *cobra.Command) []string {
	var out []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) { out = append(out, "--"+f.Name) })
	sort.Strings(out)
	return out
}

// --- delivery artifacts ----------------------------------------------------

// helmArgRE matches one element of a container `args:` list in the chart
// templates: a sequence item whose whole value is a CLI flag name.
var helmArgRE = regexp.MustCompile(`(?m)^\s*-\s+(--[a-z0-9][a-z0-9-]*)\s*$`)

// systemdFlagRE matches a flag on an ExecStart continuation line.
var systemdFlagRE = regexp.MustCompile(`^\s+(-{1,2}[a-z][a-z0-9-]*)`)

type artifactFlag struct {
	file, line, flag string
}

// TestDeliveryArtifactsPassRegisteredServeFlags is the reason the Helm chart
// could not keep shipping `--cluster-dispatch-worker-capacity`: the binary
// registers `--cluster-dispatch-capacity`, so every pod the chart rendered
// exited on "unknown flag" and nothing read the args list against the command
// tree. Docs are prose an operator can sanity-check; a rendered Deployment is
// a machine that crash-loops until someone checks the logs.
func TestDeliveryArtifactsPassRegisteredServeFlags(t *testing.T) {
	serve := findServeCommand(t)

	var flags []artifactFlag
	patterns := []string{
		filepath.Join("..", "..", "deploy", "helm", "levee", "templates", "*.yaml"),
		filepath.Join("..", "..", "deploy", "helm", "levee", "templates", "tests", "*.yaml"),
	}
	for _, pattern := range patterns {
		paths, err := filepath.Glob(pattern)
		require.NoError(t, err)
		for _, p := range paths {
			flags = append(flags, scanFile(p, helmArgRE, artifactLineRaw)...)
		}
	}
	systemdPath := filepath.Join("..", "..", "deploy", "systemd", "levee.service")
	flags = append(flags, scanExecStartBlock(systemdPath)...)

	seen := map[string]bool{}
	checked := 0
	for _, f := range flags {
		key := f.file + ":" + f.flag
		if seen[key] {
			continue
		}
		seen[key] = true
		checked++
		if lookupFlag(serve, strings.TrimLeft(f.flag, "-")) {
			continue
		}
		t.Errorf("%s: %s passes %s to `levee serve`, which does not register it (the pod would exit "+
			"with \"unknown flag\")", f.file, f.line, f.flag)
	}
	assert.GreaterOrEqual(t, checked, 15,
		"the artifact scan must keep covering the delivered args lists; a regex matching nothing "+
			"would make this gate vacuously green")
}

// scanFile applies re to every line of path and reports each match. keep
// decides whether a matched line is data or prose (chart comments legitimately
// discuss flags they do not pass).
func scanFile(path string, re *regexp.Regexp, keep func(string, string) bool) []artifactFlag {
	src, err := os.ReadFile(path)
	if err != nil {
		// A missing optional artifact directory is a real finding, but only
		// for files the delivery kit is expected to ship; callers glob these
		// paths, so no match is simply no flag.
		return nil
	}
	var out []artifactFlag
	for _, line := range strings.Split(string(src), "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil || !keep(line, m[1]) {
			continue
		}
		out = append(out, artifactFlag{file: filepath.Base(path), line: strings.TrimSpace(line), flag: m[1]})
	}
	return out
}

func artifactLineRaw(_ string, _ string) bool { return true }

// scanExecStartBlock reads only the ExecStart invocation of a systemd unit.
// Scanning the whole file would judge the header comments, which list example
// extra flags an operator may set in $LEVEE_SERVE_EXTRA — prose, not args.
func scanExecStartBlock(path string) []artifactFlag {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []artifactFlag
	inBlock := false
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			inBlock = true
		} else if !inBlock {
			continue
		}
		if m := systemdFlagRE.FindStringSubmatch(line); m != nil {
			out = append(out, artifactFlag{file: filepath.Base(path), line: strings.TrimSpace(line), flag: m[1]})
		}
		if !strings.HasSuffix(strings.TrimSpace(line), "\\") {
			inBlock = false
		}
	}
	return out
}

// findServeCommand resolves `serve` from the live command tree rather than a
// copied flag list, so renaming or adding a serve flag needs no test edit.
func findServeCommand(t *testing.T) *cobra.Command {
	t.Helper()
	for _, c := range rootCmd.Commands() {
		if c.Name() == "serve" {
			return c
		}
	}
	t.Fatal("the cobra tree has no `serve` command; the artifact gate has nothing to check against")
	return nil
}
