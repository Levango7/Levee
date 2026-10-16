// Package runstatus_test contains the cross-language guards. A vocabulary that
// only the Go side knows about is still a vocabulary problem: the web UI needs
// the list in TypeScript (it cannot import Go), and the docs need a table. Both
// used to be hand-written copies, and copies drift — that is how the UI ended up
// querying a `pending_approval` status the backend never produced.
//
// The web side is no longer a copy: internal/docgen writes it from this package,
// so the tests here check the generated artifact and the wiring around it rather
// than re-parsing a transcription and hoping it matches.
package runstatus_test

import (
	"os"
	"path/filepath"
	"regexp"
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

// tsGeneratedList extracts the quoted members of a generated
// `export const NAME = [ 'a', 'b', … ] as const` array, in file order.
//
// It replaces tsUnionMembers, which parsed the hand-written `ChangeStatus` union
// out of levee.ts. That declaration is gone, and the parser with it: the value
// list now has exactly one TypeScript home, and internal/docgen writes it from
// runstatus.All. A missing declaration therefore fails loudly here rather than
// quietly returning nothing, which is the failure this guard's predecessor had
// when the union's shape changed under its regexp.
func tsGeneratedList(t *testing.T, src []byte, name string) []string {
	t.Helper()
	// Anchored on the opener and the closer so the scan cannot reach into the
	// next declaration: an unanchored `[^\]]*` would swallow a sibling array's
	// members and report them as this one's.
	block := regexp.MustCompile(`(?ms)^export const ` + regexp.QuoteMeta(name) +
		` = \[([^\]]*)\] as const`).FindStringSubmatch(string(src))
	require.Len(t, block, 2,
		"could not find `export const %s = […] as const` in the generated vocabulary — "+
			"either the generator stopped emitting it or its output shape changed", name)

	var members []string
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(block[1], -1) {
		members = append(members, m[1])
	}
	require.NotEmpty(t, members, "%s parsed empty in the generated vocabulary — the regexp is blind", name)
	return members
}

// TestWebStatusMirrorMatchesGo is the D-3 mechanism proper, aimed at the
// generated artifact instead of at a hand copy: the TypeScript status list must
// carry exactly the Go vocabulary — no missing status (the UI would render it
// unlabelled) and no extra one (the UI would offer a filter that can never
// match, which is precisely the pending_approval bug: a silently empty list, no
// error anywhere).
//
// Comparing the whole slice, order included, is what makes both directions one
// assertion. Order is not decorative here: it is the generator copying
// runstatus.All verbatim, so a mismatch means the file was edited by hand
// instead of regenerated.
func TestWebStatusMirrorMatchesGo(t *testing.T) {
	src, err := os.ReadFile(repoFile(t, "web/src/types/vocabulary.ts"))
	require.NoError(t, err)

	assert.Equal(t, runstatus.All, tsGeneratedList(t, src, "CHANGE_STATUSES"),
		"web/src/types/vocabulary.ts CHANGE_STATUSES has drifted from internal/runstatus.All; "+
			"add the status to runstatus (with its label/colour), then run `go run ./internal/docgen` — "+
			"never edit the TypeScript list by hand, it is a generated artifact")
}

// TestWebStatusVocabularyIsWiredNotCopied is the other half: the consumers must
// read the generated list rather than keep their own. It exists because moving
// the list is only half of killing a copy — if levee.ts re-declared the union
// beside the generated one, the guard above would stay green while the UI read a
// value that runstatus.All never produced.
func TestWebStatusVocabularyIsWiredNotCopied(t *testing.T) {
	vocab, err := os.ReadFile(repoFile(t, "web/src/types/vocabulary.ts"))
	require.NoError(t, err)
	assert.Contains(t, string(vocab), "export type ChangeStatus = (typeof CHANGE_STATUSES)[number]",
		"the ChangeStatus union must be derived from the generated list; a hand-written union is a second copy")

	// The type the rest of the app imports.
	consumer, err := os.ReadFile(repoFile(t, "web/src/types/levee.ts"))
	require.NoError(t, err)
	// Matched as a property, not as one spelling: `export type { X } from '…'`
	// alone does not bring X into the file's own scope, so the honest form is an
	// `import type` plus an `export type` — and pinning only the re-export form
	// failed the run that fixed it. Either shape is fine; a declaration is not.
	assert.Regexp(t, `(?:import|export)\s+type\s*\{[^}]*\bChangeStatus\b[^}]*\}\s+from\s+'\./vocabulary'`, string(consumer),
		"web/src/types/levee.ts must obtain ChangeStatus from the generated vocabulary (import or re-export)")
	assert.NotRegexp(t, `export type ChangeStatus\s*=`, string(consumer),
		"web/src/types/levee.ts declares its own ChangeStatus union beside the generated one")

	// And no status spelled as a literal there: the file that re-exports the
	// vocabulary must not also re-state it.
	for _, s := range runstatus.All {
		assert.NotContains(t, string(consumer), "'"+s+"'",
			"web/src/types/levee.ts spells the status %q by hand — that is the copy this file exists to remove", s)
	}
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
// underscore statuses in non-test Go files under internal/ AND cmd/, and skips
// this package plus packages that legitimately define their OWN vocabulary:
//
//   - internal/engine (ClosurePhase) — the closure execution phase, which is
//     then MAPPED into a run status; TestClosurePhasesMapToRunStatuses in
//     internal/engine pins that mapping.
//
// internal/metrics used to be exempt here and no longer is: its label
// constants now ALIAS runstatus (the two labels it keeps its own spelling
// for — created / succeeded — are counter-lifecycle names, not run statuses,
// and are not scanned). cmd/ joined the scan for the same reason: the D-3
// note's third leftover copy was the CLI, whose predicates
// (isRollbackableStatus, isTerminalStatus) and mappings (the change executor,
// the audit report) now reference runstatus constants, so a re-spelled
// literal anywhere in the CLI fails here instead of becoming a sixth fork.
//
// Only the two distinctive rollback verdicts are grepped: short names like
// "draft" or "running" appear in unrelated contexts and would be noise.
func TestNoBareRunStatusLiteralsOutsideRunstatus(t *testing.T) {
	distinctive := []string{"rolled_back_partial", "rollback_incomplete"}

	// Packages that own a distinct vocabulary and are covered by their own
	// correspondence tests instead.
	exempt := map[string]string{
		filepath.Join("internal", "engine"): "ClosurePhase — see TestClosurePhasesMapToRunStatuses",
	}

	repoRoot := filepath.Dir(repoFile(t, "internal"))
	var offenders []string
	for _, root := range []string{
		filepath.Join(repoRoot, "internal"),
		filepath.Join(repoRoot, "cmd"),
	} {
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
	}

	assert.Empty(t, offenders,
		"bare run-status literals found; use runstatus constants so the vocabulary has one home:\n"+
			strings.Join(offenders, "\n"))
}

// TestProtoStatusCommentMatchesGo pins the Change.status comment in
// proto/levee.proto to runstatus.All. proto is a copy that cannot import Go,
// and it had already drifted once: the comment cited
// internal/grpc/change_service.go as its source while the vocabulary itself
// had moved to this package, so an integrator reading the contract followed a
// two-hop reference to a state machine that was no longer the authority.
// A new or retired status now has to be reflected in the contract, not
// discovered by whoever reads the comment next.
func TestProtoStatusCommentMatchesGo(t *testing.T) {
	src, err := os.ReadFile(repoFile(t, filepath.Join("proto", "levee.proto")))
	require.NoError(t, err)

	lines := strings.Split(string(src), "\n")
	idx := -1
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "string status = 3;") {
			idx = i
			break
		}
	}
	require.Greater(t, idx, 0, "`string status = 3;` not found in proto/levee.proto")

	// Collect the contiguous comment block directly above the field.
	var block strings.Builder
	for j := idx - 1; j >= 0; j-- {
		trimmed := strings.TrimSpace(lines[j])
		if !strings.HasPrefix(trimmed, "//") {
			break
		}
		block.WriteString(strings.TrimPrefix(trimmed, "//"))
		block.WriteString(" ")
	}
	require.NotEmpty(t, strings.TrimSpace(block.String()),
		"proto/levee.proto documents no status set above `string status = 3;`")

	// Tokenise on anything that is not a lowercase letter or an underscore:
	// the set is spelled slash-separated, so slashes, dots and the capitals of
	// Go identifiers mentioned in the prose all act as separators.
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(block.String(), func(r rune) bool {
		return r != '_' && (r < 'a' || r > 'z')
	}) {
		seen[tok] = true
	}
	require.NotEmpty(t, seen, "no status tokens parsed from the proto status comment")

	for _, s := range runstatus.All {
		assert.Truef(t, seen[s], "proto Change.status comment is missing run status %q", s)
	}
	// Nothing that looks like a status may be listed unless it is one — this
	// is what catches a retired value creeping back into the contract.
	for tok := range seen {
		if strings.Contains(tok, "_") {
			assert.Containsf(t, runstatus.All, tok,
				"proto Change.status comment carries unknown status-like token %q", tok)
		}
	}
}
