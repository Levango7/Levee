// Package runstatus_test contains the cross-language guards. A vocabulary that
// only the Go side knows about is still a vocabulary problem: the web UI keeps
// a hand-written TypeScript mirror (it cannot import Go), and the docs keep
// hand-written tables. Both are copies, and copies drift — that is how the UI
// ended up querying a `pending_approval` status the backend never produced.
package runstatus_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/runstatus"
)

// repoFile reads a file relative to the repository root, walking up from the
// test's working directory so the test works from any package depth.
func repoFile(t *testing.T, rel string) string {
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

// tsUnionMembers extracts the quoted members of a named TypeScript union type.
//
// A TypeScript union alias needs no terminating semicolon, so matching up to
// the first ";" runs off the end of the declaration and swallows every type
// that follows — this guard's first version duly reported Priority and
// TargetType values as run statuses. Instead: take the declaration's own
// lines (they all start with "|" after the first) and stop at the first line
// that does not.
func tsUnionMembers(t *testing.T, src []byte, name string) []string {
	t.Helper()
	text := string(src)
	start := regexp.MustCompile(`(?m)^export type ` + regexp.QuoteMeta(name) + `\s*=`).FindStringIndex(text)
	require.Len(t, start, 2, "could not find `export type %s =` in levee.ts", name)

	var members []string
	rest := text[start[1]:]
	for _, line := range strings.Split(rest, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "|") {
			break
		}
		for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(trimmed, -1) {
			members = append(members, m[1])
		}
	}
	require.NotEmpty(t, members, "no members parsed for the %s union", name)
	return members
}

// TestWebStatusMirrorMatchesGo is the D-3 mechanism proper: the TypeScript
// union in web/src/types/levee.ts must carry exactly the Go vocabulary — no
// missing status (the UI would render it unlabelled) and no extra one (the UI
// would offer a filter that can never match, which is precisely the
// pending_approval bug: a silently empty list, no error anywhere).
func TestWebStatusMirrorMatchesGo(t *testing.T) {
	src, err := os.ReadFile(repoFile(t, "web/src/types/levee.ts"))
	require.NoError(t, err)

	tsUnion := map[string]bool{}
	for _, m := range tsUnionMembers(t, src, "ChangeStatus") {
		tsUnion[m] = true
	}
	require.NotEmpty(t, tsUnion, "ChangeStatus union parsed empty — the regexp is stale")

	var goSet, tsSet []string
	goSet = append(goSet, runstatus.All...)
	for s := range tsUnion {
		tsSet = append(tsSet, s)
	}
	sort.Strings(goSet)
	sort.Strings(tsSet)

	assert.Equal(t, goSet, tsSet,
		"web/src/types/levee.ts ChangeStatus has drifted from internal/runstatus.All; "+
			"add the status to runstatus (with its label/colour) rather than to the UI only")
}

// TestSpecStatusTableMatchesGo checks the documentation table. Prose is the
// copy most likely to rot, because nothing fails when it is wrong — an
// operator simply follows a table that describes a system that no longer
// exists.
func TestSpecStatusTableMatchesGo(t *testing.T) {
	src, err := os.ReadFile(repoFile(t, "docs/leveelang-spec.md"))
	require.NoError(t, err)

	// Collect the table by line scanning rather than one big regex: the
	// section is followed by a blockquote AND a blank line before the table,
	// which every regex shape tried here either skipped past or truncated.
	//
	// Scanning must start AT section 0.3 — the document opens with a metadata
	// table, and a whole-file scan collects that one instead.
	text := string(src)
	start := strings.Index(text, "### 0.3 run 状态")
	require.NotEqual(t, -1, start, "run status section (### 0.3) not found in docs/leveelang-spec.md")

	var rows []string
	inTable := false
	for _, line := range strings.Split(text[start:], "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "| ") {
			inTable = true
			rows = append(rows, trimmed)
			continue
		}
		if inTable && trimmed == "" {
			break // end of the table block
		}
	}
	require.Greater(t, len(rows), 2, "run status table not found in docs/leveelang-spec.md (### 0.3)")

	docSet := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(strings.Join(rows, "\n"), -1) {
		docSet[m[1]] = true
	}

	for _, s := range runstatus.All {
		assert.True(t, docSet[s], "run status %q is missing from the leveelang-spec.md table", s)
	}
	// The retired value must not reappear in the authoritative table.
	assert.False(t, docSet["pending_approval"])
}

// TestNoBareRunStatusLiteralsOutsideRunstatus is the ratchet that keeps this
// from rotting again. New code must reference runstatus constants rather than
// re-spelling a status, because a re-spelled literal is a new copy.
//
// The scan is intentionally narrow: it looks for the distinctive multi-word /
// underscore statuses in non-test Go files under internal/, and skips this
// package plus files that legitimately define their OWN vocabulary:
//
//   - internal/engine (ClosurePhase) — the closure execution phase, which is
//     then MAPPED into a run status; TestClosurePhasesMapToRunStatuses in
//     internal/engine pins that mapping.
//   - internal/metrics — counter label values, a deliberately different
//     vocabulary (it has created/succeeded, which are not run statuses).
//
// Only the two distinctive rollback verdicts are grepped: short names like
// "draft" or "running" appear in unrelated contexts and would be noise.
func TestNoBareRunStatusLiteralsOutsideRunstatus(t *testing.T) {
	distinctive := []string{"rolled_back_partial", "rollback_incomplete"}

	// Packages that own a distinct vocabulary and are covered by their own
	// correspondence tests instead.
	exempt := map[string]string{
		filepath.Join("internal", "engine"):  "ClosurePhase — see TestClosurePhasesMapToRunStatuses",
		filepath.Join("internal", "metrics"): "counter labels — a separate vocabulary by design",
	}

	root := repoFile(t, "internal")
	repoRoot := filepath.Dir(root)

	var offenders []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || strings.Contains(path, "runstatus") {
			return nil
		}
		rel, rerr := filepath.Rel(repoRoot, filepath.Dir(path))
		if rerr == nil {
			if _, ok := exempt[rel]; ok {
				return nil
			}
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, status := range distinctive {
				if strings.Contains(line, `"`+status+`"`) {
					offenders = append(offenders, path+" :: "+trimmed)
				}
			}
		}
		return nil
	})

	assert.Empty(t, offenders,
		"bare run-status literals found; use runstatus constants so the vocabulary has one home:\n"+
			strings.Join(offenders, "\n"))
}
