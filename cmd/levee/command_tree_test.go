package main

// command_tree_test.go holds the ledgers' claims about CLI command *paths*.
//
// `cli_reference_flags_test.go` checks one direction of the doc/tree relation:
// every command and flag the references name must exist. This file holds the
// other half — commands that must NOT exist — which the ledgers assert in prose
// and no gate could see. Two of them are claims about the *absence* of a path,
// and a keyword search cannot hold them because the same word legitimately
// exists elsewhere in the tree:
//
//   - "levee schedule（cron 定时触发变更）定档：不做" (docs/blocked-items.md E-4)
//     — but `schedule` exists as `drift schedule`.
//   - "drift scan / drift show <report-id> 从未注册" (docs/levee-api.md §6)
//     — but `show` exists as `drift baseline show`.
//
// Only the tree can tell a top-level `schedule` from a nested one, so these
// claims are asserted against `rootCmd` rather than by grepping the sources.

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commandClaim is one ledger assertion about a command path.
type commandClaim struct {
	ledger string   // the live doc that makes the claim
	what   string   // what it asserts, for the failure message
	path   []string // the command path from the root
	absent bool     // true: the path must NOT resolve; false: it must
}

var commandClaims = []commandClaim{
	// Present: the drift family the API reference documents.
	{"docs/levee-api.md", "drift detect 已注册", []string{"drift", "detect"}, false},
	{"docs/levee-api.md", "drift report 已注册", []string{"drift", "report"}, false},
	{"docs/levee-api.md", "drift baseline 子命令族在", []string{"drift", "baseline", "list"}, false},
	{"docs/levee-api.md", "drift schedule 子命令族在", []string{"drift", "schedule", "run"}, false},
	// Absent: commands the ledgers record as never registered / decided against.
	{"docs/blocked-items.md", "levee schedule（顶层）定档不做", []string{"schedule"}, true},
	{"docs/levee-api.md", "levee delegate 从未实现", []string{"delegate"}, true},
	{"docs/levee-api.md", "levee rollback-all 从未实现", []string{"rollback-all"}, true},
	{"docs/levee-api.md", "drift scan 从未注册", []string{"drift", "scan"}, true},
	{"docs/levee-api.md", "drift show <report-id> 从未注册", []string{"drift", "show"}, true},
}

// resolvesCommand walks the tree by child name/alias and reports whether the
// whole path resolves. It shares `childByName` with the flag guard so the two
// gates cannot disagree about what "a command exists" means.
func resolvesCommand(root *cobra.Command, path []string) bool {
	cmd := root
	for _, name := range path {
		next, ok := childByName(cmd, name)
		if !ok {
			return false
		}
		cmd = next
	}
	return true
}

func TestCommandTreeClaims(t *testing.T) {
	require.GreaterOrEqual(t, len(commandClaims), 8,
		"the command-claim table must not shrink silently")
	for _, c := range commandClaims {
		got := resolvesCommand(rootCmd, c.path)
		joined := strings.Join(c.path, " ")
		if c.absent {
			assert.Falsef(t, got, "%s: `levee %s` must NOT exist — %s", c.ledger, joined, c.what)
		} else {
			assert.Truef(t, got, "%s: `levee %s` must exist — %s", c.ledger, joined, c.what)
		}
	}
}

// TestCommandTreeResolverSeesBothDirections keeps the resolver honest with
// paired samples: a resolver that always returned true (or false) would pass
// every present claim (or every absent one) and look green.
func TestCommandTreeResolverSeesBothDirections(t *testing.T) {
	// Resolves.
	assert.True(t, resolvesCommand(rootCmd, []string{"version"}))
	assert.True(t, resolvesCommand(rootCmd, []string{"drift", "baseline", "list"}))
	// Does not resolve.
	assert.False(t, resolvesCommand(rootCmd, []string{"definitely-not-a-command"}))
	assert.False(t, resolvesCommand(rootCmd, []string{"drift", "baseline", "no-such-sub"}))

	// The reason this file exists: the same word is present nested but absent
	// at the level the ledger's claim is about.
	assert.True(t, resolvesCommand(rootCmd, []string{"drift", "baseline", "show"}),
		"`show` exists as `drift baseline show`")
	assert.False(t, resolvesCommand(rootCmd, []string{"drift", "show"}),
		"`drift show` is the unregistered command the API reference records")
	assert.True(t, resolvesCommand(rootCmd, []string{"drift", "schedule"}),
		"`schedule` exists as `drift schedule`")
	assert.False(t, resolvesCommand(rootCmd, []string{"schedule"}),
		"a top-level `levee schedule` is the one blocked-items.md E-4 decides against")
}
